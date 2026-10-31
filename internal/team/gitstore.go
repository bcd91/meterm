package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"meterm/internal/activity"
)

// GitStore is a team kept in a Git repository the team already controls —
// for teams that won't put their inventory in someone else's cloud. Access
// is whatever the Git host grants: who can clone reads the team, who can
// push writes it. Every change is a commit, so `git log` is the history.
//
// Layout of the repository:
//
//	meterm-team.yaml          name and members (roles are advisory: the Git host enforces access)
//	hosts/<name>.yaml         one shared host, with rev / updated_by / updated_at
//	audits/<name>.json        the latest `audit watch` result for a host
//	events/<device>.jsonl     each device's activity, appended only by that device
//
// meterm keeps its own clone under the team directory and resets it to the
// remote before every change, so there are never local edits to merge: a
// change is re-applied on top of whatever was pushed meanwhile, and a host
// changed by someone else since your last sync comes back as ErrConflict,
// exactly like the cloud backend.
type GitStore struct {
	ID, Name   string
	Dir        string // the clone
	Remote     string
	Branch     string
	mu         sync.Mutex
	identityFn func() (Identity, error)
}

// GitTeam is how a Git team is remembered in State.
type GitTeam struct {
	Name   string `json:"name"`
	Remote string `json:"remote"`
	Branch string `json:"branch,omitempty"`
}

// GitTeamID is the team ID for a repository: stable across machines, so
// every member's bases and ignores line up.
func GitTeamID(remote string) string {
	return "git:" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(remote), "/"), ".git")
}

// IsGitTeam reports whether teamID names a Git team.
func IsGitTeam(teamID string) bool { return strings.HasPrefix(teamID, "git:") }

type gitTeamFile struct {
	Name    string          `yaml:"name"`
	Members []gitTeamMember `yaml:"members,omitempty"`
	// MigratedFrom is the cloud team this one was moved from, so each
	// member's `team git add` can move their copies of its hosts over.
	MigratedFrom string `yaml:"migrated_from,omitempty"`
}

type gitTeamMember struct {
	Email string `yaml:"email"`
	Role  Role   `yaml:"role"`
}

// OpenGitStore returns the store for a remembered Git team, cloning it
// into dir/git/<hash> the first time.
func OpenGitStore(ctx context.Context, dir string, gt GitTeam) (*GitStore, error) {
	branch := gt.Branch
	if branch == "" {
		branch = "main"
	}
	id := GitTeamID(gt.Remote)
	g := &GitStore{ID: id, Name: gt.Name, Remote: gt.Remote, Branch: branch,
		Dir: filepath.Join(dir, "git", safeName(strings.TrimPrefix(id, "git:")))}
	g.identityFn = g.gitIdentity
	if _, err := os.Stat(filepath.Join(g.Dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(g.Dir), 0o700); err != nil {
			return nil, err
		}
		if out, err := quiet(exec.CommandContext(ctx, "git", "clone", "-q", "--", gt.Remote, g.Dir)).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git clone %s: %s", gt.Remote, strings.TrimSpace(string(out)))
		}
	}
	return g, nil
}

// InitGitTeam prepares a repository for meterm: clones it, and if it has no
// meterm-team.yaml yet, writes one naming the team and its creator as owner.
// It returns the name found or written.
func InitGitTeam(ctx context.Context, dir string, gt GitTeam) (string, error) {
	g, err := OpenGitStore(ctx, dir, gt)
	if err != nil {
		return "", err
	}
	name := gt.Name
	err = g.change(ctx, "meterm: set up team", func() error {
		var tf gitTeamFile
		if found, err := g.readYAML("meterm-team.yaml", &tf); err != nil {
			return err
		} else if found {
			name = tf.Name
			return errNoChange
		}
		id, err := g.identityFn()
		if err != nil {
			return err
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(gt.Remote), ".git")
		}
		return g.writeYAML("meterm-team.yaml", gitTeamFile{Name: name, Members: []gitTeamMember{{Email: id.Email, Role: RoleOwner}}})
	})
	return name, err
}

// ---- git plumbing ----------------------------------------------------------

func (g *GitStore) git(ctx context.Context, args ...string) (string, error) {
	c := quiet(exec.CommandContext(ctx, "git", append([]string{"-C", g.Dir}, args...)...))
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		debugf("git %s -> %s", strings.Join(args, " "), msg)
		return out.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	debugf("git %s -> ok", strings.Join(args, " "))
	return out.String(), nil
}

// refresh makes the clone exactly the remote branch. An empty repository
// (no branch yet) is fine: the first change creates it.
func (g *GitStore) refresh(ctx context.Context) error { return g.refreshBranch(ctx, g.Branch) }

