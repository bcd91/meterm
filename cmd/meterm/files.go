package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/fileman"
	"meterm/internal/sshrun"
)

func newFilesCmd() *cobra.Command {
	var localDir string
	var shell, yes bool
	cmd := &cobra.Command{
		Use:     "files <host> [remote-dir]",
		Aliases: []string{"fm"},
		Short:   "Two-pane file manager: this machine on the left, the host on the right",
		Long: "Browse both sides and copy between them, like vifm or Midnight Commander.\n\n" +
			"  tab        switch side            h j k l / arrows   move, enter, go up\n" +
			"  space      select / unselect      *                  select all / none\n" +
			"  c (F5)     copy to the other side d (F8)             delete (asks first)\n" +
			"  R (F6)     rename                 m (F7)             new directory\n" +
			"  /          find by name (n: next) .                  show hidden files\n" +
			"  ~          home directory         g G                top, bottom\n" +
			"  r          reload                 q                  quit\n" +
			"  ?          all keys               esc                dismiss a message, then\n" +
			"                                                       clear the selection\n" +
			"  esc        cancel what's running; press again (or ctrl+c) if the host\n" +
			"             has stopped answering, to drop the connection and quit.\n" +
			"             A file you agreed to have written in place is finished first\n" +
			"             (esc again stops it at once, leaving it partly written).\n" +
			"             meterm never removes an existing file it is overwriting.\n\n" +
			"The keys mean what they mean in the host picker (meterm with no\n" +
			"arguments) wherever both have the idea: r reloads, R renames, d deletes,\n" +
			"space marks, / finds, esc clears, ? helps, q quits.\n\n" +
			"A copy is written under a temporary name and renamed into place when\n" +
			"complete, so a cancelled or failed copy leaves the target as it was.\n" +
			"No room for that name (a full disk) stops the copy, the target as it\n" +
			"was; a target that can't be renamed over (bind-mounted) is written in\n" +
			"place, and a file you may not write is never renamed over.\n" +
			"Copying over existing files asks first: overwrite them, or skip them.\n" +
			"An overwrite keeps the old file's permissions, setuid too, and its\n" +
			"owner and group where they can be given; when they can't, or the file\n" +
			"has other hard links (seen on this machine only), it asks whether to\n" +
			"write in place instead. It never keeps ACLs or extended attributes.\n" +
			"The README says exactly what's kept.\n" +
			"It runs SFTP through your own ssh, so jump hosts and keys work as for\n" +
			"`meterm connect`. Uploads, downloads, deletes, renames and new directories\n" +
			"on the host are recorded in the activity log, each as a start and an\n" +
			"outcome, as for `meterm cp`.\n\n" +
			"On a production host it asks first (--yes: without asking; from the\n" +
			"picker, which has asked already, it doesn't ask again; with stdin not a\n" +
			"terminal it warns and goes on), the host's pane is marked ⚑PROD, and its\n" +
			"delete and overwrite questions say PRODUCTION. The activity log says\n" +
			"which it was.\n\n" +
			"--shell opens OpenSSH's plain sftp prompt (ls, cd, get, put, mget, mput)\n" +
			"instead. For one-off copies from scripts, use `meterm cp`.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Asked before anything connects, for the sftp prompt as well.
			note := ""
			if cfg, err := loadConfig(); err == nil {
				if h, idx, err := cfg.Target(args[0]); err == nil && idx >= 0 {
					var ok bool
					if note, ok = filesProductionOK(cfg, *h, yes, shell); !ok {
						// Not what was asked for: a script must see it didn't happen.
						return fmt.Errorf("%w: nothing was opened", errCancelled)
					}
				}
			}
			if shell {
				return runSftpShell(args)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
			}
			host := *h

			logDir := "~"
			if len(args) == 2 {
				logDir = args[1]
			}
			fail := func(err error) error {
				recordActivity(cfg, host, "sftp", "failed: files "+logDir+withNote("", note)+": "+lbl(err.Error()))
				return err
			}

			c := sshrun.SubsystemCommand(cfg.Resolve(host), "sftp")
			stdin, err := c.StdinPipe()
			if err != nil {
				return err
			}
			stdout, err := c.StdoutPipe()
			if err != nil {
				return err
			}
			var stderr limitedBuffer
			c.Stderr = &stderr
			if err := c.Start(); err != nil {
				return fail(fmt.Errorf("start ssh: %w", err))
			}
			client, err := sftp.NewClientPipe(stdout, stdin,
				sftp.UseConcurrentWrites(true), sftp.MaxConcurrentRequestsPerFile(64))
			if err != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
				// ssh's stderr carries whatever the host and its banner
				// sent: shown as plain lines, never as terminal commands.
				msg := strings.TrimSpace(lblLines(strings.ReplaceAll(stderr.String(), "\r\n", "\n")))
				if msg == "" {
					msg = err.Error()
				}
				return fail(fmt.Errorf("couldn't open SFTP on %s: %s", lbl(host.Name), msg))
			}
			defer func() {
				client.Close()
				_ = c.Wait()
			}()

			remote := fileman.Remote{Host: host.Name, Client: client}
			rdir := ""
			if len(args) == 2 {
				rdir = args[1]
			} else if rdir, err = remote.Home(); err != nil {
				rdir = "."
			}
			ldir := localDir
			if ldir == "" {
				if ldir, err = os.Getwd(); err != nil {
					ldir, _ = os.UserHomeDir()
				}
			}

			// The session is a start and an outcome; each change it makes on
			// the host is an event of its own, how it went first.
			run := beginActivity(cfg, host, "sftp", noteDetail(note, "files "+rdir))
			kickAutosync(false, true)
			err = fileman.RunUI(fileman.Options{
				Left: fileman.Local{}, Right: remote, LeftDir: ldir, RightDir: rdir,
				// Killing ssh closes the pipe, which ends every SFTP call
				// still waiting on a host that has stopped answering.
				Abort:   func() { _ = c.Process.Kill() },
				Env:     cfg.Env(host),
				OnEvent: fileEventLogger(cfg, host, note),
			})
			run.end(err)
			if errors.Is(err, fileman.ErrDropped) {
				tellPicker("stopped")
			}
			// A full-screen app needs no "press enter" pause when it ends
			// cleanly; when it doesn't, the reason stays until enter.
			quietOnSuccess = true
			return err
		},
	}
	cmd.Flags().StringVar(&localDir, "local", "", "local directory to start in (default: the current directory)")
	cmd.Flags().BoolVar(&shell, "shell", false, "open the plain sftp prompt instead of the two-pane manager")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before opening a production host")
	return cmd
}

