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
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

// `meterm auth-methods`: which ways of logging in a host offers, without logging in. It asks
// for no authentication at all, and ssh prints what the server says could continue. `audit`
// learns about passwords from sshd_config, which takes a login, and its --probe only opens
// a TCP connection, so neither can answer this from the outside.
//
// Like `agents` it writes no activity entry: it offers no key or password, and changes
// nothing. A jump host on the way is logged in to as usual, and a server that lets anyone in
// runs authProbeCommand. Each check shows in the host's sshd log.

const authMethodsSchema = 1

// authProbeCommand can only run on a server that lets anyone in with no authentication at
// all, which is the thing to know. It's a constant, and does nothing.
const authProbeCommand = "true"

// authProbeMaxStderr bounds what's kept of ssh's debug output.
const authProbeMaxStderr = 64 << 10

const authProbeMaxMethods = 16

type authMethodsHost struct {
	Host      string `json:"host"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error"`
	// Methods is what the server says, in its order. It's null when Error is set: not
	// knowing isn't offering nothing.
	Methods []string `json:"methods"`
	// These are null when it can't be said. A false "no password" is the worst thing this
	// could report, so false only ever comes from a list that came from this host and could
	// be read in full.
	PasswordLogin       *bool `json:"password_login"`
	KeyboardInteractive *bool `json:"keyboard_interactive"`
	// NoneAccepted says it let this connection in with no authentication.
	NoneAccepted *bool `json:"none_accepted"`
}

type authMethodsReport struct {
	Schema int               `json:"schema"`
	Hosts  []authMethodsHost `json:"hosts"`
}

func newAuthMethodsCmd() *cobra.Command {
	var flags lookFlags
	cmd := &cobra.Command{
		Use:   "auth-methods [host...] [--tag t] [--group g]",
		Short: "Ask hosts which login methods they accept, without logging in (read-only, not logged)",
		Long: "Connects with PreferredAuthentications=none, so no key or password is offered, and\n" +
			"reports what the server says can continue: password_login says it takes passwords.\n" +
			"keyboard_interactive usually means passwords too, through PAM. A jump host on the way\n" +
			"is logged in to as usual, and a server that lets anyone in with no authentication runs\n" +
			"`true`. Hosts are chosen, and production refused unless named, as with `agents`.\n\n" +
			"If it can't tell what this host itself answered, as when a jump host refuses, that host\n" +
			"has an error, and password_login is null, not false.\n\n" +
			"It's the server's own statement, to anyone who asks, and it says what's on offer for\n" +
			"this account from this address. A Match block can say otherwise for others. It doesn't\n" +
			"say whether any password would work.\n\n" +
			"--format json prints {\"schema\":1,\"hosts\":[{\"host\",\"reachable\",\"error\",\"methods\",\n" +
			"\"password_login\",\"keyboard_interactive\",\"none_accepted\"}]}. methods is null when\n" +
			"error is set. Exit codes are those of `agents`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			jobs, _, err := flags.jobs("auth-methods", args)
			if err != nil {
				return err
			}
			sshConfig, removeSSHConfig, err := writeAgentSSHConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			rep := authMethodsReport{Schema: authMethodsSchema, Hosts: make([]authMethodsHost, len(jobs))}
			sem := make(chan struct{}, flags.parallel)
			var wg sync.WaitGroup
			for i, job := range jobs {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					rep.Hosts[i] = askAuthMethods(ctx, job, sshAuthProbe(sshConfig, flags.timeout), flags.timeout)
				}()
			}
			wg.Wait()
			removeSSHConfig()
			if err := writeAuthMethods(os.Stdout, rep, flags.format); err != nil {
				return err
			}
			failed := 0
			for _, h := range rep.Hosts {
				if h.Error != "" {
					failed++
				}
			}
			if failed > 0 {
				fmt.Fprintf(os.Stderr, "meterm: %d of %d host(s) didn't answer\n", failed, len(rep.Hosts))
				finish(agentsIncomplete)
			}
			return nil
		},
	}
	flags.add(cmd)
	return cmd
}

