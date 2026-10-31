package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/team"
)

func teamGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Add or forget a team, kept in a Git repository you control",
		Long: "A Git team lives in a repository the team already has (GitHub, GitLab, Gitea,\n" +
			"a bare repo over SSH). Whoever can clone it sees the team; whoever can push\n" +
			"changes it — access is the Git host's, and every change is a commit, so\n" +
			"`git log` is the team's history. No meterm account is needed.\n\n" +
			"  meterm team git add ACME git@github.com:acme/meterm-team.git\n" +
			"  meterm team share --tag acme\n" +
			"  meterm team invite someone@example.com --role admin   # then grant repo access\n\n" +
			"meterm commits as your `git config user.email`. Roles in meterm-team.yaml are\n" +
			"honoured by meterm but enforced only by what the Git host lets each person push.",
	}
	cmd.AddCommand(teamGitAddCmd(), teamGitForgetCmd())
	return cmd
}

func teamGitAddCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "add <name> <repo-url>",
		Short: "Use a Git repository as a team (creates meterm-team.yaml if it's empty)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := team.Dir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			team.ForgetCloud(dir)
			remote := gitRemote(args[1])
			gt := team.GitTeam{Name: args[0], Remote: remote, Branch: branch}
			name, err := team.InitGitTeam(cmd.Context(), dir, gt)
			if err != nil {
				return err
			}
			gt.Name = name
			id := team.GitTeamID(remote)
			moved := 0
			_, err = updateTeamState(dir, func(st *team.State) error {
				if st.GitTeams == nil {
					st.GitTeams = map[string]team.GitTeam{}
				}
				st.GitTeams[id] = gt
				st.Current = id
				// A team moved here from the cloud: bring this machine's copies along.
				g, err := team.OpenGitStore(cmd.Context(), dir, gt)
				if err != nil {
					return nil
				}
				from, err := g.MigratedFrom(cmd.Context())
				if err != nil || from == "" {
					return nil
				}
				hosts, err := g.ListHosts(cmd.Context(), id)
				if err != nil {
					return err
				}
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				return cfg.Update(func(c *config.Config) error {
					moved = team.Adopt(c, st, from, id, hosts)
					return nil
				})
			})
			if err != nil {
				return err
			}
			fmt.Printf("Team %q is kept in %s, and is now the current team.\n", name, remote)
			if moved > 0 {
				fmt.Printf("Moved %d of your hosts over from the cloud team it was migrated from.\n", moved)
			}
			fmt.Println("Next: meterm team pull   (or: meterm team share <host>…)")
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "main", "branch that holds the team")
	return cmd
}

func teamGitForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <name>",
		Short: "Stop using a Git team on this machine (the repository is untouched)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := team.Dir()
			if err != nil {
				return err
			}
			var forgot string
			if _, err := updateTeamState(dir, func(st *team.State) error {
				for id, gt := range st.GitTeams {
					if gt.Name == args[0] || id == args[0] {
						delete(st.GitTeams, id)
						if st.Current == id {
							st.Current = ""
						}
						forgot = gt.Name
						return nil
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if forgot != "" {
				fmt.Printf("Forgot %q on this machine. Its hosts stay in your list; `meterm rm` any you no longer need.\n", forgot)
				return nil
			}
			return fmt.Errorf("no Git team called %q", args[0])
		},
	}
}

// gitRemote keeps a local repository path working from any directory.
func gitRemote(remote string) string {
	if abs, err := filepath.Abs(remote); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			return abs
		}
	}
	return remote
}