// refreshBranch makes the clone exactly the remote branch, or an empty tree
// for a branch that doesn't exist yet.
func (g *GitStore) refreshBranch(ctx context.Context, branch string) error {
	if _, err := g.git(ctx, "fetch", "-q", "origin"); err != nil {
		return err
	}
	if _, err := g.git(ctx, "rev-parse", "-q", "--verify", "origin/"+branch); err != nil {
		cur, _ := g.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
		if strings.TrimSpace(cur) != branch {
			if _, err := g.git(ctx, "checkout", "-q", "--orphan", branch); err != nil && !strings.Contains(err.Error(), "already exists") {
				return err
			}
		}
		// An orphan keeps the previous branch's files; a new branch starts empty.
		_, _ = g.git(ctx, "rm", "-rfq", "--ignore-unmatch", ".")
		_, err := g.git(ctx, "clean", "-qfd")
		return err
	}
	if _, err := g.git(ctx, "checkout", "-q", "-f", "-B", branch, "origin/"+branch); err != nil {
		return err
	}
	_, err := g.git(ctx, "reset", "-q", "--hard", "origin/"+branch)
	if err == nil {
		_, err = g.git(ctx, "clean", "-qfd")
	}
	return err
}

var errNoChange = errors.New("no change")

// change applies edit on top of the latest remote state, commits and pushes,
// redoing it if someone pushed in between. edit returning errNoChange
// leaves everything as is.
func (g *GitStore) change(ctx context.Context, msg string, edit func() error) error {
	return g.changeOn(ctx, g.Branch, msg, edit)
}

// changeOn is change on another branch of the repository. The clone is
// left on the team's branch afterwards.
func (g *GitStore) changeOn(ctx context.Context, branch, msg string, edit func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if branch != g.Branch {
		defer func() { _ = g.onTeamBranch(ctx) }()
	}
	id, err := g.identityFn()
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := g.refreshBranch(ctx, branch); err != nil {
			return err
		}
		if err := edit(); err != nil {
			if errors.Is(err, errNoChange) {
				return nil
			}
			_ = g.refreshBranch(ctx, branch) // drop the half-made edit
			return err
		}
		if _, err := g.git(ctx, "add", "-A"); err != nil {
			return err
		}
		if out, _ := g.git(ctx, "status", "--porcelain"); strings.TrimSpace(out) == "" {
			return nil
		}
		if _, err := g.git(ctx, "-c", "user.email="+id.Email, "-c", "user.name="+id.Name(), "commit", "-q", "-m", msg); err != nil {
			return err
		}
		_, err := g.git(ctx, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "rejected") && !strings.Contains(err.Error(), "fetch first") && !strings.Contains(err.Error(), "non-fast-forward") {
			_ = g.refreshBranch(ctx, branch)
			if strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "Permission") {
				return fmt.Errorf("%w: %v", ErrForbidden, err)
			}
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return errors.New("too many concurrent changes to the team repository; try again")
}

// read refreshes the clone for a read-only call.
func (g *GitStore) read(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refresh(ctx)
}

func (g *GitStore) path(rel string) string { return filepath.Join(g.Dir, filepath.FromSlash(rel)) }

func (g *GitStore) readYAML(rel string, v any) (bool, error) {
	b, err := os.ReadFile(g.path(rel))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, yaml.Unmarshal(b, v)
}

func (g *GitStore) writeYAML(rel string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return g.writeFile(rel, b)
}

