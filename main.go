package main

// A lightweight CLI tool written in Go to read text files,
// apply optional line filtering using regular expressions
// and optionally tail new lines in real-time.
//
// Calling it only with a file -file="file path" path works as cat
// Calling it with file path -file="file path" and filter -filter="text" works as cat | grep
// Calling it with file path, filter and -t works as cat | grep tail
// file and filter flags can be also pass as positional args, file always 1st and filter 2nd.
//
// Examples:
//   see /path/to/file "INFO" -t
//   see /path/to/file "^ERROR"
//
// Build info:
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
	"strconv"
	"strings"
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

	// Usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "see — quick cat/grep/tail for text & logs\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [options] <file> [filter]\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Positional args:
  <file>     Path to a file. You can use -file instead.
  [filter]   Optional Go/RE2 regex. You can use -filter instead.
             Tip: use (?i) for case-insensitive; ^ and $ to anchor.

Behavior:
  • Default: prints all lines (filtered if a filter is provided).
  • -t               Follow new lines (like tail -f). Handles truncate/rotate.
  • -ns N            Print first N matching lines; can combine with -t to follow.
  • -ne N            Print last  N matching lines; can combine with -t to follow.
  • -ns and -ne are mutually exclusive.
  • Flags may also appear after <file> (e.g., "... /var/log/app.log -ne 100 -t").

Coloring:
  • Auto on TTY, off when piped or NO_COLOR/TERM=dumb.
  • Levels (INFO/WARN/ERROR etc.), timestamps, URLs, IPs, paths, numbers highlighted.
  • JSON/YAML keys subtly colored; punctuation dimmed.
  • Filter matches are highlighted and override generic coloring.

Examples:
  see /var/log/nginx/access.log "(?i)error"
  see -file /var/log/nginx/access.log -filter "^WARN" -t
  see /var/log/nginx/access.log -ne 200 -t

Notes:
  • Regex syntax is Go's RE2 (no lookbehind).
  • Directory rotation (logrotate) is followed automatically.
  • Exit with Ctrl+C while tailing.`)
	}

	// Flags
	versionFlag := flag.Bool("version", false, "Show version info and exit")
	filePathPtr := flag.String("file", "", "Path to the log file to watch")
	filterPtr := flag.String("filter", "", "Optional regex or string literal to filter lines")
	tailPtr := flag.Bool("t", false, "Tail (follow) appended lines")
	nsPtr := flag.Int("ns", 0, "Print first N matching lines from start")
	nePtr := flag.Int("ne", 0, "Print last N matching lines from end")

	flag.Parse()

	if *versionFlag {
		fmt.Printf("see version %s (built %s)\n", version, buildDate)
		return
	}

	// Positional args
	// after: flag.Parse()
	args := flag.Args()

	// --- Post-parse sweep so flags after <file> also work ---
	for i := 0; i < len(args); i++ {
		tok := args[i]

		// -t / --t
		if tok == "-t" || tok == "--t" {
			*tailPtr = true
			// mark consumed
			args[i] = ""
			continue
		}

		// -ns=N or -ns N
		if strings.HasPrefix(tok, "-ns=") {
			if v, err := strconv.Atoi(tok[len("-ns="):]); err == nil {
				*nsPtr = v
			}
			args[i] = ""
			continue
		}
		if tok == "-ns" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				*nsPtr = v
				args[i], args[i+1] = "", "" // consume both
				i++
				continue
			}
		}

		// -ne=N or -ne N
		if strings.HasPrefix(tok, "-ne=") {
			if v, err := strconv.Atoi(tok[len("-ne="):]); err == nil {
				*nePtr = v
			}
			args[i] = ""
			continue
		}
		if tok == "-ne" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				*nePtr = v
				args[i], args[i+1] = "", "" // consume both
				i++
				continue
			}
		}
	}

	// Now resolve <file> and optional [filter] from remaining args (skipping consumed slots)
	clean := args[:0]
	for _, a := range args {
		if a != "" {
			clean = append(clean, a)
		}
	}
	args = clean

	if *filePathPtr == "" {
		if len(args) > 0 {
			*filePathPtr = args[0]
			args = args[1:]
		} else {
			log.Fatal("Please provide a file path either as -file or as first argument")
		}
	}
	if *filterPtr == "" && len(args) > 0 {
		*filterPtr = args[0]
	}

	// Enforce mutual exclusion after the sweep
	if *nsPtr > 0 && *nePtr > 0 {
		log.Fatalf("Use either -ns or -ne, not both")
	}

	// Pre-check
	info, err := os.Stat(*filePathPtr)
	if err != nil {
		log.Fatalf("Cannot access file: %v", err)
	}
	if info.IsDir() {
		log.Fatalf("Expected a file, but got a directory")
	}

	// Ctrl+C handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() { <-c; cancel() }()

	// Run
	go see(ctx, &wg, *filterPtr, *filePathPtr, *tailPtr, *nsPtr, *nePtr)
	wg.Wait()
}

// ------- Coloring helpers (no flags; auto-enable on TTY) -------

// ---  ANSI color wrappers ---
const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[2m" // faint
	ansiBold    = "\x1b[1m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
)

// Precompiled, broad-but-safe regexes
var (
	reTS       = regexp.MustCompile(`\b(?:\d{4}-\d{2}-\d{2}[T\s]\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?|[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2})\b`)
	reLvl      = regexp.MustCompile(`(?i)\b(debug|info|warn|warning|error|err|crit|fatal|fail)\b`)
	reURL      = regexp.MustCompile(`\bhttps?://[^\s]+`)
	reIP       = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	rePath     = regexp.MustCompile(`(?:^|[\s"'])/[\w./\-\+@%~]+`)
	reNum      = regexp.MustCompile(`\b\d+\b`)
	reJSONLine = regexp.MustCompile(`^\s*[\{\[]`)                  // starts like JSON
	reJSONKey  = regexp.MustCompile(`"([^"\\]|\\.)*"\s*:`)         // "key":
	reYAMLKey  = regexp.MustCompile(`^(\s*)([A-Za-z0-9_.-]+)\s*:`) // key:
)

