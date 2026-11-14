package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshkey"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

// A look with a watcher key (agents_identity): that key and no other goes to the host.
// The limit on what the key may do is on the host's side (`meterm agents key`); what's
// here makes sure a look can't quietly get in some other way.

// sshAgentQuery runs agents.RemoteCommand over ssh. r.ExtraArgs have passed
// checkAgentExtraArgs by now. A host with a watcher key gets a look of its own
// (watcherLook), with a config of its own.
func sshAgentQuery(sshConfig string, timeout time.Duration) agentQuery {
	return func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		if r.AgentsIdentity == "" {
			return sshLookQuery(sshConfig, timeout, agents.RemoteCommand, agents.MaxOutput)(ctx, r)
		}
		wr, wcfg, remove, err := watcherLook(ctx, r, timeout)
		if err != nil {
			return nil, "", false, err
		}
		defer remove()
		stdout, stderr, overflow, err := sshLookQuery(wcfg, timeout, agents.RemoteCommand, agents.MaxOutput)(ctx, wr.Resolved)
		if wr.refused(stderr) {
			err = errWatcherKeyRefused
		}
		return stdout, stderr, overflow, err
	}
}

// agentsWatcherRefused is a look's error when the host refused the watcher key. The
// watch stops asking a host that says this (see watchHost).
const agentsWatcherRefused = "not let in: the host refused the watcher key (agents_identity); put the line from `meterm agents key` on it, check it with `meterm agents key --check`, and start again"

var (
	// errWatcherKeyRefused is the host saying no to the watcher key. There's no second
	// try with another key: that would be the look the key was meant to rule out.
	errWatcherKeyRefused = errors.New("watcher key refused")
	// errWatcherSettings wraps why a watcher look couldn't be set up; its text is
	// meterm's own.
	errWatcherSettings = errors.New("watcher look")
)

func watcherSettingsError(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errWatcherSettings}, a...)...)
}

// sshG runs `ssh -G` with these arguments: what ssh would use, read from the config,
// with nothing contacted. A test can stand in for it.
var sshG = func(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "ssh", append([]string{"-G"}, args...)...).Output()
}

// sshSettings is `ssh -G`'s answer: each option, lower case, with its values in order.
type sshSettings map[string][]string

