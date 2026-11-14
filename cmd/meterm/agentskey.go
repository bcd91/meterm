package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshkey"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

func newAgentsKeyCmd() *cobra.Command {
	var identity string
	var check bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "key <host> [--identity file] [--check]",
		Short: "Print the authorized_keys line that limits a host's watcher key to `claude agents --json`",
		Long: "Prints one line for the host's authorized_keys: the public half of its\n" +
			"agents_identity (or --identity), with a forced command that runs only\n" +
			"`claude agents --json` and `restrict`, so the key can't open a shell, a pty or a\n" +
			"forward. It doesn't install anything or contact the host: put the line there\n" +
			"yourself, ideally in an AuthorizedKeysFile the account can't write.\n\n" +
			"It refuses a key that an ordinary login to the host could use too: the host's\n" +
			"identity_file, any IdentityFile ssh would try for it (ssh's own defaults\n" +
			"included), or a key ssh-agent holds. sshd uses the first line a key matches, so\n" +
			"such a key would either stay unlimited, or limit your own logins.\n\n" +
			"--check, after the line is in, logs in with the watcher key alone and asks for\n" +
			"something else (echo of a random word, as the command and on stdin), then for a\n" +
			"port forward both ways (-W to the host's own sshd, -R on its loopback). Then it\n" +
			"logs in a second way, with your ordinary login to the host (your own key, as\n" +
			"`meterm agents` would without a watcher key), and reads ~/.ssh/authorized_keys and\n" +
			"authorized_keys2 there for the watcher key's line. A probe can't prove a key is\n" +
			"limited; only the line can. Exit 0: the installed line for this key is exactly\n" +
			"meterm's line, and the probes found no way through. Exit 2: a probe got through,\n" +
			"or the line has other options (it says which). Exit 1: anything that can't tell,\n" +
			"such as the key not being in those files (sshd may read keys from elsewhere), a\n" +
			"file over 256 KiB, a line meterm can't read, or the host not answering.\n" +
			"Exit 0 covers those two files and no more: sshd's own settings and other key\n" +
			"files (AuthorizedKeysFile set elsewhere, AuthorizedKeysCommand, Match,\n" +
			"ForceCommand) aren't read, nor the account's shell startup files, which run\n" +
			"before the forced command, nor which claude its PATH finds. The read is in the\n" +
			"activity log, as exec is.\n\n" +
			"The limit is on the host's side because nothing on this machine can be: anything\n" +
			"running as you here, an agent included, can read the key and use it as it likes.\n\n" +
			"To set one up:\n" +
			"  meterm keygen --file ~/.ssh/meterm_agents\n" +
			"  # in hosts.yaml, on the host, its group or defaults: agents_identity: ~/.ssh/meterm_agents\n" +
			"  meterm agents key devbox            # then add the line on devbox\n" +
			"  meterm agents key devbox --check    # then see that it holds\n\n" +
			"A host named \"key\" is listed with `meterm agents -- key`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveCfgPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			if err := checkAgentsClient(agentsGOOS); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			if check {
				if identity != "" {
					return fmt.Errorf("--check uses the host's agents_identity, as a look does; --identity doesn't go with it")
				}
				jobs, err := selectAgentHosts(cfg, args, nil, nil)
				if err != nil {
					return err
				}
				// The ordinary read of authorized_keys is a login with your own key that
				// runs a command: recorded, as exec is, by program name.
				record := func() *activityRun {
					return beginActivity(cfg, jobs[0].host, "exec", commandDetail([]string{readAuthorizedKeys("")}, false))
				}
				msg, code := checkWatcherKey(ctx, jobs[0], timeout, record)
				fmt.Fprintln(os.Stderr, "meterm: "+msg)
				if code != 0 {
					finish(code)
				}
				return nil
			}
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s", exactName(args[0]))
			}
			r := cfg.Resolve(*h)
			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			line, account, err := watcherKeyLine(sctx, r, identity, timeout)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Add this line to the authorized_keys of %s on %s; meterm doesn't install it. Then run `meterm agents key %s --check`.\n", lbl(account), lbl(r.Name), exactName(args[0]))
			_, err = fmt.Fprintln(os.Stdout, line)
			return err
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "the watcher key (private or .pub), instead of the host's agents_identity")
	cmd.Flags().BoolVar(&check, "check", false, "probe the watcher key, and read its authorized_keys line with your ordinary login (exit 0 meterm's line and no way through, 2 not limited, 1 can't tell)")
	cmd.Flags().DurationVar(&timeout, "timeout", agentsDefaultTimeout, "give up on the host after this long")
	return cmd
}