// First pass for structured data (JSON/YAML):
// - JSON keys colored (contents inside quotes)
// - YAML leading keys colored
// - braces/brackets/commas/colon dimmed
func colorizeStructured(seg string, enable bool) string {
	if !enable {
		return seg
	}
	// JSON keys: color the content between quotes, keep quotes/colon normal
	seg = reJSONKey.ReplaceAllStringFunc(seg, func(s string) string {
		i := strings.IndexByte(s, '"')
		j := strings.LastIndexByte(s, '"')
		if i == -1 || j <= i {
			return s
		}
		return s[:i+1] + ansiGreen + s[i+1:j] + ansiReset + s[j:]
	})
	// YAML leading key
	seg = reYAMLKey.ReplaceAllString(seg, `$1`+ansiBlue+`$2`+ansiReset+`:`)
	// Dim common punctuation
	seg = strings.NewReplacer(
		"{", ansiDim+"{"+ansiReset,
		"}", ansiDim+"}"+ansiReset,
		"[", ansiDim+"["+ansiReset,
		"]", ansiDim+"]"+ansiReset,
		",", ansiDim+","+ansiReset,
		": ", ansiDim+": "+ansiReset,
	).Replace(seg)
	return seg
}

// Apply “generic” coloring to a plain segment (no filter-match spans inside).
func colorizeGeneric(seg string, enable bool) string {
	if !enable {
		return seg
	}
	// If it looks like JSON or starts with a YAML key, do a structured pass first.
	if reJSONLine.MatchString(seg) || reYAMLKey.MatchString(seg) {
		seg = colorizeStructured(seg, enable)
	}

	// (keep your existing generic passes here)
	seg = reTS.ReplaceAllStringFunc(seg, func(s string) string { return ansiDim + s + ansiReset })
	seg = reURL.ReplaceAllStringFunc(seg, func(s string) string { return ansiMagenta + s + ansiReset })
	seg = reIP.ReplaceAllStringFunc(seg, func(s string) string { return ansiCyan + s + ansiReset })
	seg = rePath.ReplaceAllStringFunc(seg, func(s string) string { return ansiMagenta + s + ansiReset })
	seg = reLvl.ReplaceAllStringFunc(seg, func(s string) string {
		switch strings.ToUpper(s) {
		case "DEBUG":
			return ansiBlue + s + ansiReset
		case "INFO":
			return ansiGreen + s + ansiReset
		case "WARN", "WARNING":
			return ansiYellow + s + ansiReset
		case "ERROR", "ERR", "CRIT", "FATAL", "FAIL":
			return ansiRed + s + ansiReset
		default:
			return s
		}
	})
	seg = reNum.ReplaceAllStringFunc(seg, func(s string) string { return ansiBold + s + ansiReset })
	return seg
}

// Render a line with generic coloring, then overlay filter matches (matches win).
// We avoid breaking matches by splitting the raw line on match spans.
func renderLine(raw string, filter *regexp.Regexp, enable bool) string {
	if filter == nil || !enable {
		// Only generic color if no filter or color disabled.
		return colorizeGeneric(raw, enable)
	}
	// Find match spans on RAW (no ANSI)
	idxs := filter.FindAllStringIndex(raw, -1)
	if len(idxs) == 0 {
		return colorizeGeneric(raw, enable)
	}

	out := make([]byte, 0, len(raw)+32)
	last := 0
	for _, span := range idxs {
		s, e := span[0], span[1]
		// generic on the gap before match
		if s > last {
			out = append(out, colorizeGeneric(raw[last:s], enable)...)
		}
		// highlight the match as a single unit (distinct color)
		out = append(out, ansiCyan...) // your match color
		out = append(out, raw[s:e]...) // raw, no generic inside
		out = append(out, ansiReset...)
		last = e
	}
	// generic on trailing remainder
	if last < len(raw) {
		out = append(out, colorizeGeneric(raw[last:], enable)...)
	}
	return string(out)
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	// If not a char device, most likely piped/redirected.
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return stdoutIsTTY()
}

