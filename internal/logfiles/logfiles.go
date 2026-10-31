// Package logfiles parses `ls -la` output into structured entries, shared
// by `meterm logs list`.
package logfiles

import (
	"regexp"
	"strconv"
	"strings"
)

// DefaultDir is where meterm looks for logs when not told where.
const DefaultDir = "/var/log"

// Entry is one file from a directory listing.
type Entry struct {
	Name string
	Size int64
	Raw  string // the original `ls -la` line, for display
}

// rotatedSuffix matches a trailing ".N" (logrotate's numbered rotation,
// e.g. "access.log.1") whether or not it's also .gz-compressed.
var rotatedSuffix = regexp.MustCompile(`\.[0-9]+$`)

// IsRotated reports whether name looks like logrotate history rather than
// a currently-active log file.
func IsRotated(name string) bool {
	return strings.HasSuffix(name, ".gz") || rotatedSuffix.MatchString(name)
}

// Parse parses `ls -la` output into entries, skipping the "total N" summary
// line, "." and "..", and — unless all is true — logrotate-style
// rotated/compressed names, so the currently-active files are what's left.
func Parse(raw string, all bool) []Entry {
	var out []Entry
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := fields[len(fields)-1]
		if name == "." || name == ".." {
			continue
		}
		if !all && IsRotated(name) {
			continue
		}
		size, _ := strconv.ParseInt(fields[4], 10, 64)
		out = append(out, Entry{Name: name, Size: size, Raw: line})
	}
	return out
}
