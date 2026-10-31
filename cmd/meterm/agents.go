package main

import (
	"bytes"
	"context"
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
	"meterm/internal/route"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

// `meterm agents`: which Claude Code sessions are running on which hosts.
//
// It deliberately writes no activity entry, unlike `exec`. The activity log is
// tamper-evident and shared with the team, so nothing can be taken out of it later:
// a status bar asking every 30 seconds would bury everything else in there. There's
// also little to record, because the command that runs is one constant that reads
// and changes nothing (agents.RemoteCommand), with no user data in it. So this also
// doesn't touch history or team sync. A look leaves nothing on this machine: no route
// remembered, no host key carried. What still shows is the host's own: sshd logs every
// look as a login, since none is reused, and a revoked key stops working at once.
// Anything else on a host belongs to `meterm exec`, which records.

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
			"--group; there's no way to ask all of them. A production host, or one reached\n" +
			"through a production host, answers only when named: picked up by a tag or\n" +
			"group, it's listed as refused. So is a host whose extra_args could do more than\n" +
			"this command should.\n\n" +
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
			if err := checkAgentsClient(agentsGOOS); err != nil {
				return err
			}
			if len(args) == 0 && len(tags) == 0 && len(groups) == 0 {
				return fmt.Errorf("say which hosts: name them, or use --tag or --group; `agents` won't go to every host")
			}
			path, err := resolveCfgPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path) // not loadConfig: its route picker writes
			if err != nil {
				return err
			}
			jobs, err := selectAgentHosts(cfg, args, tags, groups)
			if err != nil {
				return err
			}
			sshConfig, removeSSHConfig, err := writeAgentSSHConfig()
			if err != nil {
				return err
			}
			// Its ssh processes have no terminal, so Ctrl-C has to reach them from here.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			rep := runAgents(ctx, jobs, sshAgentQuery(sshConfig, timeout), timeout, parallel)
			removeSSHConfig()
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

// agentsGOOS is the machine `agents` runs on; tests set it.
var agentsGOOS = runtime.GOOS

// checkAgentsClient refuses Windows as the machine asking. A look depends on a temp ssh
// config reaching each jump hop: OpenSSH puts that path unquoted into the command it builds
// for a hop, and a config's Include lines name Unix paths. Neither is known to hold for
// Windows' ssh and C:\ paths, and a look that silently lost its options would be worse.
func checkAgentsClient(goos string) error {
	if goos == "windows" {
		return errors.New("`agents` doesn't run on Windows yet, as it can't be sure its ssh options reach jump hops there; run it from Linux or macOS")
	}
	return nil
}

// agentJob is one host to ask. refused holds why not to, if so.
type agentJob struct {
	label   string
	host    config.Host
	resolve func(context.Context) config.Resolved
	refused string
}

const agentsRefusedProduction = "refused: production host or route; name it to include it"

// selectAgentHosts picks the hosts, and says which ones that are production, or are
// reached through production, weren't asked for by name. Those stay in the list,
// refused, so the report shows they're missing.
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
	look := route.NewLook(cfg, filepath.Join(filepath.Dir(cfg.Path()), "routes.json"))
	jobs := make([]agentJob, 0, len(hosts))
	for _, h := range hosts {
		label := h.Name
		if saved, i := cfg.Find(h.Name); i >= 0 && saved.User != h.User {
			label = h.User + "@" + h.Name // deploy@API and API are two logins
		}
		job := agentJob{label: label, host: h, resolve: func(ctx context.Context) config.Resolved {
			return cfg.WithRoutePicker(look.Look(ctx)).Resolve(h)
		}}
		if cfg.TouchesProduction(h) && !named[h.User+"@"+h.Name] {
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := job.resolve(ctx)
	if r.Err != nil {
		out.Error = "not asked: " + termsafe.Text(r.Err.Error())
		return out
	}
	if err := checkAgentExtraArgs(r.ExtraArgs); err != nil {
		out.Error = "not asked: " + termsafe.Text(err.Error())
		return out
	}
	if ctx.Err() == context.DeadlineExceeded {
		out.Error = "timed out after " + timeout.String()
		return out
	}
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

// sshAgentQuery runs agents.RemoteCommand over ssh. r.ExtraArgs have passed
// checkAgentExtraArgs by now.
func sshAgentQuery(sshConfig string, timeout time.Duration) agentQuery {
	return func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		r.Forwards = nil
		r.ExtraArgs = agentSSHArgs(r, sshConfig, timeout)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		c := sshrun.SSHContext(ctx, r, agents.RemoteCommand)
		noPrompts(c)
		stdout := &cappedBuffer{max: agents.MaxOutput, over: cancel}
		stderr := &sshrun.Tail{Max: 16 << 10}
		c.Stdout, c.Stderr = stdout, stderr
		// -J's ssh can hold stderr open after this ssh has ended. There's nothing more
		// to wait for by then.
		c.WaitDelay = time.Second
		err := c.Run()
		return stdout.b, stderr.String(), stdout.overflow, err
	}
}

// agentFixedOptions make a look one login that runs one command and leaves nothing
// behind. No master: every look is a login of its own, which sshd sees, and which a
// revoked key can't get past. A key that isn't trusted is an error and not a question,
// and known_hosts stays as it is.
var agentFixedOptions = []string{
	"BatchMode=yes",
	"StrictHostKeyChecking=yes",
	"UpdateHostKeys=no",
	"ControlMaster=no",
	"ControlPath=none",
	"LogLevel=ERROR",
	"RemoteCommand=none",
	"RequestTTY=no",
	"ClearAllForwardings=yes",
	"ForwardAgent=no",
	"ForwardX11=no",
	"PermitLocalCommand=no",
}

// agentSSHArgs are the options for one run, on its command line alone: nothing is
// written to anyone's ssh config. ssh keeps the first value it sees of an option, so
// meterm's come before the host's extra args. That alone isn't enough, as a flag beats
// any -o and a bare word becomes the destination, which is why those have to pass
// checkAgentExtraArgs. -T comes after them because of the two tty flags the last one
// wins. sshConfig carries the same options to jump hops, where -o doesn't reach.
func agentSSHArgs(r config.Resolved, sshConfig string, timeout time.Duration) []string {
	args := []string{"-n"}
	if sshConfig != "" {
		args = append(args, "-F", sshConfig)
	}
	for _, o := range agentFixedOptions {
		args = append(args, "-o", o)
	}
	args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(int(math.Ceil(timeout.Seconds()))), "-o", "ConnectionAttempts=1")
	args = append(args, r.ExtraArgs...)
	return append(args, "-T")
}

