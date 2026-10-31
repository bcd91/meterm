package team

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
)

var ctx = context.Background()

var (
	alice = Identity{UID: "u-alice", Email: "alice@example.com", EmailVerified: true}
	bob   = Identity{UID: "u-bob", Email: "bob@example.com", EmailVerified: true}
	carol = Identity{UID: "u-carol", Email: "carol@example.com", EmailVerified: true}
)

// teamOf sets up a team owned by alice, with bob invited as role.
func teamOf(t *testing.T, s *MemoryStore, bobRole Role) (string, *memBackend, *memBackend) {
	t.Helper()
	a, b := s.As(alice), s.As(bob)
	tm, err := a.CreateTeam(ctx, "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Invite(ctx, tm.ID, "Bob@Example.com", bobRole); err != nil {
		t.Fatal(err)
	}
	joined, err := b.AcceptInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(joined) != 1 || joined[0].ID != tm.ID || joined[0].Role != bobRole {
		t.Fatalf("bob joined %+v, want %s as %s", joined, tm.ID, bobRole)
	}
	return tm.ID, a, b
}

func emptyCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func outcomes(r Result) map[string]Outcome {
	m := map[string]Outcome{}
	for _, c := range r.Changes {
		m[c.Host] = c.Outcome
	}
	return m
}

func TestShareThenPullOnAnotherMachine(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, b := teamOf(t, s, RoleMember)

	// Alice marks a personal host as shared with the team and syncs.
	aCfg := emptyCfg(t)
	aCfg.Upsert(config.Host{Name: "web1", Address: "10.0.0.1", User: "dev", IdentityFile: "~/.ssh/alice", Team: teamID})
	res, err := Sync(ctx, a, teamID, RoleOwner, aCfg, Base{}, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["web1"] != Pushed {
		t.Fatalf("alice sync = %+v, want web1 pushed", res.Changes)
	}

	// Bob pulls it; his own identity_file is kept, alice's never arrives.
	bCfg := emptyCfg(t)
	bCfg.Upsert(config.Host{Name: "web1", Address: "old", IdentityFile: "~/.ssh/bob", Team: teamID})
	res, err = Sync(ctx, b, teamID, RoleMember, bCfg, Base{"web1": {Name: "web1", Address: "old"}}, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["web1"] != Pulled {
		t.Fatalf("bob sync = %+v, want web1 pulled", res.Changes)
	}
	h, _ := bCfg.Find("web1")
	if h.Address != "10.0.0.1" || h.User != "dev" || h.IdentityFile != "~/.ssh/bob" {
		t.Errorf("bob's web1 = %+v, want alice's address/user with bob's own key", *h)
	}
}

func TestMemberCannotEditHosts(t *testing.T) {
	s := NewMemoryStore()
	teamID, _, b := teamOf(t, s, RoleMember)

	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "db1", Address: "10.0.0.9", Team: teamID})
	res, err := Sync(ctx, b, teamID, RoleMember, cfg, Base{}, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["db1"] != Forbidden {
		t.Errorf("member sync = %+v, want db1 forbidden", res.Changes)
	}
	// And the store refuses even if the client skips its own check.
	if _, err := b.PutHost(ctx, teamID, SharedHost{Name: "db1"}, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("PutHost as member = %v, want ErrForbidden", err)
	}
}

func TestBothSidesChangedIsAConflictAndKeepsLocal(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleAdmin)
	stored, err := a.PutHost(ctx, teamID, SharedHost{Name: "web1", Address: "10.0.0.1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	base := Base{"web1": stored}

	// Someone changes it on the server…
	if _, err := a.PutHost(ctx, teamID, SharedHost{Name: "web1", Address: "10.0.0.2"}, stored.Rev); err != nil {
		t.Fatal(err)
	}
	// …while bob edited his copy differently.
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "web1", Address: "10.0.0.3", Team: teamID})

	res, err := Sync(ctx, s.As(bob), teamID, RoleAdmin, cfg, base, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["web1"] != Conflict {
		t.Fatalf("sync = %+v, want a conflict", res.Changes)
	}
	if h, _ := cfg.Find("web1"); h.Address != "10.0.0.3" {
		t.Errorf("local copy was overwritten during a conflict: %s", h.Address)
	}
	if res.Base["web1"].Address != "10.0.0.1" {
		t.Errorf("base moved past an unresolved conflict; it would be forgotten next sync")
	}
}

func TestRemovalsTravelBothWays(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	old, _ := a.PutHost(ctx, teamID, SharedHost{Name: "old", Address: "1"}, 0)
	gone, _ := a.PutHost(ctx, teamID, SharedHost{Name: "gone", Address: "2"}, 0)

	// Remote removes "gone"; alice locally removes "old".
	if err := a.DeleteHost(ctx, teamID, "gone", gone.Rev); err != nil {
		t.Fatal(err)
	}
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "gone", Address: "2", Team: teamID})
	base := Base{"old": old, "gone": gone}

	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, base, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["gone"] != PulledRemoval || got["old"] != PushedRemoval {
		t.Fatalf("sync = %+v, want gone removed locally and old removed remotely", res.Changes)
	}
	if _, i := cfg.Find("gone"); i >= 0 {
		t.Error("gone is still in the local config")
	}
	if hosts, _ := a.ListHosts(ctx, teamID); len(hosts) != 0 {
		t.Errorf("remote still has %+v", hosts)
	}
}

