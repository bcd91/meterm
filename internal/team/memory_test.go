package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"meterm/internal/activity"
)

// MemoryStore is an in-process team store for the sync tests, with the role
// rules a team applies (members read, admins and owners edit hosts, owners
// manage people). Sessions made with As share one store, the way several
// people share one repository.
type MemoryStore struct {
	mu      sync.Mutex
	nextID  int
	teams   map[string]*memTeam
	invites map[string]map[string]Role // email -> teamID -> role
}

type memTeam struct {
	name    string
	members map[string]Member // by UID
	hosts   map[string]SharedHost
	events  map[string]TeamEvent // by device/seq
	audits  map[string]AuditRecord
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{teams: map[string]*memTeam{}, invites: map[string]map[string]Role{}}
}

// As returns a Backend acting as id.
func (s *MemoryStore) As(id Identity) *memBackend { return &memBackend{s: s, id: id} }

type memBackend struct {
	s  *MemoryStore
	id Identity
}

func (m *memBackend) Identity(context.Context) (Identity, error) {
	if m.id.UID == "" {
		return Identity{}, ErrNotSignedIn
	}
	return m.id, nil
}

// member returns the caller's membership, the gate on every team-scoped call.
// Caller holds s.mu.
func (m *memBackend) member(teamID string) (*memTeam, Member, error) {
	if m.id.UID == "" {
		return nil, Member{}, ErrNotSignedIn
	}
	t, ok := m.s.teams[teamID]
	if !ok {
		return nil, Member{}, ErrNotFound
	}
	mem, ok := t.members[m.id.UID]
	if !ok {
		return nil, Member{}, ErrForbidden
	}
	return t, mem, nil
}