// Exit codes of `agents key --check`.
const (
	keyCheckLimited   = 0
	keyCheckUnsure    = 1
	keyCheckUnlimited = 2
)

// checkWatcherKey probes the watcher key, logging in with it alone, as a look does:
// an echo of a random word sent as the command and on stdin, then a forward each way.
// Any of them getting through is "not limited". Nothing a probe sees can prove a key is
// limited, so "limited" rests on the line itself, read with the person's ordinary login
// (installedLine). The probes aren't in the activity log, as looks aren't; the ordinary
// read is, through record, as a command run with the person's key. record may be nil.
func checkWatcherKey(ctx context.Context, job agentJob, timeout time.Duration, record func() *activityRun) (string, int) {
	// Four logins (the echo, -W, -R, the ordinary read), each held to timeout, and the
	// ssh -G before the watcher look.
	ctx, cancel := context.WithTimeout(ctx, 4*timeout+2*agentsLookSlack)
	defer cancel()
	r, refused := prepareLook(ctx, job, timeout)
	if refused == "" {
		refused = checkAgentsIdentity(r)
	}
	if refused != "" {
		return refused, keyCheckUnsure
	}
	if r.AgentsIdentity == "" {
		return "it has no agents_identity, so there's no watcher key to check", keyCheckUnsure
	}
	wr, sshConfig, remove, err := watcherLook(ctx, r, timeout)
	if err != nil {
		return "not checked: " + watcherSettingsText(err), keyCheckUnsure
	}
	defer remove()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "not checked: no random word", keyCheckUnsure
	}
	word := "meterm-watcher-check-" + hex.EncodeToString(nonce[:])
	// The word goes in the command and on stdin: a forced command that's a shell
	// (command="/bin/sh") ignores the one and runs the other.
	stdout, stderr, overflow, err := lookWithStdin(ctx, sshConfig, timeout, "echo "+word, "echo "+word+"\nexit\n", 64<<10, wr.Resolved)
	var exit *exec.ExitError
	exited := errors.As(err, &exit)
	switch {
	case bytes.Contains(stdout, []byte(word)):
		return "NOT LIMITED: the watcher key ran a command of meterm's choosing on " + lbl(r.Name) +
			". Anything that can read the key can run anything there. Put the line from `meterm agents key` " +
			"before any other line with this key, or take the other line out, and check again.", keyCheckUnlimited
	case wr.refused(stderr):
		return "the host refused the watcher key: the line isn't there, or isn't for this key", keyCheckUnsure
	case ctx.Err() != nil:
		return "no answer within " + timeout.String(), keyCheckUnsure
	case errors.As(err, &exit) && exit.ExitCode() == sshConnectionError:
		return "couldn't log in: " + sshFailureText(stderr), keyCheckUnsure
	case overflow:
		// The word could have been in what wasn't kept.
		return "more output than the check reads, so it can't tell", keyCheckUnsure
	case exited && exit.ExitCode() < 0:
		return "ssh was stopped before it finished, so it can't tell", keyCheckUnsure
	case err != nil && !exited:
		return "couldn't run ssh: " + termsafe.Text(err.Error()), keyCheckUnsure
	}
	// ssh ran to its end, logged in (not 255), and the word didn't come back. That
	// alone says nothing about which command the host ran, or what else the key may do:
	// no probe can prove a key is limited. So the probes can only find a way through,
	// and what decides "limited" is the line on the host, read below.
	local := forwarding(ctx, wr, sshConfig, timeout)
	remote := remoteForwarding(ctx, wr, sshConfig, timeout)
	if local == forwardAllowed || remote == forwardAllowed {
		way := "local (-L, -W)"
		if remote == forwardAllowed {
			way = "remote (-R)"
		}
		return "NOT LIMITED: " + way + " port forwarding is allowed for the watcher key on " + lbl(r.Name) +
			", so anything that can read the key can reach whatever that host can, or be reached from it. " +
			"Use the line from `meterm agents key` as it is, and check again.", keyCheckUnlimited
	}
	msg, code := installedLine(ctx, r, timeout, record)
	if code != keyCheckLimited {
		return msg, code
	}
	if local == forwardUnknown || remote == forwardUnknown {
		return "the line for the watcher key is meterm's, but whether the host refuses port forwarding couldn't be told", keyCheckUnsure
	}
	return "limited, as far as these files go: the installed authorized_keys line for this key is exactly " +
		"meterm's, and the probes (a command, stdin, -W, -R) found no way through. sshd's own settings and any " +
		"other key file aren't read (an AuthorizedKeysFile set elsewhere, AuthorizedKeysCommand, Match, " +
		"ForceCommand), nor the account's shell startup files (~/.bashrc and the like run before the forced " +
		"command) or which claude its PATH finds. The line's restrict is what turns off forwarding, a pty, agent " +
		"and X11 forwarding and ~/.ssh/rc for this key", keyCheckLimited
}

