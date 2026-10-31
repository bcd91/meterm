package team

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"meterm/internal/config"
)

// Base is the team's host list as of the last successful sync, kept locally.
// It is what makes the merge three-way: comparing local and remote against it
// tells "I changed this" apart from "someone else changed this".
type Base map[string]SharedHost

// Outcome is what sync did (or, in a dry run, would do) with one host.
type Outcome string

const (
	Pulled         Outcome = "pulled"          // remote change applied locally
	PulledRemoval  Outcome = "removed"         // removed remotely, so removed locally
	Pushed         Outcome = "pushed"          // local change uploaded
	PushedRemoval  Outcome = "removed-remote"  // removed locally, so removed remotely
	Pending        Outcome = "not-pushed"      // local change waiting for `team push`
	PendingRemoval Outcome = "removal-pending" // local removal waiting for `team push`
	Conflict       Outcome = "conflict"        // both sides changed it; left alone locally
	Forbidden      Outcome = "forbidden"       // local change, but this role can't edit hosts
	NameTaken      Outcome = "name-taken"      // a personal host already uses this name
	Rejected       Outcome = "rejected"        // the team's copy isn't safe to use (see Detail); not applied
)

// NeedsAttention reports whether o is something a person has to act on.
func (o Outcome) NeedsAttention() bool {
	switch o {
	case Pending, PendingRemoval, Conflict, Forbidden, NameTaken, Rejected:
		return true
	}
	return false
}

// Change is one line of a sync report.
type Change struct {
	Host    string  `json:"host"`
	Outcome Outcome `json:"outcome"`
	Detail  string  `json:"detail,omitempty"`
}

// Result reports a sync. Base is the new base to store; it records only what
// is known to match the server, so an unresolved conflict or an unpushed
// local change comes up again next time instead of being forgotten.
type Result struct {
	Changes []Change
	Base    Base
}

// Options selects what Sync does.
type Options struct {
	Pull bool // apply remote changes locally
	Push bool // upload local changes (needs admin or owner)
	// Only, if non-nil, limits pushing to these host names; other local
	// changes are reported as pending.
	Only map[string]bool
	// Ignored names are skipped entirely: hosts this person chose not to
	// receive from the team (`team ignore`).
	Ignored map[string]bool
	// DryRun computes the plan without writing to the backend or cfg.
	DryRun bool
}

