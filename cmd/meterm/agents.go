package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshrun"
	"meterm/internal/status"
	"meterm/internal/termsafe"
)

// `meterm agents`: which Claude Code sessions are running on which hosts.
//
// It deliberately writes no activity entry, unlike `exec`. The activity log is
// tamper-evident and shared with the team, so nothing can be taken out of it later:
// a status bar asking every 30 seconds would bury everything else in there, and each
// entry would tell the whole team what's in this user's working directories. There's
// also little to record, because the command that runs is one constant that reads
// and changes nothing (agents.RemoteCommand), with no user data in it. So this also
// doesn't touch history or team sync. What still shows is the host's own: sshd logs
// each login there. Anything else on a host belongs to `meterm exec`, which records.

const (
	// Exit codes of `meterm agents`. 1 is any error from before a host was asked,
	// like every other command; 2 means there's a report, and it isn't complete.
	agentsIncomplete = 2

	agentsDefaultTimeout  = 8 * time.Second
	agentsDefaultParallel = 8
	agentsMaxParallel     = 64
)

func newAgentsCmd() *cobra.Command {
	var tags, groups []string
	var format string
	var timeout time.Duration
	var parallel int
	cmd := &cobra.Command{
		Use:   "agents [host...] [--tag t] [--group g]",
		Short: "List the Claude Code sessions running on hosts (read-only, not logged)",
		Long: "Asks each selected host for `claude agents --json`, and nothing else: the\n" +
			"command is fixed and read-only. Hosts have to be chosen, by name, --tag or\n" +
			"--group; there's no way to ask all of them. A production host answers only\n" +
			"when named: picked up by a tag or group, it's listed as refused.\n\n" +
			"It writes no activity entry, so it's safe to poll: that log is permanent and\n" +
			"shared with the team. What shows instead is each login in the host's sshd log.\n\n" +
			"--format json prints {\"schema\":1,\"hosts\":[{\"host\",\"reachable\",\"error\",\"agents\"}]}.\n" +
			"reachable says the command ran. agents is null when error is set: a host that\n" +
			"couldn't say isn't a host with no agents.\n\n" +
			"Exit codes:\n" +
			"  0  every host answered (having no agents is an answer)\n" +
			"  1  nothing was asked: bad flags, no selection, or an unknown host\n" +
			"  2  the report is on stdout, and at least one host has an error in it",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json")
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be above zero")
			}
			if parallel < 1 || parallel > agentsMaxParallel {
				return fmt.Errorf("-j must be between 1 and %d", agentsMaxParallel)
			}
			if len(args) == 0 && len(tags) == 0 && len(groups) == 0 {
				return fmt.Errorf("say which hosts: name them, or use --tag or --group; `agents` won't go to every host")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			jobs, err := selectAgentHosts(cfg, args, tags, groups)
			if err != nil {
				return err
			}
			// Its ssh processes have no terminal, so Ctrl-C has to reach them from here.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			rep := runAgents(ctx, jobs, sshAgentQuery(timeout), timeout, parallel)
			if err := writeAgents(os.Stdout, rep, format); err != nil {
				return err
			}
			if failed := agentsFailed(rep); failed > 0 {
				fmt.Fprintf(os.Stderr, "meterm: %d of %d host(s) didn't answer\n", failed, len(rep.Hosts))
				finish(agentsIncomplete)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "select hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "select hosts in this group (repeatable)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().DurationVar(&timeout, "timeout", agentsDefaultTimeout, "give up on a host after this long")
	cmd.Flags().IntVarP(&parallel, "parallel", "j", agentsDefaultParallel, "maximum hosts asked at once")
	return cmd
}

// agentJob is one host to ask. refused holds why not to, if so.
type agentJob struct {
	label   string
	host    config.Host
	resolve func() config.Resolved
	refused string
}

const agentsRefusedProduction = "refused: production host; name it to include it"

// selectAgentHosts picks the hosts, and says which production hosts weren't asked
// for by name. Those stay in the list, refused, so the report shows they're missing.
func selectAgentHosts(cfg *config.Config, names, tags, groups []string) ([]agentJob, error) {
	hosts, err := selectHosts(cfg, names, tags, groups, false)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no hosts matched")
	}
	named := map[string]bool{}
	for _, n := range names {
		if h, i, err := cfg.Target(n); err == nil && i >= 0 {
			named[h.User+"@"+h.Name] = true
		}
	}
	jobs := make([]agentJob, 0, len(hosts))
	for _, h := range hosts {
		label := h.Name
		if saved, i := cfg.Find(h.Name); i >= 0 && saved.User != h.User {
			label = h.User + "@" + h.Name // deploy@API and API are two logins
		}
		job := agentJob{label: label, host: h, resolve: func() config.Resolved { return cfg.Resolve(h) }}
		if cfg.IsProduction(h) && !named[h.User+"@"+h.Name] {
			job.refused = agentsRefusedProduction
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// agentQuery runs the command on a host. Its stderr is for telling failures apart,
// and goes no further than that. overflow says stdout went past the limit.
type agentQuery func(ctx context.Context, r config.Resolved) (stdout []byte, stderr string, overflow bool, err error)

func runAgents(ctx context.Context, jobs []agentJob, query agentQuery, timeout time.Duration, parallel int) agents.Report {
	rep := agents.Report{Schema: agents.Schema, Hosts: make([]agents.Host, len(jobs))}
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rep.Hosts[i] = askAgents(ctx, job, query, timeout)
		}()
	}
	wg.Wait()
	return rep
}

func agentsFailed(rep agents.Report) int {
	n := 0
	for _, h := range rep.Hosts {
		if h.Error != "" {
			n++
		}
	}
	return n
}

// askAgents turns one host's run into its line of the report. Every error is in
// meterm's words: a banner or a shell's startup file can say anything on stderr,
// and that's not to be taken for what meterm found.
func askAgents(ctx context.Context, job agentJob, query agentQuery, timeout time.Duration) agents.Host {
	out := agents.Host{Host: termsafe.Text(job.label)}
	if job.refused != "" {
		out.Error = job.refused
		return out
	}
	r := job.resolve()
	if r.Err != nil {
		out.Error = "not asked: " + termsafe.Text(r.Err.Error())
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, overflow, err := query(ctx, r)
	var exit *exec.ExitError
	switch {
	case overflow:
		out.Reachable = true
		out.Error = "unexpected output from claude: more than meterm reads"
		return out
	case ctx.Err() == context.DeadlineExceeded:
		out.Error = "timed out after " + timeout.String()
		return out
	case ctx.Err() != nil:
		out.Error = "stopped before it answered"
		return out
	case errors.As(err, &exit):
		switch exit.ExitCode() {
		case -1: // ssh itself was killed, so nothing is known of the host
			out.Error = "ssh was killed before it answered"
		case sshConnectionError:
			out.Error = sshFailureText(stderr)
		case agents.ExitNoClaude:
			out.Reachable = true
			out.Error = "claude not found on this host"
		default:
			out.Reachable = true
			out.Error = "claude agents failed there; it may be too old to have --json"
		}
		return out
	case err != nil && !errors.Is(err, exec.ErrWaitDelay):
		out.Error = "couldn't run ssh: " + termsafe.Text(err.Error())
		return out
	}
	out.Reachable = true
	list, err := agents.Parse(stdout)
	if err != nil {
		out.Error = "unexpected output from claude: " + err.Error()
		return out
	}
	out.Agents = list
	return out
}

// sshFailureText says why ssh couldn't get in.
func sshFailureText(stderr string) string {
	if f := sshrun.ClassifyFailure(stderr); f != sshrun.FailureNone {
		return "unreachable: " + f.String()
	}
	for _, m := range []struct{ mark, text string }{
		{"Connection refused", "connection refused"},
		{"Could not resolve hostname", "no such host name"},
		{"Name or service not known", "no such host name"},
		{"No route to host", "no route to host"},
		{"Network is unreachable", "no network"},
		{"timed out", "connection timed out"},
	} {
		if strings.Contains(stderr, m.mark) {
			return "unreachable: " + m.text
		}
	}
	return "unreachable: ssh couldn't connect"
}

// sshAgentQuery runs agents.RemoteCommand over ssh.
func sshAgentQuery(timeout time.Duration) agentQuery {
	return func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		r.Forwards = nil
		r.ExtraArgs = agentSSHArgs(r, timeout, agentControlPath(r))
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		c := sshrun.SSHContext(ctx, r, agents.RemoteCommand)
		noPrompts(c)
		stdout := &cappedBuffer{max: agents.MaxOutput, over: cancel}
		stderr := &sshrun.Tail{Max: 16 << 10}
		c.Stdout, c.Stderr = stdout, stderr
		// A master that stays behind, or -J's ssh, can hold stderr open after this
		// ssh has ended. There's nothing more to wait for by then.
		c.WaitDelay = time.Second
		err := c.Run()
		return stdout.b, stderr.String(), stdout.overflow, err
	}
}

// agentSSHArgs are the options for one run, on its command line alone: nothing is
// written to anyone's ssh config. ssh keeps the first value it sees of an option, so
// meterm's come before the host's extra args, which may include a teammate's
// BatchMode=no. -T comes after them because of the two tty flags the last one wins.
//
// Forwards from the user's ssh config are cleared and a RemoteCommand there is
// overridden: this login is for the one command.
func agentSSHArgs(r config.Resolved, timeout time.Duration, controlPath string) []string {
	args := []string{"-n",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(int(math.Ceil(timeout.Seconds()))),
		"-o", "ConnectionAttempts=1",
		"-o", "LogLevel=ERROR",
		"-o", "RemoteCommand=none",
		"-o", "RequestTTY=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "PermitLocalCommand=no",
		// A dead master has to go soon, or every look waits on it until its timeout.
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
	}
	if controlPath == "" {
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
	} else {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+controlPath, "-o", "ControlPersist="+agentControlPersist)
	}
	args = append(args, r.ExtraArgs...)
	return append(args, "-T")
}

const (
	// agentControlPersist has one poll's login carry over to the next one, at a
	// status bar's 30 seconds, and then it's gone.
	agentControlPersist = "60"
	// agentControlPathMax is macOS's 104 bytes for a socket's path, less the
	// terminating NUL, less the 18 bytes of the random suffix ssh binds it under
	// first. Linux allows 108, and the smaller limit does there too.
	agentControlPathMax = 104 - 1 - 18
)

// agentControlPathRE is what may go into ssh's option without a thought: no %
// tokens, spaces or quotes from a $TMPDIR.
var agentControlPathRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// agentControlPath is where this host's short-lived master lives, or "" for none.
// None for production, for any host on the way there, and wherever it can't be
// done safely: then every look is a login of its own, which works just the same.
//
// `dev` and `tunnel up` are out of its reach. Those set ControlPath=none, and this
// path is known only to this command, so neither can land on this master or lose
// anything when it goes.
func agentControlPath(r config.Resolved) string {
	if runtime.GOOS == "windows" || usesProduction(r) {
		return ""
	}
	return controlPathIn(r, []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir(), "/tmp"}, status.EnsureDir)
}