// authorizedKeysCap is how much of each authorized_keys file --check reads. More than
// that is "too big to read all of it": a line past it could be the one sshd uses.
const authorizedKeysCap = 256 << 10

// readAuthorizedKeys is the one command `--check` runs with the person's ordinary
// login: the account's own authorized_keys files, named in the command itself, each
// behind a header with its size and after it an end mark, all with a random word of
// meterm's, so nothing in a file can pass for a header. Up to one byte more than the
// cap is read, so a file over it shows. Nothing from outside goes into it; nonce is
// hex.
func readAuthorizedKeys(nonce string) string {
	m := "--meterm-" + nonce
	return `sh -c 'for f in "$HOME/.ssh/authorized_keys" "$HOME/.ssh/authorized_keys2"; do ` +
		`if [ ! -e "$f" ]; then echo "` + m + `-missing-- $f"; ` +
		`elif [ ! -r "$f" ]; then echo "` + m + `-unreadable-- $f"; ` +
		`else echo "` + m + `-file-- $(wc -c <"$f") $f"; head -c ` + strconv.Itoa(authorizedKeysCap+1) + ` "$f"; echo; echo "` + m + `-end--"; fi; done'`
}

// installedLine reads the account's authorized_keys over an ordinary login (the
// person's own key, as any meterm command would log in, not the watcher key) and
// finds the lines for the watcher key. keyCheckLimited only for exactly one line,
// with exactly meterm's options, and every other line read.
func installedLine(ctx context.Context, r config.Resolved, timeout time.Duration, record func() *activityRun) (string, int) {
	key, err := readPublicKey(r.AgentsIdentity)
	if err != nil {
		return "not checked: " + err.Error(), keyCheckUnsure
	}
	want, err := agents.KeyOptions()
	if err != nil {
		return "not checked: " + err.Error(), keyCheckUnsure
	}
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "not checked: no random word", keyCheckUnsure
	}
	nonce := hex.EncodeToString(nb[:])
	sshConfig, remove, err := writeAgentSSHConfig()
	if err != nil {
		return "not checked: " + err.Error(), keyCheckUnsure
	}
	defer remove()
	// r as resolved, not the watcher look's: its identity_file, the person's config,
	// no IdentitiesOnly.
	ordinary := r
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var run *activityRun
	if record != nil {
		run = record()
	}
	stdout, stderr, overflow, err := sshLookQuery(sshConfig, timeout, readAuthorizedKeys(nonce), 1<<20)(ctx, ordinary)
	if run != nil {
		run.end(err)
	}
	switch {
	case overflow:
		return "couldn't read the host's authorized_keys: more than meterm reads", keyCheckUnsure
	case err != nil:
		why := "it failed"
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == sshConnectionError {
			why = sshFailureText(stderr)
		}
		return "couldn't read the host's authorized_keys with your ordinary login (" + termsafe.Text(why) + "), so it can't tell", keyCheckUnsure
	}
	entries, problems := watcherEntries(string(stdout), key, nonce)
	for _, e := range entries {
		if e != want {
			return "NOT LIMITED: the authorized_keys line for the watcher key on " + lbl(r.Name) +
				" isn't meterm's: " + optionsDiff(e, want) + ". Replace it with the line from `meterm agents key`, and check again.", keyCheckUnlimited
		}
	}
	switch {
	case len(problems) > 0:
		return "couldn't read all of the host's authorized_keys (" + strings.Join(problems, "; ") + "), so it can't tell", keyCheckUnsure
	case len(entries) == 0:
		return "the watcher key isn't in ~/.ssh/authorized_keys or authorized_keys2 on " + lbl(r.Name) +
			"; sshd may take keys from an AuthorizedKeysFile elsewhere or an AuthorizedKeysCommand, which meterm can't see, so it can't tell", keyCheckUnsure
	case len(entries) > 1:
		return "the watcher key is in authorized_keys more than once on " + lbl(r.Name) + "; keep one line, and check again", keyCheckUnsure
	}
	return "", keyCheckLimited
}