func TestPersonalHostNameIsNotOverwritten(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	a.PutHost(ctx, teamID, SharedHost{Name: "web1", Address: "team-address"}, 0)

	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "web1", Address: "my-own"}) // personal: no Team
	res, err := Sync(ctx, s.As(bob), teamID, RoleMember, cfg, Base{}, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["web1"] != NameTaken {
		t.Fatalf("sync = %+v, want name-taken", res.Changes)
	}
	if h, _ := cfg.Find("web1"); h.Address != "my-own" || h.Team != "" {
		t.Errorf("personal host was replaced: %+v", *h)
	}
}

func TestTeamsAreIsolated(t *testing.T) {
	s := NewMemoryStore()
	teamID, _, _ := teamOf(t, s, RoleMember)
	c := s.As(carol)
	other, _ := c.CreateTeam(ctx, "Client X")

	if _, err := c.ListHosts(ctx, teamID); !errors.Is(err, ErrForbidden) {
		t.Errorf("carol reading ACME's hosts = %v, want ErrForbidden", err)
	}
	if err := c.AppendEvents(ctx, teamID, []activity.Event{{Device: "d", Seq: 1}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("carol writing ACME's log = %v, want ErrForbidden", err)
	}
	teams, _ := s.As(alice).Teams(ctx)
	for _, tm := range teams {
		if tm.ID == other.ID {
			t.Error("alice can see a team she was never invited to")
		}
	}
}

func TestOnlyOwnerCanInviteAnOwner(t *testing.T) {
	s := NewMemoryStore()
	teamID, _, b := teamOf(t, s, RoleAdmin)
	if err := b.Invite(ctx, teamID, carol.Email, RoleOwner); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin inviting an owner = %v, want ErrForbidden", err)
	}
	if err := b.Invite(ctx, teamID, carol.Email, RoleMember); err != nil {
		t.Errorf("admin inviting a member = %v, want ok", err)
	}
}

