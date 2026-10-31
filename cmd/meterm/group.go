package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshkey"
)

func newGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage host groups (shared user/key/jump host for an environment)",
	}
	cmd.AddCommand(newGroupAddCmd(), newGroupListCmd(), newGroupRmCmd())
	return cmd
}

func newGroupAddCmd() *cobra.Command {
	var user, identity, proxyJump, tags, env string
	var port int
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create or update a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := config.NormalizeEnv(env)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			err = cfg.Update(func(c *config.Config) error {
				if c.Groups == nil {
					c.Groups = map[string]config.Group{}
				}
				g := c.Groups[args[0]]
				if cmd.Flags().Changed("user") {
					g.User = user
				}
				if cmd.Flags().Changed("port") {
					g.Port = port
				}
				if cmd.Flags().Changed("identity") {
					g.IdentityFile = sshkey.ContractHome(identity)
				}
				if cmd.Flags().Changed("proxy-jump") {
					g.ProxyJump = proxyJump
				}
				if cmd.Flags().Changed("tags") {
					g.Tags = config.SplitTags(tags)
				}
				if cmd.Flags().Changed("env") {
					g.Env = env
				}
				c.Groups[args[0]] = g
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Saved group %q.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "default SSH user for this group")
	cmd.Flags().IntVar(&port, "port", 0, "default SSH port for this group")
	cmd.Flags().StringVar(&identity, "identity", "", "default identity file for this group")
	cmd.Flags().StringVar(&proxyJump, "proxy-jump", "", "default jump host for this group")
	cmd.Flags().StringVar(&tags, "tags", "", "comma-separated tags applied to every host in this group")
	cmd.Flags().StringVar(&env, "env", "", "environment of this group's hosts: "+config.EnvList)
	return cmd
}

func newGroupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			showEnv := false
			for _, g := range cfg.Groups {
				showEnv = showEnv || g.Env != ""
			}
			header := "NAME\tUSER\tPORT\tIDENTITY\tPROXY-JUMP"
			if showEnv {
				header += "\tENV"
			}
			fmt.Fprintln(w, header)
			for _, name := range cfg.SortedGroupNames() {
				g := cfg.Groups[name]
				port := ""
				if g.Port != 0 {
					port = strconv.Itoa(g.Port)
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", name, g.User, port, g.IdentityFile, g.ProxyJump)
				if showEnv {
					line += "\t" + cfg.Env(config.Host{Group: name})
				}
				fmt.Fprintln(w, line)
			}
			return w.Flush()
		},
	}
}

func newGroupRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Delete a group (hosts referencing it keep their name but lose inherited settings)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return cfg.Update(func(c *config.Config) error {
				if _, ok := c.Groups[args[0]]; !ok {
					return fmt.Errorf("no such group: %s", args[0])
				}
				delete(c.Groups, args[0])
				return nil
			})
		},
	}
}