// fileEventLogger is the file manager's OnEvent for a session on host:
// each copy, delete, rename and mkdir there is a start, logged before it
// touches anything (with note, the production note, as cp has it), and an
// outcome pointing back at it, as connect, exec and cp are. An answer given
// part way is an event of its own. What only the end knows (a file written
// in place, an error, what was done before a stop) can't go in an outcome,
// whose note is a word from this code, so it's logged in words first, as
// the file manager has always said it. Events on this machine aren't logged.
func fileEventLogger(cfg *config.Config, host config.Host, note string) func(fileman.Event) {
	var mu sync.Mutex // the file manager calls from more than one goroutine
	runs := map[int64]*activityRun{}
	return func(e fileman.Event) {
		if !e.Remote {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case e.Outcome == "started":
			runs[e.Op] = beginActivity(cfg, host, "sftp", noteDetail(note, e.Kind+" "+e.Detail))
		case !e.Ends():
			recordActivity(cfg, host, "sftp", e.Line())
		default:
			run := runs[e.Op]
			delete(runs, e.Op)
			if e.More || run == nil {
				recordActivity(cfg, host, "sftp", e.Line())
			}
			if run != nil {
				run.outcome(fileOutcome(e.Outcome, time.Since(run.began)))
			}
		}
	}
}

// fileOutcome is how a file manager operation ended, for the log: exit 0
// for one that did what was asked (or found it done already), otherwise no
// exit code and the file manager's word for it.
func fileOutcome(word string, took time.Duration) activity.Outcome {
	o := activity.Outcome{Took: took}
	zero := 0
	switch word {
	case "ok":
		o.ExitCode = &zero
	case "already gone", "already there":
		o.ExitCode, o.Note = &zero, word
	case "failed", "partial", "cancelled", "stopped", "aborted":
		o.Note = word
	default:
		o.Note = "ended" // a word this code doesn't know: not repeated
	}
	return o
}

// filesProductionOK asks before the file manager opens on a host that is
// production or is reached through one (config.TouchesProduction), y only.
// It doesn't ask with --yes, nor when the picker that started this process
// asked already and left its one-time token, taken before any command runs
// (pickerAsked). With stdin not a terminal nothing can answer: it goes on,
// says so on stderr. It returns what the log should say about it ("" when
// the host doesn't touch production; "via …" when only its route does) and
// whether to go on.
func filesProductionOK(cfg *config.Config, h config.Host, yes, shell bool) (string, bool) {
	if !cfg.TouchesProduction(h) {
		return "", true
	}
	via, what := "", "a PRODUCTION host"
	if !cfg.IsProduction(h) {
		via, what = "via ", "reached through a PRODUCTION host"
	}
	p := pickerAsked()
	switch {
	case p.asked:
		return via + prodPickerAsked, true
	case p.from:
		// Only the picker's own children are told: it should have left a token.
		fmt.Fprintf(os.Stderr, "meterm: %s; asking here.\n", p.why)
	}
	if yes {
		fmt.Fprintf(os.Stderr, "⚠ opening files on %s, %s.\n", lbl(h.Name), what)
		return via + prodYes, true
	}
	if !stdinTerminal() {
		fmt.Fprintf(os.Stderr, "⚠ opening files on %s, %s, not asking (stdin isn't a terminal).\n", lbl(h.Name), what)
		return via + prodNoTerminal, true
	}
	if via == "" {
		fmt.Printf("⚠ %s is a PRODUCTION host: deletes and overwrites there are for real.\n", lbl(h.Name))
	} else {
		fmt.Printf("⚠ %s is reached through a PRODUCTION host.\n", lbl(h.Name))
	}
	q := "Open the file manager on it?"
	if shell {
		q = "Open the sftp prompt on it?"
	}
	if !confirm(q) {
		return "", false
	}
	return via + prodAsked, true
}

// limitedBuffer keeps the first 8 KB of ssh's stderr: enough for the error
// that explains a failed connection, without growing during a long session.
type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := 8192 - len(l.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }
