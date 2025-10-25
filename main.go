package main

// A lightweight CLI tool written in Go to read text files,
// apply optional line filtering using regular expressions
// and optionally tail new lines in real-time.
//
// Calling it only with a file -file="file path" path works as cat
// Calling it with file path -file="file path" and filter -filter="text" works as cat | grep
// Calling it with file path, filter and -t works as cat | grep tail
// file and filter flags can be also pass as positional args, file always 1st and filter 2nd.
// Usage:
//
//	see /path to file "INFO" -t
//	see /path to file "^ERROR"
//
// Build info. To embed version and build date, build with for example:
//
//   # Windows (PowerShell)
//   $version = "v1.0.0"
//   $buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
//   go build -ldflags "-X main.version=$version -X main.buildDate=$buildDate" -o see.exe
//
//   # Linux / macOS (bash/zsh)
//   VERSION="v1.0.0"
//   DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
//   go build -ldflags "-X main.version=$VERSION -X main.buildDate=$DATE" -o see

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	version   = "dev"
	buildDate = "unknown"
)

func main() {

	var wg sync.WaitGroup
	wg.Add(1)

	// Override the default usage output
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options] <file> [filter]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Notes:
  - The <file> argument can be provided as a positional argument or with -file flag.
  - The filter argument can be provided as a positional argument or with -filter flag.
  - Flags override positional arguments.
  - Examples:

    # Just read a file (cat)
    see logfile.txt

    # Read a file and filter lines containing "ERROR"
    see logfile.txt "ERROR"
    see -file logfile.txt -filter "ERROR"

    # Tail a file with filter
    see logfile.txt "ERROR" -t
    see -file logfile.txt -filter "ERROR" -t

Filter supports basic regular expressions:
  - "ERROR"      : any line containing ERROR
  - "^INFO"      : lines starting with INFO
  - "timeout$"   : lines ending with timeout
  - ".*ERR.*"    : lines containing ERR anywhere`)
	}

	// command line flags
	versionFlag := flag.Bool("version", false, "Show version info and exit")
	filePathPtr := flag.String("file", "", "Path to the log file to watch")
	filterPtr := flag.String("filter", "", "Optional regex or string literal to filter lines")
	tailPtr := flag.Bool("t", false, "Optional -t to tail the file")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("see version %s (built %s)\n", version, buildDate)
		return
	}

	args := flag.Args()

	// Detect -t when passed as positional at the end (e.g. see file "filter" -t)
	for _, a := range args {
		if a == "-t" || a == "--t" {
			*tailPtr = true
		}
	}

	// validate input
	if *filePathPtr == "" {
		if len(args) > 0 {
			*filePathPtr = args[0]
			args = args[1:] // remove the filepath from args
		} else {
			log.Fatal("Please provide a file path either as -file or as first argument")
		}
	}

	// Handle positional filter
	if *filterPtr == "" && len(args) > 0 {
		*filterPtr = args[0]
	}

	// pre check filepath
	info, err := os.Stat(*filePathPtr)
	if err != nil {
		log.Fatalf("Cannot access file: %v", err)
	}
	if info.IsDir() {
		log.Fatalf("Expected a file, but got a directory")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		cancel()
	}()

	go see(ctx, &wg, *filterPtr, *filePathPtr, *tailPtr)

	wg.Wait()
}

// see reads the given file, optionally filtering lines with a regex.
// If tail mode (-t) is enabled, it continues watching for new lines.
func see(ctx context.Context, wg *sync.WaitGroup, filterPtr string, filePathPtr string, tailPtr bool) {

	defer wg.Done()

	var (
		filterRegex *regexp.Regexp
	)

	if filterPtr != "" {
		var err error
		filterRegex, err = regexp.Compile(filterPtr)
		if err != nil {
			log.Fatalf("Invalid regex pattern: %v", err)
		}
	}

	// open file
	file, err := os.Open(filePathPtr)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	// create a scanner to read lines
	scanner := bufio.NewScanner(file)

	const maxLine = 1024 * 1024
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxLine)

	// read existing lines
	fmt.Println("Reading existing lines...")
	for scanner.Scan() {
		line := scanner.Text()
		if filterRegex == nil || filterRegex.MatchString(line) {
			fmt.Println(line)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading file: %v", err)
		return // stop the goroutine and return to main
	}

	// record current file position from the fd (authoritative)
	// and prepare for tailing
	pos, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		log.Printf("seek error: %v", err)
		return
	}
	offset := pos // <-- this is our single source of truth

	if tailPtr {
		fmt.Println("Entering tail mode (Ctrl+C to stop)...")

		// authoritative offset from the fd (you already computed it above as `offset`)
		// we’ll keep one carry buffer for partial lines between events
		carry := make([]byte, 0, 4096)

		dir := filepath.Dir(filePathPtr)
		base := filepath.Base(filePathPtr)

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Printf("fsnotify watcher error: %v", err)
			return
		}
		defer watcher.Close()

		if err := watcher.Add(dir); err != nil {
			log.Printf("fsnotify add error: %v", err)
			return
		}

		// helper: read exactly [offset:currentSize) and advance offset
		readNew := func() {
			fi, err := os.Stat(filePathPtr)
			if err != nil {
				// file may not exist briefly during rotation
				return
			}
			cur := fi.Size()

			// truncated/rotated
			if cur < offset {
				offset = 0
				carry = carry[:0]
			}
			if cur == offset {
				return
			}

			f, err := os.Open(filePathPtr)
			if err != nil {
				return
			}

			const chunk = 64 * 1024
			buf := make([]byte, chunk)

			start := offset
			for start < cur {
				want := cur - start
				if want > int64(len(buf)) {
					want = int64(len(buf))
				}

				n, rerr := f.ReadAt(buf[:want], start)
				if n > 0 {
					carry = append(carry, buf[:n]...)

					// emit complete lines (keep partial in carry)
					for {
						nl := -1
						for i := 0; i < len(carry); i++ {
							if carry[i] == '\n' {
								nl = i
								break
							}
						}
						if nl == -1 {
							break
						}
						line := string(carry[:nl]) // strip '\n'
						carry = carry[nl+1:]
						if filterRegex == nil || filterRegex.MatchString(line) {
							fmt.Println(line)
						}
					}
					start += int64(n)
				}
				if rerr != nil && rerr != io.EOF {
					// rotation mid-read; stop now and try next event/tick
					break
				}
			}

			_ = f.Close()
			offset = cur
		}

		// safety net ticker in case some FS events are missed
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		// immediate check in case something was appended after initial scan
		readNew()

		for {
			select {
			case <-ctx.Done():
				fmt.Println("\nShutting down see")
				return

			case evt := <-watcher.Events:
				// only react to our file
				if filepath.Base(evt.Name) != base {
					continue
				}

				// writes & flushes
				if evt.Op&(fsnotify.Write|fsnotify.Chmod) != 0 {
					readNew()
					continue
				}

				// rotation or truncate+recreate
				if evt.Op&(fsnotify.Rename|fsnotify.Remove|fsnotify.Create) != 0 {
					// small delay to let the new file appear
					time.Sleep(80 * time.Millisecond)
					readNew()
					continue
				}

				// default: try a read
				readNew()

			case <-ticker.C:
				readNew()
			}
		}
	}

}
