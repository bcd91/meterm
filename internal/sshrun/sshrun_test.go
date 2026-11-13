package sshrun

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestBuildArgsBasic(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "deploy", Port: 22}
	args := BuildArgs(r)
	if got, want := strings.Join(args, " "), "deploy@10.0.0.1"; got != want {
		t.Errorf("BuildArgs = %q, want %q", got, want)
	}
}

func TestBuildArgsNonDefaultPortAndIdentity(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "deploy", Port: 2222, IdentityFile: "~/.ssh/key"}
	args := BuildArgs(r)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("missing -p 2222: %q", joined)
	}
	if !strings.HasSuffix(joined, "deploy@10.0.0.1") {
		t.Errorf("target should be last: %q", joined)
	}
}

func TestBuildArgsProxyJumpChain(t *testing.T) {
	r := config.Resolved{
		Address: "10.0.0.5",
		User:    "app",
		Port:    22,
		ProxyJump: []config.Resolved{
			{Address: "203.0.113.9", User: "jump", Port: 22},
		},
	}
	args := BuildArgs(r)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-J jump@203.0.113.9") {
		t.Errorf("expected -J jump@203.0.113.9, got %q", joined)
	}
}

func TestBuildArgsForwardsAndExtra(t *testing.T) {
	r := config.Resolved{
		Address: "10.0.0.5",
		Forwards: []config.Forward{
			{Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 80},
			{Type: "D", LocalPort: 1080},
		},
	}
	args := BuildArgs(r, "uptime")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-L 8080:localhost:80") {
		t.Errorf("missing local forward: %q", joined)
	}
	if !strings.Contains(joined, "-D 1080") {
		t.Errorf("missing dynamic forward: %q", joined)
	}
	if !strings.HasSuffix(joined, "10.0.0.5 uptime") {
		t.Errorf("expected trailing remote command, got %q", joined)
	}
}

// The destination can never be read as an option, and IPv6 addresses get
// the brackets scp/sftp/rsync and jump specs need.
func TestDestinationIsNeverAnOption(t *testing.T) {
	r := config.Resolved{Address: "-oProxyCommand=touch /tmp/x"}
	args := BuildArgs(r)
	if args[len(args)-2] != "--" {
		t.Errorf("no -- before an option-like destination: %q", args)
	}
	if got := remoteSpec(config.Resolved{Address: "2001:db8::1", User: "u"}, "/p"); got != "u@[2001:db8::1]:/p" {
		t.Errorf("remoteSpec = %q", got)
	}
	r = config.Resolved{Address: "x", ProxyJump: []config.Resolved{{Address: "2001:db8::2", User: "j", Port: 2222}}}
	if got := proxyJumpSpec(r); got != "j@[2001:db8::2]:2222" {
		t.Errorf("jump = %q", got)
	}
}

func TestUnsafeHostsDontRun(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", Err: errUnsafe}
	for name, c := range map[string]interface{ Start() error }{
		"ssh": SSH(r), "scp": ScpCommand(r, "/a", "b", false, false), "sftp": SftpCommand(r, ""),
		"rsync": RsyncCommand(r, "/a", "b", false, nil), "subsystem": SubsystemCommand(r, "sftp"),
		"copy-id": CopyIDCommand(r, "k"), "stats": StatsCommand(r),
	} {
		if err := c.Start(); err != errUnsafe {
			t.Errorf("%s started anyway: %v", name, err)
		}
	}
}

var errUnsafe = errors.New("unsafe host")

// sshParse reads argv the way OpenSSH's ssh(1) does, as far as these tests
// need: getopt over its option letters, then the destination, then — unless a
// "--" ended the options — getopt again over what follows it (ssh.c, "goto
// again"), then the remote command. Tests never run the real ssh, not even
// -G: METERM_TEST_REAL_SSH_G=1 also checks this model against the ssh in PATH.
type sshParsed struct {
	dest     string
	opts     map[byte][]string
	command  []string
	badFlags []string
}

const sshArgFlags = "BbcDEeFIiJLlmOoPpQRSWw"

