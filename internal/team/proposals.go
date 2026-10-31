package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"meterm/internal/config"
)

// Proposals: a member can't change the team's hosts, but can propose a
// change for an owner or admin to approve. A proposal lives on its own
// branch of the team repository (ProposalsBranch), so nobody pulls it —
// nobody sees or connects to the host — until it's approved onto the
// team's branch. Protecting the team's branch on the Git host, so members
// can push only the proposals branch, is what makes that binding rather
// than a convention.

// ProposalsBranch holds proposals, one file per host: proposals/<name>.yaml.
const ProposalsBranch = "meterm-proposals"

// Proposed is the outcome for a member's change sent for approval.
const Proposed Outcome = "proposed"

// Proposal is one proposed change to a team host.
type Proposal struct {
	Host   SharedHost `yaml:"host"`
	Remove bool       `yaml:"remove,omitempty"`
	// BaseRev is the team's revision of the host the change was made
	// against (0: a new host), so an approver can tell when the host has
	// changed since.
	BaseRev int64     `yaml:"base_rev,omitempty"`
	By      string    `yaml:"by"` // the proposer's git email: what they say, not proof
	At      time.Time `yaml:"at"`
}

// Proposer is a Backend that takes proposals.
type Proposer interface {
	Propose(ctx context.Context, teamID string, p Proposal) error
	Proposals(ctx context.Context, teamID string) ([]Proposal, error)
	DropProposal(ctx context.Context, teamID, name string) error
}

// ErrNoProposals means the backend doesn't take proposals.
var ErrNoProposals = errors.New("this team's store doesn't take proposals")

func proposalPath(name string) string { return "proposals/" + fileName(name) + ".yaml" }

func (g *GitStore) Propose(ctx context.Context, teamID string, p Proposal) error {
	id, err := g.identityFn()
	if err != nil {
		return err
	}
	p.By, p.At = id.Email, time.Now().UTC().Truncate(time.Second)
	p.Host.Rev, p.Host.UpdatedBy, p.Host.UpdatedAt = 0, "", time.Time{}
	return g.changeOn(ctx, ProposalsBranch, "meterm: propose "+p.Host.Name, func() error {
		var old Proposal
		if found, err := g.readYAML(proposalPath(p.Host.Name), &old); err != nil {
			return err
		} else if found && old.Remove == p.Remove && old.BaseRev == p.BaseRev && old.Host.SameContent(p.Host) {
			return errNoChange // already proposed as is
		}
		return g.writeYAML(proposalPath(p.Host.Name), p)
	})
}

