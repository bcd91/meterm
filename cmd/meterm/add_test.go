package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestEditWizardClearsWithDash(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "API", Address: "10.0.0.1", User: "deploy", Port: 2222,
			Tags: []string{"acme"}, ProxyJump: "DEVBOX-1", Notes: "old note"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("API")
	// name, address, user, users, port, identity, group, tags, jump, notes
	in := strings.NewReader("\n\n\nroot, deploy\n-\n\n\n\n-\n-\n")
	if err := addWizard(cfg, h, in); err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Find("API")
	if got.ProxyJump != "" || got.Port != 0 || got.Notes != "" {
		t.Errorf("not cleared: jump %q port %d notes %q", got.ProxyJump, got.Port, got.Notes)
	}
	if strings.Join(got.Users, ",") != "root,deploy" {
		t.Errorf("users = %q", got.Users)
	}
	if got.User != "deploy" || got.Address != "10.0.0.1" || len(got.Tags) != 1 {
		t.Errorf("blank answers changed fields: %+v", *got)
	}
	reloaded, _ := config.Load(cfg.Path())
	if r, _ := reloaded.Find("API"); r == nil || r.ProxyJump != "" {
		t.Errorf("clear not saved: %+v", r)
	}
}

func TestEnvFlagAndWizard(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h := &config.Host{Name: "a", Address: "192.0.2.1"}
	set := func(env string) error {
		return applyFlags(h, hostFlags{env: env}, changedOnly{"env"})
	}
	if err := set("Production"); err != nil || h.Env != config.EnvProduction {
		t.Fatalf("env %q, err %v", h.Env, err)
	}
	if err := set("live"); err == nil || !strings.Contains(err.Error(), config.EnvList) {
		t.Errorf("bad value: err = %v", err)
	}
	if h.Env != config.EnvProduction {
		t.Errorf("a refused value changed env to %q", h.Env)
	}
	if err := set(""); err != nil || h.Env != "" {
		t.Errorf("clearing: env %q, err %v", h.Env, err)
	}

	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(*h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ := cfg.Find("a")
	// name, address, user, users, port, identity, group, tags, jump, notes, env
	if err := addWizard(cfg, stored, strings.NewReader("\n\n\n\n\n\n\n\n\n\nstaging\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := cfg.Find("a"); got.Env != config.EnvStaging {
		t.Errorf("wizard env = %q", got.Env)
	}
	stored, _ = cfg.Find("a")
	if err := addWizard(cfg, stored, strings.NewReader("\n\n\n\n\n\n\n\n\n\nnope\n")); err == nil {
		t.Error("wizard took a bad env")
	}
	if got, _ := cfg.Find("a"); got.Env != config.EnvStaging {
		t.Errorf("a refused wizard run changed env to %q", got.Env)
	}
}

// changedOnly is the flag set applyFlags sees when only these flags were given.
type changedOnly []string

func (c changedOnly) Changed(name string) bool { return slices.Contains(c, name) }

// addWizard's prompts read in here, so something else can change the file while
// they wait.
type answerAfter struct {
	before  func()
	answers *strings.Reader
}

func (a *answerAfter) Read(p []byte) (int, error) {
	if a.before != nil {
		a.before()
		a.before = nil
	}
	return a.answers.Read(p)
}

func TestEditWizardKeepsWhatChangedMeanwhile(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web", Address: "10.0.0.1", Notes: "old"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("web")
	in := &answerAfter{
		before: func() {
			other, _ := config.Load(cfg.Path())
			if err := other.Update(func(c *config.Config) error {
				w, _ := c.Find("web")
				w.Routes = []config.Route{{Name: "alt", Address: "10.0.0.2"}}
				w.ExtraArgs = []string{"-C"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		// name, address, user, users, port, identity, group, tags, jump, notes, env
		answers: strings.NewReader("\n\n\n\n\n\n\n\n\nnew\n\n"),
	}
	if err := addWizard(cfg, h, in); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(cfg.Path())
	got, _ := saved.Find("web")
	if len(got.Routes) != 1 || len(got.ExtraArgs) != 1 {
		t.Errorf("lost what changed meanwhile: %+v", *got)
	}
	if got.Notes != "new" {
		t.Errorf("answer not applied: %+v", *got)
	}
}

func TestEditWizardDoesNotBringBackARemovedHost(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web", Address: "10.0.0.1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("web")
	in := &answerAfter{
		before: func() {
			other, _ := config.Load(cfg.Path())
			if err := other.Update(func(c *config.Config) error {
				c.Remove("web")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		answers: strings.NewReader("\n\n\n\n\n\n\n\n\n\n\n"),
	}
	if err := addWizard(cfg, h, in); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("got %v", err)
	}
	saved, _ := config.Load(cfg.Path())
	if _, i := saved.Find("web"); i >= 0 {
		t.Error("the removed host came back")
	}
}
