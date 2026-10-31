package team

import (
	"context"
	"sort"

	"meterm/internal/activity"
)

// Router is the Backend the CLI uses: each team goes to its own repository,
// chosen by team ID.
type Router struct {
	Git map[string]*GitStore
}

func (r *Router) pick(teamID string) (Backend, error) {
	if g, ok := r.Git[teamID]; ok {
		return g, nil
	}
	return nil, ErrNotFound
}

func (r *Router) Identity(ctx context.Context) (Identity, error) {
	for _, g := range r.Git {
		return g.Identity(ctx)
	}
	return Identity{}, ErrNotSignedIn
}

// IdentityFor is who you are in teamID: your Git identity.
func (r *Router) IdentityFor(ctx context.Context, teamID string) (Identity, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return Identity{}, err
	}
	return b.Identity(ctx)
}

// Teams lists every team. One whose repository can't be read is left out
// rather than hiding the rest.
func (r *Router) Teams(ctx context.Context) ([]Team, error) {
	if len(r.Git) == 0 {
		return nil, ErrNotSignedIn
	}
	var out []Team
	for _, g := range r.Git {
		if ts, err := g.Teams(ctx); err == nil {
			out = append(out, ts...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Router) Members(ctx context.Context, teamID string) ([]Member, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return nil, err
	}
	return b.Members(ctx, teamID)
}

func (r *Router) Invite(ctx context.Context, teamID, email string, role Role) error {
	b, err := r.pick(teamID)
	if err != nil {
		return err
	}
	return b.Invite(ctx, teamID, email, role)
}

func (r *Router) RemoveMember(ctx context.Context, teamID, uid string) error {
	b, err := r.pick(teamID)
	if err != nil {
		return err
	}
	return b.RemoveMember(ctx, teamID, uid)
}

func (r *Router) ListHosts(ctx context.Context, teamID string) ([]SharedHost, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return nil, err
	}
	return b.ListHosts(ctx, teamID)
}

func (r *Router) PutHost(ctx context.Context, teamID string, h SharedHost, expectedRev int64) (SharedHost, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return SharedHost{}, err
	}
	return b.PutHost(ctx, teamID, h, expectedRev)
}

func (r *Router) DeleteHost(ctx context.Context, teamID, name string, expectedRev int64) error {
	b, err := r.pick(teamID)
	if err != nil {
		return err
	}
	return b.DeleteHost(ctx, teamID, name, expectedRev)
}

func (r *Router) AppendEvents(ctx context.Context, teamID string, events []activity.Event) error {
	b, err := r.pick(teamID)
	if err != nil {
		return err
	}
	return b.AppendEvents(ctx, teamID, events)
}

func (r *Router) ListEvents(ctx context.Context, teamID string, f EventFilter) ([]TeamEvent, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return nil, err
	}
	return b.ListEvents(ctx, teamID, f)
}

func (r *Router) PutAudit(ctx context.Context, teamID string, a AuditRecord) error {
	b, err := r.pick(teamID)
	if err != nil {
		return err
	}
	return b.PutAudit(ctx, teamID, a)
}

func (r *Router) ListAudits(ctx context.Context, teamID string) ([]AuditRecord, error) {
	b, err := r.pick(teamID)
	if err != nil {
		return nil, err
	}
	return b.ListAudits(ctx, teamID)
}
