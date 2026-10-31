package team

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Tracing for `meterm --debug` (or METERM_DEBUG=1): every git command the
// team store runs goes to stderr with its outcome. Paths and statuses only: never field
// values, tokens or API keys, so a trace can be pasted into a bug report.

var (
	debugMu  sync.Mutex
	debugOut io.Writer
)

func init() {
	if os.Getenv("METERM_DEBUG") != "" {
		debugOut = os.Stderr
	}
}

// SetDebug sends the trace to w; nil turns it off.
func SetDebug(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugOut = w
}

func debugf(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugOut != nil {
		fmt.Fprintf(debugOut, "team: "+format+"\n", args...)
	}
}
