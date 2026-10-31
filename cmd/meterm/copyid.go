package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/history"
	"meterm/internal/sshkey"
	"meterm/internal/sshrun"
)

func newCopyIDCmd() *cobra.Command {
	var identity, via string
	var windows, yes bool
	cmd := &cobra.Command{
		Use:   "copy-id <name|user@name> [--via <account>]",
		Short: "Install your public key on a saved host (jump-host aware, unlike plain ssh-copy-id)",
		Long: "Appends your public key to the host's ~/.ssh/authorized_keys, using the same\n" +
			"resolved user/port/jump-host meterm already knows about — no re-typing ssh-copy-id's\n" +
			"own -i/-p/-J flags by hand. It's idempotent (safe to run again) and will still prompt\n" +
			"for the account's current password if that's the only way in so far.\n\n" +
			"Which key gets installed: --identity, else the host's configured identity_file, else\n" +
			"the first of ~/.ssh/id_ed25519, id_ecdsa, id_rsa found. No key yet? Run `meterm keygen`.\n\n" +
			"A Windows OpenSSH server is recognised from the version it announces (checked\n" +
			"without logging in): the key goes to %USERPROFILE%\\.ssh\\authorized_keys, or for an\n" +
			"administrator to %ProgramData%\\ssh\\administrators_authorized_keys with the\n" +
			"permissions sshd insists on. --windows forces that when the check can't tell.\n\n" +
			"--via: for a server with password logins off (as it should be), plain copy-id\n" +
			"has no way in for a new account. `copy-id bs@web1 --via dev` logs in as dev\n" +
			"(who needs sudo) and writes the key as bs itself, then checks sshd would let\n" +
			"bs in at all: AllowUsers, AllowGroups, DenyUsers, DenyGroups. If AllowUsers is\n" +
			"in the way it asks to add bs (--yes: without asking), backs the files up, runs\n" +
			"sshd -t before reloading and puts them back if that fails. Then it tries the\n" +
			"login, and adds bs to the host's users.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
			}
			if via != "" {
				return copyIDVia(cfg, idx, h.User, via, identity, yes)
			}
			r := cfg.Resolve(*h)

			pubPath, err := sshkey.ResolvePublicKey(identity, r.IdentityFile)
			if err != nil {
				return err
			}
			pubKey, err := os.ReadFile(pubPath)
			if err != nil {
				return fmt.Errorf("reading public key %s: %w", pubPath, err)
			}

			c := sshrun.CopyIDCommand(r, string(pubKey))
			if windows || (r.Err == nil && isWindowsServer(r)) {
				if c, err = sshrun.CopyIDCommandWindows(r, string(pubKey)); err != nil {
					return fmt.Errorf("%s: %w", pubPath, err)
				}
				fmt.Printf("Installing %s on %s (%s, a Windows server)...\n", pubPath, args[0], r.Address)
			} else {
				fmt.Printf("Installing %s on %s (%s)...\n", pubPath, args[0], r.Address)
			}
			var stderr bytes.Buffer
			c.Stderr = io.MultiWriter(os.Stderr, &stderr)
			if err := c.Run(); err != nil {
				if strings.Contains(stderr.String(), "Permission denied (publickey") {
					fmt.Fprintf(os.Stderr, "\n%s doesn't take passwords, so copy-id can't get in as %s. Through an account that can log in and has sudo:\n  meterm copy-id %s --via <account>\n", cfg.Hosts[idx].Name, h.User, args[0])
				}
				if ee, ok := err.(*exec.ExitError); ok {
					finish(ee.ExitCode())
				}
				return err
			}
			if cfg.Hosts[idx].User == h.User { // ⚷ means the host's own login; another user's key doesn't count
				_ = history.MarkKeyInstalledAndSave(h.Name) // best-effort; don't fail a successful install over it
			}
			fmt.Printf("Key installed on %q.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "private key whose public half (<path>.pub) to install")
	cmd.Flags().BoolVar(&windows, "windows", false, "the host is a Windows OpenSSH server (normally detected)")
	cmd.Flags().StringVar(&via, "via", "", "install through this account (it needs sudo): for servers that don't take passwords")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --via: add the account to sshd's AllowUsers without asking, if that's what's in the way")
	return cmd
}

// copyIDVia is copy-id --via: see the command's help.
func copyIDVia(cfg *config.Config, idx int, target, via, identity string, yes bool) error {
	host := cfg.Hosts[idx]
	if target == "" {
		return fmt.Errorf("say which account to install for: meterm copy-id <account>@%s --via %s", host.Name, via)
	}
	viaHost, _, err := cfg.Target(via + "@" + host.Name)
	if err != nil {
		return err
	}
	r := cfg.Resolve(*viaHost)
	if r.Err != nil {
		return r.Err
	}
	if isWindowsServer(r) {
		return fmt.Errorf("--via is for Linux and macOS servers; on Windows, copy-id %s@%s installs keys for the account itself", target, host.Name)
	}
	pubPath, err := sshkey.ResolvePublicKey(identity, r.IdentityFile)
	if err != nil {
		return err
	}
	pubKey, err := os.ReadFile(pubPath)
	if err != nil {
		return fmt.Errorf("reading public key %s: %w", pubPath, err)
	}
	tty := isatty.IsTerminal(os.Stdin.Fd())
	if !tty && !yes {
		fmt.Fprintln(os.Stderr, "meterm: no terminal: if AllowUsers is in the way it won't be changed (pass --yes to allow that)")
	}
	c, err := sshrun.CopyIDViaCommand(r, target, string(pubKey), yes, tty)
	if err != nil {
		return err
	}
	fmt.Printf("Installing %s for %s on %s, through %s...\n", pubPath, target, host.Name, via)
	var out bytes.Buffer
	c.Stdout = io.MultiWriter(&out, &markerFilter{w: os.Stdout})
	if err := c.Run(); err != nil {
		if msg := viaMarker(out.String(), sshrun.ViaMarkFailure); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("installing through %s: %w", via, err)
	}
	key, access := viaMarker(out.String(), sshrun.ViaMarkKey), viaMarker(out.String(), sshrun.ViaMarkAccess)
	switch key {
	case "installed":
		fmt.Printf("Key installed for %s.\n", target)
	case "present":
		fmt.Printf("The key was already there for %s.\n", target)
	default:
		return fmt.Errorf("couldn't tell whether the key was installed (no answer from the script)")
	}
	kind, why, _ := strings.Cut(access, ":")
	switch kind {
	case "denied":
		return fmt.Errorf("sshd still won't let %s in: %s. Change that on the server if it's meant to log in", target, why)
	case "groups":
		return fmt.Errorf("sshd still won't let %s in: %s. Add %s to one of those groups (sudo usermod -aG <group> %s)", target, why, target, target)
	case "declined":
		return fmt.Errorf("left sshd's AllowUsers as it is, so %s can't log in yet", target)
	case "unknown":
		fmt.Printf("Couldn't check sshd's settings: %s.\n", why)
	}

	// The proof: log in as the account with the key.
	h := host
	h.User = target
	test := sshrun.SSH(cfg.Resolve(h), "-o", "BatchMode=yes", "true")
	test.Stdin = nil
	if b, err := test.CombinedOutput(); err != nil {
		return fmt.Errorf("the key is installed, but logging in as %s still fails: %s", target, strings.TrimSpace(string(b)))
	}
	fmt.Printf("Logged in as %s@%s with the key: it works.\n", target, host.Name)
	if !slices.Contains(host.Logins(), target) {
		err := cfg.Update(func(c *config.Config) error {
			if h, i := c.Find(host.Name); i >= 0 && !slices.Contains(h.Logins(), target) {
				h.Users = append(h.Users, target)
			}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("Added %s to %s's users (meterm connect %s@%s).\n", target, host.Name, target, host.Name)
		if host.Team != "" {
			fmt.Println("It's a team host: `meterm team push` shares that with the team.")
		}
	}
	return nil
}

// viaMarker is the value after marker on the script's output, if any.
func viaMarker(out, marker string) string {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if i := strings.Index(l, marker); i >= 0 {
			return strings.TrimSpace(l[i+len(marker):])
		}
	}
	return ""
}
