package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/relay"
	"meterm/internal/route"
)

// pendingInvite is an invite waiting for its reply. The port and relay
// are taken from here when accepting, never from the reply.
type pendingInvite struct {
	Relay   string    `json:"relay"`
	Port    int       `json:"port"`
	User    string    `json:"user,omitempty"`
	Created time.Time `json:"created"`
}

func invitesPath() (string, error) {
	p, err := resolveCfgPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "relay-invites.json"), nil
}

func loadInvites() (map[string]pendingInvite, error) {
	m := map[string]pendingInvite{}
	p, err := invitesPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %v", p, err)
	}
	return m, nil
}

func saveInvites(m map[string]pendingInvite) error {
	p, err := invitesPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// adminPublicKey is the public key the invite installs: --key, or the
// first of the usual ones in ~/.ssh.
func adminPublicKey(path string) (string, string, error) {
	cands := []string{path}
	if path == "" {
		home, _ := os.UserHomeDir()
		cands = []string{filepath.Join(home, ".ssh", "id_ed25519.pub"), filepath.Join(home, ".ssh", "id_ecdsa.pub"), filepath.Join(home, ".ssh", "id_rsa.pub")}
	}
	for _, c := range cands {
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			continue
		}
		return f[0] + " " + f[1] + " meterm-admin", c, nil
	}
	return "", "", fmt.Errorf("no public key found; pass --key ~/.ssh/<key>.pub")
}

