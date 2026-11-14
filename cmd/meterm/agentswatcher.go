package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
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
			stdout, stderr, overflow, err := sshLookQuery(sshConfig, timeout, agents.RemoteCommand, agents.MaxOutput)(ctx, r)
			if authRefusedBy(stderr, r.Address) {
				err = errKeyRefused
			}
			return stdout, stderr, overflow, err
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

// agentsKeyRefused is a look's error when the host, not a jump host, refused the key
// of an ordinary look (no agents_identity). It's asked again, but no faster than once a
// minute (see watchHost): each try is a failed login there.
const agentsKeyRefused = "not let in: the host refused the key ssh offered it"

// agentsWatcherRefused is a look's error when the host refused the watcher key. The
// watch stops asking a host that says this (see watchHost).
const agentsWatcherRefused = "not let in: the host refused the watcher key (agents_identity); put the line from `meterm agents key` on it, check it with `meterm agents key --check`, and start again"

var (
	// errKeyRefused is the host saying no to an ordinary look's key.
	errKeyRefused = errors.New("key refused")
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

// watcherSettingsText is a watcherSettingsError's own words, without the marker.
func watcherSettingsText(err error) string {
	return strings.TrimPrefix(err.Error(), errWatcherSettings.Error()+": ")
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
// for the host, while it leaves the config out for it: where the host is, as whom, how
// to get there, and which host keys count. Keys, agents and certificates stay out.
// StrictHostKeyChecking is a look's own, yes, and isn't carried. A ProxyCommand isn't
// carried either, and a host that has one isn't looked at (errProxyCommand): going
// straight to its HostName would be a different route than the person set up.
var carriedOptions = []struct{ key, name string }{
	{"hostname", "HostName"}, {"user", "User"}, {"port", "Port"}, {"proxyjump", "ProxyJump"},
	{"userknownhostsfile", "UserKnownHostsFile"}, {"globalknownhostsfile", "GlobalKnownHostsFile"},
	{"hostkeyalias", "HostKeyAlias"},
	{"addressfamily", "AddressFamily"}, {"bindaddress", "BindAddress"}, {"bindinterface", "BindInterface"},
}

// carriedValueRE is a value that's one plain word to ssh's config parser: no quotes, no
// spaces, nothing for it to split.
var carriedValueRE = regexp.MustCompile(`^[A-Za-z0-9._/@:%~+=,\[\]-]+$`)

func carry(set sshSettings) ([]string, error) {
	if pc := set.one("proxycommand"); pc != "" && pc != "none" {
		return nil, watcherSettingsError("the host is reached through a ProxyCommand in your ssh config, and watcher looks don't run one; use ProxyJump for it, or take agents_identity off the host")
	}
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
	user, hostname, port string
}

// refused says whether stderr is the host at the end refusing the login, not a jump
// hop. ssh names it "user@host" with the user it logged in as, whoever chose it, and
// the host as it connected to it.
func (w watcherResolved) refused(stderr string) bool {
	return authRefusedBy(stderr, w.Address, w.hostname)
}

// authRefusedBy says whether stderr is one of hosts refusing the login, as ssh names
// it: "user@host: Permission denied", whatever the user.
func authRefusedBy(stderr string, hosts ...string) bool {
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
		for _, h := range hosts {
			if h != "" && head == h {
				return true
			}
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
	return watcherResolved{Resolved: r, user: set.one("user"), hostname: set.one("hostname"), port: set.one("port")}, sshConfig, remove, nil
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
	if why := watcherKeyUnusable(r.AgentsIdentity); why != "" {
		return "not asked: " + why
	}
	if why := watcherKeyHasCert(r.AgentsIdentity); why != "" {
		return "not asked: " + why
	}
	return ""
}

// watcherKeyHasCert refuses a watcher key with a certificate beside it: ssh offers
// KEY-cert.pub with KEY on its own, and a host that trusts the certificate's CA lets it
// in without reading authorized_keys, so without the line's limits.
func watcherKeyHasCert(path string) string {
	p := strings.TrimSuffix(sshkey.LocalPath(path), ".pub")
	if _, err := os.Lstat(p + "-cert.pub"); err == nil {
		return "the watcher key has a certificate beside it (" + termsafe.Quote(path+"-cert.pub") + "); ssh would offer it too, and a host that trusts its CA lets it in without the authorized_keys line's limits. Use a key with no certificate"
	}
	return ""
}

// watcherKeyUnusable says why ssh on this machine would pass over the watcher key, or
// "". ssh skips a key it can't open without asking (BatchMode) and one others can read,
// and goes on to say the host refused it: that would send the person to fix the wrong
// machine, so it's caught here, before the host is asked.
func watcherKeyUnusable(path string) string {
	p := sshkey.LocalPath(path)
	b, err := os.ReadFile(p)
	if err != nil {
		return "this machine can't use the watcher key: it can't be read"
	}
	if _, err := ssh.ParsePrivateKey(b); err != nil {
		var pass *ssh.PassphraseMissingError
		if errors.As(err, &pass) {
			return "this machine can't use the watcher key: it has a passphrase, and a look can't type one (make it without one; the host is what limits it)"
		}
		return "this machine can't use the watcher key: it isn't a private key ssh can use unattended"
	}
	if mode := keyFileMode(p); mode&0o077 != 0 {
		return fmt.Sprintf("this machine can't use the watcher key: its mode is %04o, and ssh won't use a key others can read (chmod 600 it)", mode)
	}
	return ""
}

// agentKeys lists the keys the ssh-agent at sock holds ("" for $SSH_AUTH_SOCK), as
// `ssh-add -L` says. With no agent there at all (no $SSH_AUTH_SOCK) it's none, and
// fine. An agent that's there but can't be asked (a dead socket, a broken agent) comes
// back as unchecked, so it isn't taken for one with no keys. A locked agent answers as
// one with no keys, and can't be told apart. A test can stand in for it.
var agentKeys = func(ctx context.Context, sock string) (keys []ssh.PublicKey, unchecked string) {
	if sock == "" && os.Getenv("SSH_AUTH_SOCK") == "" {
		return nil, ""
	}
	c := exec.CommandContext(ctx, "ssh-add", "-L")
	if sock != "" {
		c.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	}
	where := "ssh-agent"
	if sock != "" {
		where = "the agent at " + termsafe.Quote(sock)
	}
	out, err := c.Output()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return nil, "" // "The agent has no identities."
	case err != nil:
		return nil, where + " (it couldn't be asked)"
	}
	for _, l := range strings.Split(string(out), "\n") {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l)); err == nil {
			keys = append(keys, k)
		}
	}
	return keys, ""
}