func (s sshSettings) one(key string) string {
	if v := s[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// lookSettings is what ssh would make of r for an ordinary look: the same arguments, the
// same config, the person's own included.
func lookSettings(ctx context.Context, r config.Resolved, timeout time.Duration) (sshSettings, error) {
	sshConfig, remove, err := writeAgentSSHConfig()
	if err != nil {
		return nil, watcherSettingsError("%s", err.Error())
	}
	defer remove()
	rr := r
	rr.Forwards = nil
	rr.ExtraArgs = agentSSHArgs(r, sshConfig, timeout)
	out, err := sshG(ctx, sshrun.BuildArgs(rr))
	if err != nil {
		return nil, watcherSettingsError("ssh -G couldn't say what it would use for this host")
	}
	set := sshSettings{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			k = strings.ToLower(k)
			set[k] = append(set[k], v)
		}
	}
	if len(set) == 0 {
		return nil, watcherSettingsError("ssh -G said nothing about this host")
	}
	return set, nil
}

// carriedOptions are the settings of the person's ssh config that a watcher look keeps
// for the host, while it leaves the config out for it: where the host is, as whom, and
// which host keys count. Keys and agents stay out, as does anything that runs a
// program (ProxyCommand, LocalCommand, KnownHostsCommand), which a look doesn't carry.
// StrictHostKeyChecking is a look's own, yes, and isn't carried.
var carriedOptions = []struct{ key, name string }{
	{"hostname", "HostName"}, {"user", "User"}, {"port", "Port"}, {"proxyjump", "ProxyJump"},
	{"userknownhostsfile", "UserKnownHostsFile"}, {"globalknownhostsfile", "GlobalKnownHostsFile"},
	{"hostkeyalias", "HostKeyAlias"},
}

// carriedValueRE is a value that's one plain word to ssh's config parser: no quotes, no
// spaces, nothing for it to split.
var carriedValueRE = regexp.MustCompile(`^[A-Za-z0-9._/@:%~+=,\[\]-]+$`)

func carry(set sshSettings) ([]string, error) {
	var lines []string
	for _, o := range carriedOptions {
		vals := set[o.key]
		if len(vals) == 0 || len(vals) == 1 && vals[0] == "none" {
			continue
		}
		var words []string
		for _, v := range vals {
			words = append(words, strings.Fields(v)...)
		}
		for _, w := range words {
			if !carriedValueRE.MatchString(w) {
				return nil, watcherSettingsError("its %s in your ssh config has characters a watcher look can't carry", o.name)
			}
		}
		lines = append(lines, o.name+" "+strings.Join(words, " "))
	}
	return lines, nil
}

// watcherResolved is r for a watcher look, and who ssh will say it is.
type watcherResolved struct {
	config.Resolved
	user, hostname string
}

// refused says whether stderr is the host at the end refusing the login, not a jump
// hop. ssh names it "user@host" with the user it logged in as, whoever chose it, and
// the host as it connected to it.
func (w watcherResolved) refused(stderr string) bool {
	if sshrun.ClassifyFailure(stderr) != sshrun.FailureAuth {
		return false
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		head, _, ok := strings.Cut(line, ": Permission denied")
		if !ok {
			continue
		}
		if i := strings.LastIndex(head, "@"); i >= 0 {
			head = head[i+1:]
		}
		if head == w.Address || head == w.hostname {
			return true
		}
	}
	return false
}

// watcherLook is a look with r's watcher key and no other: in place of its
// identity_file, with IdentitiesOnly, without the keys its extra_args name, and with an
// ssh config that leaves out the person's own for this host, so that no IdentityFile,
// IdentityAgent or CertificateFile there is added. What else that config says about the
// host (carriedOptions) is read with ssh -G first and written in for it. ssh-agent is
// still asked, for that key alone, in case it holds it. Jump hops read the person's
// config as always and log in with their usual keys: the watcher key is for the host
// at the end. The returned func removes the config.
func watcherLook(ctx context.Context, r config.Resolved, timeout time.Duration) (watcherResolved, string, func(), error) {
	set, err := lookSettings(ctx, r, timeout)
	if err != nil {
		return watcherResolved{}, "", nil, err
	}
	carried, err := carry(set)
	if err != nil {
		return watcherResolved{}, "", nil, err
	}
	sshConfig, remove, err := writeLookSSHConfig(r.Address, carried)
	if err != nil {
		return watcherResolved{}, "", nil, watcherSettingsError("%s", err.Error())
	}
	r.IdentityFile = r.AgentsIdentity
	extra := []string{"-o", "IdentitiesOnly=yes"}
	for i := 0; i < len(r.ExtraArgs); i++ {
		a := r.ExtraArgs[i]
		switch {
		case a == "-i":
			i++ // and its value: checkAgentExtraArgs made sure there is one
			continue
		case strings.HasPrefix(a, "-i"):
			continue
		case a == "-o" && i+1 < len(r.ExtraArgs) && identityOption(r.ExtraArgs[i+1]):
			i++
			continue
		case strings.HasPrefix(a, "-o") && identityOption(a[2:]):
			continue
		}
		extra = append(extra, a)
	}
	r.ExtraArgs = extra
	return watcherResolved{Resolved: r, user: set.one("user"), hostname: set.one("hostname")}, sshConfig, remove, nil
}

// identityOption is an -o value that names a key or how keys are chosen.
func identityOption(v string) bool {
	key, _, found := strings.Cut(v, "=")
	if !found {
		key, _, _ = strings.Cut(v, " ")
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "identityfile", "identitiesonly", "identityagent", "certificatefile":
		return true
	}
	return false
}

// checkAgentsIdentity refuses a look whose watcher key isn't there, or whose path
// has characters that don't show: ssh would carry on without a missing key, with
// whatever other key gets in, and the look would quietly not be the limited one it
// was set up to be. The path may come from a group or defaults, which nothing checks
// when they're written.
func checkAgentsIdentity(r config.Resolved) string {
	if r.AgentsIdentity == "" {
		return ""
	}
	if config.CheckText("agents_identity", r.AgentsIdentity) != nil {
		return "not asked: its agents_identity has characters that don't show"
	}
	if st, err := os.Stat(sshkey.LocalPath(r.AgentsIdentity)); err != nil || !st.Mode().IsRegular() {
		return "not asked: its agents_identity " + termsafe.Quote(r.AgentsIdentity) + " isn't a file here"
	}
	return ""
}

// agentKeys lists the keys ssh-agent holds, as `ssh-add -L` says: none when there's no
// agent. A test can stand in for it.
var agentKeys = func(ctx context.Context) []ssh.PublicKey {
	out, err := exec.CommandContext(ctx, "ssh-add", "-L").Output()
	if err != nil {
		return nil
	}
	var keys []ssh.PublicKey
	for _, l := range strings.Split(string(out), "\n") {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l)); err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

// otherLoginKeys is every key an ordinary login to the host could use: each
// IdentityFile ssh -G names for it (the host's identity_file, the ssh config's, or
// ssh's defaults), and every key in ssh-agent. unchecked lists files whose public key
// couldn't be read.
func otherLoginKeys(ctx context.Context, set sshSettings) (keys []ssh.PublicKey, unchecked []string) {
	for _, f := range set["identityfile"] {
		k, err := publicKeyOf(f)
		switch {
		case err == nil:
			keys = append(keys, k)
		case !errors.Is(err, os.ErrNotExist):
			unchecked = append(unchecked, f)
		}
	}
	return append(keys, agentKeys(ctx)...), unchecked
}

// publicKeyOf is the public half of an identity file: its .pub, or the file itself if
// it's an unencrypted private key. os.ErrNotExist when neither is there.
func publicKeyOf(path string) (ssh.PublicKey, error) {
	p := sshkey.LocalPath(path)
	if k, err := readPublicKey(p); err == nil {
		return k, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, errors.New("not readable without a passphrase")
	}
	return signer.PublicKey(), nil
}

func sameKey(a, b ssh.PublicKey) bool { return bytes.Equal(a.Marshal(), b.Marshal()) }
