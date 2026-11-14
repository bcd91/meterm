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
			"or the line clearly grants more than meterm's (it says how). Exit 1: anything that\n" +
			"can't tell, such as the key not being in those files (sshd may read keys from\n" +
			"elsewhere), a file over 256 KiB, a line meterm can't read, or the host not\n" +
			"answering.\n" +
			"Exit 0 covers those two files and no more: sshd's own settings and other key\n" +
			"files (AuthorizedKeysFile set elsewhere, AuthorizedKeysCommand, Match,\n" +
			"ForceCommand) aren't read, nor the account's shell startup files, which run\n" +
			"before the forced command (the read runs through that shell too, which could\n" +
			"fake its answer), nor which claude its PATH finds. A key file kept outside the\n" +
			"account's home (/etc/ssh/authorized_keys/%u) can't be checked: exit 1. A line\n" +
			"that isn't meterm's but doesn't clearly grant more is exit 1, not 2. The read is\n" +
			"in the activity log, as exec is.\n\n" +
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
	echoCtx, echoCancel := context.WithTimeout(ctx, timeout) // its own share of the budget
	found, stderr, err := echoProbe(echoCtx, sshConfig, timeout, word, wr.Resolved)
	timedOut := echoCtx.Err() != nil
	echoCancel()
	var exit *exec.ExitError
	exited := errors.As(err, &exit)
	switch {
	case found:
		return "NOT LIMITED: the watcher key ran a command of meterm's choosing on " + lbl(r.Name) +
			". Anything that can read the key can run anything there. Put the line from `meterm agents key` " +
			"before any other line with this key, or take the other line out, and check again.", keyCheckUnlimited
	case wr.refused(stderr):
		return "the host refused the watcher key: the line isn't there, or isn't for this key", keyCheckUnsure
	case timedOut:
		return "no answer within " + timeout.String(), keyCheckUnsure
	case errors.As(err, &exit) && exit.ExitCode() == sshConnectionError:
		return "couldn't log in: " + sshFailureText(stderr), keyCheckUnsure
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
		"command, and the read itself runs through that shell, which could fake it) or which claude its PATH finds. " +
		"The line's restrict is what turns off forwarding, a pty, agent " +
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
		`elif [ ! -f "$f" ]; then echo "` + m + `-not-a-file-- $f"; ` +
		`elif [ ! -r "$f" ]; then echo "` + m + `-unreadable-- $f"; ` +
		`else echo "` + m + `-file-- $(wc -c <"$f" | tr -d " ") $f"; head -c ` + strconv.Itoa(authorizedKeysCap) + ` "$f"; echo; echo "` + m + `-end--"; fi; done'`
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
	// With one line for the key, it's the one sshd uses. With more, any could be,
	// depending on which comes first after the file next changes: say "could be".
	which := "the authorized_keys line for the watcher key on " + lbl(r.Name) + " isn't meterm's"
	if len(entries) > 1 {
		which = "there's more than one authorized_keys line for the watcher key on " + lbl(r.Name) +
			", and another line for this key could be used and isn't meterm's"
	}
	for _, e := range entries {
		if e != want && grantsMore(e, want) {
			return "NOT LIMITED: " + which + ", and it lets the key do more: " + optionsDiff(e, want) +
				". Replace it with the line from `meterm agents key`, and check again.", keyCheckUnlimited
		}
	}
	for _, e := range entries {
		if e != want {
			return which + ", so meterm can't vouch for it (" + optionsDiff(e, want) +
				"). It may be as limited or more; to have it checked, use the line from `meterm agents key` as it is", keyCheckUnsure
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
	file, n, got, size := "", 0, 0, 0
	inFile := false
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, m+"-file-- "):
			files++
			// "SIZE NAME". BSD's wc pads the count with spaces; the command takes them
			// out, and Fields doesn't mind them either.
			f := strings.Fields(strings.TrimPrefix(line, m+"-file-- "))
			name := ""
			if len(f) > 1 {
				name = strings.Join(f[1:], " ")
			}
			file, n, got, inFile = termsafe.Text(name), 0, 0, true
			sz, err := -1, errors.New("no size")
			if len(f) > 0 {
				sz, err = strconv.Atoi(f[0])
			}
			switch {
			case err != nil || sz < 0:
				problems = append(problems, file+": its size couldn't be read")
				size = -1
			case sz > authorizedKeysCap:
				problems = append(problems, file+" is too big to read all of it")
				size = -1
			default:
				size = sz
			}
			continue
		case line == m+"-end--":
			// What came has to be the whole file: the size wc gave, plus the newline
			// the command adds after it. Anything else, the file changed while it was
			// read, or the read stopped partway.
			if size >= 0 && got-1 != size {
				problems = append(problems, file+" changed while it was read, or wasn't read whole")
			}
			inFile = false
			continue
		case strings.HasPrefix(line, m+"-missing-- "):
			files++
			continue
		case strings.HasPrefix(line, m+"-not-a-file-- "):
			// sshd skips a key file that isn't a regular file (auth_openfile): so does
			// this. A FIFO would hang a read.
			files++
			continue
		case strings.HasPrefix(line, m+"-unreadable-- "):
			files++
			problems = append(problems, termsafe.Text(strings.TrimPrefix(line, m+"-unreadable-- "))+" can't be read")
			continue
		}
		if !inFile {
			// Anything outside the blocks is something else talking: a shell startup
			// file, a banner. It can't be told from the answer, so it spoils it.
			problems = append(problems, "the answer wasn't what was asked for")
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
		pk, opts, err := parseKeyLine(t)
		if err != nil {
			problems = append(problems, where+" is a line meterm can't read, and sshd might")
			continue
		}
		if sameKey(pk, key) {
			entries = append(entries, opts)
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

// grantsMore says whether an installed line's options clearly let the key do more than
// meterm's: no restrict, an option that lets something back in, or another command.
// Options that only narrow it (from=, expiry-time=, verify-required, no-*), or meterm's
// own in another case or order, don't; such a line is still not meterm's, and gets a
// "can't vouch" rather than a pass.
func grantsMore(got, want string) bool {
	opts := func(s string) map[string]string {
		m := map[string]string{}
		for _, o := range splitOptions(s) {
			k, v, _ := strings.Cut(o, "=")
			m[strings.ToLower(k)] = v
		}
		return m
	}
	g, w := opts(got), opts(want)
	if _, ok := g["restrict"]; !ok {
		return true
	}
	for _, k := range []string{"port-forwarding", "permitopen", "permitlisten", "pty", "agent-forwarding",
		"x11-forwarding", "user-rc", "cert-authority", "environment", "tunnel", "no-touch-required", "principals"} {
		if _, ok := g[k]; ok {
			return true
		}
	}
	return g["command"] != w["command"]
}

// parseKeyLine reads an authorized_keys line as sshd does (authfile.c): a key at the
// start, or else options and then a key. The options end at the first space or tab
// outside quotes, and inside quotes only \" escapes a quote; a backslash before
// anything else is just a backslash (sshkey_advance_past_options). Go's own parser
// takes a backslash as escaping whatever follows, and so can end the options in
// another place than sshd, and hide an option; it's only used here for the key.
func parseKeyLine(t string) (ssh.PublicKey, string, error) {
	if pk, err := parseBareKey(t); err == nil {
		return pk, "", nil
	}
	quoted := false
	i := 0
	for ; i < len(t) && (quoted || (t[i] != ' ' && t[i] != '\t')); i++ {
		if t[i] == '\\' && i+1 < len(t) && t[i+1] == '"' {
			i++
			continue
		}
		if t[i] == '"' {
			quoted = !quoted
		}
	}
	pk, err := parseBareKey(strings.TrimLeft(t[i:], " \t"))
	return pk, t[:i], err
}

// parseBareKey reads "type base64 [comment]", with no options. An RSA key written
// under a signature algorithm's name (rsa-sha2-256, -512) is read as the ssh-rsa key
// sshd takes it for.
func parseBareKey(s string) (ssh.PublicKey, error) {
	for _, try := range []string{s, rsaSHA2RE.ReplaceAllString(s, "${1}ssh-rsa${3}")} {
		pk, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(try))
		if err == nil && len(opts) == 0 && strings.HasPrefix(try, pk.Type()) {
			return pk, nil
		}
	}
	return nil, errors.New("not a key")
}

// splitOptions splits an authorized_keys options string at commas outside quotes, as
// sshd does: inside quotes only \" escapes.
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quoted && i+1 < len(s) && s[i+1] == '"':
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

// echoProbe runs command "echo word", with the same on stdin, and says whether word
// came back. Output of any size is read to the end, kept no further than it takes to
// find the word, so a forced command that answers at length is no reason to stop.
func echoProbe(ctx context.Context, sshConfig string, timeout time.Duration, word string, r config.Resolved) (found bool, stderr string, err error) {
	r.Forwards = nil
	var args []string
	for i, a := range agentSSHArgs(r, sshConfig, timeout) {
		if a == "-n" && i == 0 {
			continue
		}
		args = append(args, a)
	}
	r.ExtraArgs = args
	c := sshrun.SSHContext(ctx, r, "echo "+word)
	noPrompts(c)
	c.Stdin = strings.NewReader("echo " + word + "\nexit\n")
	out := &wordFinder{word: []byte(word)}
	errs := &sshrun.Tail{Max: 16 << 10}
	c.Stdout, c.Stderr = out, errs
	c.WaitDelay = time.Second
	err = c.Run()
	return out.found(), errs.String(), err
}

// wordFinder looks for word in everything written, keeping only enough of the end to
// find it across two writes.
type wordFinder struct {
	mu   sync.Mutex
	word []byte
	tail []byte
	hit  bool
}

func (w *wordFinder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.hit {
		buf := append(w.tail, p...)
		if bytes.Contains(buf, w.word) {
			w.hit = true
		}
		if keep := len(w.word) - 1; len(buf) > keep {
			buf = buf[len(buf)-keep:]
		}
		w.tail = append([]byte(nil), buf...)
	}
	return len(p), nil
}

func (w *wordFinder) found() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.hit }

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