// Helpers: ns / ne selections
func printFirstNFiltered(path string, rx *regexp.Regexp, n int, enableColor bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	const max = 1024 * 1024
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, max)

	count := 0
	for sc.Scan() {
		s := sc.Text()
		if rx == nil || rx.MatchString(s) {
			fmt.Println(renderLine(s, rx, enableColor))
			count++
			if count >= n {
				break
			}
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return count, err
	}
	return count, nil
}

func printLastNFiltered(path string, rx *regexp.Regexp, n int, enableColor bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	const max = 1024 * 1024
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, max)

	ring := make([]string, n)
	size, idx := 0, 0
	for sc.Scan() {
		s := sc.Text()
		if rx == nil || rx.MatchString(s) {
			ring[idx] = s
			idx = (idx + 1) % n
			if size < n {
				size++
			}
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return 0, err
	}

	start := 0
	if size == n {
		start = idx
	}
	for i := 0; i < size; i++ {
		fmt.Println(renderLine(ring[(start+i)%n], rx, enableColor))
	}
	return size, nil
}

func see(ctx context.Context, wg *sync.WaitGroup, filterPtr, filePathPtr string, tailPtr bool, ns, ne int) {
	defer wg.Done()

	// Compile filter strictly as Go/RE2 regex (no modes)
	var rx *regexp.Regexp
	if filterPtr != "" {
		rxc, err := regexp.Compile(filterPtr)
		if err != nil {
			log.Fatalf("Invalid regex pattern %q: %v", filterPtr, err)
		}
		rx = rxc
	}
	enableColor := colorEnabled()

	// --- Initial print phase (head/last/all) ---
	var offset int64

	switch {
	case ns > 0 && ne > 0:
		log.Fatalf("Use either -ns or -ne, not both")

	case ns > 0:
		// print first N matching
		if _, err := printFirstNFiltered(filePathPtr, rx, ns, enableColor); err != nil {
			log.Printf("Error: %v", err)
			return
		}
		// start tailing from EOF
		if fi, err := os.Stat(filePathPtr); err == nil {
			offset = fi.Size()
		}

	case ne > 0:
		// print last N matching
		if _, err := printLastNFiltered(filePathPtr, rx, ne, enableColor); err != nil {
			log.Printf("Error: %v", err)
			return
		}
		// start tailing from EOF
		if fi, err := os.Stat(filePathPtr); err == nil {
			offset = fi.Size()
		}

	default:
		// print all (filtered), then tail from the *FD position we read to*
		f, err := os.Open(filePathPtr)
		if err != nil {
			log.Fatalf("Error opening file: %v", err)
		}
		fmt.Println("Reading existing lines...")
		sc := bufio.NewScanner(f)
		const maxLine = 1024 * 1024
		buf := make([]byte, 64*1024)
		sc.Buffer(buf, maxLine)

		for sc.Scan() {
			line := sc.Text()
			if rx == nil || rx.MatchString(line) {
				fmt.Println(renderLine(line, rx, enableColor))
			}
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			_ = f.Close()
			log.Printf("Error reading file: %v", err)
			return
		}
		// authoritative position from THIS FD so we don’t miss append-during-scan
		if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
			offset = pos
		} else if fi, err2 := os.Stat(filePathPtr); err2 == nil {
			offset = fi.Size()
		}
		_ = f.Close()
	}

	// If no follow requested, we’re done.
	if !tailPtr {
		return
	}

	fmt.Println("Entering tail mode (Ctrl+C to stop)...")

	dir := filepath.Dir(filePathPtr)
	base := filepath.Base(filePathPtr)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("fsnotify watcher error: %v", err)
		return
	}
	defer watcher.Close()

	// watch the directory; filter by basename (works with rotation)
	if err := watcher.Add(dir); err != nil {
		log.Printf("fsnotify add error: %v", err)
		return
	}

	// carry for partial line across chunks
	carry := make([]byte, 0, 4096)

	readNew := func() {
		fi, err := os.Stat(filePathPtr)
		if err != nil {
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
		defer f.Close()

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
					line := string(carry[:nl])
					carry = carry[nl+1:]
					if rx == nil || rx.MatchString(line) {
						fmt.Println(renderLine(line, rx, enableColor))
					}
				}
				start += int64(n)
			}
			if rerr != nil && rerr != io.EOF {
				break // rotation mid-read; next event will pick up
			}
		}
		offset = cur
	}

	// Safety net ticker in case events are missed
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	// Catch data appended between initial print and watcher start
	readNew()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nShutting down see")
			return

		case evt := <-watcher.Events:
			if filepath.Base(evt.Name) != base {
				continue
			}
			switch {
			case evt.Op&(fsnotify.Write|fsnotify.Chmod) != 0:
				readNew()
			case evt.Op&(fsnotify.Rename|fsnotify.Remove|fsnotify.Create) != 0:
				time.Sleep(80 * time.Millisecond)
				readNew()
			default:
				readNew()
			}

		case err := <-watcher.Errors:
			log.Printf("fsnotify error: %v", err)

		case <-ticker.C:
			readNew()
		}
	}
}
