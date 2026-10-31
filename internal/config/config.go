// Package config defines meterm's host inventory and its on-disk storage.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"meterm/internal/lockfile"
	"meterm/internal/safefile"
)

// Forward describes a saved port-forwarding rule for a host. An unnamed
// forward opens with every `meterm connect`; a named one is a tunnel, opened
// only on demand by `meterm tunnel up`.
type Forward struct {
	Name       string `yaml:"name,omitempty"`
	Type       string `yaml:"type"` // "L" (local), "R" (remote), "D" (dynamic/SOCKS)
	LocalPort  int    `yaml:"local_port,omitempty"`
	RemoteHost string `yaml:"remote_host,omitempty"`
	RemotePort int    `yaml:"remote_port,omitempty"`
	// Note says what a tunnel reaches ("Billing API (prod)"); required for
	// tunnels on team hosts, since teammates open them too.
	Note string `yaml:"note,omitempty"`
}

// Route is another way to reach a host than its own address: say, a public
// port forward next to a VPN address, or a different jump host. A route
// gives the whole path: an empty ProxyJump means "connect directly", even if
// the host or its group normally jumps.
type Route struct {
	Name      string `yaml:"name" json:"name"`
	Address   string `yaml:"address,omitempty" json:"address,omitempty"` // empty: the host's own
	Port      int    `yaml:"port,omitempty" json:"port,omitempty"`       // 0: the host's own
	ProxyJump string `yaml:"proxy_jump,omitempty" json:"proxy_jump,omitempty"`
}

// RoutePicker chooses which of a host's routes to use right now; ok false
// means its own address. See Config.SetRoutePicker.
type RoutePicker func(h Host) (r Route, ok bool)

// Defaults holds fallback connection settings applied when a host or its
// group leaves a field empty.
type Defaults struct {
	User         string `yaml:"user,omitempty"`
	Port         int    `yaml:"port,omitempty"`
	IdentityFile string `yaml:"identity_file,omitempty"`
}

// Group is a named bundle of settings hosts can inherit from, so an entire
// environment (e.g. "prod") can share a user, key, and bastion in one place.
type Group struct {
	User         string   `yaml:"user,omitempty"`
	Port         int      `yaml:"port,omitempty"`
	IdentityFile string   `yaml:"identity_file,omitempty"`
	ProxyJump    string   `yaml:"proxy_jump,omitempty"`
	Tags         []string `yaml:"tags,omitempty"`
	// Env is the environment of the group's hosts, unless a host says its own.
	Env string `yaml:"env,omitempty"`
}

// Host is a single saved machine.
type Host struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	Port    int    `yaml:"port,omitempty"`
	User    string `yaml:"user,omitempty"`
	// Users are other accounts people log in to this host as (deploy,
	// root, ...): `meterm connect deploy@API`, or a choice in the picker.
	Users        []string `yaml:"users,omitempty"`
	IdentityFile string   `yaml:"identity_file,omitempty"`
	Group        string   `yaml:"group,omitempty"`
	Tags         []string `yaml:"tags,omitempty"`
	// Env is what this machine is: production, staging, dev or test. See
	// Config.Env for how it's read, and Config.IsProduction for what it decides.
	Env       string    `yaml:"env,omitempty"`
	ProxyJump string    `yaml:"proxy_jump,omitempty"`
	Forwards  []Forward `yaml:"forwards,omitempty"`
	ExtraArgs []string  `yaml:"extra_args,omitempty"`
	Notes     string    `yaml:"notes,omitempty"`
	// Routes are other ways to reach the host, tried in order after its own
	// address when that doesn't answer (see internal/route).
	Routes []Route `yaml:"routes,omitempty"`
	// Team is the ID of the team this host is shared with, or empty for a
	// personal host. Set by `meterm team` when pulling or sharing.
	Team string `yaml:"team,omitempty"`
}

// Config is the full on-disk document.
type Config struct {
	Version  int              `yaml:"version"`
	Defaults Defaults         `yaml:"defaults"`
	Groups   map[string]Group `yaml:"groups,omitempty"`
	Hosts    []Host           `yaml:"hosts"`

	path   string      // where this was loaded from / will be saved to
	loaded []byte      // the file as it was when read or last written; nil if absent
	picker RoutePicker // chooses among hosts' routes at resolve time; never saved
}

// SetRoutePicker makes Resolve consult p for hosts that have routes. The
// choice only affects how hosts are resolved, never what's saved.
func (c *Config) SetRoutePicker(p RoutePicker) { c.picker = p }

// WithRoutePicker is c with another picker, for resolving under a deadline of its
// own. It's for reading: the two share everything else.
func (c *Config) WithRoutePicker(p RoutePicker) *Config {
	cp := *c
	cp.picker = p
	return &cp
}

