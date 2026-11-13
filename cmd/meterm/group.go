package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
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
			if _, ok := cfg.Groups[args[0]]; !ok {
				if err := config.CheckName("group name", args[0]); err != nil {
					return err
				}
			}
			var changes []envChange
			err = cfg.Update(func(c *config.Config) error {
				before := envSnapshot(c)
				defer func() { changes = envChanges(before, c) }()
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
			logEnvChanges(cfg, changes, "group "+args[0]+" env")
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
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", exactName(name), lbl(g.User), port, lbl(g.IdentityFile), lbl(g.ProxyJump))
				if showEnv {
					env := cfg.Env(config.Host{Group: name})
					if raw := cfg.EnvUnrecognised(config.Host{Group: name}); raw != "" {
						env += " (" + lbl(raw) + "?)" // a typo: taken as production, as list says
					}
					line += "\t" + env
				}
				fmt.Fprintln(w, line)
			}
			return w.Flush()
		},
	}
}

func newGroupRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Delete a group (hosts referencing it keep their name but lose inherited settings)",
		Long: "Deletes a group. Hosts in it keep the group's name but lose what they\n" +
			"inherited from it: user, port, key, jump host, tags and env. When that\n" +
			"changes a host's env (production becoming none, say), the hosts and the\n" +
			"change are listed and asked about first (y only); --yes doesn't ask, and\n" +
			"when stdin isn't a terminal it refuses without --yes. Each such host's\n" +
			"activity log records the change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Groups[name]; !ok {
				return fmt.Errorf("no such group: %s", exactName(name))
			}
			after := envSnapshot(cfg)
			delete(after.Groups, name)
			changes := envChanges(cfg, after)
			if len(changes) > 0 && !yes {
				if !stdinTerminal() {
					list := make([]string, len(changes))
					for i, ch := range changes {
						list[i] = exactName(ch.host.Name) + " (" + ch.text() + ")"
					}
					return fmt.Errorf("removing group %s changes the env of %d host(s): %s; run it in a terminal to be asked, or add --yes",
						exactName(name), len(changes), strings.Join(list, ", "))
				}
				fmt.Printf("Removing group %s changes these hosts' env:\n", exactName(name))
				for _, ch := range changes {
					fmt.Printf("  %s: %s\n", exactName(ch.host.Name), ch.text())
				}
				fmt.Print("Remove it? Only y goes. [y/N] ")
				resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if r := strings.TrimSpace(resp); r != "y" && r != "Y" {
					fmt.Println("Cancelled.")
					return nil
				}
			}
			err = cfg.Update(func(c *config.Config) error {
				if _, ok := c.Groups[name]; !ok {
					return fmt.Errorf("no such group: %s", exactName(name))
				}
				before := envSnapshot(c)
				delete(c.Groups, name)
				changes = envChanges(before, c)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Removed group %s.\n", exactName(name))
			logEnvChanges(cfg, changes, "group "+name+" removed: env")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before hosts' env changes")
	return cmd
}

// envChange is a host whose env (its own, or its group's) differs between
// two versions of the host file.
type envChange struct {
	host     config.Host
	from, to string
}

func (ch envChange) text() string {
	show := func(env string) string {
		if env == "" {
			return "none"
		}
		return env
	}
	return show(ch.from) + " → " + show(ch.to)
}

// envChanges is every host in after whose env isn't what it was in before.
func envChanges(before, after *config.Config) []envChange {
	var out []envChange
	for _, h := range after.Hosts {
		was, i := before.Find(h.Name)
		if i < 0 {
			continue
		}
		if from, to := before.Env(*was), after.Env(h); from != to {
			out = append(out, envChange{host: h, from: from, to: to})
		}
	}
	return out
}

// logEnvChanges records each change on its host: "group pay removed: env
// production → none".
func logEnvChanges(cfg *config.Config, changes []envChange, what string) {
	for _, ch := range changes {
		recordActivity(cfg, ch.host, "group", what+" "+ch.text())
	}
	if len(changes) > 0 {
		kickAutosync(false, true)
	}
}

// envSnapshot is a copy of c that Env reads the same way: its hosts and
// its groups, which later changes to c don't touch.
func envSnapshot(c *config.Config) *config.Config {
	out := &config.Config{Hosts: append([]config.Host(nil), c.Hosts...), Groups: map[string]config.Group{}}
	for k, g := range c.Groups {
		out.Groups[k] = g
	}
	return out
}