// rsaSHA2RE is an RSA key written under a signature algorithm's name, which sshd takes
// as ssh-rsa and Go's parser doesn't.
var rsaSHA2RE = regexp.MustCompile(`(^|[ \t])rsa-sha2-(256|512)([ \t])`)

// watcherEntries is the options of every authorized_keys line for key, in the files
// readAuthorizedKeys(nonce) printed, and what couldn't be read. A line meterm can't
// make out is a problem, not something to pass over: sshd might read it, and it might
// be the watcher key's.
func watcherEntries(out string, key ssh.PublicKey, nonce string) (entries, problems []string) {
	m := "--meterm-" + nonce
	files := 0
	file, n, got := "", 0, 0
	inFile := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, m+"-file-- "):
			files++
			size, name, _ := strings.Cut(strings.TrimPrefix(line, m+"-file-- "), " ")
			file, n, got, inFile = termsafe.Text(name), 0, 0, true
			if sz, err := strconv.Atoi(strings.TrimSpace(size)); err != nil || sz > authorizedKeysCap {
				problems = append(problems, file+" is too big to read all of it")
			}
			continue
		case line == m+"-end--":
			// The size said in the header is wc's; what came may be more if the file
			// grew in between. The extra newline is the command's own.
			if got-1 > authorizedKeysCap && !slices.Contains(problems, file+" is too big to read all of it") {
				problems = append(problems, file+" is too big to read all of it")
			}
			inFile = false
			continue
		case strings.HasPrefix(line, m+"-missing-- "):
			files++
			continue
		case strings.HasPrefix(line, m+"-unreadable-- "):
			files++
			problems = append(problems, termsafe.Text(strings.TrimPrefix(line, m+"-unreadable-- "))+" can't be read")
			continue
		}
		if !inFile {
			if strings.TrimSpace(line) != "" {
				problems = append(problems, "the answer wasn't what was asked for")
			}
			continue
		}
		n++
		got += len(line) + 1
		line = strings.TrimSuffix(line, "\r")
		t := strings.TrimLeft(line, " \t")
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		where := fmt.Sprintf("line %d of %s", n, file)
		if strings.IndexFunc(t, func(r rune) bool { return r != '\t' && (r < 0x20 || r == 0x7f) }) >= 0 {
			problems = append(problems, where+" has a control character: a line meterm can't read, and sshd might")
			continue
		}
		pk, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(t))
		if err != nil {
			pk, _, opts, _, err = ssh.ParseAuthorizedKey([]byte(rsaSHA2RE.ReplaceAllString(t, "${1}ssh-rsa${3}")))
		}
		if err != nil {
			problems = append(problems, where+" is a line meterm can't read, and sshd might")
			continue
		}
		if sameKey(pk, key) {
			entries = append(entries, strings.Join(opts, ","))
		}
	}
	if files != 2 || inFile {
		problems = append(problems, "the answer wasn't what was asked for")
	}
	return entries, problems
}

