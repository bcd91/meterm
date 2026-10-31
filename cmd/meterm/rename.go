package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/history"
)

func newRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a saved host, keeping its settings, history and jump-host references",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := args[0], args[1]
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			var wasTeam string
			var n int
			err = cfg.Update(func(c *config.Config) error {
				h, i := c.Find(oldName)
				if i < 0 {
					return fmt.Errorf("no such host: %s", oldName)
				}
				if _, j := c.Find(newName); j >= 0 {
					return fmt.Errorf("a host called %s already exists", newName)
				}
				if strings.ContainsAny(newName, " ,:") {
					return fmt.Errorf("host names can't contain spaces, commas or colons")
				}
				wasTeam = h.Team
				h.Name = newName
				n = renameJumps(c, oldName, newName)
				return nil
			})
			if err != nil {
				return err
			}

			// History is keyed by name; carry it over (best-effort).
			if p, err := history.Path(); err == nil {
				if hist, err := history.Load(p); err == nil {
					if e, ok := hist[oldName]; ok {
						hist[newName] = e
						delete(hist, oldName)
						_ = hist.Save(p)
					}
				}
			}

			fmt.Printf("Renamed %s to %s.", oldName, newName)
			if n > 0 {
				fmt.Printf(" Updated %d jump-host reference(s).", n)
			}
			fmt.Println()
			if wasTeam != "" {
				fmt.Fprintf(os.Stderr, "%s is a team host: `meterm team push` will rename it for everyone (remove %s, add %s).\n", newName, oldName, newName)
			}
			return nil
		},
	}
}

// renameJumps rewrites proxy_jump chains (comma-separated) in hosts and
// groups that name oldName, returning how many were changed.
func renameJumps(cfg *config.Config, oldName, newName string) int {
	n := 0
	fix := func(spec string) string {
		if spec == "" {
			return spec
		}
		parts := strings.Split(spec, ",")
		changed := false
		for i, p := range parts {
			if strings.TrimSpace(p) == oldName {
				parts[i] = newName
				changed = true
			}
		}
		if !changed {
			return spec
		}
		n++
		return strings.Join(parts, ",")
	}
	for i := range cfg.Hosts {
		cfg.Hosts[i].ProxyJump = fix(cfg.Hosts[i].ProxyJump)
	}
	for name, g := range cfg.Groups {
		g.ProxyJump = fix(g.ProxyJump)
		cfg.Groups[name] = g
	}
	return n
}
