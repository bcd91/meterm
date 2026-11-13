package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// export writes ssh_config lines. A name with a line break in it added a
// line of its own ("nl\nname" became "Host nl" and a bare "name": a crafted
// one could add a ProxyCommand), a tab or a space added an alias, and "*"
// would have made a block for every host ssh connects to. Such hosts are
// left out, named on stderr, and so are hosts that jump through one.
// v0.23.1's allowances stay: a '#' inside a name (ssh reads it as a comment
// only at the start of a word) and an identity file with a space, written
// double-quoted, are exported.
func TestExportLeavesOutWhatSSHConfigWouldSplit(t *testing.T) {
	dir := cliSandbox(t)
	saveHosts(t,
		config.Host{Name: "ok", Address: "192.0.2.1", User: "deploy"},
		config.Host{Name: "nl\n    ProxyCommand touch /tmp/x", Address: "192.0.2.2"},
		config.Host{Name: "tabs\there", Address: "192.0.2.3"},
		config.Host{Name: "two words", Address: "192.0.2.4"},
		config.Host{Name: "*", Address: "192.0.2.5"},
		config.Host{Name: "hash#x", Address: "192.0.2.6"},
		config.Host{Name: "zw\u200bsp", Address: "192.0.2.7"},
		config.Host{Name: "badaddr", Address: "192.0.2.8 ProxyCommand=x"},
		config.Host{Name: "behind", Address: "10.0.0.9", ProxyJump: "tabs\there"},
		config.Host{Name: "key", Address: "192.0.2.10", IdentityFile: "~/my keys/id"},
		config.Host{Name: "jumpok", Address: "10.0.0.11", ProxyJump: "ok"},
	)
	c := newExportCmd()
	c.SetArgs([]string{})
	var errOut string
	_ = stdout(t, func() {
		errOut = stderrOf(t, func() {
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	})
	b, err := os.ReadFile(filepath.Join(dir, "config", "meterm", "ssh_config"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, line := range strings.Split(got, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch f[0] {
		case "Host", "HostName", "User", "Port", "IdentityFile", "ProxyJump":
		default:
			t.Errorf("a line ssh would read as something else: %q", line)
		}
		if len(f) != 2 && !(f[0] == "IdentityFile" && strings.HasPrefix(f[1], `"`) && strings.HasSuffix(line, `"`)) {
			t.Errorf("a line ssh would split: %q", line)
		}
	}
	for _, want := range []string{"Host ok\n", "Host jumpok\n", "ProxyJump ok\n", "Host hash#x\n", "Host key\n", `IdentityFile "~/my keys/id"`} {
		if !strings.Contains(got, want) {
			t.Errorf("export lacks %q:\n%s", want, got)
		}
	}
	for _, name := range []string{`"nl\n    ProxyCommand touch /tmp/x"`, `"tabs\there"`, `"two words"`, `"*"`, `"zw\u200bsp"`, `"badaddr"`, `"behind"`} {
		if !strings.Contains(errOut, name) {
			t.Errorf("stderr doesn't name %s:\n%s", name, errOut)
		}
	}
}

// JSON spells every character the picker wouldn't show as \u escapes: C1
// controls (U+009B is CSI to a terminal that reads 8-bit controls), zero-width
// and other format characters, not only bidi controls.
func TestJSONEscapesWhatLabelDrops(t *testing.T) {
	var b strings.Builder
	in := map[string]string{"name": "a\u009b2Jb\u200bc\u00add\u202ee\u0085f\u2028g"}
	if err := encodeJSON(&b, in); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, r := range out {
		if r >= 0x7f && r <= 0x9f || r == 0x200b || r == 0xad || r == 0x202e || r == 0x2028 {
			t.Errorf("JSON holds %U raw: %q", r, out)
		}
	}
	for _, esc := range []string{`\u009b`, `\u200b`, `\u00ad`, `\u202e`, `\u0085`} {
		if !strings.Contains(out, esc) {
			t.Errorf("JSON lacks %s: %s", esc, out)
		}
	}
}

// group list shows a hand-edited group name safely, and an env meterm
// doesn't know as production flagged, as list does.
func TestGroupListIsSafeAndFlagsAnUnknownEnv(t *testing.T) {
	cliSandbox(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Groups = map[string]config.Group{"g\x1b[2Jclear": {User: "u\x1b]0;t\x07"}, "pay": {Env: "prodd"}, "dev": {Env: "dev"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := newGroupListCmd()
	out := stdout(t, func() {
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("group list printed control characters: %q", out)
	}
	if !strings.Contains(out, "production (prodd?)") {
		t.Errorf("unknown env not flagged:\n%s", out)
	}
}

// export --format ansible wrote the same hosts as they were: a hidden or
// control character in a name, and a jump with a space put into
// ansible_ssh_common_args, which Ansible splits into ssh's arguments.
// The same hosts are left out, named on stderr.
func TestExportAnsibleLeavesOutTheSameHosts(t *testing.T) {
	cliSandbox(t)
	saveHosts(t,
		config.Host{Name: "ok", Address: "192.0.2.1", User: "deploy"},
		config.Host{Name: "zw\u200bsp", Address: "192.0.2.7"},
		config.Host{Name: "esc\x1b[2Jx", Address: "192.0.2.8"},
		config.Host{Name: "nl\nname", Address: "192.0.2.9"},
		config.Host{Name: "behind", Address: "10.0.0.9", ProxyJump: "zw\u200bsp"},
		config.Host{Name: "rawjump", Address: "10.0.0.10", ProxyJump: "x@192.0.2.30 -oProxyCommand=touch"},
		config.Host{Name: "jumpok", Address: "10.0.0.11", ProxyJump: "ok"},
	)
	c := newExportCmd()
	c.SetArgs([]string{"--format", "ansible"})
	var out, errOut string
	out = stdout(t, func() {
		errOut = stderrOf(t, func() {
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	})
	for _, bad := range []string{"\u200b", "\x1b", "nl\n", "ProxyCommand", "behind", "rawjump"} {
		if strings.Contains(out, bad) {
			t.Errorf("inventory holds %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{"ok:", "jumpok:", "-J deploy@192.0.2.1"} {
		if !strings.Contains(out, good) {
			t.Errorf("inventory lacks %q:\n%s", good, out)
		}
	}
	for _, named := range []string{`"zw\u200bsp"`, `"esc\x1b[2Jx"`, `"nl\nname"`, `"behind"`, `"rawjump"`} {
		if !strings.Contains(errOut, named) {
			t.Errorf("stderr doesn't name %s:\n%s", named, errOut)
		}
	}
}

// export names a host it left out with every invisible character spelled
// out, the same way for each: a Hangul filler (U+3164), which Go's %q
// prints as it is, as much as a line separator (U+2028), which it escapes.
func TestExportSpellsEveryInvisibleInWhatItLeftOut(t *testing.T) {
	cliSandbox(t)
	saveHosts(t,
		config.Host{Name: "web\u31641", Address: "192.0.2.1"},
		config.Host{Name: "web\u20282", Address: "192.0.2.2"},
	)
	var errOut string
	_ = stdout(t, func() { errOut = stderrOf(t, func() { _ = runRoot(t, "export") }) })
	for _, want := range []string{`web\u31641`, `web\u20282`} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr doesn't spell %s:\n%s", want, errOut)
		}
	}
	if strings.ContainsAny(errOut, "\u3164\u2028") {
		t.Errorf("stderr has an invisible character as it is: %q", errOut)
	}
}