func usesProduction(r config.Resolved) bool {
	if r.IsProduction() {
		return true
	}
	for _, hop := range r.ProxyJump {
		if usesProduction(hop) {
			return true
		}
	}
	return false
}

// controlPathIn picks the first base that gives a short enough path, in a directory
// that's this user's own. The file's name covers everything about the connection, so
// another route or login never lands on this one's master.
func controlPathIn(r config.Resolved, bases []string, ensure func(string) error) string {
	r.Forwards = nil
	sum := sha256.Sum256([]byte(strings.Join(sshrun.BuildArgs(r), "\x00")))
	name := hex.EncodeToString(sum[:8])
	for _, base := range bases {
		if base == "" {
			continue
		}
		dir := filepath.Join(base, "meterm-cm-"+strconv.Itoa(os.Getuid()))
		p := filepath.Join(dir, name)
		if !filepath.IsAbs(p) || len(p) > agentControlPathMax || !agentControlPathRE.MatchString(p) {
			continue
		}
		if ensure(dir) == nil {
			return p
		}
	}
	return ""
}

// cappedBuffer keeps up to max bytes. Past that it stops keeping, and calls over.
type cappedBuffer struct {
	max      int
	b        []byte
	overflow bool
	over     func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.overflow {
		return len(p), nil
	}
	if len(c.b)+len(p) > c.max {
		c.overflow = true
		c.b = nil
		if c.over != nil {
			c.over()
		}
		return len(p), nil
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

func writeAgents(w io.Writer, rep agents.Report, format string) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	// All at once, as in writeStatus.
	var buf bytes.Buffer
	t := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(t, "HOST\tSTATUS\tKIND\tPID\tFOR\tNAME\tCWD")
	for _, h := range rep.Hosts {
		switch {
		case h.Error != "":
			word := "error"
			if !h.Reachable {
				word = "unreachable"
			}
			if h.Error == agentsRefusedProduction {
				word = "refused"
			}
			fmt.Fprintf(t, "%s\t%s\t\t\t\t%s\t\n", h.Host, word, h.Error)
		case len(h.Agents) == 0:
			fmt.Fprintf(t, "%s\tno agents\t\t\t\t\t\n", h.Host)
		}
		for _, a := range h.Agents {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Host, agentStatus(a), a.Kind, agentPID(a), agentAge(a, time.Now()), agentName(a), a.Cwd)
		}
	}
	if err := t.Flush(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func agentStatus(a agents.Agent) string {
	s := a.Status
	if s == "" {
		s = a.State
	}
	if s == "" {
		s = "?"
	}
	if a.WaitingFor != "" {
		s += " (" + a.WaitingFor + ")"
	}
	return s
}

func agentPID(a agents.Agent) string {
	if a.PID == nil {
		return ""
	}
	return strconv.FormatInt(*a.PID, 10)
}

func agentAge(a agents.Agent, now time.Time) string {
	if a.StartedAt == nil {
		return ""
	}
	d := now.Sub(time.UnixMilli(*a.StartedAt)).Round(time.Second)
	if d < 0 {
		return ""
	}
	return d.String()
}

func agentName(a agents.Agent) string {
	for _, s := range []string{a.Name, a.ID, a.SessionID} {
		if s != "" {
			return s
		}
	}
	return ""
}
