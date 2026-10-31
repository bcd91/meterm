package team

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestMemberProposesOwnerApproves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	if err := alice.Invite(ctx, alice.ID, "bob@example.com", RoleMember); err != nil {
		t.Fatal(err)
	}
	// The team already has one host, reached at 10.0.0.1.
	if _, err := alice.PutHost(ctx, alice.ID, SharedHost{Name: "web1", Address: "10.0.0.1"}, 0); err != nil {
		t.Fatal(err)
	}

	// Bob, a member, shares a new host: it's proposed, not added.
	bCfg := emptyCfg(t)
	bCfg.Upsert(config.Host{Name: "PROD-DB", Address: "203.0.113.66", User: "dba", Team: bob.ID})
	res, err := Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{}, Options{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["PROD-DB"] != Proposed {
		t.Fatalf("bob's share = %+v, want proposed", res.Changes)
	}
	if hosts, _ := alice.ListHosts(ctx, alice.ID); len(hosts) != 1 {
		t.Fatalf("a proposal reached the team: %+v", hosts)
	}
	// Proposing again with no change makes no new commit.
	before := commits(t, alice, ProposalsBranch)
	if _, err := Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{}, Options{Push: true}); err != nil {
		t.Fatal(err)
	}
	if commits(t, alice, ProposalsBranch) != before {
		t.Error("an unchanged proposal was committed again")
	}

	// Alice sees it, with the address warning, and the members' branch stays clean of team files.
	ps, err := alice.Proposals(ctx, alice.ID)
	if err != nil || len(ps) != 1 || ps[0].By != "bob@example.com" || ps[0].Host.User != "dba" {
		t.Fatalf("proposals = %+v, %v", ps, err)
	}
	hosts, _ := alice.ListHosts(ctx, alice.ID)
	rv := ReviewProposal(ps[0], hosts)
	if rv.Invalid != nil || rv.Current != nil || len(rv.Warnings) == 0 || !strings.Contains(rv.Warnings[0], "203.0.113.66") {
		t.Errorf("review = %+v", rv)
	}
	if files := lsTree(t, alice, ProposalsBranch); strings.Contains(files, "meterm-team.yaml") || strings.Contains(files, "hosts/") {
		t.Errorf("proposals branch carries team files: %s", files)
	}

	// Bob can't approve his own proposal.
	if _, err := Approve(ctx, bob, bob, bob.ID, RoleMember, "PROD-DB"); err == nil {
		t.Error("a member approved")
	}
	// Alice approves: it's in the team, and the proposal is gone.
	if _, err := Approve(ctx, alice, alice, alice.ID, RoleOwner, "PROD-DB"); err != nil {
		t.Fatal(err)
	}
	hosts, _ = alice.ListHosts(ctx, alice.ID)
	if len(hosts) != 2 {
		t.Fatalf("after approve: %+v", hosts)
	}
	if ps, _ := alice.Proposals(ctx, alice.ID); len(ps) != 0 {
		t.Errorf("proposal still there: %+v", ps)
	}
	// Bob pulls: his copy now matches the team's.
	res, err = Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{}, Options{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcomes(res)["PROD-DB"]; o != "" && o != Pulled {
		t.Errorf("after approval bob sees %s", o)
	}
}