func TestEventsAreIdempotentAndImmutable(t *testing.T) {
	s := NewMemoryStore()
	teamID, _, b := teamOf(t, s, RoleMember)

	l, err := activity.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Append(activity.Event{Action: "connect", Host: "web1"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second send is a retry
		if err := b.AppendEvents(ctx, teamID, []activity.Event{e}); err != nil {
			t.Fatalf("append #%d: %v", i+1, err)
		}
	}
	got, _ := b.ListEvents(ctx, teamID, EventFilter{})
	if len(got) != 1 || got[0].UploaderEmail != bob.Email {
		t.Fatalf("events = %+v, want one, attributed to bob by the backend", got)
	}

	forged := e
	forged.Host = "somewhere-else"
	if err := b.AppendEvents(ctx, teamID, []activity.Event{forged}); !errors.Is(err, ErrForbidden) {
		t.Errorf("rewriting a stored event = %v, want ErrForbidden", err)
	}
}

func TestUploadOnlySendsThatTeamsHosts(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)

	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "shared", Address: "1", Team: teamID})
	cfg.Upsert(config.Host{Name: "personal", Address: "2"})
	cfg.Upsert(config.Host{Name: "other-team", Address: "3", Team: "some-other-team"})

	l, err := activity.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"shared", "personal", "other-team", "shared"} {
		if _, err := l.Append(activity.Event{Action: "connect", Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := UploadActivity(ctx, a, teamID, cfg, l)
	if err != nil || n != 2 {
		t.Fatalf("uploaded %d, %v; want 2", n, err)
	}
	got, _ := a.ListEvents(ctx, teamID, EventFilter{})
	for _, e := range got {
		if e.Host != "shared" {
			t.Errorf("team received an event on %q", e.Host)
		}
	}
	if broken := activity.VerifyShared(eventsOf(got)); broken != nil {
		t.Errorf("shared subset should verify: %v", broken)
	}
	if n, _ := UploadActivity(ctx, a, teamID, cfg, l); n != 0 {
		t.Errorf("second upload sent %d again", n)
	}
}

func eventsOf(te []TeamEvent) []activity.Event {
	out := make([]activity.Event, len(te))
	for i, e := range te {
		out[i] = e.Event
	}
	return out
}

func TestPullOnlyLeavesLocalChangesPending(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	web, _ := a.PutHost(ctx, teamID, SharedHost{Name: "web1", Address: "1"}, 0)
	db, _ := a.PutHost(ctx, teamID, SharedHost{Name: "db1", Address: "2"}, 0)
	base := Base{"web1": web, "db1": db}

	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "web1", Address: "1-edited", Team: teamID}) // edited here
	// db1 removed here (not in cfg); new1 added here
	cfg.Upsert(config.Host{Name: "new1", Address: "3", Team: teamID})

	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, base, Options{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["web1"] != Pending || got["new1"] != Pending || got["db1"] != PendingRemoval {
		t.Fatalf("pull-only = %+v, want everything local reported pending", res.Changes)
	}
	if hosts, _ := a.ListHosts(ctx, teamID); len(hosts) != 2 || hosts[1].Address != "1" {
		t.Errorf("pull-only wrote to the team: %+v", hosts)
	}
	// Still pending next time: the base didn't move past the local changes.
	res2, _ := Sync(ctx, a, teamID, RoleOwner, cfg, res.Base, Options{Pull: true})
	if outcomes(res2)["db1"] != PendingRemoval || outcomes(res2)["web1"] != Pending {
		t.Errorf("second pull forgot pending changes: %+v", res2.Changes)
	}
}

func TestPushOnlySelectedHosts(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "a1", Address: "1", Team: teamID})
	cfg.Upsert(config.Host{Name: "b1", Address: "2", Team: teamID})

	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, Base{}, Options{Push: true, Only: map[string]bool{"a1": true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res); got["a1"] != Pushed || got["b1"] != Pending {
		t.Fatalf("got %+v, want a1 pushed and b1 pending", res.Changes)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	a.PutHost(ctx, teamID, SharedHost{Name: "remote1", Address: "r"}, 0)
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "local1", Address: "l", Team: teamID})

	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, Base{}, Options{Pull: true, Push: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(res); got["remote1"] != Pulled || got["local1"] != Pushed {
		t.Fatalf("plan = %+v", res.Changes)
	}
	if _, i := cfg.Find("remote1"); i >= 0 {
		t.Error("dry run changed the local config")
	}
	if hosts, _ := a.ListHosts(ctx, teamID); len(hosts) != 1 {
		t.Errorf("dry run wrote to the team: %+v", hosts)
	}
}