func newRelayInviteCmd() *cobra.Command {
	var via, user, keyPath, out string
	var sshPort int
	cmd := &cobra.Command{
		Use:   "invite <name> --via <relay>",
		Short: "Add a machine you can't reach yet: a script for someone at it to run once",
		Long: "For a machine nobody can connect to (behind NAT, no address) that can reach the\n" +
			"relay. This writes a script for someone at that machine to run with sudo. It\n" +
			"installs your public key for the login user (by default, whoever runs sudo),\n" +
			"creates the tunnel's key there — the private half never leaves the machine —\n" +
			"starts the tunnel, and prints one line to send back to you for\n" +
			"`meterm relay accept`. The script holds no secrets.\n\n" +
			"The relay must be set up first (meterm relay init).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if via == "" {
				return fmt.Errorf("say which relay: --via <host>")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, i := cfg.Find(name); i >= 0 {
				return fmt.Errorf("%s is already a saved host; for one you can reach, use `meterm relay enroll`", name)
			}
			rh, ri := cfg.Find(via)
			if ri < 0 {
				return fmt.Errorf("no such host: %s", via)
			}
			cands, known, err := relayEndpoints(cfg, *rh)
			if err != nil {
				return err
			}
			key, keyFile, err := adminPublicKey(keyPath)
			if err != nil {
				return err
			}
			invites, err := loadInvites()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			ready, err := runScript(ctx, cfg.Resolve(*rh), "id -u "+relay.User+" >/dev/null 2>&1 && echo ready || echo missing")
			if err == nil && !strings.Contains(ready, "ready") {
				cancel()
				return fmt.Errorf("%s isn't set up as a relay yet: run `meterm relay init %s` first", rh.Name, rh.Name)
			}
			lst, err := runScript(ctx, cfg.Resolve(*rh), relay.ListeningScript)
			cancel()
			if err != nil {
				return fmt.Errorf("checking ports on %s: %w", rh.Name, err)
			}
			used := relay.UsedPorts(cfg)
			for _, inv := range invites {
				used[inv.Port] = true
			}
			port := 0
			if old, ok := invites[name]; ok && strings.EqualFold(old.Relay, rh.Name) {
				port = old.Port // inviting again: keep the port
			} else {
				port = relay.FreePort(used, relay.ParsePorts(lst))
			}
			script, err := relay.InviteScript(relay.InviteParams{
				NodeParams: relay.NodeParams{Relay: rh.Name, Candidates: cands, KnownHosts: known, Port: port, LocalPort: sshPort},
				Name:       name, AdminKey: key, User: user,
			})
			if err != nil {
				return err
			}
			if out == "" {
				out = "meterm-invite-" + name + ".sh"
			}
			if err := os.WriteFile(out, []byte(script), 0o644); err != nil {
				return err
			}
			invites[name] = pendingInvite{Relay: rh.Name, Port: port, User: user, Created: time.Now().UTC()}
			if err := saveInvites(invites); err != nil {
				return err
			}
			who := "whoever runs it with sudo"
			if user != "" {
				who = user
			}
			fmt.Printf("Wrote %s (no secrets in it).\n\n", out)
			fmt.Println("Send it to someone at the machine. They run:")
			fmt.Printf("    sudo sh %s\n", filepath.Base(out))
			fmt.Printf("It lets your key (%s) log in as %s, and starts a tunnel to %s.\n", keyFile, who, rh.Name)
			fmt.Println("It prints a line starting with `meterm relay accept`; run that line here.")
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "the relay (already set up with `meterm relay init`)")
	cmd.Flags().StringVar(&user, "user", "", "login user on the machine (default: whoever runs the script with sudo)")
	cmd.Flags().StringVar(&keyPath, "key", "", "your public key to install (default: ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	cmd.Flags().StringVarP(&out, "out", "o", "", "where to write the script (default: meterm-invite-<name>.sh)")
	cmd.Flags().IntVar(&sshPort, "ssh-port", 22, "the machine's own sshd port")
	return cmd
}

func newRelayAcceptCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "accept <name> <reply>",
		Short: "Finish adding an invited machine, with the line its invite script printed",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			invites, err := loadInvites()
			if err != nil {
				return err
			}
			inv, ok := invites[name]
			if !ok {
				return fmt.Errorf("no invite waiting for %s (meterm relay invite %s --via <relay>)", name, name)
			}
			reply, err := relay.ParseReply(args[1])
			if err != nil {
				return err
			}
			if inv.User != "" && reply.User != inv.User {
				return fmt.Errorf("the reply is for user %q, but the invite was for %q", reply.User, inv.User)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, i := cfg.Find(name); i >= 0 {
				return fmt.Errorf("%s is already a saved host", name)
			}
			rh, ri := cfg.Find(inv.Relay)
			if ri < 0 {
				return fmt.Errorf("the relay %s isn't a saved host any more", inv.Relay)
			}
			// Nothing else may already be trusted at this tunnel address.
			if have, err := route.TrustedKeys("127.0.0.1", inv.Port); err != nil {
				return err
			} else if !subsetOf(have, reply.HostKeys) {
				return fmt.Errorf("your known_hosts already trusts other keys for %s; if the machine that used that port is gone, remove them with:\n  ssh-keygen -R '%s'", route.KnownName("127.0.0.1", inv.Port), route.KnownName("127.0.0.1", inv.Port))
			}

			fmt.Printf("%s, through %s (localhost:%d), logging in as %s.\n", name, rh.Name, inv.Port, reply.User)
			fmt.Println("Its host keys, as the reply says:")
			for _, k := range reply.HostKeys {
				fmt.Println("  " + fingerprint(k))
			}
			fmt.Println("Before accepting, ask the person at the machine to read out its fingerprint:")
			fmt.Println("  ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub")
			fmt.Println("If it doesn't match, the reply was changed on the way: don't accept it.")
			if !yes && !confirm("Fingerprint checked; accept?") {
				fmt.Println("Nothing changed.")
				return nil
			}

			add, err := relay.AddKeyScript(reply.TunnelKey, name, inv.Port)
			if err != nil {
				return err
			}
			out, err := runAsRoot(cfg, *rh, add)
			if err := scriptResult(rh.Name, out, err); err != nil {
				return err
			}
			if err := route.AddKnownHosts("127.0.0.1", inv.Port, missing(reply.HostKeys, mustTrusted(inv.Port))); err != nil {
				return err
			}
			h := config.Host{Name: name, Address: "127.0.0.1", Port: inv.Port, User: reply.User, ProxyJump: rh.Name}
			if err := h.CheckConnection(); err != nil {
				return err
			}
			err = cfg.Update(func(c *config.Config) error {
				c.Upsert(h)
				return nil
			})
			if err != nil {
				return err
			}
			delete(invites, name)
			if err := saveInvites(invites); err != nil {
				return err
			}

			fmt.Print("Waiting for the tunnel")
			up := waitListening(cfg, *rh, inv.Port, 40*time.Second)
			fmt.Println()
			if !up {
				fmt.Printf("%s is saved, but its tunnel isn't up on %s yet. On the machine: systemctl status %s\n", name, rh.Name, relay.UnitName(rh.Name))
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			saved, _ := cfg.Find(name)
			if _, err := runScript(ctx, cfg.Resolve(*saved), "true"); err != nil {
				return fmt.Errorf("the tunnel is up, but logging in failed: %w", err)
			}
			recordActivity(cfg, *saved, "relay-accept", fmt.Sprintf("via %s, port %d", rh.Name, inv.Port))
			fmt.Printf("%s is in: `meterm connect %s`. Share it with the team with `meterm team share %s`.\n", name, name, name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask (only if you've checked the fingerprint some other way)")
	return cmd
}

// subsetOf reports whether every key in have is also in want.
func subsetOf(have, want []string) bool {
	m := map[string]bool{}
	for _, k := range want {
		m[k] = true
	}
	for _, k := range have {
		if !m[k] {
			return false
		}
	}
	return true
}

func mustTrusted(port int) []string {
	ks, _ := route.TrustedKeys("127.0.0.1", port)
	return ks
}

// missing is the keys in want that have doesn't hold yet.
func missing(want, have []string) []string {
	m := map[string]bool{}
	for _, k := range have {
		m[k] = true
	}
	var out []string
	for _, k := range want {
		if !m[k] {
			out = append(out, k)
		}
	}
	return out
}

// fingerprint renders a "type base64" key as ssh-keygen -l does.
func fingerprint(key string) string {
	c := exec.Command("ssh-keygen", "-lf", "-")
	c.Stdin = strings.NewReader(key + "\n")
	out, err := c.Output()
	if err != nil {
		return key
	}
	return strings.TrimSpace(string(out))
}
