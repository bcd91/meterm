// Package team shares a host list and an activity log between people, with
// any number of teams per person. A team is kept in a Git repository the
// team controls (see gitstore.go); the repository host's access controls
// decide who can read and write it. This package defines the data model, the
// Backend a store implements, and the sync logic that reconciles a team's
// hosts with the local hosts.yaml.
//
// What is shared is deliberately limited to connection metadata. Private keys
// never leave the machine, and identity_file stays per-person because a key
// path on one laptop means nothing on another.
package team

import (
	"context"
	"errors"
	"slices"
	"time"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// Role is what a member may do in a team.
type Role string

const (
	RoleOwner  Role = "owner"  // everything, including deleting the team and changing roles
	RoleAdmin  Role = "admin"  // edit hosts, invite people
	RoleMember Role = "member" // read hosts, contribute activity
)

// CanEditHosts reports whether r may add, change or remove shared hosts.
func (r Role) CanEditHosts() bool { return r == RoleOwner || r == RoleAdmin }

// CanInvite reports whether r may invite people.
func (r Role) CanInvite() bool { return r == RoleOwner || r == RoleAdmin }

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool { return r == RoleOwner || r == RoleAdmin || r == RoleMember }

// Identity is who the backend says the current user is.
type Identity struct {
	UID           string
	Email         string
	EmailVerified bool
}

// Team is one shared workspace. A person can belong to several (their own
// company, plus each client they look after).
type Team struct {
	ID   string
	Name string
	Role Role // the current user's role in it
	// MovedFrom is the ID of the old cloud team this one was migrated from,
	// so hosts this machine still has under that ID can be adopted.
	MovedFrom string
}

// Member is one person's membership of a team.
type Member struct {
	UID   string
	Email string
	Role  Role
}

// SharedHost is the part of a saved host that a team shares.
type SharedHost struct {
	Name      string           `json:"name" yaml:"name"`
	Address   string           `json:"address" yaml:"address"`
	Port      int              `json:"port,omitempty" yaml:"port,omitempty"`
	User      string           `json:"user,omitempty" yaml:"user,omitempty"`
	Users     []string         `json:"users,omitempty" yaml:"users,omitempty"`
	Group     string           `json:"group,omitempty" yaml:"group,omitempty"`
	Tags      []string         `json:"tags,omitempty" yaml:"tags,omitempty"`
	Env       string           `json:"env,omitempty" yaml:"env,omitempty"`
	ProxyJump string           `json:"proxy_jump,omitempty" yaml:"proxy_jump,omitempty"`
	Forwards  []config.Forward `json:"forwards,omitempty" yaml:"forwards,omitempty"`
	ExtraArgs []string         `json:"extra_args,omitempty" yaml:"extra_args,omitempty"`
	Notes     string           `json:"notes,omitempty" yaml:"notes,omitempty"`
	Routes    []config.Route   `json:"routes,omitempty" yaml:"routes,omitempty"`

	// Bookkeeping set by the backend on every write.
	Rev       int64     `json:"rev" yaml:"rev"`
	UpdatedBy string    `json:"updated_by,omitempty" yaml:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
}

// SharedFromHost takes the shareable fields of h.
func SharedFromHost(h config.Host) SharedHost {
	return SharedHost{
		Name: h.Name, Address: h.Address, Port: h.Port, User: h.User, Users: h.Users, Group: h.Group,
		Tags: h.Tags, Env: h.Env, ProxyJump: h.ProxyJump, Forwards: h.Forwards, ExtraArgs: h.ExtraArgs, Notes: h.Notes,
		Routes: h.Routes,
	}
}

// ApplyTo copies s onto a local host for team teamID, keeping the fields that
// stay personal (identity_file) from local.
func (s SharedHost) ApplyTo(local config.Host, teamID string) config.Host {
	return config.Host{
		Name: s.Name, Address: s.Address, Port: s.Port, User: s.User, Users: s.Users, Group: s.Group,
		Tags: s.Tags, Env: s.Env, ProxyJump: s.ProxyJump, Forwards: s.Forwards, ExtraArgs: s.ExtraArgs, Notes: s.Notes,
		Routes:       s.Routes,
		IdentityFile: local.IdentityFile,
		Team:         teamID,
	}
}

// SameContent compares the shared fields only, ignoring bookkeeping.
func (s SharedHost) SameContent(o SharedHost) bool {
	return s.Name == o.Name && s.Address == o.Address && s.Port == o.Port && s.User == o.User && slices.Equal(s.Users, o.Users) &&
		s.Group == o.Group && slices.Equal(s.Tags, o.Tags) && s.Env == o.Env && s.ProxyJump == o.ProxyJump &&
		slices.Equal(s.Forwards, o.Forwards) && slices.Equal(s.ExtraArgs, o.ExtraArgs) && s.Notes == o.Notes &&
		slices.Equal(s.Routes, o.Routes)
}

// EventFilter narrows ListEvents.
type EventFilter struct {
	Host  string    // only this host, if set
	Since time.Time // only events at or after this, if set
	Limit int       // most recent N, if > 0
}

// TeamEvent is an activity event as the team sees it: the device's own
// hash-chained record, plus who uploaded it according to the backend's auth
// (which the uploader can't forge, unlike the event's own User field).
type TeamEvent struct {
	activity.Event
	UploaderUID   string `json:"uploader_uid"`
	UploaderEmail string `json:"uploader_email"`
}

// AuditRecord is the latest `audit watch` result for one shared host, so the
// whole team sees the same security picture without each person re-running it.
type AuditRecord struct {
	Host     string         `json:"host"`
	Time     time.Time      `json:"time"`
	By       string         `json:"by"` // set by the backend from the signed-in account
	Fails    int            `json:"fails"`
	Unknowns int            `json:"unknowns"`
	Worst    string         `json:"worst,omitempty"` // severity of the worst failure
	Error    string         `json:"error,omitempty"` // the host couldn't be audited
	Findings []AuditFinding `json:"findings,omitempty"`
}

// AuditFinding is one failed control in an AuditRecord.
type AuditFinding struct {
	Control  string `json:"control"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Detail   string `json:"detail,omitempty"`
}

// Errors a Backend returns for the cases callers handle differently.
var (
	ErrNotSignedIn = errors.New("no team on this machine — add one with `meterm team git add <name> <repo-url>`")
	ErrForbidden   = errors.New("your role in this team does not allow that")
	ErrNotFound    = errors.New("not found")
	// ErrConflict means someone else changed the host since we last read it.
	ErrConflict  = errors.New("changed by someone else since your last sync")
	ErrLastOwner = errors.New("the team's last owner can't leave or be removed; make someone else owner first")
)

// Backend is a team store. Every call acts as the current user. For a Git
// team the roles in meterm-team.yaml guide the client, and the repository's
// own access controls are what actually enforce who may write.
type Backend interface {
	Identity(ctx context.Context) (Identity, error)

	Teams(ctx context.Context) ([]Team, error)
	Members(ctx context.Context, teamID string) ([]Member, error)

	// Invite records email as a member of teamID with role.
	Invite(ctx context.Context, teamID, email string, role Role) error
	// RemoveMember takes uid out of teamID: an owner removing someone, or
	// anyone leaving (uid = their own). The last owner can't leave.
	RemoveMember(ctx context.Context, teamID, uid string) error

	ListHosts(ctx context.Context, teamID string) ([]SharedHost, error)
	// PutHost writes h if the stored host is still at expectedRev (0 = must
	// not exist yet), returning the stored copy with its new Rev, or
	// ErrConflict.
	PutHost(ctx context.Context, teamID string, h SharedHost, expectedRev int64) (SharedHost, error)
	// DeleteHost removes name if it is still at expectedRev, or ErrConflict.
	DeleteHost(ctx context.Context, teamID, name string, expectedRev int64) error

	// AppendEvents uploads events. It is idempotent per (Device, Seq), so a
	// retry after a timeout never duplicates entries, and stored events can
	// never be changed or deleted.
	AppendEvents(ctx context.Context, teamID string, events []activity.Event) error
	ListEvents(ctx context.Context, teamID string, f EventFilter) ([]TeamEvent, error)

	// PutAudit replaces the stored latest audit of a.Host (any member may).
	PutAudit(ctx context.Context, teamID string, a AuditRecord) error
	ListAudits(ctx context.Context, teamID string) ([]AuditRecord, error)
}