func TestIgnoredHostsAreSkipped(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	a.PutHost(ctx, teamID, SharedHost{Name: "noisy", Address: "n"}, 0)
	cfg := emptyCfg(t)
	res, err := Sync(ctx, s.As(bob), teamID, RoleMember, cfg, Base{}, Options{Pull: true, Ignored: map[string]bool{"noisy": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(cfg.Hosts) != 0 {
		t.Errorf("ignored host came through: %+v", res.Changes)
	}
}

func TestResolveTheirsAndMine(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	a.PutHost(ctx, teamID, SharedHost{Name: "WEB", Address: "team"}, 0)

	// name-taken, take the team's: keeps the personal key.
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "WEB", Address: "mine", IdentityFile: "~/.ssh/k"})
	base, err := Resolve(ctx, s.As(bob), teamID, RoleMember, cfg, Base{}, "WEB", true)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("WEB")
	if h.Address != "team" || h.Team != teamID || h.IdentityFile != "~/.ssh/k" {
		t.Errorf("theirs = %+v", *h)
	}
	if res, _ := Sync(ctx, s.As(bob), teamID, RoleMember, cfg, base, Options{Pull: true}); len(res.Changes) != 0 {
		t.Errorf("after resolving, sync still reports %+v", res.Changes)
	}

	// conflict, keep mine (owner): pushed over the team's.
	cfg2 := emptyCfg(t)
	cfg2.Upsert(config.Host{Name: "WEB", Address: "owner-version"})
	if _, err := Resolve(ctx, a, teamID, RoleOwner, cfg2, Base{}, "WEB", false); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := a.ListHosts(ctx, teamID); hosts[0].Address != "owner-version" {
		t.Errorf("mine wasn't pushed: %+v", hosts)
	}
	// members can't keep theirs over the team's.
	if _, err := Resolve(ctx, s.As(bob), teamID, RoleMember, cfg, base, "WEB", false); err == nil {
		t.Error("member 'mine' should be refused")
	}
}

func TestAuditsAreSharedAndAttributed(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, b := teamOf(t, s, RoleMember)
	if err := b.PutAudit(ctx, teamID, AuditRecord{Host: "WEB", Fails: 2, By: "spoofed@example.com"}); err != nil {
		t.Fatalf("member PutAudit: %v", err)
	}
	recs, err := a.ListAudits(ctx, teamID)
	if err != nil || len(recs) != 1 || recs[0].By != bob.Email || recs[0].Fails != 2 {
		t.Fatalf("audits = %+v, %v", recs, err)
	}
	if err := s.As(carol).PutAudit(ctx, teamID, AuditRecord{Host: "WEB"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("outsider PutAudit = %v, want ErrForbidden", err)
	}
}

func TestRemoveMember(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, b := teamOf(t, s, RoleAdmin)
	if err := b.RemoveMember(ctx, teamID, alice.UID); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin removing the owner = %v, want ErrForbidden", err)
	}
	if err := a.RemoveMember(ctx, teamID, alice.UID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("last owner leaving = %v, want ErrLastOwner", err)
	}
	if err := a.RemoveMember(ctx, teamID, bob.UID); err != nil {
		t.Fatalf("owner removing bob = %v", err)
	}
	if _, err := b.ListHosts(ctx, teamID); !errors.Is(err, ErrForbidden) {
		t.Errorf("removed member ListHosts = %v, want ErrForbidden", err)
	}
	if teams, _ := b.Teams(ctx); len(teams) != 0 {
		t.Errorf("removed member still sees %+v", teams)
	}
}

// A teammate (or whoever controls the team store) can't push a host that
// makes ssh run a command on everyone else's machine.
func TestUnsafeTeamHostsAreNeitherPulledNorPushed(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, b := teamOf(t, s, RoleAdmin)
	// Written straight to the store, as an older client or a forged write would.
	if _, err := a.PutHost(ctx, teamID, SharedHost{Name: "evil", Address: "10.0.0.9", ExtraArgs: []string{"-oProxyCommand=touch /tmp/pwned"}}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PutHost(ctx, teamID, SharedHost{Name: "ok", Address: "10.0.0.10"}, 0); err != nil {
		t.Fatal(err)
	}
	bCfg := emptyCfg(t)
	res, err := Sync(ctx, b, teamID, RoleAdmin, bCfg, Base{}, Options{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, i := bCfg.Find("evil"); i >= 0 {
		t.Error("the unsafe host was applied")
	}
	if _, i := bCfg.Find("ok"); i < 0 {
		t.Error("the safe host wasn't applied")
	}
	found := false
	for _, c := range res.Changes {
		if c.Host == "evil" && c.Outcome == Rejected && c.Outcome.NeedsAttention() {
			found = true
		}
	}
	if !found {
		t.Errorf("no rejection reported: %+v", res.Changes)
	}
	if _, ok := res.Base["evil"]; ok {
		t.Error("rejected host went into the base")
	}

	// Pushing one is refused too.
	bCfg.Upsert(config.Host{Name: "evil2", Address: "-oProxyCommand=x", Team: teamID})
	res, err = Sync(ctx, b, teamID, RoleAdmin, bCfg, res.Base, Options{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if hs, _ := b.ListHosts(ctx, teamID); len(hs) != 2 {
		t.Errorf("store has %d hosts after an unsafe push", len(hs))
	}
}

// A machine with none of a team's hosts but a base listing them all (what a
// new member's `team git add` left behind before v0.16.1) must pull them,
// not read every host as removed here: a push would have wiped the team.
func TestNoLocalHostsWithBasePullsInsteadOfRemoving(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	web, _ := a.PutHost(ctx, teamID, SharedHost{Name: "web1", Address: "1"}, 0)
	db, _ := a.PutHost(ctx, teamID, SharedHost{Name: "db1", Address: "2"}, 0)
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "mine", Address: "9"})

	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, Base{"web1": web, "db1": db}, Options{Pull: true, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["web1"] != Pulled || got["db1"] != Pulled {
		t.Fatalf("got %+v, want both pulled", res.Changes)
	}
	if hosts, _ := a.ListHosts(ctx, teamID); len(hosts) != 2 {
		t.Errorf("the team lost hosts: %+v", hosts)
	}
	if _, i := cfg.Find("web1"); i < 0 {
		t.Errorf("web1 not pulled")
	}
}

// A member who never had the cloud team's hosts here gets no base for them.
func TestAdoptWithoutLocalHostsLeavesBaseEmpty(t *testing.T) {
	cfg := emptyCfg(t)
	st := State{}
	Adopt(cfg, &st, "cloud1", "git:repo", []SharedHost{{Name: "web1", Address: "1"}})
	if len(st.Bases["git:repo"]) != 0 {
		t.Errorf("base = %+v, want empty", st.Bases["git:repo"])
	}
}