// authProbeArgs are the look's options but for its log level, which has to let the line
// through, and with no authentication asked for. -v, -q and the like go: they'd change the
// level, and -v would reach a jump hop and mix its answer in with this host's. These -o
// reach no hop, which keeps its own way in, and its LogLevel=ERROR from the -F file.
func authProbeArgs(r config.Resolved, sshConfig string, timeout time.Duration) []string {
	args := []string{"-n"}
	if sshConfig != "" {
		args = append(args, "-F", sshConfig)
	}
	args = append(args, "-o", "PreferredAuthentications=none", "-o", "LogLevel=DEBUG1")
	for _, o := range agentFixedOptions {
		if !strings.HasPrefix(o, "LogLevel=") {
			args = append(args, "-o", o)
		}
	}
	args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(int(timeout.Seconds()+0.999)), "-o", "ConnectionAttempts=1")
	for _, a := range r.ExtraArgs {
		if a == "-q" || a == "-v" || a == "-vv" || a == "-vvv" {
			continue
		}
		args = append(args, a)
	}
	return append(args, "-T")
}

func sshAuthProbe(sshConfig string, timeout time.Duration) agentQuery {
	return func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		r.Forwards = nil
		r.ExtraArgs = authProbeArgs(r, sshConfig, timeout)
		c := sshrun.SSHContext(ctx, r, authProbeCommand)
		noPrompts(c)
		stderr := &sshrun.Tail{Max: authProbeMaxStderr}
		c.Stdout, c.Stderr = io.Discard, stderr
		c.WaitDelay = time.Second
		err := c.Run()
		return nil, stderr.String(), false, err
	}
}

var (
	authContinueRE = regexp.MustCompile(`^debug1: Authentications that can continue: ([^\r\n]*)\r?$`)
	authNoneRE     = regexp.MustCompile(`^(debug1: )?(Authentication succeeded \(none\)\.|Authenticated to .* using "none"\.)\r?$`)
	authMethodRE   = regexp.MustCompile(`^[a-z][a-z0-9@._+-]{0,63}$`)
	authKnownRE    = regexp.MustCompile(`^debug1: Host '.*' is known and matches the \S+ host key\.\r?$`)
	authNewKeysRE  = regexp.MustCompile(`^debug1: SSH2_MSG_NEWKEYS received\r?$`)
)

// What ssh prints with text from the other end in it, newlines kept, before any key is
// checked: anyone answering on the path can write these. The text can look like any line of
// ssh's own, so it's what comes first that decides: these starts of a line are ssh's own, and
// the other end can't leave them off. A disconnect is no answer at any point.
const authRemoteCarrier = "debug1: Remote: "

var authEndCarriers = []string{"Received disconnect from ", "Unable to negotiate with "}

// authAnswer is what ssh's debug output says the target answered.
type authAnswer struct {
	found, noneAccepted bool
	methods             []string
	dropped             int  // names in the list that couldn't be believed
	carrier             bool // text from the other end came through before it could be trusted
}

// authOwnLine is true of the only lines ssh prints before the host key is checked in a run
// that's going well. Until that check, anything else ends the reading for good, and so a line
// forged by text from the other end is never reached: its carrier comes first.
func authOwnLine(line string) bool {
	if strings.HasPrefix(line, authRemoteCarrier) {
		return false
	}
	return strings.HasPrefix(line, "debug1: ") || strings.HasPrefix(line, "** ") || strings.HasPrefix(line, "Warning: ") || strings.TrimSpace(line) == ""
}

// parseAuthMethods finds the target's own answer, trusting it only from a point nothing unproven
// can reach. That takes, in order and each from ssh itself: this host's start of authentication,
// its host key matching the one known, and the new keys in use. From then on all that comes is
// encrypted and from the host whose key was checked, so a banner there saying otherwise is only
// its own claim about itself, and the real answer follows it, so the last one counts. Any
// disconnect means no answer. A jump host's lines aren't in this: it runs at LogLevel=ERROR, and
// any that it prints before those three ends the reading too.
func parseAuthMethods(stderr, address string, port int) authAnswer {
	if port == 0 {
		port = 22
	}
	lines := strings.Split(stderr, "\n")
	for _, line := range lines {
		for _, c := range authEndCarriers {
			if strings.HasPrefix(line, c) {
				return authAnswer{carrier: true}
			}
		}
	}
	// A host's own name can differ from what ssh prints: another case, or an alias in the ssh
	// config. With just the one start of authentication, that's this host, as a jump host
	// prints none of them. With more, the name has to say so.
	exact := strings.ToLower("debug1: Authenticating to " + address + ":" + strconv.Itoa(port) + " as ")
	markers := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "debug1: Authenticating to ") {
			markers++
		}
	}
	var ans authAnswer
	armed, known, verified := false, false, false
	for _, line := range lines {
		// Another start begins it all again: what's after the last one is what counts.
		if strings.HasPrefix(line, "debug1: Authenticating to ") {
			armed, known, verified = markers == 1 || strings.HasPrefix(strings.ToLower(line), exact), false, false
			ans = authAnswer{}
			continue
		}
		if !verified {
			if !authOwnLine(line) {
				return authAnswer{carrier: strings.HasPrefix(line, authRemoteCarrier)}
			}
			switch {
			case armed && authKnownRE.MatchString(line):
				known = true
			case known && authNewKeysRE.MatchString(line):
				verified = true
			}
			continue
		}
		if authNoneRE.MatchString(line) {
			ans = authAnswer{found: true, noneAccepted: true, methods: []string{}}
			continue
		}
		m := authContinueRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ans = authAnswer{found: true, methods: []string{}}
		for _, name := range strings.Split(m[1], ",") {
			switch {
			case name == "":
			case authMethodRE.MatchString(name) && len(ans.methods) < authProbeMaxMethods:
				ans.methods = append(ans.methods, name)
			default:
				ans.dropped++
			}
		}
	}
	return ans
}