// DefaultPath returns the standard location for meterm's config file,
// honoring $METERM_CONFIG for overrides (tests, alternate profiles).
func DefaultPath() (string, error) {
	if p := os.Getenv("METERM_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "meterm", "hosts.yaml"), nil
}

// Load reads the config at path, creating an empty one if it doesn't exist yet.
func Load(path string) (*Config, error) {
	data, err := safefile.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := &Config{Version: 1, Defaults: Defaults{Port: 22}, path: path}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	cfg.path = path
	cfg.loaded = data
	return &cfg, nil
}

// ErrChanged is what Save returns when the file isn't what this config was
// loaded from any more.
var ErrChanged = errors.New("the host file changed while this was running, so nothing was saved; try again")

// lock serializes writers across processes: two terminals, or a background team
// sync next to a command. Held only around reading and writing the file. Team
// commands take the team's sync lock first and this one second, never the
// other way round.
func (c *Config) lock() (func(), error) {
	unlock, err := lockfile.Acquire(c.path+".lock", 15*time.Second, 10*time.Millisecond)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil, fmt.Errorf("%s is locked by another meterm (%s.lock)", c.path, c.path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock config: %w", err)
	}
	return unlock, nil
}

// Update is how a command changes the file: under the lock, it reads the file
// as it is now, runs fn on that, and writes the result, so nobody else's change
// is lost. c then holds what was written; if fn fails nothing is written and c
// stays as it was. fn must work on the config it's given, not on c or on hosts
// found in c, and shouldn't prompt or use the network: everyone else waits.
func (c *Config) Update(fn func(*Config) error) error {
	if c.path == "" {
		return fmt.Errorf("config has no associated path")
	}
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock()
	fresh, err := Load(c.path)
	if err != nil {
		return err
	}
	fresh.picker = c.picker
	if err := fn(fresh); err != nil {
		return err
	}
	if err := fresh.write(); err != nil {
		return err
	}
	*c = *fresh
	return nil
}

// Save writes c as it stands, unless the file changed since it was loaded, in
// which case it returns ErrChanged and leaves the file alone. It's for team
// sync, which works on one config for as long as the network takes and so can't
// run inside Update. Everything else should use Update.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no associated path")
	}
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock()
	now, err := safefile.ReadFile(c.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	if !bytes.Equal(now, c.loaded) {
		return ErrChanged
	}
	return c.write()
}

// write replaces the file in one step, so a crash leaves the old file or the
// new one. A temp file of its own means writers can't trip over each other's.
func (c *Config) write() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if c.loaded != nil && bytes.Equal(data, c.loaded) {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = safefile.Rename(tmp, c.path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write config: %w", err)
	}
	// So the rename itself survives a power cut. Windows can't sync a
	// directory, and doesn't need to.
	if d, err := os.Open(filepath.Dir(c.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	c.loaded = data
	return nil
}

// Path reports where this config was loaded from / saves to.
func (c *Config) Path() string { return c.path }

// Find returns the host with the given name, if any.
func (c *Config) Find(name string) (*Host, int) {
	for i := range c.Hosts {
		if c.Hosts[i].Name == name {
			return &c.Hosts[i], i
		}
	}
	return nil, -1
}

// Target finds the host a connection-style argument names: a saved host's
// name, or user@name to log in to it as another account. The user@ form
// returns a copy with User set — it never changes the saved host — and
// only applies when no saved host is literally called that. The index is
// -1 when nothing matches, and err says why a user@ form was refused.
func (c *Config) Target(arg string) (*Host, int, error) {
	if h, i := c.Find(arg); i >= 0 {
		return h, i, nil
	}
	at := strings.LastIndex(arg, "@")
	if at <= 0 {
		return nil, -1, nil
	}
	h, i := c.Find(arg[at+1:])
	if i < 0 {
		return nil, -1, nil
	}
	user := arg[:at]
	if err := CheckUser(user); err != nil {
		return nil, -1, err
	}
	cp := *h
	cp.User = user
	return &cp, i, nil
}

// Logins is every account people log in to h as: its user, then its other
// users, without repeats.
func (h Host) Logins() []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range append([]string{h.User}, h.Users...) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// Upsert adds h, or replaces the existing host with the same name.
func (c *Config) Upsert(h Host) {
	if _, i := c.Find(h.Name); i >= 0 {
		c.Hosts[i] = h
		return
	}
	c.Hosts = append(c.Hosts, h)
}

// Remove deletes the host with the given name. Reports whether it existed.
func (c *Config) Remove(name string) bool {
	_, i := c.Find(name)
	if i < 0 {
		return false
	}
	c.Hosts = append(c.Hosts[:i], c.Hosts[i+1:]...)
	return true
}

// SortedHosts returns hosts sorted by name for stable, predictable output.
func (c *Config) SortedHosts() []Host {
	hosts := make([]Host, len(c.Hosts))
	copy(hosts, c.Hosts)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts
}

// SortedGroupNames returns group names in alphabetical order.
func (c *Config) SortedGroupNames() []string {
	names := make([]string, 0, len(c.Groups))
	for name := range c.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