func (m *memBackend) CreateTeam(_ context.Context, name string) (Team, error) {
	if m.id.UID == "" {
		return Team{}, ErrNotSignedIn
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.nextID++
	id := fmt.Sprintf("t%d", m.s.nextID)
	m.s.teams[id] = &memTeam{
		name:    name,
		members: map[string]Member{m.id.UID: {UID: m.id.UID, Email: m.id.Email, Role: RoleOwner}},
		hosts:   map[string]SharedHost{},
		events:  map[string]TeamEvent{},
		audits:  map[string]AuditRecord{},
	}
	return Team{ID: id, Name: name, Role: RoleOwner}, nil
}

func (m *memBackend) Teams(context.Context) ([]Team, error) {
	if m.id.UID == "" {
		return nil, ErrNotSignedIn
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	var out []Team
	for id, t := range m.s.teams {
		if mem, ok := t.members[m.id.UID]; ok {
			out = append(out, Team{ID: id, Name: t.name, Role: mem.Role})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memBackend) Members(_ context.Context, teamID string) ([]Member, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return nil, err
	}
	var out []Member
	for _, mem := range t.members {
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (m *memBackend) Invite(_ context.Context, teamID, email string, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("unknown role %q", role)
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	_, me, err := m.member(teamID)
	if err != nil {
		return err
	}
	// Admins invite; only an owner can hand out owner.
	if !me.Role.CanInvite() || (role == RoleOwner && me.Role != RoleOwner) {
		return ErrForbidden
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if m.s.invites[email] == nil {
		m.s.invites[email] = map[string]Role{}
	}
	m.s.invites[email][teamID] = role
	return nil
}

func (m *memBackend) RemoveMember(_ context.Context, teamID, uid string) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, me, err := m.member(teamID)
	if err != nil {
		return err
	}
	target, ok := t.members[uid]
	if !ok {
		return ErrNotFound
	}
	if uid != me.UID && me.Role != RoleOwner {
		return ErrForbidden
	}
	if target.Role == RoleOwner && countOwners(t.members) == 1 {
		return ErrLastOwner
	}
	delete(t.members, uid)
	return nil
}

func countOwners(ms map[string]Member) int {
	n := 0
	for _, m := range ms {
		if m.Role == RoleOwner {
			n++
		}
	}
	return n
}

// AcceptInvites joins every team id's email was invited to.
func (m *memBackend) AcceptInvites(context.Context) ([]Team, error) {
	if m.id.UID == "" {
		return nil, ErrNotSignedIn
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	email := strings.ToLower(m.id.Email)
	var joined []Team
	for teamID, role := range m.s.invites[email] {
		t, ok := m.s.teams[teamID]
		if !ok {
			continue
		}
		t.members[m.id.UID] = Member{UID: m.id.UID, Email: m.id.Email, Role: role}
		joined = append(joined, Team{ID: teamID, Name: t.name, Role: role})
	}
	delete(m.s.invites, email)
	sort.Slice(joined, func(i, j int) bool { return joined[i].Name < joined[j].Name })
	return joined, nil
}

func (m *memBackend) ListHosts(_ context.Context, teamID string) ([]SharedHost, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return nil, err
	}
	out := make([]SharedHost, 0, len(t.hosts))
	for _, h := range t.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memBackend) PutHost(_ context.Context, teamID string, h SharedHost, expectedRev int64) (SharedHost, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, me, err := m.member(teamID)
	if err != nil {
		return SharedHost{}, err
	}
	if !me.Role.CanEditHosts() {
		return SharedHost{}, ErrForbidden
	}
	cur, exists := t.hosts[h.Name]
	if (exists && cur.Rev != expectedRev) || (!exists && expectedRev != 0) {
		return SharedHost{}, ErrConflict
	}
	h.Rev = cur.Rev + 1
	h.UpdatedBy = m.id.Email
	h.UpdatedAt = time.Now().UTC()
	t.hosts[h.Name] = h
	return h, nil
}

func (m *memBackend) DeleteHost(_ context.Context, teamID, name string, expectedRev int64) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, me, err := m.member(teamID)
	if err != nil {
		return err
	}
	if !me.Role.CanEditHosts() {
		return ErrForbidden
	}
	cur, exists := t.hosts[name]
	if !exists {
		return nil // already gone: the outcome the caller wanted
	}
	if cur.Rev != expectedRev {
		return ErrConflict
	}
	delete(t.hosts, name)
	return nil
}

func (m *memBackend) AppendEvents(_ context.Context, teamID string, events []activity.Event) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return err
	}
	for _, e := range events {
		key := fmt.Sprintf("%s/%d", e.Device, e.Seq)
		if existing, ok := t.events[key]; ok {
			// Stored events are never overwritten (Firestore rules: create
			// only). Re-sending the identical event is a harmless retry;
			// different content under the same key is an attempt to rewrite
			// history.
			a, _ := json.Marshal(existing.Event)
			b, _ := json.Marshal(e)
			if !bytes.Equal(a, b) {
				return fmt.Errorf("event %s already stored with different content: %w", key, ErrForbidden)
			}
			continue
		}
		t.events[key] = TeamEvent{Event: e, UploaderUID: m.id.UID, UploaderEmail: m.id.Email}
	}
	return nil
}

func (m *memBackend) ListEvents(_ context.Context, teamID string, f EventFilter) ([]TeamEvent, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return nil, err
	}
	var out []TeamEvent
	for _, e := range t.events {
		if f.Host != "" && e.Host != f.Host {
			continue
		}
		if !f.Since.IsZero() && e.Time.Before(f.Since) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

func (m *memBackend) PutAudit(_ context.Context, teamID string, a AuditRecord) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return err
	}
	a.By = m.id.Email
	t.audits[a.Host] = a
	return nil
}

func (m *memBackend) ListAudits(_ context.Context, teamID string) ([]AuditRecord, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	t, _, err := m.member(teamID)
	if err != nil {
		return nil, err
	}
	out := make([]AuditRecord, 0, len(t.audits))
	for _, a := range t.audits {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}
