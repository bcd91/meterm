package sshrun

import (
	"errors"
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

// What `meterm connect web1 -L 8080:localhost:80` relies on: ssh still
// reads options after the destination, and `connect web1 -- cmd` relies on
// ssh taking that "--" itself. Checked with the real ssh (-G parses only).
func TestOptionsAfterTheDestinationStillWork(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	r := config.Resolved{Address: "198.51.100.7", User: "deploy", Port: 2222}
	out, err := exec.Command("ssh", append([]string{"-G", "-F", "/dev/null"}, BuildArgs(r, "-L", "8080:localhost:80")...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}
	cfg := string(out)
	for _, want := range []string{"hostname 198.51.100.7", "user deploy", "port 2222", "localforward 8080 [localhost]:80"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("ssh -G lacks %q; the options after the destination weren't read", want)
		}
	}
}

func TestCommandAfterDoubleDashReachesTheHost(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	// With "--" between options and destination, ssh would hand the second
	// "--" to the remote shell ("bash: --: invalid option"). -G stops before
	// connecting, so what we can check is that the args parse as a command.
	r := config.Resolved{Address: "198.51.100.7"}
	args := BuildArgs(r, "--", "uptime")
	for i, a := range args {
		if a == "198.51.100.7" && i > 0 && args[i-1] == "--" {
			t.Errorf("-- before the destination: %q", args)
		}
	}
	if err := exec.Command("ssh", append([]string{"-G", "-F", "/dev/null"}, args...)...).Run(); err != nil {
		t.Errorf("ssh -G rejects %q: %v", args, err)
	}
}
