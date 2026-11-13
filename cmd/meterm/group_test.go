package main

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

func groupCmd(t *testing.T, stdin string, args ...string) (o, e string, err error) {
	t.Helper()
	withStdin(t, stdin, func() {
		o = stdout(t, func() {
			e = stderrOf(t, func() {
				c := newGroupCmd()
				c.SetArgs(args)
				err = c.Execute()
			})
		})
	})
	return
}

func saveGroup(t *testing.T, name string, g config.Group) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		if c.Groups == nil {
			c.Groups = map[string]config.Group{}
		}
		c.Groups[name] = g
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func groupExists(t *testing.T, name string) bool {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := cfg.Groups[name]
	return ok
}

// Removing a group that gives hosts their production env takes production
// away from them: it says which hosts and what changes, asks (y only), and
// logs it on each host. Not on a terminal it refuses without --yes.
func TestGroupRmAsksBeforeTakingProductionAway(t *testing.T) {
	cliSandbox(t)
	saveGroup(t, "pay", config.Group{Env: config.EnvProduction})
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.60", Group: "pay"},
		config.Host{Name: "pay2", Address: "192.0.2.61", Group: "pay", Env: config.EnvStaging},
		config.Host{Name: "web1", Address: "192.0.2.62"})

	asTerminal(t, true)
	o, _, err := groupCmd(t, "n\n", "rm", "pay")
	if err != nil || !strings.Contains(o, "pay1") || !strings.Contains(o, "production → none") || strings.Contains(o, "pay2:") || !groupExists(t, "pay") {
		t.Errorf("answered n: %q %v, group still there: %v", o, err, groupExists(t, "pay"))
	}
	asTerminal(t, false)
	if _, _, err := groupCmd(t, "", "rm", "pay"); err == nil || !strings.Contains(err.Error(), "--yes") || !groupExists(t, "pay") {
		t.Errorf("not a terminal, no --yes: %v", err)
	}
	if ev := events(t); len(ev) != 0 {
		t.Errorf("nothing removed, yet logged: %q", ev)
	}
	asTerminal(t, true)
	if _, _, err := groupCmd(t, "y\n", "rm", "pay"); err != nil || groupExists(t, "pay") {
		t.Fatalf("answered y: %v", err)
	}
	if ev := events(t); !has(ev, "group pay1:", "production → none") || has(ev, "pay2") || has(ev, "web1") {
		t.Errorf("events: %q", ev)
	}

	// A group no host takes its env from goes without a question.
	saveGroup(t, "empty", config.Group{Env: config.EnvProduction})
	if _, _, err := groupCmd(t, "", "rm", "empty"); err != nil || groupExists(t, "empty") {
		t.Errorf("empty group: %v", err)
	}
}

// Changing a group's env changes its hosts' env: each host's log says so.
func TestGroupEnvChangeIsLogged(t *testing.T) {
	cliSandbox(t)
	saveGroup(t, "pay", config.Group{Env: config.EnvProduction})
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.60", Group: "pay"},
		config.Host{Name: "pay2", Address: "192.0.2.61", Group: "pay", Env: config.EnvProduction})
	if _, _, err := groupCmd(t, "", "add", "pay", "--env", "staging"); err != nil {
		t.Fatal(err)
	}
	if ev := events(t); !has(ev, "group pay1:", "production → staging") || has(ev, "pay2") {
		t.Errorf("events: %q", ev)
	}
	if _, _, err := groupCmd(t, "", "add", "pay", "--user", "deploy"); err != nil {
		t.Fatal(err)
	}
	if ev := events(t); len(ev) != 1 {
		t.Errorf("an env that didn't change was logged: %q", ev)
	}
}

// A host whose group is gone says so in list.
func TestListFlagsAMissingGroup(t *testing.T) {
	cliSandbox(t)
	saveGroup(t, "web", config.Group{})
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.60", Group: "pay"},
		config.Host{Name: "web1", Address: "192.0.2.61", Group: "web"})
	o := stdout(t, func() {
		c := newListCmd()
		c.SetArgs(nil)
		_ = c.Execute()
	})
	if !strings.Contains(o, "pay (no such group)") || strings.Contains(o, "web (no such group)") {
		t.Errorf("list:\n%s", o)
	}
	o = stdout(t, func() {
		c := newListCmd()
		c.SetArgs([]string{"--format", "json"})
		_ = c.Execute()
	})
	if !strings.Contains(o, `"group_missing": true`) {
		t.Errorf("json: %s", o)
	}
}