func (g *GitStore) writeFile(rel string, b []byte) error {
	p := g.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func (g *GitStore) gitIdentity() (Identity, error) {
	out, err := quiet(exec.Command("git", "-C", g.Dir, "config", "user.email")).Output()
	email := strings.ToLower(strings.TrimSpace(string(out)))
	if err != nil || email == "" {
		return Identity{}, errors.New("set your Git email first: git config --global user.email you@example.com")
	}
	return Identity{UID: email, Email: email, EmailVerified: true}, nil
}

// Name is the display name used for commits.
func (id Identity) Name() string {
	if i := strings.Index(id.Email, "@"); i > 0 {
		return id.Email[:i]
	}
	return id.Email
}

// fileName keeps a host name usable as a file name everywhere.
func fileName(name string) string { return url.PathEscape(name) }

// ---- Backend ----------------------------------------------------------------

func (g *GitStore) Identity(context.Context) (Identity, error) { return g.identityFn() }

func (g *GitStore) teamFile() (gitTeamFile, error) {
	var tf gitTeamFile
	_, err := g.readYAML("meterm-team.yaml", &tf)
	if tf.Name == "" {
		tf.Name = g.Name
	}
	return tf, err
}

func (g *GitStore) role(tf gitTeamFile, email string) Role {
	for _, m := range tf.Members {
		if strings.EqualFold(m.Email, email) {
			return m.Role
		}
	}
	if len(tf.Members) == 0 {
		return RoleAdmin
	}
	return RoleMember
}

func (g *GitStore) Teams(ctx context.Context) ([]Team, error) {
	id, err := g.identityFn()
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	err = g.onTeamBranch(ctx)
	var tf gitTeamFile
	found := false
	if err == nil {
		found, err = g.readYAML("meterm-team.yaml", &tf)
	}
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if tf.Name == "" {
		tf.Name = g.Name
	}
	role := g.role(tf, id.Email)
	if !found {
		role = RoleMember // no team file: nothing grants more
	}
	return []Team{{ID: g.ID, Name: tf.Name, Role: role, MovedFrom: tf.MigratedFrom}}, nil
}

// onTeamBranch puts the clone back on the team's branch, without the
// network, if another branch (the proposals) was left checked out: what's
// read from the working tree — the team file and its roles above all —
// must be the team's, never a proposal branch's.
func (g *GitStore) onTeamBranch(ctx context.Context) error {
	head, err := os.ReadFile(filepath.Join(g.Dir, ".git", "HEAD"))
	if err == nil && strings.TrimSpace(string(head)) == "ref: refs/heads/"+g.Branch {
		return nil
	}
	if _, err := g.git(ctx, "checkout", "-q", "-f", g.Branch); err != nil {
		return err
	}
	_, err = g.git(ctx, "clean", "-qfd")
	return err
}

func (g *GitStore) Members(ctx context.Context, teamID string) ([]Member, error) {
	if err := g.read(ctx); err != nil {
		return nil, err
	}
	tf, err := g.teamFile()
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(tf.Members))
	for _, m := range tf.Members {
		out = append(out, Member{UID: m.Email, Email: m.Email, Role: m.Role})
	}
	return out, nil
}

// Invite records the person in meterm-team.yaml. Access itself is granted
// on the Git host (add them to the repository).
func (g *GitStore) Invite(ctx context.Context, teamID, email string, role Role) error {
	email = strings.ToLower(strings.TrimSpace(email))
	return g.change(ctx, "meterm: add "+email+" as "+string(role), func() error {
		tf, err := g.teamFile()
		if err != nil {
			return err
		}
		id, _ := g.identityFn()
		if r := g.role(tf, id.Email); !r.CanInvite() || (role == RoleOwner && r != RoleOwner) {
			return ErrForbidden
		}
		for i, m := range tf.Members {
			if m.Email == email {
				tf.Members[i].Role = role
				return g.writeYAML("meterm-team.yaml", tf)
			}
		}
		tf.Members = append(tf.Members, gitTeamMember{Email: email, Role: role})
		return g.writeYAML("meterm-team.yaml", tf)
	})
}

func (g *GitStore) RemoveMember(ctx context.Context, teamID, uid string) error {
	return g.change(ctx, "meterm: remove "+uid, func() error {
		tf, err := g.teamFile()
		if err != nil {
			return err
		}
		id, _ := g.identityFn()
		if uid != id.Email && g.role(tf, id.Email) != RoleOwner {
			return ErrForbidden
		}
		owners, kept := 0, tf.Members[:0:0]
		found := false
		var target gitTeamMember
		for _, m := range tf.Members {
			if m.Role == RoleOwner {
				owners++
			}
			if strings.EqualFold(m.Email, uid) {
				found, target = true, m
				continue
			}
			kept = append(kept, m)
		}
		if !found {
			return ErrNotFound
		}
		if target.Role == RoleOwner && owners == 1 {
			return ErrLastOwner
		}
		tf.Members = kept
		return g.writeYAML("meterm-team.yaml", tf)
	})
}

func (g *GitStore) ListHosts(ctx context.Context, teamID string) ([]SharedHost, error) {
	if err := g.read(ctx); err != nil {
		return nil, err
	}
	return g.hosts()
}

