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
	"regexp"
	"sync"
	"time"
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

	// compile regex if provided
	var filterRegex *regexp.Regexp
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

	if tailPtr {
		fmt.Println("Entering tail mode (Ctrl+C to stop)...")
		f, err := os.Open(filePathPtr)
		if err != nil {
			log.Fatalf("Error reopening file: %v", err)
		}
		defer f.Close()

		// Start at EOF so we only get new lines
		offset, _ := f.Seek(0, io.SeekEnd)
		reader := bufio.NewReader(f)

		for {
			select {
			case <-ctx.Done():
				fmt.Println("\nShutting down see")
				return
			default:
				line, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						time.Sleep(200 * time.Millisecond)
						// In case file grew, re-seek
						newOffset, _ := f.Seek(0, io.SeekCurrent)
						if newOffset < offset {
							// File was truncated or rotated
							f.Seek(0, io.SeekStart)
						}
						continue
					}
					log.Printf("Error reading file: %v", err)
					return
				}
				offset += int64(len(line))
				if filterRegex == nil || filterRegex.MatchString(line) {
					fmt.Print(line)
				}
			}
		}
	}

}
