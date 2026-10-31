package team

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// gitTeam makes a bare repository and two members' clones of it.
func gitTeam(t *testing.T) (alice, bob *GitStore) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "team.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	open := func(who, email string) *GitStore {
		g, err := OpenGitStore(ctx, filepath.Join(root, who), GitTeam{Name: "ACME", Remote: remote})
		if err != nil {
			t.Fatal(err)
		}
		g.identityFn = func() (Identity, error) { return Identity{UID: email, Email: email, EmailVerified: true}, nil }
		return g
	}
	alice = open("alice", "alice@example.com")
	if _, err := InitGitTeam(ctx, filepath.Join(root, "alice"), GitTeam{Name: "ACME", Remote: remote}); err == nil {
		t.Fatal("init without a git identity should fail in a fresh HOME") // InitGitTeam builds its own store
	}
	// Init through alice's store so her test identity is used.
	if err := alice.change(ctx, "init", func() error {
		return alice.writeYAML("meterm-team.yaml", gitTeamFile{Name: "ACME", Members: []gitTeamMember{{Email: "alice@example.com", Role: RoleOwner}}})
	}); err != nil {
		t.Fatal(err)
	}
	bob = open("bob", "bob@example.com")
	return alice, bob
}

func TestGitStoreSyncBetweenMembers(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no global git identity
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	if err := alice.Invite(ctx, alice.ID, "Bob@Example.com", RoleMember); err != nil {
		t.Fatal(err)
	}
	if teams, _ := bob.Teams(ctx); len(teams) != 1 || teams[0].Role != RoleMember {
		t.Fatalf("bob's teams before refresh = %+v", teams)
	}

	// Alice shares a host through the normal Sync; bob pulls it.
	aCfg := emptyCfg(t)
	aCfg.Upsert(config.Host{Name: "web1", Address: "10.0.0.1", User: "dev", Team: alice.ID,
		Forwards: []config.Forward{{Name: "pg", Type: "L", LocalPort: 5432, RemoteHost: "localhost", RemotePort: 5432}}})
	if res, err := Sync(ctx, alice, alice.ID, RoleOwner, aCfg, Base{}, Options{Push: true, Pull: true}); err != nil || outcomes(res)["web1"] != Pushed {
		t.Fatalf("alice sync = %+v, %v", res.Changes, err)
	}
	bCfg := emptyCfg(t)
	res, err := Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{}, Options{Pull: true})
	if err != nil || outcomes(res)["web1"] != Pulled {
		t.Fatalf("bob sync = %+v, %v", res.Changes, err)
	}
	if h, _ := bCfg.Find("web1"); h.Address != "10.0.0.1" || len(h.Forwards) != 1 || h.Forwards[0].Name != "pg" {
		t.Errorf("bob's web1 = %+v", *h)
	}
	// Members can't edit hosts (advisory in Git, but meterm keeps to it).
	if _, err := bob.PutHost(ctx, bob.ID, SharedHost{Name: "web1", Address: "evil"}, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("member PutHost = %v, want ErrForbidden", err)
	}
	// Optimistic concurrency, as in the cloud backend.
	if _, err := alice.PutHost(ctx, alice.ID, SharedHost{Name: "web1", Address: "10.0.0.9"}, 0); !errors.Is(err, ErrConflict) {
		t.Errorf("stale PutHost = %v, want ErrConflict", err)
	}

	// Two devices log activity at once: separate files, no push conflict.
	done := make(chan error, 2)
	for _, s := range []*GitStore{alice, bob} {
		go func(s *GitStore, dev string) {
			done <- s.AppendEvents(ctx, s.ID, []activity.Event{{Device: dev, Seq: 1, Time: time.Now().UTC(), Action: "connect", Host: "web1"}})
		}(s, strings.Split(s.Dir, string(filepath.Separator))[len(strings.Split(s.Dir, string(filepath.Separator)))-3])
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	ev, err := alice.ListEvents(ctx, alice.ID, EventFilter{Host: "web1"})
	if err != nil || len(ev) != 2 {
		t.Fatalf("events = %+v, %v", ev, err)
	}
	// A retry is a no-op; a rewrite is refused.
	e := ev[0].Event
	if err := alice.AppendEvents(ctx, alice.ID, []activity.Event{e}); err != nil {
		t.Errorf("retry = %v", err)
	}
	e.Detail = "rewritten"
	if err := alice.AppendEvents(ctx, alice.ID, []activity.Event{e}); !errors.Is(err, ErrForbidden) {
		t.Errorf("rewrite = %v, want ErrForbidden", err)
	}

	// Audits and removal.
	if err := bob.PutAudit(ctx, bob.ID, AuditRecord{Host: "web1", Time: time.Now(), Fails: 2}); err != nil {
		t.Fatal(err)
	}
	if as, _ := alice.ListAudits(ctx, alice.ID); len(as) != 1 || as[0].By != "bob@example.com" {
		t.Errorf("audits = %+v", as)
	}
	if err := bob.RemoveMember(ctx, bob.ID, "alice@example.com"); !errors.Is(err, ErrForbidden) {
		t.Errorf("member removing owner = %v", err)
	}
	if err := alice.RemoveMember(ctx, alice.ID, "alice@example.com"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("last owner leaving = %v", err)
	}
	if err := alice.RemoveMember(ctx, alice.ID, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := alice.Members(ctx, alice.ID); len(ms) != 1 {
		t.Errorf("members after removal = %+v", ms)
	}
	// Every change is a commit by its author.
	log, _ := alice.git(ctx, "log", "--format=%ae %s")
	if !strings.Contains(log, "bob@example.com meterm: audit web1") || !strings.Contains(log, "alice@example.com meterm: update host web1") {
		t.Errorf("git log:\n%s", log)
	}
}
