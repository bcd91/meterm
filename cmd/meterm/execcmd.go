package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

var execLabelColors = []lipgloss.Color{"39", "214", "78", "205", "141", "81", "208", "111"}

func newExecCmd() *cobra.Command {
	var tags, groups []string
	var all bool
	var parallel int
	var timeout time.Duration
	var format string
	var logArgs, raw bool
	cmd := &cobra.Command{
		Use:   "exec [host...] -- <command...>",
		Short: "Run a command on several saved hosts in parallel",
		Long: "Runs the same non-interactive command on every selected host at once and\n" +
			"streams output back with a per-host prefix, so you don't ssh in one at a time\n" +
			"to do a fleet-wide check or restart.\n\n" +
			"Select hosts by name, --tag, --group, or --all — you can combine them.\n" +
			"Because each host runs unattended (stdin is /dev/null), this is for\n" +
			"commands that don't need a password or an interactive TTY; make sure key-\n" +
			"based auth is set up for every target.\n\n" +
			"Examples:\n" +
			"  meterm exec --tag web -- systemctl restart nginx\n" +
			"  meterm exec --group prod -- uptime\n" +
			"  meterm exec web1 web2 -- df -h\n" +
			"  meterm exec --all --timeout 30s --format json -- uptime\n\n" +
			"Several arguments reach the host exactly as given, each quoted, so `$(...)` or\n" +
			"`;` in one is only text. One argument is a command line for the host's shell:\n" +
			"  meterm exec web1 -- 'df -h | grep /srv'\n" +
			"On a Windows server arguments are joined with spaces, whatever their number,\n" +
			"so don't pass it text you don't trust.\n\n" +
			"Exits nonzero when any host fails, so it can gate a CI step.",
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 0 {
				return fmt.Errorf("missing `--` before the remote command, e.g. `meterm exec --tag web -- uptime`")
			}
			hostArgs := args[:dash]
			command := args[dash:]
			if len(command) == 0 {
				return fmt.Errorf("no command given after --")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := selectHosts(cfg, hostArgs, tags, groups, all)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				return fmt.Errorf("no hosts matched (use names, --tag, --group, or --all)")
			}
			for _, h := range hosts {
				recordActivity(cfg, h, "exec", commandDetail(command, logArgs))
			}
			kickAutosync(false, true)
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json")
			}
			return runExec(hosts, command, sshRunner(cfg), execOptions{parallel: parallel, timeout: timeout, format: format,
				safeOut: !raw && isTerminal(os.Stdout), safeErr: !raw && isTerminal(os.Stderr)})
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "select hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "select hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "select every saved host")
	cmd.Flags().IntVarP(&parallel, "parallel", "j", 10, "maximum concurrent connections")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up on a host after this long, e.g. 30s (default: no limit)")
	cmd.Flags().BoolVar(&raw, "raw", false, "show the hosts' output as it is, escape sequences included")
	cmd.Flags().BoolVar(&logArgs, "log-args", false, "put the command's arguments in the activity log, redacted (default: just the program)")
	cmd.Flags().StringVar(&format, "format", "text", "text (live output + summary) or json (captured output per host, for scripts and CI)")
	return cmd
}