// writeAgentSSHConfig writes the file behind ssh's -F for this run, and returns how to
// remove it. -J starts an ssh for each hop that gets none of the -o options, but it
// does get -F, so this is how a hop comes to obey them. The first value of an option
// wins here too, and the two Includes are what ssh would have read anyway. The path goes
// unquoted into the command ssh builds for a hop, so it has to be a plain one.
func writeAgentSSHConfig() (string, func(), error) {
	var b strings.Builder
	for _, o := range agentFixedOptions {
		key, val, _ := strings.Cut(o, "=")
		fmt.Fprintf(&b, "%s %s\n", key, val)
	}
	b.WriteString("Include ~/.ssh/config\nInclude /etc/ssh/ssh_config\n")
	for _, base := range []string{os.TempDir(), "/tmp"} {
		if !agentPlainPathRE.MatchString(base) {
			continue
		}
		dir, err := os.MkdirTemp(base, "meterm-agents-")
		if err != nil {
			continue
		}
		remove := func() { _ = os.RemoveAll(dir) }
		p := filepath.Join(dir, "ssh_config")
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			remove()
			continue
		}
		return p, remove, nil
	}
	return "", nil, errors.New("no temp directory with a plain path to work in")
}

var agentPlainPathRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// agentSafeOptions are the -o options a host's extra_args may carry into a look: where
// and as whom to connect, and connection tuning. Nothing that runs a program, forwards
// anything, changes the destination, shares a connection or decides which host keys
// count. HostName is out for that reason, while Port and User aren't: the host it goes
// to is still the one whose production label and route were looked at.
var agentSafeOptions = map[string]bool{
	"port": true, "user": true, "identityfile": true, "identitiesonly": true,
	"connecttimeout": true, "connectionattempts": true,
	"serveraliveinterval": true, "serveralivecountmax": true, "tcpkeepalive": true,
	"compression": true, "addressfamily": true, "ipqos": true, "rekeylimit": true,
	"kexalgorithms": true, "ciphers": true, "macs": true, "hostkeyalgorithms": true,
	"pubkeyacceptedalgorithms": true, "pubkeyacceptedkeytypes": true,
	"preferredauthentications": true, "numberofpasswordprompts": true,
	"loglevel": true, "checkhostip": true, "visualhostkey": true, "batchmode": true,
	"requesttty": true, "escapechar": true,
}

// agentSafeFlags stay harmless with what agentSSHArgs does. -t is here because -T
// comes after it.
var agentSafeFlags = map[string]bool{"-4": true, "-6": true, "-C": true, "-q": true, "-T": true, "-t": true, "-tt": true, "-v": true, "-vv": true, "-vvv": true}

// checkAgentExtraArgs lets through what can't change what a look is. It's an allowlist
// and has to stay one: a bare word would become ssh's destination, with the real target
// and the fixed command as a remote command on it; -o ProxyCommand runs a program here;
// and -A -X -M -S override the options set above. The same args stay fine for commands
// that make their own record.
func checkAgentExtraArgs(args []string) error {
	refuse := func(a string) error {
		return fmt.Errorf("its extra_args have %q, and `agents` runs only with -p, -i, -l, connection flags and connection -o options", a)
	}
	value := func(i *int, a, flag string) (string, bool) {
		if a != flag {
			return a[len(flag):], true
		}
		*i++
		if *i >= len(args) {
			return "", false
		}
		return args[*i], true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case agentSafeFlags[a]:
		case strings.HasPrefix(a, "-p"):
			v, ok := value(&i, a, "-p")
			if n, err := strconv.Atoi(v); !ok || err != nil || n < 1 || n > 65535 {
				return refuse(a)
			}
		case strings.HasPrefix(a, "-l"):
			v, ok := value(&i, a, "-l")
			if !ok || v == "" || config.CheckUser(v) != nil {
				return refuse(a)
			}
		case strings.HasPrefix(a, "-i"):
			v, ok := value(&i, a, "-i")
			if !ok || !agentSafeValue(v) {
				return refuse(a)
			}
		case strings.HasPrefix(a, "-o"):
			v, ok := value(&i, a, "-o")
			if !ok {
				return refuse(a)
			}
			key, val, found := strings.Cut(v, "=")
			if !found {
				key, val, found = strings.Cut(v, " ")
			}
			if !found || !agentSafeOptions[strings.ToLower(strings.TrimSpace(key))] || !agentSafeValue(strings.TrimSpace(val)) {
				return refuse(v)
			}
		default:
			return refuse(a)
		}
	}
	return nil
}

// agentSafeValue is one plain word: no tokens for ssh to expand, nothing for its config
// parser to split, and nothing to pass for a flag.
func agentSafeValue(v string) bool {
	return v != "" && !strings.HasPrefix(v, "-") && !termsafe.Unsafe(v) && !strings.ContainsAny(v, " \t%$`\"'\\;&|<>()")
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
