//go:build !windows && !plan9

package activity

import (
	"log/syslog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"meterm/internal/lockfile"
)

func holdSinkLock(t *testing.T, l *Log) func() {
	t.Helper()
	unlock, err := lockfile.Acquire(l.sinkLockPath(), time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

func TestSyslogSinkSends(t *testing.T) {
	l, _ := sinkLog(t)
	dir, err := os.MkdirTemp("", "msl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Skip(err)
	}
	defer conn.Close()
	old, oldOS := syslogDial, syslogGOOS
	defer func() { syslogDial, syslogGOOS = old, oldOS }()
	syslogGOOS = "linux"
	syslogDial = func() (*syslog.Writer, error) {
		return syslog.Dial("unixgram", path, syslog.LOG_NOTICE|syslog.LOG_USER, "meterm")
	}
	all := appendN(t, l, 1)
	SendToSink("syslog")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8192)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); !strings.Contains(got, all[0].Hash) || !strings.Contains(got, "meterm") {
		t.Fatalf("got %q", got)
	}
	rep := l.CheckSink("syslog")
	if !rep.Healthy() || rep.Copy != "not comparable" {
		t.Fatalf("%+v", rep)
	}
	// With nothing to send to, it's a failure, reported, and nothing else.
	syslogDial = func() (*syslog.Writer, error) {
		return syslog.Dial("unixgram", path+"-gone", syslog.LOG_NOTICE|syslog.LOG_USER, "meterm")
	}
	appendN(t, l, 1)
	SendToSink("syslog")
	if rep := l.CheckSink("syslog"); rep.State != "failing" {
		t.Fatalf("%+v", rep)
	}
}

func TestSyslogSinkRefusedOnMacOS(t *testing.T) {
	l, _ := sinkLog(t)
	oldOS := syslogGOOS
	defer func() { syslogGOOS = oldOS }()
	syslogGOOS = "darwin"
	appendN(t, l, 1)
	SendToSink("syslog")
	if rep := l.CheckSink("syslog"); rep.State != "failing" || !strings.Contains(rep.LastError, "macOS") {
		t.Fatalf("%+v", rep)
	}
}

// Opening a FIFO to write waits for a reader, so it's refused without being opened.
func TestSinkRefusesAFIFO(t *testing.T) {
	l, path := sinkLog(t)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	appendN(t, l, 1)
	SendToSink(path)
	if rep := l.CheckSink(path); rep.State != "failing" || !strings.Contains(rep.LastError, "regular file") {
		t.Fatalf("%+v", rep)
	}
}

// However the name is written, a sink can't lead into the directory it's copying.
func TestSinkCantLeadIntoTheLog(t *testing.T) {
	cases := map[string]func(t *testing.T, l *Log, out string) string{
		"the log":     func(_ *testing.T, l *Log, _ string) string { return l.eventsPath() },
		"a new file":  func(_ *testing.T, l *Log, _ string) string { return filepath.Join(l.dir, "copy") },
		"dotted name": func(_ *testing.T, l *Log, _ string) string { return filepath.Join(l.dir, "..hidden") },
		"through a symlinked directory": func(t *testing.T, l *Log, out string) string {
			if err := os.Symlink(l.dir, filepath.Join(out, "d")); err != nil {
				t.Skip(err)
			}
			return filepath.Join(out, "d", "events.jsonl")
		},
		"a new file through a symlinked directory": func(t *testing.T, l *Log, out string) string {
			if err := os.Symlink(l.dir, filepath.Join(out, "d")); err != nil {
				t.Skip(err)
			}
			return filepath.Join(out, "d", "copy")
		},
		"a symlink to the log": func(t *testing.T, l *Log, out string) string {
			if err := os.Symlink(l.eventsPath(), filepath.Join(out, "l")); err != nil {
				t.Skip(err)
			}
			return filepath.Join(out, "l")
		},
		"a hard link to the log": func(t *testing.T, l *Log, out string) string {
			if err := os.Link(l.eventsPath(), filepath.Join(out, "h")); err != nil {
				t.Skip(err)
			}
			return filepath.Join(out, "h")
		},
		"another case of the name": func(t *testing.T, l *Log, _ string) string {
			upper := filepath.Join(filepath.Dir(l.dir), strings.ToUpper(filepath.Base(l.dir)))
			if _, err := os.Stat(upper); err != nil {
				t.Skip("this file system cares about case")
			}
			return filepath.Join(upper, "events.jsonl")
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			l, _ := sinkLog(t)
			appendN(t, l, 2)
			before, _ := os.ReadFile(l.eventsPath())
			sink := mk(t, l, t.TempDir())
			SendToSink(sink)
			if after, _ := os.ReadFile(l.eventsPath()); string(after) != string(before) {
				t.Fatal("it wrote into the log")
			}
			if rep := l.CheckSink(sink); rep.State != "failing" || !strings.Contains(rep.LastError, "inside") {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

func TestSinkFileIsPrivate(t *testing.T) {
	l, path := sinkLog(t)
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	appendN(t, l, 1)
	SendToSink(path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}

// A link to nothing is followed on creating the file, so it could land anywhere.
func TestSinkRefusesADanglingSymlink(t *testing.T) {
	l, _ := sinkLog(t)
	appendN(t, l, 1)
	planted := filepath.Join(l.dir, "planted.jsonl")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(planted, link); err != nil {
		t.Skip(err)
	}
	SendToSink(link)
	if _, err := os.Lstat(planted); !os.IsNotExist(err) {
		t.Fatal("it wrote into the activity directory")
	}
	if rep := l.CheckSink(link); rep.State != "failing" {
		t.Fatalf("%+v", rep)
	}
}
