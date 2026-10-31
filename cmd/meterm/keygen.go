package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshkey"
)

func newKeygenCmd() *cobra.Command {
	var path, comment, setFor string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate a new SSH key (ed25519), optionally set it as a host's identity",
		Long: "Wraps `ssh-keygen -t ed25519` so setting up a brand-new host doesn't need leaving\n" +
			"meterm. Pass --for to also save the new key as that host's identity_file, so the\n" +
			"very next step (`meterm copy-id <name>`) already knows which key to install.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				path = filepath.Join(home, ".ssh", "id_ed25519")
			} else {
				path = sshkey.ExpandHome(path)
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists — pass --file to choose a different path", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}

			if err := runInherited(sshkey.GenerateCommand(path, comment)); err != nil {
				return err
			}
			fmt.Printf("\nGenerated %s (public key: %s.pub)\n", path, path)

			if setFor == "" {
				return nil
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			err = cfg.Update(func(c *config.Config) error {
				h, idx := c.Find(setFor)
				if idx < 0 {
					return fmt.Errorf("no such host: %s (the key was still generated at %s)", setFor, path)
				}
				h.IdentityFile = sshkey.ContractHome(path)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Set it as the identity for %q. Next: meterm copy-id %s\n", setFor, setFor)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "file", "", "where to save the new key (default ~/.ssh/id_ed25519)")
	cmd.Flags().StringVar(&comment, "comment", "", "comment embedded in the key")
	cmd.Flags().StringVar(&setFor, "for", "", "also set this as the identity_file for a saved host")
	return cmd
}