func askAuthMethods(ctx context.Context, job agentJob, query agentQuery, timeout time.Duration) authMethodsHost {
	out := authMethodsHost{Host: termsafe.Text(job.label)}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, refused := prepareLook(ctx, job, timeout)
	if refused != "" {
		out.Error = refused
		return out
	}
	_, stderr, _, err := query(ctx, r)
	if ctx.Err() == context.DeadlineExceeded {
		out.Error = "timed out after " + timeout.String()
		return out
	}
	if ctx.Err() != nil {
		out.Error = "stopped before it answered"
		return out
	}
	yes, no := true, false
	ans := parseAuthMethods(stderr, r.Address, r.Port)
	switch {
	case ans.noneAccepted, err == nil:
		// In with no authentication, whatever came of the command: a forced command can
		// fail. Exit 0 says the same, as the command can't have run any other way.
		out.Reachable, out.NoneAccepted, out.Methods = true, &yes, []string{}
		return out
	case ans.found:
		out.Reachable, out.Methods, out.NoneAccepted = true, ans.methods, &no
		for _, f := range []struct {
			name string
			to   **bool
		}{{"password", &out.PasswordLogin}, {"keyboard-interactive", &out.KeyboardInteractive}} {
			switch {
			case containsStr(ans.methods, f.name):
				*f.to = &yes
			case ans.dropped == 0: // a name that couldn't be read could have been this one
				*f.to = &no
			}
		}
		return out
	}
	var exit *exec.ExitError
	hop := len(r.ProxyJump) > 0 || r.ProxyJumpRaw != ""
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == -1:
		out.Error = "ssh was killed before it answered"
	case ans.carrier:
		out.Error = "unknown: the other end sent text before its host key was checked, or disconnected, so nothing it said counts"
	case hop:
		out.Error = "unknown: no answer from this host, and it's reached through a jump host, which may be what refused"
	case errors.As(err, &exit):
		// Asking for no authentication can't have a key refused, whatever that line says.
		if sshrun.ClassifyFailure(stderr) == sshrun.FailureAuth {
			out.Error = "unknown: ssh didn't get an answer it could tie to this host"
		} else {
			out.Error = sshFailureText(stderr)
		}
	default:
		out.Error = "couldn't run ssh: " + termsafe.Text(err.Error())
	}
	return out
}

func writeAuthMethods(w io.Writer, rep authMethodsReport, format string) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	var buf bytes.Buffer
	t := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(t, "HOST\tPASSWORD\tMETHODS")
	for _, h := range rep.Hosts {
		switch {
		case h.Error != "":
			fmt.Fprintf(t, "%s\t?\t%s\n", h.Host, h.Error)
		case h.NoneAccepted != nil && *h.NoneAccepted:
			fmt.Fprintf(t, "%s\tno login needed\tlet in with no authentication at all\n", h.Host)
		default:
			pw := "?"
			switch {
			case h.PasswordLogin != nil && *h.PasswordLogin:
				pw = "yes"
			case h.KeyboardInteractive != nil && *h.KeyboardInteractive:
				pw = "probably"
			case h.PasswordLogin != nil && h.KeyboardInteractive != nil:
				pw = "no"
			}
			fmt.Fprintf(t, "%s\t%s\t%s\n", h.Host, pw, strings.Join(h.Methods, ","))
		}
	}
	if err := t.Flush(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