func sshParse(args []string) sshParsed {
	p := sshParsed{opts: map[byte][]string{}}
	getopt := func(av []string) (rest []string, terminated bool) {
		i := 0
		for i < len(av) {
			a := av[i]
			if a == "--" {
				return av[i+1:], true
			}
			if len(a) < 2 || a[0] != '-' {
				return av[i:], false
			}
			i++
			for k := 1; k < len(a); k++ {
				c := a[k]
				if !strings.ContainsRune(sshArgFlags, rune(c)) {
					if !strings.ContainsRune("46AaCfGgKkMNnqsTtVvXxYy", rune(c)) {
						p.badFlags = append(p.badFlags, string(c))
					}
					p.opts[c] = append(p.opts[c], "")
					continue
				}
				val := a[k+1:]
				if val == "" && i < len(av) {
					val = av[i]
					i++
				}
				p.opts[c] = append(p.opts[c], val)
				break
			}
		}
		return nil, false
	}
	rest, term := getopt(args)
	if len(rest) > 0 {
		p.dest = rest[0]
		rest = rest[1:]
		if len(rest) > 0 && !term {
			rest, _ = getopt(rest)
		}
	}
	p.command = rest
	return p
}

func TestSSHParseModel(t *testing.T) {
	p := sshParse([]string{"-p", "22", "-tt", "-oX=1", "u@h", "-L", "1:a:2", "--", "-x", "y"})
	if p.dest != "u@h" || strings.Join(p.opts['p'], ",") != "22" || strings.Join(p.opts['o'], ",") != "X=1" ||
		strings.Join(p.opts['L'], ",") != "1:a:2" || strings.Join(p.command, " ") != "-x y" || len(p.opts['t']) != 2 {
		t.Errorf("model: %+v", p)
	}
	if p := sshParse([]string{"h", "uptime", "-L", "1:a:2"}); strings.Join(p.command, " ") != "uptime -L 1:a:2" || len(p.opts['L']) != 0 {
		t.Errorf("after the command nothing is an option: %+v", p)
	}
}

// realSSHG runs the real `ssh -G` on args, only when asked (it parses and
// prints its config; it doesn't connect).
func realSSHG(t *testing.T, args []string) string {
	t.Helper()
	if os.Getenv("METERM_TEST_REAL_SSH_G") != "1" {
		return ""
	}
	out, err := exec.Command("ssh", append([]string{"-G", "-F", "/dev/null"}, args...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G %q: %v", args, err)
	}
	return string(out)
}

// What `meterm connect web1 -L 8080:localhost:80` relies on: ssh still
// reads options after the destination, and `connect web1 -- cmd` relies on
// ssh taking that "--" itself.
func TestOptionsAfterTheDestinationStillWork(t *testing.T) {
	r := config.Resolved{Address: "198.51.100.7", User: "deploy", Port: 2222}
	args := BuildArgs(r, "-L", "8080:localhost:80")
	p := sshParse(args)
	if p.dest != "deploy@198.51.100.7" || strings.Join(p.opts['p'], ",") != "2222" ||
		strings.Join(p.opts['L'], ",") != "8080:localhost:80" || len(p.command) != 0 || len(p.badFlags) != 0 {
		t.Errorf("ssh would read %q as %+v", args, p)
	}
	if cfg := realSSHG(t, args); cfg != "" {
		for _, want := range []string{"hostname 198.51.100.7", "user deploy", "port 2222", "localforward 8080 [localhost]:80"} {
			if !strings.Contains(cfg, want) {
				t.Errorf("ssh -G lacks %q; the options after the destination weren't read", want)
			}
		}
	}
}

func TestCommandAfterDoubleDashReachesTheHost(t *testing.T) {
	// With "--" between options and destination, ssh would hand the second
	// "--" to the remote shell ("bash: --: invalid option").
	r := config.Resolved{Address: "198.51.100.7"}
	args := BuildArgs(r, "--", "uptime")
	for i, a := range args {
		if a == "198.51.100.7" && i > 0 && args[i-1] == "--" {
			t.Errorf("-- before the destination: %q", args)
		}
	}
	if p := sshParse(args); p.dest != "198.51.100.7" || strings.Join(p.command, " ") != "uptime" {
		t.Errorf("ssh would read %q as %+v, not a command", args, p)
	}
	realSSHG(t, args)
}
