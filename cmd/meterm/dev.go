package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"meterm/internal/config"
	"meterm/internal/history"
	"meterm/internal/sshrun"
	"meterm/internal/status"
)

// `meterm dev <host>:<dir>`: one command back to where you were working on a
// host. It attaches to a tmux session named after the project, keeps it through
// dropped connections and network changes, and holds the tunnels the work needs.

const (
	defaultDevSession = "dev"
	// devDirMissing is the exit code when the project directory isn't there.
	devDirMissing = 3
	// sshConnectionError is how ssh says the connection failed or dropped; any
	// other code came from the remote command.
	sshConnectionError = 255
	// devSettle is how long ssh must have run to count as having connected,
	// where connectMarker can't tell (Windows). It's past ConnectTimeout below,
	// or a host that never answers would look like one that had.
	devSettle = 15 * time.Second
	// connectPoll is how often a running ssh is looked at for having got in.
	connectPoll = 250 * time.Millisecond
)

// devSSHOptions apply to the session and its tunnels. A connection shared with
// a ControlMaster would take everything down with it and ignore the keepalives,
// so these stay on connections of their own.
var devSSHOptions = []string{
	"-o", "ControlPath=none",
	"-o", "ConnectTimeout=10",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
}

func newDevCmd() *cobra.Command {
	var session string
	var tunnels []string
	var noReconnect, logArgs bool
	cmd := &cobra.Command{
		Use:   "dev <name|user@name>[:<dir>] [-- <command...>]",
		Short: "Pick up work on a host: named tmux session, reconnecting, with tunnels",
		Long: "Attaches to a tmux session on the host, creating it in <dir> if it isn't\n" +
			"there yet. The session is named after the directory unless --session says\n" +
			"otherwise, so the same command always lands in the same place.\n\n" +
			"A dropped connection is retried with backoff, checking the host's routes again\n" +
			"each time; detaching or ending the session stops it. Without tmux on the host\n" +
			"it opens a plain shell in <dir>.\n\n" +
			"After --, a command runs in a new window of the session, in <dir> or else the\n" +
			"session's directory, its arguments reaching the host exactly as given. It runs\n" +
			"once: a reconnect comes back to the session and doesn't start it again. The exit\n" +
			"code is the session's then, not the command's: detaching is 0. Without tmux on\n" +
			"the host the command runs on its own and the exit code is its own; a dropped\n" +
			"connection ends it, so meterm stops with exit 1 instead of reconnecting. The\n" +
			"activity log records the program; --log-args adds its arguments, redacted.\n\n" +
			"Examples:\n" +
			"  meterm dev devbox:~/src/api                  # session \"api\"\n" +
			"  meterm dev devbox:~/src/api --tunnel web     # and hold the saved tunnel web\n" +
			"  meterm dev devbox --session scratch --tunnel all\n" +
			"  meterm dev devbox:~/src/api -- claude agents  # in a new window there",
		Args: func(cmd *cobra.Command, args []string) error {
			before := len(args)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				before = dash
				if dash == len(args) {
					return fmt.Errorf("no command given after --")
				}
			}
			if before != 1 {
				return fmt.Errorf("accepts 1 target before --, received %d", before)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDev(args[0], args[1:], session, tunnels, !noReconnect, logArgs)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "tmux session name (default: the directory's name)")
	cmd.Flags().StringArrayVar(&tunnels, "tunnel", nil, "saved tunnel to hold open while connected, or all (repeatable)")
	cmd.Flags().BoolVar(&noReconnect, "no-reconnect", false, "stop when the connection drops")
	cmd.Flags().BoolVar(&logArgs, "log-args", false, "put the command's arguments in the activity log, redacted (default: just the program)")
	return cmd
}

func runDev(target string, command []string, session string, tunnels []string, reconnect, logArgs bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	hostArg, dir := splitDevTarget(target, func(name string) bool {
		_, idx, err := cfg.Target(name)
		return err == nil && idx >= 0
	})
	h, idx, err := cfg.Target(hostArg)
	if err != nil {
		return err
	}
	if idx < 0 {
		return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", hostArg)
	}
	name, err := devSessionName(session, dir)
	if err != nil {
		return err
	}
	script, err := devRemoteScript(dir, name, session == "")
	if err != nil {
		return err
	}
	again := script
	if len(command) > 0 {
		if script, again, err = devCommandScripts(dir, name, session == "", command); err != nil {
			return err
		}
	}
	var specs []string
	for _, t := range tunnels {
		if t == "all" {
			specs = append(specs, hostArg)
		} else {
			specs = append(specs, hostArg+"/"+t)
		}
	}
	var sets []tunnelSet
	if len(specs) > 0 {
		if sets, err = pickTunnels(cfg, specs); err != nil {
			return err
		}
	}

	if err := cfg.Resolve(*h).Err; err != nil {
		return err
	}
	_ = history.RecordAndSave(h.Name)
	detail := "dev " + name
	if len(command) > 0 {
		detail += ": " + commandDetail(command, logArgs)
	}
	recordActivity(cfg, *h, "connect", detail)
	for _, s := range sets {
		var names []string
		for _, f := range s.forward {
			names = append(names, f.Name)
			if cfg.IsProduction(s.host) || f.MentionsProduction() {
				fmt.Fprintf(os.Stderr, "⚠ %s/%s reaches PRODUCTION: %s\n", s.host.Name, f.Name, f.Note)
			}
		}
		recordActivity(cfg, s.host, "tunnel", strings.Join(names, ","))
	}
	kickAutosync(false, true)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	reg := status.Open()
	defer reg.Close()

	var wg sync.WaitGroup
	if len(sets) > 0 {
		log, logPath, err := openDevTunnelLog()
		if err != nil {
			return err
		}
		defer log.Close()
		fmt.Fprintf(os.Stderr, "meterm: holding tunnels; problems are logged in %s\n", logPath)
		for _, s := range sets {
			wg.Add(1)
			go func(s tunnelSet) {
				defer wg.Done()
				hold := tunnelHold{keep: true, notice: devNotice(logPath), nextRoute: tunnelNextRoute(&s), noPrompts: true}
				stop := hold.track(reg, &s)
				defer stop()
				if err := holdTunnel(ctx, s.host.Name, func() ([]string, error) { return devTunnelArgs(cfg, s, hold.marker.options()...) }, hold, log); err != nil {
					hold.notice(err.Error())
				}
			}(s)
		}
	}

	code, err := holdDevSession(ctx, hostArg, script, again, reconnect, reg.Add(status.KindSession, h.Name, name))
	stop()
	wg.Wait()
	reg.Close() // finish doesn't run deferred calls
	if err != nil {
		return err
	}
	if code != 0 {
		if code == devDirMissing {
			fmt.Fprintf(os.Stderr, "meterm: no directory %s on %s\n", dir, h.Name)
		}
		finish(code)
	}
	return nil
}

// holdDevSession runs the session, reconnecting after drops, and returns the
// exit code to leave with. script runs until one has got in, and again after that,
// so a command to start starts once.
func holdDevSession(ctx context.Context, hostArg, script, again string, reconnect bool, state *status.Handle) (int, error) {
	backoff := 2 * time.Second
	connected := false
	marker := newConnectMarker()
	defer marker.close()
	stderr := &sshrun.Tail{Max: 16 << 10}
	var kicks <-chan string
	if reconnect {
		kicks = watchKicks(ctx)
	}
	tty, _ := term.GetState(int(os.Stdin.Fd()))
	var inTmux atomic.Bool
	for {
		// A fresh load so routes are checked again: the network may have changed.
		cfg, err := loadConfig()
		if err != nil {
			return 1, err
		}
		h, idx, err := cfg.Target(hostArg)
		if err != nil {
			return 1, err
		}
		if idx < 0 {
			return 1, fmt.Errorf("no such host: %s", hostArg)
		}
		r := cfg.Resolve(*h)
		if r.Err != nil {
			return 1, r.Err
		}
		if via := routeNote(r); via != "" {
			fmt.Fprintf(os.Stderr, "meterm: %s\n", via)
		}
		baseArgs := append(append([]string(nil), r.ExtraArgs...), devSSHOptions...)
		r.ExtraArgs = append(append([]string(nil), baseArgs...), marker.options()...)
		state.SetRoute(routeOrDefault(r.Route))

		stderr.Reset()
		marker.reset()
		started := time.Now()
		// A prompt in progress for the first login isn't to be killed, only a
		// session that has been in and can come back, and only inside tmux: a plain
		// shell would lose whatever is running in it, on a connection that may well
		// have survived. Its keepalives can decide that instead.
		attempt, stopAttempt := kickable(ctx, kicks, func() bool {
			return inTmux.Load() && (connected || marker.seen(time.Since(started)))
		})
		remote := script
		if connected {
			remote = again
		}
		c := sshrun.CommandContext(attempt, r, "-t", remote)
		c.Stderr = io.MultiWriter(os.Stderr, stderr)
		// SIGTERM rather than the default kill, so ssh leaves the terminal as it
		// found it. A jump host's ssh can leave a shared master behind holding
		// stderr open, and an ssh with no network can't always leave at once.
		c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
		c.WaitDelay = 2 * time.Second
		endWatch := watchConnected(attempt, marker, started, func() {
			state.Set(status.Connected, "")
			if kicks != nil {
				go probeTmux(attempt, r, baseArgs, &inTmux)
			}
		})
		err = c.Run()
		endWatch()
		lasted := time.Since(started)
		kicked := stopAttempt()
		if ctx.Err() != nil {
			return 0, nil
		}
		if kicked != "" {
			state.Set(status.Reconnecting, kicked)
			if tty != nil {
				_ = term.Restore(int(os.Stdin.Fd()), tty)
			}
			restoreTerminal(os.Stdout)
			connected = true
			backoff = 2 * time.Second
			fmt.Fprintf(os.Stderr, "meterm: %s; reconnecting to %s\n", kicked, h.Name)
			continue
		}
		if err == nil || errors.Is(err, exec.ErrWaitDelay) {
			return 0, nil
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return 1, err
		}
		if exit.ExitCode() == sshConnectionError {
			restoreTerminal(os.Stdout)
		}
		got := marker.seen(lasted)
		connected = connected || got
		failure := sshrun.ClassifyFailure(stderr.String())
		if failure == sshrun.FailureHostKey && (reconnect || !connected) {
			if bad, ok := avoidRoute(cfg, *h, r); ok {
				fmt.Fprintf(os.Stderr, "meterm: WARNING: %s has the wrong host key, so it may not be that machine; trying another route\n", bad)
				continue
			}
		}
		if !devShouldRetry(exit.ExitCode(), connected, reconnect, failure) {
			if failure != sshrun.FailureNone && (connected || failure == sshrun.FailureUnknownHost) {
				fmt.Fprintf(os.Stderr, "meterm: giving up on %s: %s\n", h.Name, failure)
			}
			return exit.ExitCode(), nil
		}
		if lasted > time.Minute {
			backoff = 2 * time.Second
		}
		if got {
			state.Set(status.Reconnecting, "connection lost")
			fmt.Fprintf(os.Stderr, "meterm: connection to %s lost; reconnecting in %s (Ctrl-C to stop)\n", h.Name, backoff)
		} else {
			if connected {
				state.Set(status.Reconnecting, "no connection")
			} else {
				state.Set(status.Connecting, "no connection")
			}
			fmt.Fprintf(os.Stderr, "meterm: still no connection to %s; trying again in %s (Ctrl-C to stop)\n", h.Name, backoff)
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case why := <-kicks:
			state.Set(status.Reconnecting, why)
			fmt.Fprintf(os.Stderr, "meterm: %s; trying %s now\n", why, h.Name)
			backoff = 2 * time.Second
			continue
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// probeTmux finds out whether the session's script took its tmux branch, on a
// connection of its own: what comes back on the session's terminal belongs to the
// session. It asks what the script asks, with no prompts, and a login that fails
// leaves what was known before.
func probeTmux(ctx context.Context, r config.Resolved, extraArgs []string, inTmux *atomic.Bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r.ExtraArgs = append(append([]string(nil), extraArgs...), "-o", "BatchMode=yes")
	r.Forwards = nil
	c := sshrun.SSHContext(ctx, r, "command -v tmux >/dev/null 2>&1 && echo meterm-has-tmux")
	noPrompts(c)
	out, err := c.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == sshConnectionError || err != nil && !errors.As(err, &exit) {
		return
	}
	inTmux.Store(strings.Contains(string(out), "meterm-has-tmux"))
}

// devShouldRetry says whether to connect again after ssh exited with code. Only
// a session that really got in comes back, and only from what can pass by itself:
// a revoked key or a changed host key will be just the same in 30 seconds.
func devShouldRetry(code int, connected, reconnect bool, failure sshrun.Failure) bool {
	return reconnect && connected && code == sshConnectionError && failure == sshrun.FailureNone
}

// connectMarker tells whether ssh got all the way in: it has ssh touch a file
// once it has connected and authenticated, which says so whatever the time it
// took. It's nil where that can't work, and the time it ran has to do instead.
type connectMarker struct {
	dir, path string
}

func newConnectMarker() *connectMarker {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.MkdirTemp("", "meterm-dev-")
	if err != nil {
		return nil
	}
	return &connectMarker{dir: dir, path: filepath.Join(dir, "connected")}
}

// options are the ssh options that make ssh leave the mark. They're meterm's
// own, added after the host's extra args have been checked; ssh's %-tokens
// apply in LocalCommand, so a % in the path has to be doubled.
func (m *connectMarker) options() []string {
	if m == nil {
		return nil
	}
	return []string{"-o", "PermitLocalCommand=yes",
		"-o", "LocalCommand=touch " + shQuote(strings.ReplaceAll(m.path, "%", "%%"))}
}

func (m *connectMarker) reset() {
	if m != nil {
		_ = os.Remove(m.path)
	}
}

// seen says whether the attempt that ran for lasted got in.
func (m *connectMarker) seen(lasted time.Duration) bool {
	if m == nil {
		return lasted >= devSettle
	}
	_, err := os.Stat(m.path)
	return err == nil
}

// watchConnected calls got once the attempt started at started has got in, for
// whoever wants to know before ssh ends. The returned stop waits for it to finish,
// so nothing is reported about an attempt after it's over.
func watchConnected(attempt context.Context, m *connectMarker, started time.Time, got func()) (stop func()) {
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		tick := time.NewTicker(connectPoll)
		defer tick.Stop()
		for {
			if m.seen(time.Since(started)) {
				got()
				return
			}
			select {
			case <-attempt.Done():
				return
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}

func (m *connectMarker) close() {
	if m != nil {
		_ = os.RemoveAll(m.dir)
	}
}

// splitDevTarget splits host:dir. A saved host whose whole name fits wins, so
// the colon is only taken as the separator when it has to be.
func splitDevTarget(target string, isHost func(string) bool) (hostArg, dir string) {
	if isHost(target) {
		return target, ""
	}
	hostArg, dir, _ = strings.Cut(target, ":")
	return hostArg, dir
}

var devNameDropRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// devSessionName is the session to use: explicit, or from the directory's name.
// tmux won't take "." or ":" in a name, so those don't get through.
func devSessionName(explicit, dir string) (string, error) {
	if explicit != "" {
		if !tmuxNameRE.MatchString(explicit) || strings.HasPrefix(explicit, "-") || strings.Contains(explicit, ".") {
			return "", fmt.Errorf("--session %q: use letters, digits, - or _", explicit)
		}
		return explicit, nil
	}
	base := path.Base(strings.TrimRight(dir, "/"))
	if dir == "" || base == "~" || base == "." || base == "/" {
		return defaultDevSession, nil
	}
	name := strings.Trim(devNameDropRE.ReplaceAllString(base, "_"), "-_")
	if name == "" {
		return defaultDevSession, nil
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name, nil
}

// devRemoteScript is the command run on the host. dir comes from the command
// line and goes into a remote shell, so it's quoted; only a leading ~ stays
// live, to mean the remote home. With derived set, session came from the
// directory's name, and other directories can share that name.
func devRemoteScript(dir, session string, derived bool) (string, error) {
	var b strings.Builder
	if err := devEnterDir(&b, dir); err != nil {
		return "", err
	}
	if derived && dir != "" {
		fmt.Fprintf(&b, "if command -v tmux >/dev/null 2>&1; then %s; exec tmux new-session -A -s \"$s\"; fi; ", devPickSession(dir, session))
	} else {
		fmt.Fprintf(&b, "if command -v tmux >/dev/null 2>&1; then exec tmux new-session -A -s %s; fi; ", shQuote(session))
	}
	b.WriteString(`echo "meterm: tmux isn't installed here; opening a plain shell." >&2; exec "${SHELL:-/bin/sh}" -l`)
	return b.String(), nil
}

// devEnterDir starts a script by changing to dir, if there is one.
func devEnterDir(b *strings.Builder, dir string) error {
	if strings.ContainsAny(dir, "\n\r\x00") {
		return fmt.Errorf("directory %q: control characters aren't allowed", dir)
	}
	if dir != "" {
		fmt.Fprintf(b, "cd -- %s || exit %d; ", devRemoteDir(dir), devDirMissing)
	}
	return nil
}

// devCommandScripts are devRemoteScript for a session with a command to run: first
// starts it, in a new window that attaching then shows, and again comes back to the
// session without starting it. With no tmux the command runs on the connection itself
// and ends with it, so again has nothing to come back to.
func devCommandScripts(dir, session string, derived bool, command []string) (first, again string, err error) {
	var enter strings.Builder
	if err := devEnterDir(&enter, dir); err != nil {
		return "", "", err
	}
	pick := "s=" + shQuote(session)
	if derived && dir != "" {
		pick = devPickSession(dir, session)
	}
	// A directory asked for wins; otherwise tmux knows where the session is.
	where := `"$PWD"`
	if dir == "" {
		where = "'#{session_path}'"
	}
	line := shQuoteWords(command)
	const attach = `exec tmux new-session -A -s "$s"; fi; `
	first = enter.String() + "if command -v tmux >/dev/null 2>&1; then " + pick +
		`; tmux has-session -t "=$s" 2>/dev/null || tmux new-session -d -s "$s" || exit 1; ` +
		`tmux new-window -t "=$s:" -c ` + where + " " + shQuote(line) + " || exit 1; " + attach + "exec " + line
	again = enter.String() + "if command -v tmux >/dev/null 2>&1; then " + pick + "; " + attach +
		`echo "meterm: the command ended with the connection: there's no tmux here to keep it." >&2; exit 1`
	return first, again, nil
}

// devPickSession sets $s to the session for the current directory. ~/a/api and
// ~/b/api both come out as "api", and attaching to the other one's session would
// land in the wrong project without a word. So a session of that name stays if
// it was started here, and otherwise this directory gets a name with a hash of
// its own, which is looked for first: it has to stay this directory's session
// even after the other "api" is gone. Targets start with = to match the name
// exactly, not any session that begins with it.
func devPickSession(dir, session string) string {
	sum := sha256.Sum256([]byte(dir))
	own := fmt.Sprintf("%s-%x", session, sum[:3])
	return fmt.Sprintf(`s=%[1]s; if tmux has-session -t %[3]s 2>/dev/null; then s=%[2]s; `+
		`elif p=$(tmux display-message -p -t %[4]s '#{session_path}' 2>/dev/null) && p=$(cd -- "$p" 2>/dev/null && pwd -P) && `+
		`[ -n "$p" ] && [ "$p" != "$(pwd -P)" ]; then s=%[2]s; fi`,
		shQuote(session), shQuote(own), shQuote("="+own), shQuote("="+session+":"))
}

func devRemoteDir(dir string) string {
	if dir == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return `"$HOME"/` + shQuote(rest)
	}
	return shQuote(dir)
}

// devTunnelArgs is tunnelArgs for tunnels held behind a session: BatchMode so a
// prompt can't fight the session for the terminal, along with tunnelHold.noPrompts
// for the hops BatchMode doesn't reach.
func devTunnelArgs(cfg *config.Config, s tunnelSet, options ...string) ([]string, error) {
	return freshTunnelArgs(cfg, s, append([]string{"-o", "BatchMode=yes"}, options...)...)
}

// devNotice writes one line over the session about a tunnel, and rings the bell
// so a window in the background shows it. The session redraws over it, so the
// log keeps the details. \r\n because the session has the terminal in raw mode.
func devNotice(logPath string) func(string) {
	return func(msg string) {
		fmt.Fprintf(os.Stderr, "\r\n\ameterm: %s (see %s)\r\n", msg, logPath)
	}
}

// openDevTunnelLog opens the file tunnel output goes to, since on the terminal
// it would scribble over the session.
func openDevTunnelLog() (*os.File, string, error) {
	cfg, err := resolveCfgPath()
	if err != nil {
		return nil, "", err
	}
	p := filepath.Join(filepath.Dir(cfg), "dev-tunnels.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, p, nil
}

// restoreTerminal undoes what a full-screen program had switched on when its
// connection vanished: alternate screen, hidden cursor, mouse and paste modes.
func restoreTerminal(w io.Writer) {
	if f, ok := w.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		fmt.Fprint(w, "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[0m\r\n")
	}
}