// optionsDiff says how an installed line's options differ from meterm's, by option.
func optionsDiff(got, want string) string {
	split := func(s string) map[string]string {
		m := map[string]string{}
		for _, o := range splitOptions(s) {
			k, _, _ := strings.Cut(o, "=")
			m[strings.ToLower(k)] = o
		}
		return m
	}
	g, w := split(got), split(want)
	var has, lacks, differ []string
	for k, v := range g {
		if wv, ok := w[k]; !ok {
			has = append(has, termsafe.Text(v))
		} else if wv != v {
			differ = append(differ, k)
		}
	}
	for k := range w {
		if _, ok := g[k]; !ok {
			lacks = append(lacks, k)
		}
	}
	slices.Sort(has)
	slices.Sort(lacks)
	slices.Sort(differ)
	var parts []string
	if len(has) > 0 {
		parts = append(parts, "has "+strings.Join(has, ", "))
	}
	if len(lacks) > 0 {
		parts = append(parts, "lacks "+strings.Join(lacks, ", "))
	}
	if len(differ) > 0 {
		parts = append(parts, "a different "+strings.Join(differ, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "the same options, written differently")
	}
	return "options differ from meterm's line: " + strings.Join(parts, "; ")
}

// splitOptions splits an authorized_keys options string at commas outside quotes.
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quoted && i+1 < len(s):
			cur.WriteByte(c)
			i++
			cur.WriteByte(s[i])
			continue
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// lookWithStdin is sshLookQuery with stdin given rather than /dev/null.
func lookWithStdin(ctx context.Context, sshConfig string, timeout time.Duration, command, stdin string, maxOut int, r config.Resolved) ([]byte, string, bool, error) {
	r.Forwards = nil
	var args []string
	for i, a := range agentSSHArgs(r, sshConfig, timeout) {
		if a == "-n" && i == 0 {
			continue
		}
		args = append(args, a)
	}
	r.ExtraArgs = args
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := sshrun.SSHContext(ctx, r, command)
	noPrompts(c)
	c.Stdin = strings.NewReader(stdin)
	stdout := &cappedBuffer{max: maxOut, over: cancel}
	stderr := &sshrun.Tail{Max: 16 << 10}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = time.Second
	err := c.Run()
	return stdout.b, stderr.String(), stdout.overflow, err
}

// remoteForwarding asks the host, with the watcher key alone, to listen on a port of
// its own for this side (ssh -R, port 0: any free one, on its loopback). "Allocated
// port" means it did. "remote port forwarding failed" is a refusal.
func remoteForwarding(ctx context.Context, wr watcherResolved, sshConfig string, timeout time.Duration) forwardResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := wr.Resolved
	r.Forwards = nil
	var args []string
	for _, a := range agentSSHArgs(r, sshConfig, timeout) {
		switch a {
		case "LogLevel=ERROR":
			a = "LogLevel=INFO" // where ssh says it allocated a port
		case "ClearAllForwardings=yes":
			a = "ClearAllForwardings=no" // or -R is dropped before it's asked
		}
		args = append(args, a)
	}
	r.ExtraArgs = append(args, "-o", "ExitOnForwardFailure=yes", "-N", "-R", "127.0.0.1:0:127.0.0.1:1")
	c := sshrun.SSHContext(ctx, r)
	noPrompts(c)
	errs := &watchFor{word: "Allocated port", found: cancel}
	c.Stdout, c.Stderr = io.Discard, errs
	c.WaitDelay = time.Second
	_ = c.Run()
	switch {
	case errs.seen():
		return forwardAllowed
	case strings.Contains(errs.String(), "remote port forwarding failed"):
		return forwardRefused
	}
	return forwardUnknown
}

