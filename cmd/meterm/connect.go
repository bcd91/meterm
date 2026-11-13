package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/history"
	"meterm/internal/sshrun"
	"meterm/internal/tui"
)

func newConnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "connect <name|user@name> [--tmux] [-- [ssh-options] [command]]",
		Aliases: []string{"c", "ssh"},
		Short:   "Connect to a saved host",
		Long: "Connects to a saved host by name; user@name logs in as another account (see\n" +
			"`users` in `meterm edit`).\n\n" +
			"--tmux works inside tmux on the host, so a dropped connection loses nothing:\n" +
			"with sessions already there it lists them to pick one up again (or start a new\n" +
			"one), otherwise it starts a session called meterm. Without tmux on the host it\n" +
			"connects as usual. Takes ssh options after --, not a command.\n\n" +
			"The activity log says how many arguments followed, and --log-args puts them\n" +
			"there too, redacted.",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			if rest, _ := takeFlag(args, "--tmux"); len(rest) == 0 {
				return cmd.Help()
			} else if rest, _ = takeFlag(rest, "--log-args"); len(rest) == 0 {
				return cmd.Help()
			}
			quietOnSuccess, sessionEnded = true, true
			code, err := connect(args)
			if err != nil {
				return err
			}
			if code != 0 {
				finish(code)
			}
			return nil
		},
	}
	return cmd
}

// connect runs ssh to the host args name, with what follows it, and returns
// ssh's exit code. Only a connection that got there counts as the host's
// last connection, and the activity log says how the attempt ended.
func connect(args []string) (int, error) {
	args, useTmux := takeFlag(args, "--tmux")
	args, logArgs := takeFlag(args, "--log-args")
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	name := args[0]
	extra := connectArgs(args[1:])
	h, idx, err := cfg.Target(name)
	if err != nil {
		return 0, err
	}
	if idx < 0 {
		return 0, fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", name)
	}
	r := cfg.Resolve(*h)
	if via := routeNote(r); via != "" {
		fmt.Fprintf(os.Stderr, "meterm: %s\n", lbl(via))
	}
	if useTmux && r.Err == nil {
		if remote, ok := tmuxCommand(name, r); ok {
			extra = append(append(extra, "-t"), remote)
		}
	}

	// Two entries: a start before ssh runs, so a crash, SIGKILL or power loss
	// mid-session still leaves evidence that it started, and an outcome when it
	// ends. The second needs meterm to outlive a Ctrl-C or a closed terminal,
	// which reach ssh as well; a TERM sent to meterm alone is passed on.
	run := beginActivity(cfg, *h, "connect", connectDetail(extra, logArgs))
	kickAutosync(false, true)
	c := sshrun.Command(r, extra...)
	started, runErr := runOutliving(c)
	if !started {
		run.end(runErr)
		return 0, runErr
	}
	code, o := connectOutcome(runErr, time.Since(run.began))
	if code != 255 && code >= 0 {
		_ = history.RecordAndSave(h.Name) // best-effort; a broken history file shouldn't fail the command
		// Tell the picker the session ran: meterm's own errors exit 1 too.
		if p := os.Getenv(tui.SessionOutcomeEnv); p != "" {
			_ = os.WriteFile(p, []byte(fmt.Sprintf("%d\n", code)), 0o600)
		}
	}
	run.outcome(o)
	kickAutosync(false, true)
	if code < 0 {
		return 1, nil
	}
	return code, nil
}

// connectOutcome is ssh's exit code and the outcome the log keeps. ssh exits
// 255 when it couldn't connect or log in; any other code is the remote
// shell's or command's own, so the session ran. Killed by a signal, it's -1:
// whether it got there is unknown.
func connectOutcome(err error, took time.Duration) (int, activity.Outcome) {
	o := outcomeOf(err, took)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, o
	case !errors.As(err, &exitErr):
		return -1, o
	}
	switch code := exitErr.ExitCode(); code {
	case 255:
		// ssh's own failure, but after hours it's more likely a dropped line
		// than a refused login: from here the two look the same.
		o.Note = "connection lost, or never made"
		return 255, o
	case -1:
		o.Note = "ended by a signal, whether it connected is unknown"
		return -1, o
	default:
		return code, o
	}
}

// shortDuration is d to the second without the zero parts: 3h, 2h3m, 1m30s, 0s.
func shortDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// connectArgs is what follows the host name, handed to ssh. The "--" that
// separates it from meterm's own arguments is meterm's, not ssh's: passed
// on, it would end ssh's options there, so `connect web1 -- -t "sudo …"`
// would run "-t sudo …" on the host instead of getting a terminal.
func connectArgs(rest []string) []string {
	if len(rest) > 0 && rest[0] == "--" {
		return rest[1:]
	}
	return rest
}

// connectDetail is what the log says about what followed the host name, which
// can be a remote command.
func connectDetail(extra []string, logArgs bool) string {
	switch {
	case len(extra) == 0:
		return ""
	case logArgs:
		return strings.Join(extra, " ")
	}
	return fmt.Sprintf("(+%d args)", len(extra))
}

// takeFlag pulls a flag out of connect's own arguments (those before
// "--"); connect parses its flags by hand so the rest can go to ssh as is.
func takeFlag(args []string, flag string) ([]string, bool) {
	var out []string
	found := false
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...), found
		}
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// runOutliving runs c, which shares the terminal, and waits for it while
// staying up through a Ctrl-C or a closed terminal (they reach c as well),
// so the caller can still log how c ended. A TERM sent to meterm alone is
// passed on to c. started is false when c couldn't be started at all.
func runOutliving(c *exec.Cmd) (started bool, err error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := c.Start(); err != nil {
		return false, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case err := <-done:
			return true, err
		case sig := <-sigs:
			if sig == syscall.SIGTERM && c.Process != nil {
				_ = c.Process.Signal(sig)
			}
		}
	}
}
