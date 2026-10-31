package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"meterm/internal/config"
	"meterm/internal/history"
	"meterm/internal/sshrun"
)

// `meterm connect <host> --tmux`: work inside tmux on the host, so a dropped
// connection loses nothing. With sessions already there it offers to pick
// one up again; otherwise it starts one.

const tmuxMarker = "__meterm_tmux__"

// tmuxListScript prints the host's tmux sessions, one per line (name,
// windows, attached clients, last activity as a Unix time), then a marker
// saying whether tmux is installed at all.
const tmuxListScript = `tmux ls -F '#{session_name}	#{session_windows}	#{session_attached}	#{session_activity}' 2>/dev/null; ` +
	`if command -v tmux >/dev/null 2>&1; then echo ` + tmuxMarker + `yes; else echo ` + tmuxMarker + `no; fi`

type tmuxSession struct {
	Name     string
	Windows  int
	Attached int
	Activity time.Time
}

// tmuxNameRE is what a session name must look like to be attached to:
// names come from the host, and go back into a command line there.
var tmuxNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// parseTmuxList reads tmuxListScript's output. Sessions whose names aren't
// safe to put back on a command line are left out.
func parseTmuxList(out string) (sessions []tmuxSession, installed, ok bool) {
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(line, tmuxMarker) {
			return sessions, strings.TrimPrefix(line, tmuxMarker) == "yes", true
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 || !tmuxNameRE.MatchString(f[0]) {
			continue
		}
		w, _ := strconv.Atoi(f[1])
		a, _ := strconv.Atoi(f[2])
		act, _ := strconv.ParseInt(f[3], 10, 64)
		sessions = append(sessions, tmuxSession{Name: f[0], Windows: w, Attached: a, Activity: time.Unix(act, 0)})
	}
	return sessions, false, false
}

// newTmuxName is "meterm", or "meterm-2", ... if that's taken.
func newTmuxName(sessions []tmuxSession) string {
	taken := map[string]bool{}
	for _, s := range sessions {
		taken[s.Name] = true
	}
	name := "meterm"
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("meterm-%d", i)
	}
	return name
}

// chooseTmux asks which session to use; "" means a new one. Without a
// terminal it takes the most recently active session.
func chooseTmux(host string, sessions []tmuxSession, in io.Reader, out io.Writer, interactive bool) string {
	best := 0
	for i, s := range sessions {
		if s.Activity.After(sessions[best].Activity) {
			best = i
		}
	}
	if !interactive {
		return sessions[best].Name
	}
	fmt.Fprintf(out, "tmux sessions on %s:\n", host)
	for i, s := range sessions {
		state := "last active " + history.FormatAgo(s.Activity)
		if s.Attached > 0 {
			state = fmt.Sprintf("attached (%d), %s", s.Attached, state)
		}
		fmt.Fprintf(out, "  %d) %-16s %d window(s)  %s\n", i+1, s.Name, s.Windows, state)
	}
	fmt.Fprintf(out, "  n) new session\nChoose [%d]: ", best+1)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	switch {
	case line == "":
		return sessions[best].Name
	case line == "n" || line == "new":
		return ""
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(sessions) {
		return sessions[n-1].Name
	}
	fmt.Fprintln(out, "Not one of those; picking up the most recent.")
	return sessions[best].Name
}

// tmuxCommand is the remote command for connect --tmux on r, after asking
// if there's a choice to make. ok false means tmux isn't there: connect
// plainly instead.
func tmuxCommand(name string, r config.Resolved) (cmd string, ok bool) {
	c := sshrun.SSH(r, tmuxListScript)
	c.Stdin = nil
	c.Stderr = os.Stderr
	raw, err := c.Output()
	sessions, installed, read := parseTmuxList(string(raw))
	if err != nil || !read {
		fmt.Fprintf(os.Stderr, "meterm: couldn't ask %s about tmux; connecting without it.\n", name)
		return "", false
	}
	if !installed {
		fmt.Fprintf(os.Stderr, "meterm: tmux isn't installed on %s; connecting without it.\n", name)
		return "", false
	}
	if len(sessions) == 0 {
		return "tmux new-session -s " + newTmuxName(nil), true
	}
	pick := chooseTmux(name, sessions, os.Stdin, os.Stderr, isatty.IsTerminal(os.Stdin.Fd()))
	if pick == "" {
		return "tmux new-session -s " + newTmuxName(sessions), true
	}
	// "=" makes it an exact name, not the first session starting with it.
	return "tmux attach-session -t =" + pick, true
}