// Sync reconciles cfg's hosts for teamID with the backend, three-way against
// base. It edits cfg in memory; the caller saves it (and the returned Base)
// only if Sync succeeds.
//
// Per host, comparing local (L), remote (R) and base (B):
//   - L == R: nothing to do
//   - L == B: only the remote changed → pull it (including a removal)
//   - R == B: only we changed it → push it, or report it pending
//   - otherwise both changed it → conflict, keep local, report
func Sync(ctx context.Context, b Backend, teamID string, role Role, cfg *config.Config, base Base, opt Options) (Result, error) {
	remoteList, err := b.ListHosts(ctx, teamID)
	if err != nil {
		return Result{}, err
	}
	remote := map[string]SharedHost{}
	for _, h := range remoteList {
		remote[h.Name] = h
	}
	local := map[string]config.Host{}
	personal := map[string]bool{}
	for _, h := range cfg.Hosts {
		switch h.Team {
		case teamID:
			local[h.Name] = h
		case "":
			personal[h.Name] = true
		}
	}

	if len(local) == 0 && len(base) > 0 {
		// None of the team's hosts are here, yet the base says they were:
		// bookkeeping from a bad `team git add` (before v0.16.1) or a
		// different hosts file. Reading that as "every host removed here" is
		// how a single push would wipe the whole team, so start over and
		// pull. (Taking every host out of a team at once is therefore done
		// in the repository, not by emptying one machine's list.)
		base = Base{}
	}

	names := map[string]bool{}
	for _, m := range []map[string]SharedHost{remote, base} {
		for n := range m {
			names[n] = true
		}
	}
	for n := range local {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		if !opt.Ignored[n] {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)

	res := Result{Base: Base{}}
	add := func(name string, o Outcome, detail string) {
		res.Changes = append(res.Changes, Change{name, o, detail})
	}
	for _, name := range sorted {
		l, lok := local[name]
		r, rok := remote[name]
		bh, bok := base[name]
		var ls SharedHost
		if lok {
			ls = SharedFromHost(l)
		}
		keepBase := func() {
			if bok {
				res.Base[name] = bh
			}
		}

		same := func(aok bool, a SharedHost, cok bool, c SharedHost) bool {
			return aok == cok && (!aok || a.SameContent(c))
		}
		localUnchanged := same(lok, ls, bok, bh)
		remoteUnchanged := same(rok, r, bok, bh)

		switch {
		case same(lok, ls, rok, r):
			if rok {
				res.Base[name] = r
			}

		case localUnchanged: // the remote moved
			if !opt.Pull {
				keepBase()
				continue
			}
			if !rok {
				if !opt.DryRun {
					cfg.Remove(name)
				}
				add(name, PulledRemoval, "")
				continue
			}
			if !lok && personal[name] {
				add(name, NameTaken, "you have a personal host with this name — see `meterm team resolve`")
				continue
			}
			incoming := r.ApplyTo(l, teamID)
			if err := incoming.CheckShared(); err != nil {
				keepBase()
				add(name, Rejected, "not applied: "+err.Error())
				continue
			}
			if !opt.DryRun {
				cfg.Upsert(incoming)
			}
			res.Base[name] = r
			add(name, Pulled, "")

		case remoteUnchanged: // we moved
			wantPush := opt.Push && (opt.Only == nil || opt.Only[name])
			if !wantPush {
				keepBase()
				if lok {
					add(name, Pending, "")
				} else {
					add(name, PendingRemoval, "")
				}
				continue
			}
			if !role.CanEditHosts() {
				keepBase()
				pr, ok := b.(Proposer)
				if !ok {
					add(name, Forbidden, fmt.Sprintf("needs admin or owner; you are %s", role))
					continue
				}
				// A member's change goes to the owners and admins for approval.
				p := Proposal{Host: ls, Remove: !lok}
				if rok {
					p.BaseRev = r.Rev
				}
				if !lok {
					p.Host = r
				} else if err := ls.ApplyTo(config.Host{}, teamID).CheckShared(); err != nil {
					add(name, Rejected, "not proposed: "+err.Error())
					continue
				}
				if !opt.DryRun {
					if err := pr.Propose(ctx, teamID, p); err != nil {
						if errors.Is(err, ErrNoProposals) {
							add(name, Forbidden, fmt.Sprintf("needs admin or owner; you are %s", role))
							continue
						}
						return res, fmt.Errorf("propose %s: %w", name, err)
					}
				}
				add(name, Proposed, "waiting for an owner or admin: meterm team approve "+name)
				continue
			}
			var expected int64
			if rok {
				expected = r.Rev
			}
			if !lok {
				if !opt.DryRun {
					if err := b.DeleteHost(ctx, teamID, name, expected); err != nil {
						if errors.Is(err, ErrConflict) {
							keepBase()
							add(name, Conflict, err.Error())
							continue
						}
						return res, fmt.Errorf("remove %s: %w", name, err)
					}
				}
				add(name, PushedRemoval, "")
				continue
			}
			if err := ls.ApplyTo(config.Host{}, teamID).CheckShared(); err != nil {
				keepBase()
				add(name, Rejected, "not shared: "+err.Error())
				continue
			}
			if opt.DryRun {
				keepBase()
				add(name, Pushed, "")
				continue
			}
			stored, err := b.PutHost(ctx, teamID, ls, expected)
			if err != nil {
				if errors.Is(err, ErrConflict) {
					keepBase()
					add(name, Conflict, err.Error())
					continue
				}
				return res, fmt.Errorf("push %s: %w", name, err)
			}
			res.Base[name] = stored
			add(name, Pushed, "")

		default:
			keepBase() // keep comparing against the old base so it stays a conflict
			add(name, Conflict, "changed both here and by someone else — see `meterm team resolve`")
		}
	}
	return res, nil
}

// Resolve settles one host that sync reported as a conflict or name-taken.
// theirs takes the team's copy (keeping this person's identity_file);
// otherwise this machine's copy wins and is pushed over the team's. Returns
// the updated base; the caller saves cfg and the base.
func Resolve(ctx context.Context, b Backend, teamID string, role Role, cfg *config.Config, base Base, name string, theirs bool) (Base, error) {
	hosts, err := b.ListHosts(ctx, teamID)
	if err != nil {
		return base, err
	}
	var r SharedHost
	rok := false
	for _, h := range hosts {
		if h.Name == name {
			r, rok = h, true
		}
	}
	nb := Base{}
	for k, v := range base {
		nb[k] = v
	}
	l, i := cfg.Find(name)

	if theirs {
		if !rok {
			cfg.Remove(name)
			delete(nb, name)
			return nb, nil
		}
		var cur config.Host
		if i >= 0 {
			cur = *l
		}
		incoming := r.ApplyTo(cur, teamID)
		if err := incoming.CheckShared(); err != nil {
			return base, fmt.Errorf("the team's copy of %s isn't safe to use: %w", name, err)
		}
		cfg.Upsert(incoming)
		nb[name] = r
		return nb, nil
	}

	if !role.CanEditHosts() {
		return base, fmt.Errorf("keeping your copy pushes it to the team, which needs admin or owner; you are %s", role)
	}
	if i < 0 {
		return base, fmt.Errorf("no local host %s to keep", name)
	}
	l.Team = teamID
	// Pretend we last saw the team's current copy: then only our side differs
	// and an ordinary push sends it.
	if rok {
		nb[name] = r
	} else {
		delete(nb, name)
	}
	res, err := Sync(ctx, b, teamID, role, cfg, nb, Options{Push: true, Only: map[string]bool{name: true}})
	if err != nil {
		return base, err
	}
	for _, c := range res.Changes {
		if c.Host == name && c.Outcome != Pushed {
			return base, fmt.Errorf("%s: %s %s", name, c.Outcome, c.Detail)
		}
	}
	for k, v := range res.Base {
		nb[k] = v
	}
	return nb, nil
}