func selectHosts(cfg *config.Config, names, tags, groups []string, all bool) ([]config.Host, error) {
	seen := map[string]bool{}
	var out []config.Host
	add := func(h config.Host) {
		if k := h.User + "@" + h.Name; !seen[k] { // deploy@API and API are two logins
			seen[k] = true
			out = append(out, h)
		}
	}

	for _, n := range names {
		h, idx, err := cfg.Target(n)
		if err != nil {
			return nil, err
		}
		if idx < 0 {
			return nil, fmt.Errorf("no such host: %s", n)
		}
		add(*h)
	}
	for _, h := range cfg.Hosts {
		if all {
			add(h)
			continue
		}
		if hasAny(h.Tags, tags) || containsStr(groups, h.Group) {
			add(h)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func hasAny(haystack, needles []string) bool {
	for _, n := range needles {
		if hasTag(haystack, n) {
			return true
		}
	}
	return false
}

func containsStr(haystack []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// execResult is how one host's run ended.
type execResult struct {
	Host     string  `json:"host"`
	ExitCode int     `json:"exit_code"` // -1 when the command never ran or was killed
	Seconds  float64 `json:"seconds"`
	Error    string  `json:"error,omitempty"` // ssh/timeout trouble, not a plain nonzero exit
	Stdout   string  `json:"stdout,omitempty"`
	Stderr   string  `json:"stderr,omitempty"`
	failed   bool
}

// execRunner runs command on one host, writing its output to stdout/stderr.
type execRunner func(ctx context.Context, h config.Host, command []string, stdout, stderr io.Writer) error

func sshRunner(cfg *config.Config) execRunner {
	return func(ctx context.Context, h config.Host, command []string, stdout, stderr io.Writer) error {
		r := cfg.Resolve(h)
		c := sshrun.SSHContext(ctx, r, execRemoteLine(command, len(command) > 1 && r.Err == nil && isWindowsServer(r)))
		c.Stdin = nil // fan-out is non-interactive; fail fast instead of hanging on a prompt
		c.Stdout = stdout
		c.Stderr = stderr
		return c.Run()
	}
}

// execRemoteLine is the one string ssh sends for command, since the host's shell
// reads whatever ssh sends. One argument is that string already; several are what
// the caller meant as separate words, so they stay that way. cmd.exe doesn't read
// single quotes, so a Windows server gets them joined, as ssh itself would.
func execRemoteLine(command []string, windows bool) string {
	if len(command) == 1 || windows {
		return strings.Join(command, " ")
	}
	return shQuoteWords(command)
}

// shQuoteWords quotes each word for a POSIX shell.
func shQuoteWords(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shQuote(w)
	}
	return strings.Join(quoted, " ")
}

type execOptions struct {
	parallel int
	timeout  time.Duration // per host; 0 = none
	format   string        // text or json
	out      io.Writer     // where streamed output and the summary go
	// safeOut and safeErr drop escape sequences from a host's stdout and stderr, for
	// when they're going to a terminal. Text mode only: JSON escapes them itself.
	safeOut, safeErr bool
}

// runExec runs command on every host. In text mode output streams live with
// a per-host prefix and a summary table follows; in json mode each host's
// output is captured and one JSON array is printed at the end. Either way
// the error is non-nil when any host failed, so scripts and CI can rely on
// meterm's exit code.
func runExec(hosts []config.Host, command []string, run execRunner, o execOptions) error {
	if o.parallel < 1 {
		o.parallel = 1
	}
	if o.out == nil {
		o.out = os.Stdout
	}
	jsonOut := o.format == "json"
	sem := make(chan struct{}, o.parallel)
	var wg sync.WaitGroup
	var stdoutMu sync.Mutex
	results := make([]execResult, len(hosts))

	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h config.Host) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx := context.Background()
			if o.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, o.timeout)
				defer cancel()
			}
			var outBuf, errBuf bytes.Buffer
			var outW, errW io.Writer = &outBuf, &errBuf
			var flush func()
			if !jsonOut {
				label := lipgloss.NewStyle().Bold(true).
					Foreground(execLabelColors[i%len(execLabelColors)]).
					Render(fmt.Sprintf("[%s]", h.Name))
				pw := &prefixWriter{mu: &stdoutMu, out: o.out, prefix: label}
				pe := &prefixWriter{mu: &stdoutMu, out: os.Stderr, prefix: label}
				outW, errW = pw, pe
				if o.safeOut {
					outW = termsafe.New(pw)
				}
				if o.safeErr {
					errW = termsafe.New(pe)
				}
				flush = func() { pw.Flush(); pe.Flush() }
			}
			start := time.Now()
			err := run(ctx, h, command, outW, errW)
			if flush != nil {
				flush()
			}
			r := execResult{Host: h.Name, Seconds: time.Since(start).Seconds(), Stdout: outBuf.String(), Stderr: errBuf.String()}
			var exitErr *exec.ExitError
			switch {
			case ctx.Err() == context.DeadlineExceeded:
				r.ExitCode, r.Error, r.failed = -1, fmt.Sprintf("timed out after %s", o.timeout), true
			case errors.As(err, &exitErr):
				r.ExitCode, r.failed = exitErr.ExitCode(), true
				if r.ExitCode == 255 { // ssh's own exit code: it never reached the command
					r.Error = "ssh failed (unreachable, auth, or host key)"
				}
			case err != nil:
				r.ExitCode, r.Error, r.failed = -1, err.Error(), true
			}
			results[i] = r
		}(i, h)
	}
	wg.Wait()

	failed := 0
	for _, r := range results {
		if r.failed {
			failed++
		}
	}
	if jsonOut {
		enc := json.NewEncoder(o.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else {
		printExecSummary(o.out, results, failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d host(s) failed", failed, len(results))
	}
	return nil
}

func printExecSummary(w io.Writer, results []execResult, failed int) {
	fmt.Fprintf(w, "\n%d/%d succeeded\n", len(results)-failed, len(results))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HOST\tEXIT\tTIME\t")
	for _, r := range results {
		exit := fmt.Sprint(r.ExitCode)
		if r.ExitCode < 0 {
			exit = "-"
		}
		note := ""
		if r.Error != "" {
			note = r.Error
		} else if r.failed {
			note = "FAILED"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%.1fs\t%s\n", r.Host, exit, r.Seconds, note)
	}
	tw.Flush()
}

// prefixWriter labels every complete line written to it with a per-host
// prefix and serializes writes to out, so concurrent goroutines streaming
// output from several hosts at once don't interleave mid-line.
type prefixWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		w.writeLine(w.buf[:idx])
		w.buf = w.buf[idx+1:]
	}
	return len(p), nil
}

func (w *prefixWriter) Flush() {
	if len(w.buf) > 0 {
		w.writeLine(w.buf)
		w.buf = nil
	}
}

func (w *prefixWriter) writeLine(line []byte) {
	w.mu.Lock()
	fmt.Fprintf(w.out, "%s %s\n", w.prefix, line)
	w.mu.Unlock()
}