// Proposals lists what's waiting for approval, oldest first. Each is as its
// proposer wrote it: untrusted until approved, and checked again then.
func (g *GitStore) Proposals(ctx context.Context, teamID string) ([]Proposal, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.git(ctx, "fetch", "-q", "origin"); err != nil {
		return nil, err
	}
	if _, err := g.git(ctx, "rev-parse", "-q", "--verify", "origin/"+ProposalsBranch); err != nil {
		return nil, nil // nobody has proposed anything yet
	}
	if err := g.refreshBranch(ctx, ProposalsBranch); err != nil {
		return nil, err
	}
	defer func() { _ = g.onTeamBranch(ctx) }()
	files, _ := filepath.Glob(filepath.Join(g.Dir, "proposals", "*.yaml"))
	var out []Proposal
	for _, f := range files {
		var p Proposal
		rel, _ := filepath.Rel(g.Dir, f)
		if found, err := g.readYAML(filepath.ToSlash(rel), &p); err != nil || !found {
			fmt.Fprintf(os.Stderr, "meterm: skipping unreadable proposal %s\n", filepath.Base(f))
			continue
		}
		if p.Host.Name == "" || proposalPath(p.Host.Name) != filepath.ToSlash(rel) {
			fmt.Fprintf(os.Stderr, "meterm: skipping proposal %s: its file doesn't match the host it names\n", filepath.Base(f))
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (g *GitStore) DropProposal(ctx context.Context, teamID, name string) error {
	return g.changeOn(ctx, ProposalsBranch, "meterm: settle proposal "+name, func() error {
		err := os.Remove(g.path(proposalPath(name)))
		if errors.Is(err, os.ErrNotExist) {
			return errNoChange
		}
		return err
	})
}

func (r *Router) proposer(teamID string) (Proposer, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return nil, err
	}
	p, ok := b.(Proposer)
	if !ok {
		return nil, ErrNoProposals
	}
	return p, nil
}

func (r *Router) Propose(ctx context.Context, teamID string, p Proposal) error {
	pr, err := r.proposer(teamID)
	if err != nil {
		return err
	}
	return pr.Propose(ctx, teamID, p)
}

func (r *Router) Proposals(ctx context.Context, teamID string) ([]Proposal, error) {
	pr, err := r.proposer(teamID)
	if err != nil {
		return nil, err
	}
	return pr.Proposals(ctx, teamID)
}

func (r *Router) DropProposal(ctx context.Context, teamID, name string) error {
	pr, err := r.proposer(teamID)
	if err != nil {
		return err
	}
	return pr.DropProposal(ctx, teamID, name)
}

// Review is what an approver should know about a proposal before saying yes.
type Review struct {
	Proposal Proposal
	Current  *SharedHost // the team's copy now; nil for a new host
	Diff     []string    // field by field, from Current
	// Warnings: the things a hostile proposal would rely on going unread —
	// an address no other team host uses, a changed address or jump path,
	// a host that changed since it was proposed.
	Warnings []string
	Invalid  error // fails the rules every team host must pass; can't be approved
}

// ReviewProposal compares p with the team's hosts.
func ReviewProposal(p Proposal, team []SharedHost) Review {
	rv := Review{Proposal: p}
	known := map[string]bool{}
	for i := range team {
		h := team[i]
		if h.Name == p.Host.Name {
			rv.Current = &team[i]
			continue
		}
		known[strings.ToLower(h.Address)] = true
		for _, rt := range h.Routes {
			if rt.Address != "" {
				known[strings.ToLower(rt.Address)] = true
			}
		}
	}
	if p.Remove {
		if rv.Current == nil {
			rv.Invalid = fmt.Errorf("%s isn't in the team", p.Host.Name)
		}
		return rv
	}
	if err := p.Host.ApplyTo(config.Host{}, "proposal").CheckShared(); err != nil {
		rv.Invalid = err
		return rv
	}
	addrs := []string{p.Host.Address}
	for _, rt := range p.Host.Routes {
		if rt.Address != "" {
			addrs = append(addrs, rt.Address)
		}
	}
	for _, a := range addrs {
		if !known[strings.ToLower(a)] && (rv.Current == nil || !sameAddrIn(*rv.Current, a)) {
			rv.Warnings = append(rv.Warnings, fmt.Sprintf("%s isn't used by any other team host — check it's really the machine it claims to be", a))
		}
	}
	if rv.Current != nil {
		rv.Diff = Diff(*rv.Current, p.Host)
		if rv.Current.Address != p.Host.Address || rv.Current.ProxyJump != p.Host.ProxyJump || describeRoutes(rv.Current.Routes) != describeRoutes(p.Host.Routes) {
			rv.Warnings = append(rv.Warnings, "changes where everyone connects to when they open "+p.Host.Name)
		}
		if rv.Current.Rev != p.BaseRev {
			rv.Warnings = append(rv.Warnings, "the team's copy changed after this was proposed; the diff is against the copy now")
		}
	}
	if len(p.Host.Forwards) > 0 || len(p.Host.ExtraArgs) > 0 {
		rv.Warnings = append(rv.Warnings, "sets port forwards or ssh options: read them")
	}
	return rv
}

func sameAddrIn(h SharedHost, a string) bool {
	if strings.EqualFold(h.Address, a) {
		return true
	}
	for _, rt := range h.Routes {
		if strings.EqualFold(rt.Address, a) {
			return true
		}
	}
	return false
}

// Approve applies the proposal for name to the team and removes it from
// the proposals. Only an owner or admin may; the proposal is checked again
// here, whatever the approver was shown.
func Approve(ctx context.Context, b Backend, pr Proposer, teamID string, role Role, name string) (Proposal, error) {
	if !role.CanEditHosts() {
		return Proposal{}, fmt.Errorf("approving needs admin or owner; you are %s", role)
	}
	ps, err := pr.Proposals(ctx, teamID)
	if err != nil {
		return Proposal{}, err
	}
	var p *Proposal
	for i := range ps {
		if ps[i].Host.Name == name {
			p = &ps[i]
		}
	}
	if p == nil {
		return Proposal{}, fmt.Errorf("no proposal for %s (see `meterm team proposals`)", name)
	}
	hosts, err := b.ListHosts(ctx, teamID)
	if err != nil {
		return Proposal{}, err
	}
	rv := ReviewProposal(*p, hosts)
	if rv.Invalid != nil {
		return *p, fmt.Errorf("can't approve %s: %w", name, rv.Invalid)
	}
	var rev int64
	if rv.Current != nil {
		rev = rv.Current.Rev
	}
	if p.Remove {
		err = b.DeleteHost(ctx, teamID, name, rev)
	} else {
		_, err = b.PutHost(ctx, teamID, p.Host, rev)
	}
	if err != nil {
		return *p, err
	}
	return *p, pr.DropProposal(ctx, teamID, name)
}