// sshHome is the home directory ssh expands ~ and %d to: the password database's, not
// $HOME. A test can stand in for it.
var sshHome = func() string {
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// expandIdentityPath does to an IdentityFile what ssh does before opening it: ~ and
// ${VAR}, then the % tokens it allows there. ok is false for anything it can't be sure
// of, such as a token it doesn't know or a variable that isn't set.
func expandIdentityPath(p string, set sshSettings) (string, bool) {
	home := sshHome()
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home == "" {
			return "", false
		}
		p = home + p[1:]
	} else if strings.HasPrefix(p, "~") {
		return "", false // ~user
	}
	ok := true
	p = envVarRE.ReplaceAllStringFunc(p, func(m string) string {
		v, set := os.LookupEnv(m[2 : len(m)-1])
		if !set {
			ok = false
		}
		return v
	})
	if !ok {
		return "", false
	}
	local := ""
	if u, err := user.Current(); err == nil {
		local = u.Username
	}
	hostname, _ := os.Hostname()
	short, _, _ := strings.Cut(hostname, ".")
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '%' {
			b.WriteByte(p[i])
			continue
		}
		if i+1 >= len(p) {
			return "", false
		}
		i++
		var v string
		switch p[i] {
		case '%':
			v = "%"
		case 'd':
			v = home
		case 'u':
			v = local
		case 'h':
			v = set.one("hostname")
		case 'r':
			v = set.one("user")
		case 'p':
			v = set.one("port")
		case 'L':
			v = short
		case 'l':
			v = hostname
		case 'i':
			v = strconv.Itoa(os.Getuid())
		case 'k':
			v = set.one("hostkeyalias")
			if v == "" {
				v = set.one("hostname")
			}
		default:
			return "", false
		}
		if v == "" {
			return "", false
		}
		b.WriteString(v)
	}
	return b.String(), true
}

var envVarRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// otherLoginKeys is every key an ordinary login to the host could use: each
// IdentityFile ssh -G names for it (the host's identity_file, the ssh config's, or
// ssh's defaults), expanded as ssh would, every key in ssh-agent, and every key in the
// agent an IdentityAgent in the config names. unchecked lists files whose public key
// couldn't be read, or whose path couldn't be worked out.
func otherLoginKeys(ctx context.Context, set sshSettings) (keys []ssh.PublicKey, unchecked []string) {
	for _, f := range set["identityfile"] {
		p, ok := expandIdentityPath(f, set)
		if !ok {
			unchecked = append(unchecked, f)
			continue
		}
		k, err := publicKeyOf(p)
		switch {
		case err == nil:
			keys = append(keys, k)
		case !errors.Is(err, os.ErrNotExist):
			unchecked = append(unchecked, f)
		}
	}
	ak, bad := agentKeys(ctx, "")
	keys = append(keys, ak...)
	if bad != "" {
		unchecked = append(unchecked, bad)
	}
	if a := set.one("identityagent"); a != "" && a != "none" && a != "SSH_AUTH_SOCK" {
		if strings.HasPrefix(a, "$") {
			a = os.Getenv(strings.Trim(a, "${}"))
		}
		if a != "" {
			ak, bad := agentKeys(ctx, a)
			keys = append(keys, ak...)
			if bad != "" {
				unchecked = append(unchecked, bad)
			}
		}
	}
	return keys, unchecked
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
