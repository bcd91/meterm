package authkeys

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const alice = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk alice@laptop"
const bob = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbP3cZbPwHkT8e6S1e3lWJ2qL0nH4wqcbIu1vVYvO5K bob@desk"

func TestParseListing(t *testing.T) {
	out := "sudo=yes\n### dev /home/dev/.ssh/authorized_keys self\n# team keys\n" + alice + "\n" +
		`from="10.0.0.0/8" ` + bob + "\nnot a key\n### root /root/.ssh/authorized_keys\n" + bob + "\n"
	l := Parse(out)
	if !l.Sudo || len(l.Files) != 2 || len(l.Entries) != 3 {
		t.Fatalf("listing = %+v", l)
	}
	a, b, r := l.Entries[0], l.Entries[1], l.Entries[2]
	if a.Comment != "alice@laptop" || !a.Own || a.Owner != "dev" || !strings.HasPrefix(a.Fingerprint, "SHA256:") {
		t.Errorf("alice = %+v", a)
	}
	if b.Options != `from="10.0.0.0/8"` || b.Fingerprint != r.Fingerprint {
		t.Errorf("bob = %+v / %+v", b, r)
	}
	if r.Own || r.Owner != "root" {
		t.Errorf("root's entry = %+v", r)
	}
}

func TestMatcher(t *testing.T) {
	k, err := ParsePublicKey(bob)
	if err != nil {
		t.Fatal(err)
	}
	l := Parse("### dev /h self\n" + alice + "\n" + bob + "\n")
	if m := (Matcher{Keys: []PublicKey{k}}); m.Match(l.Entries[0]) || !m.Match(l.Entries[1]) {
		t.Error("match by key")
	}
	if m := (Matcher{Comments: []string{"ALICE@laptop"}}); !m.Match(l.Entries[0]) || m.Match(l.Entries[1]) {
		t.Error("match by comment")
	}
}

func TestRevokeScriptRemovesOnlyThatKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "authorized_keys")
	orig := "# keep me\n" + alice + "\n" + `command="/bin/false" ` + bob + "\n"
	if err := os.WriteFile(f, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _ := ParsePublicKey(bob)
	script, err := RevokeScript(f, []string{k.Blob}, false, "20261016")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "revoked") {
		t.Fatalf("script: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "# keep me\n"+alice+"\n" {
		t.Errorf("after revoke:\n%s", got)
	}
	if bak, _ := os.ReadFile(f + ".meterm-bak-20261016"); string(bak) != orig {
		t.Errorf("backup = %q", bak)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if _, err := RevokeScript("/home/x/.ssh/a'b", []string{k.Blob}, false, "x"); err == nil {
		t.Error("quote in path: want refusal")
	}
}