func TestProposalIsCheckedAgainOnApprove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	// A proposal written by hand (not through meterm) with a hostile address.
	if err := bob.Propose(ctx, bob.ID, Proposal{Host: SharedHost{Name: "evil", Address: "-oProxyCommand=touch /tmp/x"}}); err != nil {
		t.Fatal(err)
	}
	if err := bob.Propose(ctx, bob.ID, Proposal{Host: SharedHost{Name: "ssh-opts", Address: "10.0.0.9", ExtraArgs: []string{"-oProxyCommand=id"}}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"evil", "ssh-opts"} {
		if _, err := Approve(ctx, alice, alice, alice.ID, RoleOwner, n); err == nil {
			t.Errorf("approved %s", n)
		}
	}
	if hosts, _ := alice.ListHosts(ctx, alice.ID); len(hosts) != 0 {
		t.Errorf("hostile proposal reached the team: %+v", hosts)
	}
	if err := alice.DropProposal(ctx, alice.ID, "evil"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := alice.Proposals(ctx, alice.ID); len(ps) != 1 || ps[0].Host.Name != "ssh-opts" {
		t.Errorf("after reject: %+v", ps)
	}
}

func TestMemberRemovalIsProposed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	alice.Invite(ctx, alice.ID, "bob@example.com", RoleMember)
	a, _ := alice.PutHost(ctx, alice.ID, SharedHost{Name: "web1", Address: "10.0.0.1"}, 0)
	b, _ := alice.PutHost(ctx, alice.ID, SharedHost{Name: "web2", Address: "10.0.0.2"}, 0)
	bCfg := emptyCfg(t)
	bCfg.Upsert(SharedHost{Name: "web2", Address: "10.0.0.2"}.ApplyTo(config.Host{}, bob.ID)) // web1 removed here
	res, err := Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{"web1": a, "web2": b}, Options{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes(res)["web1"] != Proposed {
		t.Fatalf("got %+v", res.Changes)
	}
	ps, _ := alice.Proposals(ctx, alice.ID)
	if len(ps) != 1 || !ps[0].Remove || ps[0].Host.Address != "10.0.0.1" {
		t.Fatalf("proposals = %+v", ps)
	}
	if hosts, _ := alice.ListHosts(ctx, alice.ID); len(hosts) != 2 {
		t.Errorf("a proposed removal removed it: %+v", hosts)
	}
	if _, err := Approve(ctx, alice, alice, alice.ID, RoleAdmin, "web1"); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := alice.ListHosts(ctx, alice.ID); len(hosts) != 1 {
		t.Errorf("approved removal didn't remove: %+v", hosts)
	}
}

func commits(t *testing.T, g *GitStore, branch string) string {
	_, _ = g.git(ctx, "fetch", "-q", "origin")
	out, _ := exec.Command("git", "-C", g.Dir, "rev-parse", "origin/"+branch).Output()
	return strings.TrimSpace(string(out))
}

func lsTree(t *testing.T, g *GitStore, branch string) string {
	out, _ := exec.Command("git", "-C", g.Dir, "ls-tree", "-r", "--name-only", "origin/"+branch).Output()
	return string(out)
}

// Proposing leaves the clone on the proposals branch for a moment, where
// there's no team file. Roles must still come from the team's branch: a
// member must never read as admin because of it.
func TestRoleAfterProposingStaysMember(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	alice.Invite(ctx, alice.ID, "bob@example.com", RoleMember)
	if err := bob.Propose(ctx, bob.ID, Proposal{Host: SharedHost{Name: "x", Address: "10.0.0.5"}}); err != nil {
		t.Fatal(err)
	}
	_, _ = bob.git(ctx, "fetch", "-q") // and nothing else: Teams reads locally
	_ = bob.read(ctx)
	if ts, _ := bob.Teams(ctx); len(ts) != 1 || ts[0].Role != RoleMember {
		t.Fatalf("bob after proposing = %+v", ts)
	}
	// Even if something leaves the proposals branch checked out.
	if _, err := bob.git(ctx, "checkout", "-q", "-f", ProposalsBranch); err != nil {
		t.Fatal(err)
	}
	if ts, _ := bob.Teams(ctx); len(ts) != 1 || ts[0].Role != RoleMember {
		t.Fatalf("bob with the proposals branch checked out = %+v", ts)
	}
	if _, err := alice.Proposals(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if ts, _ := alice.Teams(ctx); ts[0].Role != RoleOwner {
		t.Fatalf("alice after listing proposals = %+v", ts)
	}
}