func (g *GitStore) hosts() ([]SharedHost, error) {
	files, _ := filepath.Glob(g.path("hosts/*.yaml"))
	out := make([]SharedHost, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var h SharedHost
		if err := yaml.Unmarshal(b, &h); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (g *GitStore) hostRev(name string) (int64, error) {
	var h SharedHost
	found, err := g.readYAML("hosts/"+fileName(name)+".yaml", &h)
	if err != nil || !found {
		return 0, err
	}
	return h.Rev, nil
}

func (g *GitStore) canEdit() error {
	tf, err := g.teamFile()
	if err != nil {
		return err
	}
	id, err := g.identityFn()
	if err != nil {
		return err
	}
	if !g.role(tf, id.Email).CanEditHosts() {
		return ErrForbidden
	}
	return nil
}

func (g *GitStore) PutHost(ctx context.Context, teamID string, h SharedHost, expectedRev int64) (SharedHost, error) {
	id, err := g.identityFn()
	if err != nil {
		return SharedHost{}, err
	}
	var stored SharedHost
	err = g.change(ctx, "meterm: update host "+h.Name, func() error {
		if err := g.canEdit(); err != nil {
			return err
		}
		rev, err := g.hostRev(h.Name)
		if err != nil {
			return err
		}
		if rev != expectedRev {
			return ErrConflict
		}
		stored = h
		stored.Rev = rev + 1
		stored.UpdatedBy = id.Email
		stored.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		return g.writeYAML("hosts/"+fileName(h.Name)+".yaml", stored)
	})
	return stored, err
}

func (g *GitStore) DeleteHost(ctx context.Context, teamID, name string, expectedRev int64) error {
	return g.change(ctx, "meterm: remove host "+name, func() error {
		if err := g.canEdit(); err != nil {
			return err
		}
		rev, err := g.hostRev(name)
		if err != nil {
			return err
		}
		if rev == 0 {
			return errNoChange
		}
		if rev != expectedRev {
			return ErrConflict
		}
		return os.Remove(g.path("hosts/" + fileName(name) + ".yaml"))
	})
}

type gitEvent struct {
	activity.Event
	UploaderEmail string `json:"uploader_email"`
}

// AppendEvents appends to this device's own file only, so two members never
// edit the same file and pushes never conflict. Events already there (a
// retry) are skipped; an event with the same seq but other content is
// refused, like the cloud backend.
func (g *GitStore) AppendEvents(ctx context.Context, teamID string, events []activity.Event) error {
	if len(events) == 0 {
		return nil
	}
	id, err := g.identityFn()
	if err != nil {
		return err
	}
	byDevice := map[string][]activity.Event{}
	for _, e := range events {
		byDevice[e.Device] = append(byDevice[e.Device], e)
	}
	return g.change(ctx, fmt.Sprintf("meterm: activity (%d)", len(events)), func() error {
		for dev, evs := range byDevice {
			rel := "events/" + fileName(dev) + ".jsonl"
			have := map[int64]activity.Event{}
			existing, _ := g.readEvents(g.path(rel))
			for _, e := range existing {
				have[e.Seq] = e.Event
			}
			var buf bytes.Buffer
			for _, e := range evs {
				if old, ok := have[e.Seq]; ok {
					if !sameEvent(old, e) {
						return fmt.Errorf("event %s_%d already stored with different content: %w", e.Device, e.Seq, ErrForbidden)
					}
					continue
				}
				b, _ := json.Marshal(gitEvent{Event: e, UploaderEmail: id.Email})
				buf.Write(append(b, '\n'))
			}
			if buf.Len() == 0 {
				continue
			}
			f, err := os.OpenFile(g.path(rel), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				if err := os.MkdirAll(g.path("events"), 0o755); err != nil {
					return err
				}
				if f, err = os.OpenFile(g.path(rel), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
					return err
				}
			}
			_, err = f.Write(buf.Bytes())
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (g *GitStore) readEvents(path string) ([]gitEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []gitEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e gitEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func (g *GitStore) ListEvents(ctx context.Context, teamID string, flt EventFilter) ([]TeamEvent, error) {
	if err := g.read(ctx); err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(g.path("events/*.jsonl"))
	var out []TeamEvent
	for _, f := range files {
		evs, err := g.readEvents(f)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if flt.Host != "" && e.Host != flt.Host {
				continue
			}
			if !flt.Since.IsZero() && e.Time.Before(flt.Since) {
				continue
			}
			out = append(out, TeamEvent{Event: e.Event, UploaderUID: e.UploaderEmail, UploaderEmail: e.UploaderEmail})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[len(out)-flt.Limit:]
	}
	return out, nil
}

func (g *GitStore) PutAudit(ctx context.Context, teamID string, a AuditRecord) error {
	id, err := g.identityFn()
	if err != nil {
		return err
	}
	a.By = id.Email
	return g.change(ctx, "meterm: audit "+a.Host, func() error {
		b, err := marshalAudit(a)
		if err != nil {
			return err
		}
		return g.writeFile("audits/"+fileName(a.Host)+".json", b)
	})
}

func marshalAudit(a AuditRecord) ([]byte, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	return append(b, '\n'), err
}

func (g *GitStore) ListAudits(ctx context.Context, teamID string) ([]AuditRecord, error) {
	if err := g.read(ctx); err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(g.path("audits/*.json"))
	out := make([]AuditRecord, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var a AuditRecord
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func sameEvent(a, b activity.Event) bool {
	ta, tb := a.Time, b.Time
	a.Time, b.Time = time.Time{}, time.Time{}
	return a == b && ta.Equal(tb)
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
