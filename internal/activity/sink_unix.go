//go:build !windows && !plan9

package activity

import (
	"errors"
	"log/syslog"
	"runtime"
)

// syslogGOOS is the machine sending; tests set it.
var syslogGOOS = runtime.GOOS

// syslogDial is replaced by tests, so that nothing goes to the machine's own syslog.
var syslogDial = func() (*syslog.Writer, error) { return syslog.New(syslog.LOG_NOTICE|syslog.LOG_USER, "meterm") }

func sendSyslog(events []Event) error {
	// macOS takes these messages without complaint, and they couldn't then be found with
	// `log show`. A copy that reads as fine and isn't there is worse than none.
	if syslogGOOS == "darwin" {
		return errors.New("macOS doesn't keep these where they can be found; use a path")
	}
	w, err := syslogDial()
	if err != nil {
		return err
	}
	defer w.Close()
	for _, e := range events {
		line, err := syslogLine(e)
		if err != nil {
			return err
		}
		if err := w.Notice(line); err != nil {
			return err
		}
	}
	return nil
}