// watchFor keeps what's written, and calls found once word is in it.
type watchFor struct {
	mu    sync.Mutex
	b     []byte
	word  string
	hit   bool
	found func()
}

func (w *watchFor) Write(p []byte) (int, error) {
	w.mu.Lock()
	if len(w.b) < 16<<10 {
		w.b = append(w.b, p...)
	}
	hit := !w.hit && bytes.Contains(w.b, []byte(w.word))
	if hit {
		w.hit = true
	}
	w.mu.Unlock()
	if hit {
		w.found()
	}
	return len(p), nil
}

func (w *watchFor) seen() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.hit }

func (w *watchFor) String() string { w.mu.Lock(); defer w.mu.Unlock(); return string(w.b) }

type forwardResult int

const (
	forwardUnknown forwardResult = iota
	forwardRefused
	forwardAllowed
)

// forwarding asks the host, with the watcher key alone, for a stdio forward to its own
// sshd (ssh -W). "administratively prohibited" is the key's options, or the server's,
// saying no. Any answer through the forward, or a failure of the connection the host
// tried to make for it, means forwarding is allowed.
func forwarding(ctx context.Context, wr watcherResolved, sshConfig string, timeout time.Duration) forwardResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	port := wr.port
	if port == "" {
		port = "22"
	}
	r := wr.Resolved
	r.Forwards = nil
	var args []string
	for i, a := range agentSSHArgs(r, sshConfig, timeout) {
		// Not -n: stdin is the forward's. ClearAllForwardings doesn't touch -W.
		if a == "-n" && i == 0 {
			continue
		}
		// ssh tells why a channel failed to open at INFO; the first LogLevel wins.
		if a == "LogLevel=ERROR" {
			a = "LogLevel=INFO"
		}
		args = append(args, a)
	}
	r.ExtraArgs = append(args, "-W", "127.0.0.1:"+port)
	c := sshrun.SSHContext(ctx, r)
	noPrompts(c)
	// stdin held open, so ssh waits for the other side. An *os.File, so that no
	// goroutine copies it, which Wait would wait for.
	pr, pw, err := os.Pipe()
	if err != nil {
		return forwardUnknown
	}
	defer pw.Close()
	defer pr.Close()
	c.Stdin = pr
	got := &firstBytes{seen: cancel}
	stderr := &sshrun.Tail{Max: 16 << 10}
	c.Stdout, c.Stderr = got, stderr
	c.WaitDelay = time.Second
	_ = c.Run()
	switch {
	case got.any():
		return forwardAllowed
	case strings.Contains(stderr.String(), "administratively prohibited"):
		return forwardRefused
	case strings.Contains(stderr.String(), "open failed"):
		return forwardAllowed // permitted; only the connection the host made failed
	}
	return forwardUnknown
}

// firstBytes notes that something came, and calls seen.
type firstBytes struct {
	mu   sync.Mutex
	n    int
	seen func()
}

func (f *firstBytes) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.n += len(p)
	f.mu.Unlock()
	if len(p) > 0 {
		f.seen()
	}
	return len(p), nil
}

func (f *firstBytes) any() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n > 0
}

