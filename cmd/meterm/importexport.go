package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/inventory"
	"meterm/internal/sshconfig"
)

func newImportCmd() *cobra.Command {
	var all, yes bool
	cmd := &cobra.Command{
		Use:   "import [path-to-ssh-config]",
		Short: "Import hosts from an existing ~/.ssh/config",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args2SSHConfigPath(args)
			imported, err := sshconfig.ParseFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			if len(imported) == 0 {
				fmt.Println("No importable single-name Host entries found (wildcards and multi-pattern hosts are skipped).")
				return nil
			}
			return importHosts(imported, all || yes)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "import everything without asking")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "same as --all")
	cmd.AddCommand(newImportTailscaleCmd(), newImportAnsibleCmd())
	return cmd
}

// importHosts adds hosts that aren't in meterm yet, asking about each one
// unless all. A host whose address is already saved under another name is
// skipped too, so importing a tailnet doesn't duplicate hosts saved by hand.
func importHosts(imported []config.Host, all bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(os.Stdin)
	var ask inventory.Ask
	if !all {
		ask = func(h config.Host) (bool, bool) {
			fmt.Printf("Import %q (%s)? [Y/n] ", h.Name, h.Address)
			resp, err := reader.ReadString('\n')
			if err != nil && resp == "" {
				fmt.Println("\nNo answer (end of input); stopping. Use --yes to import without asking.")
				return false, true
			}
			resp = strings.ToLower(strings.TrimSpace(resp))
			return resp != "n" && resp != "no", false
		}
	}
	out := inventory.Merge(cfg, imported, ask)
	for _, o := range out {
		switch o.Action {
		case "exists":
			fmt.Printf("skip %-20s (already in meterm)\n", o.Host.Name)
		case "duplicate":
			fmt.Printf("skip %-20s (%s is already saved as %s)\n", o.Host.Name, o.Host.Address, o.Other)
		case "added":
			fmt.Printf("added %-20s %s\n", o.Host.Name, o.Host.Address)
		}
	}
	if inventory.Added(out) == 0 {
		return nil
	}
	// The questions can take a while; what they added goes onto the file as
	// it is now, skipping any name that has appeared there since.
	added := 0
	err = cfg.Update(func(c *config.Config) error {
		for _, o := range out {
			if _, idx := c.Find(o.Host.Name); o.Action == "added" && idx < 0 {
				c.Upsert(o.Host)
				added++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("Imported %d host(s) into %s.\n", added, cfg.Path())
	return nil
}

func newImportTailscaleCmd() *cobra.Command {
	var o inventory.TailscaleOptions
	var all bool
	cmd := &cobra.Command{
		Use:   "tailscale",
		Short: "Import the machines in your tailnet (uses the local tailscale CLI)",
		Long: "Reads `tailscale status --json` on this machine — no API key needed — and\n" +
			"offers each device as a host named after its machine name, at its 100.x\n" +
			"address, tagged tailscale, os:<os> and its ACL tags. Phones and tablets\n" +
			"are left out unless --all-os.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := inventory.TailscaleStatus()
			if err != nil {
				return err
			}
			hosts, err := inventory.FromTailscale(raw, o)
			if err != nil {
				return fmt.Errorf("reading tailscale status: %w", err)
			}
			if len(hosts) == 0 {
				fmt.Println("No matching devices in the tailnet.")
				return nil
			}
			return importHosts(hosts, all)
		},
	}
	cmd.Flags().StringVarP(&o.User, "user", "u", "", "login user for the imported hosts")
	cmd.Flags().StringArrayVar(&o.Tags, "tag", nil, "only devices with this ACL tag, e.g. tag:server (repeatable)")
	cmd.Flags().BoolVar(&o.OnlineOnly, "online", false, "only devices online right now")
	cmd.Flags().BoolVar(&o.AllOS, "all-os", false, "include iOS and Android devices")
	cmd.Flags().BoolVarP(&all, "yes", "y", false, "import everything without asking")
	return cmd
}

func newImportAnsibleCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ansible <inventory-file>",
		Short: "Import hosts from an Ansible inventory (INI or YAML)",
		Long: "Each host keeps its inventory name. ansible_host, ansible_user, ansible_port,\n" +
			"ansible_ssh_private_key_file and a -J/ProxyJump in ansible_ssh_common_args\n" +
			"carry over, with group vars applied under host vars; group names become\n" +
			"tags. Host ranges like web[01:20] are listed but not expanded.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			hosts, skipped, err := inventory.FromAnsible(data)
			if err != nil {
				return fmt.Errorf("reading %s: %w", args[0], err)
			}
			for _, s := range skipped {
				fmt.Printf("skip %-20s (host ranges aren't expanded; add them by hand)\n", s)
			}
			if len(hosts) == 0 {
				fmt.Println("No hosts found.")
				return nil
			}
			return importHosts(hosts, all)
		},
	}
	cmd.Flags().BoolVarP(&all, "yes", "y", false, "import everything without asking")
	return cmd
}

func args2SSHConfigPath(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}

func newExportCmd() *cobra.Command {
	var writeInclude bool
	var format, out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export your meterm hosts as an ssh_config fragment or an Ansible inventory",
		Long: "Renders every meterm host as a standard ssh_config Host block, so other tools\n" +
			"that shell out to ssh (scp, rsync, git, rsync, VS Code Remote-SSH, ...) can use\n" +
			"the same names. Re-run after any change to keep it in sync.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cfg.SetRoutePicker(nil) // export each host's own address
			switch format {
			case "ansible":
				data, err := inventory.ToAnsible(cfg)
				if err != nil {
					return err
				}
				if out == "" || out == "-" {
					_, err = os.Stdout.Write(data)
					return err
				}
				if err := os.WriteFile(out, data, 0o644); err != nil {
					return err
				}
				fmt.Printf("Wrote %s (%d hosts). Use it with: ansible -i %s all -m ping\n", out, len(cfg.Hosts), out)
				return nil
			case "ssh":
			default:
				return fmt.Errorf("--format must be ssh or ansible")
			}
			path, err := sshconfig.Write(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("Wrote %s\n", path)

			if writeInclude {
				added, err := sshconfig.EnsureIncluded(path)
				if err != nil {
					return err
				}
				if added {
					fmt.Println("Added an Include line to ~/.ssh/config.")
				} else {
					fmt.Println("~/.ssh/config already includes it.")
				}
			} else {
				fmt.Printf("Add this line at the TOP of ~/.ssh/config to use these hosts with plain `ssh`:\n\n  Include %s\n\n", path)
				fmt.Println("Or re-run with --write-include to have meterm add it for you.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&writeInclude, "write-include", false, "also add an Include line to ~/.ssh/config")
	cmd.Flags().StringVar(&format, "format", "ssh", "ssh (an ssh_config fragment) or ansible (a YAML inventory)")
	cmd.Flags().StringVarP(&out, "out", "o", "", "with --format ansible: write here instead of stdout")
	return cmd
}
