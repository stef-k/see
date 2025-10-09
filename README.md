# `see`

A lightweight CLI tool written in Go to read text files, apply optional line filtering using regular expressions, and optionally tail new lines in real-time.

---

## Features

- Read a log file (`cat`)
- Filter lines using regular expressions (`grep`)
- Tail a log file for live updates (`tail`)
- Combine all three behaviors easily
- Supports both flags and positional arguments
- Shows build version info (when embedded during compilation)

---

## Usage

```bash
# Just read a file
see logfile.txt

# Read a file and filter lines containing "ERROR"
see logfile.txt "ERROR"
see -file logfile.txt -filter "ERROR"

# Tail a file with filter
see logfile.txt "ERROR" -t
see -file logfile.txt -filter "ERROR" -t

Filter examples:

"ERROR"     - lines containing ERROR
"^INFO"     - lines starting with INFO
"timeout$"  - lines ending with timeout
".*ERR.*"   - lines containing ERR anywhere
```

## Build with Version Info

Embed version and build date at compile time.

Windows (PowerShell)

```powershell
$version = "v1.0.0"
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
go build -ldflags "-X main.version=$version -X main.buildDate=$buildDate" -o see.exe
```

## Linux / macOS (bash/zsh)

```bash
VERSION="v1.0.0"
DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
go build -ldflags "-X main.version=$VERSION -X main.buildDate=$DATE" -o see
```

## Example Output

```bash
Reading existing lines...
2025-10-09 09:12:04 INFO Server started
2025-10-09 09:13:22 ERROR Failed to connect to database
```

## License

This project is licensed under the MIT License

## Author

Stef Kariotidis