// watcherKeyLine is the authorized_keys line for r's watcher key, and the account a
// look logs in to, as ssh -G works it out.
func watcherKeyLine(ctx context.Context, r config.Resolved, identity string, timeout time.Duration) (line, account string, err error) {
	if identity == "" {
		identity = r.AgentsIdentity
	}
	if identity == "" {
		return "", "", fmt.Errorf("%s has no agents_identity: make a key (`meterm keygen --file ~/.ssh/meterm_agents`), set agents_identity to it in hosts.yaml, or pass --identity", exactName(r.Name))
	}
	if config.CheckText("agents_identity", identity) != nil {
		return "", "", fmt.Errorf("the watcher key's path has characters that don't show")
	}
	key, err := readPublicKey(identity)
	if err != nil {
		return "", "", err
	}
	// What a look would refuse locally, said now rather than after the line is in.
	private := strings.TrimSuffix(identity, ".pub")
	if why := watcherKeyUnusable(private); why != "" {
		return "", "", errors.New(why)
	}
	if why := watcherKeyHasCert(private); why != "" {
		return "", "", errors.New(why)
	}
	// A key an ordinary login could use too would put those logins under the forced
	// command, or leave this line unused behind another: sshd takes the first line a
	// key matches. Either way the host wouldn't say, as the forced command is what a
	// look sends anyway.
	set, err := lookSettings(ctx, r, timeout)
	if err != nil {
		return "", "", fmt.Errorf("can't tell which keys an ordinary login to %s uses: %s", exactName(r.Name), watcherSettingsText(err))
	}
	// A host a watcher look can't reach (a ProxyCommand) gets no line: it would never be used.
	if _, err := carry(set); err != nil {
		return "", "", fmt.Errorf("%s: %s", exactName(r.Name), watcherSettingsText(err))
	}
	others, unchecked := otherLoginKeys(ctx, set)
	// ssh -G leaves out an -i whose file isn't there, so the host's own is read here too.
	if r.IdentityFile != "" {
		if k, err := publicKeyOf(r.IdentityFile); err == nil {
			others = append(others, k)
		}
	}
	for _, o := range others {
		if sameKey(o, key) {
			return "", "", fmt.Errorf("that key can log in to %s already: it's the host's identity_file, an IdentityFile ssh tries for it, or in ssh-agent. "+
				"A watcher key has to be one of its own, kept out of ssh-agent (ssh-add -d), or the line would either stay unlimited or limit your own logins", exactName(r.Name))
		}
	}
	for _, u := range unchecked {
		what := u
		if !strings.HasPrefix(u, "ssh-agent") && !strings.HasPrefix(u, "the agent at ") {
			what = termsafe.Quote(u)
		}
		fmt.Fprintf(os.Stderr, "meterm: unchecked: %s, so it couldn't be compared with the watcher key; run --check once the line is in\n", what)
	}
	opts, err := agents.KeyOptions()
	if err != nil {
		return "", "", err
	}
	account = set.one("user")
	if account == "" {
		account = "the account a look logs in as"
	}
	return opts + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + agents.KeyComment, account, nil
}

// readPublicKey reads path.pub, or path if it ends in .pub: one key, with no options.
func readPublicKey(path string) (ssh.PublicKey, error) {
	if !strings.HasSuffix(path, ".pub") {
		path += ".pub"
	}
	f, err := os.Open(sshkey.LocalPath(path))
	if err != nil {
		// Not err itself: it repeats the path as it is, and this one may come from a
		// file someone else wrote.
		why := "can't be opened"
		if os.IsNotExist(err) {
			why = "isn't there"
		}
		return nil, fmt.Errorf("can't read the public key %s: it %s", termsafe.Quote(path), why)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	key, _, opts, rest, err := ssh.ParseAuthorizedKey(b)
	if err != nil || len(opts) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("%s isn't one public key", termsafe.Quote(path))
	}
	if _, isCert := key.(*ssh.Certificate); isCert {
		return nil, fmt.Errorf("%s is a certificate; give the plain key", termsafe.Quote(path))
	}
	return key, nil
}
