package authkeys

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const kAlice = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk alice@laptop"
const kBob = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbP3cZbPwHkT8e6S1e3lWJ2qL0nH4wqcbIu1vVYvO5K bob@desk"

func listing(host, raw string) HostKeys {
	l := Parse(raw)
	return HostKeys{Host: host, Sudo: l.Sudo, Entries: l.Entries}
}

func TestPlanRevoke(t *testing.T) {
	res := []HostKeys{
		listing("web1", "sudo=yes\n### dev /home/dev/.ssh/authorized_keys self\n"+kAlice+"\n"+kBob+"\n### root /root/.ssh/authorized_keys\n"+kBob+"\n"),
		listing("web2", "sudo=no\n### dev /home/dev/.ssh/authorized_keys self\n"+kAlice+"\n"),
		{Host: "down", Error: "connect timed out"},
	}
	plan, problems := Plan(res, Matcher{Comments: []string{"bob@desk"}}, map[string]bool{})
	if len(plan) != 2 || plan[0].File != "/home/dev/.ssh/authorized_keys" || !plan[0].Own || plan[1].Own {
		t.Fatalf("plan = %+v", plan)
	}
	for _, p := range problems {
		if p.Lockout {
			t.Errorf("unexpected lockout: %s", p.Msg)
		}
	}
	msgs := ""
	for _, p := range problems {
		msgs += p.Msg + "\n"
	}
	if !strings.Contains(msgs, "down: couldn't check") || !strings.Contains(msgs, "web2: no passwordless sudo") {
		t.Errorf("problems:\n%s", msgs)
	}
}

func TestPlanRevokeRefusesLockout(t *testing.T) {
	alice, _ := ParsePublicKey(kAlice)
	res := []HostKeys{listing("web2", "sudo=yes\n### dev /home/dev/.ssh/authorized_keys self\n"+kAlice+"\n")}
	// The last key in the login file…
	_, problems := Plan(res, Matcher{Comments: []string{"alice@laptop"}}, map[string]bool{})
	if len(problems) != 1 || !problems[0].Lockout {
		t.Errorf("last key: %+v", problems)
	}
	// …and a key this machine holds.
	res = []HostKeys{listing("web1", "sudo=yes\n### dev /h self\n"+kAlice+"\n"+kBob+"\n")}
	_, problems = Plan(res, Matcher{Keys: []PublicKey{alice}}, map[string]bool{alice.Fingerprint: true})
	if len(problems) != 1 || !problems[0].Lockout || !strings.Contains(problems[0].Msg, "key on this machine") {
		t.Errorf("own key: %+v", problems)
	}
}

// Apply runs the real revoke script, here against a local file.
func TestApplyRemovesOnlyTheMatchedKeyAndBacksUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(file, []byte(kAlice+"\n"+kBob+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Parse("sudo=no\n### dev " + file + " self\n" + kAlice + "\n" + kBob + "\n")
	plan, _ := Plan([]HostKeys{{Host: "local", Entries: l.Entries}}, Matcher{Comments: []string{"bob@desk"}}, nil)
	var ran []string
	sh := func(ctx context.Context, host, script string) (string, error) {
		ran = append(ran, host)
		c := exec.CommandContext(ctx, "sh", "-s")
		c.Stdin = strings.NewReader(script)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	res := Apply(context.Background(), plan, sh, "T1")
	if len(res) != 1 || res[0].Err != nil || len(ran) != 1 {
		t.Fatalf("results = %+v", res)
	}
	got, _ := os.ReadFile(file)
	if string(got) != kAlice+"\n" {
		t.Errorf("file now = %q", got)
	}
	if bak, err := os.ReadFile(res[0].Backup); err != nil || string(bak) != kAlice+"\n"+kBob+"\n" {
		t.Errorf("backup = %q, %v", bak, err)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v", st.Mode().Perm())
	}
}

func TestApplyReportsAScriptThatDidntSayRevoked(t *testing.T) {
	plan := []FileRevoke{{Host: "h", File: "/home/dev/.ssh/authorized_keys", Own: true, Entries: []Entry{{Blob: "AAAA"}}}}
	res := Apply(context.Background(), plan, func(context.Context, string, string) (string, error) {
		return "backup of x failed; nothing changed\n", nil
	}, "T")
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "backup") {
		t.Errorf("err = %v", res[0].Err)
	}
}

func TestBlocked(t *testing.T) {
	if _, ok := Blocked([]Problem{{Msg: "a"}}); ok {
		t.Error("non-lockout blocked")
	}
	if p, ok := Blocked([]Problem{{Msg: "a"}, {Msg: "b", Lockout: true}}); !ok || p.Msg != "b" {
		t.Errorf("Blocked = %+v, %v", p, ok)
	}
}
