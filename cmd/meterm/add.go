package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshkey"
)

type hostFlags struct {
	address      string
	user         string
	users        string
	port         int
	identity     string
	group        string
	tags         string
	proxyJump    string
	notes        string
	forwardLocal string
	env          string
}

func addHostFlags(cmd *cobra.Command, f *hostFlags) {
	cmd.Flags().StringVar(&f.address, "address", "", "IP or hostname to connect to")
	cmd.Flags().StringVar(&f.user, "user", "", "SSH user (blank = inherit from group/default)")
	cmd.Flags().StringVar(&f.users, "users", "", "other accounts people log in as, comma-separated (connect deploy@HOST)")
	cmd.Flags().IntVar(&f.port, "port", 0, "SSH port (blank = inherit, default 22)")
	cmd.Flags().StringVar(&f.identity, "identity", "", "path to private key")
	cmd.Flags().StringVar(&f.group, "group", "", "group to inherit settings from")
	cmd.Flags().StringVar(&f.tags, "tags", "", "comma-separated tags")
	cmd.Flags().StringVar(&f.proxyJump, "proxy-jump", "", "jump host: another saved host name, or user@host")
	cmd.Flags().StringVar(&f.notes, "notes", "", "free-text note")
	cmd.Flags().StringVar(&f.forwardLocal, "forward-local", "", "local port forward as LOCAL_PORT:REMOTE_HOST:REMOTE_PORT")
	cmd.Flags().StringVar(&f.env, "env", "", "environment: "+config.EnvList+" (blank = inherit from group)")
}

func newAddCmd() *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:   "add [name]",
		Short: "Add a new host",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h := &config.Host{}
			if len(args) == 1 {
				h.Name = args[0]
			}
			if err := applyFlags(h, f, cmd.Flags()); err != nil {
				return err
			}

			if h.Name == "" || h.Address == "" {
				if err := runAddWizard(cfg, nonEmptyHost(h)); err != nil {
					return err
				}
				return nil
			}
			err = cfg.Update(func(c *config.Config) error {
				if _, exists := c.Find(h.Name); exists >= 0 {
					return fmt.Errorf("host %q already exists (use `meterm edit %s`)", h.Name, h.Name)
				}
				c.Upsert(*h)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Added %q (%s).\n", h.Name, cfg.Path())
			return nil
		},
	}
	addHostFlags(cmd, &f)
	return cmd
}

func newEditCmd() *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit an existing host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx := cfg.Find(args[0])
			if idx < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			if !cmd.Flags().Changed("address") && !cmd.Flags().Changed("user") &&
				!cmd.Flags().Changed("port") && !cmd.Flags().Changed("identity") && !cmd.Flags().Changed("users") &&
				!cmd.Flags().Changed("group") && !cmd.Flags().Changed("tags") &&
				!cmd.Flags().Changed("proxy-jump") && !cmd.Flags().Changed("notes") && !cmd.Flags().Changed("env") {
				if err := runAddWizard(cfg, h); err != nil {
					return err
				}
				teamEditHint(*h)
				return nil
			}
			err = cfg.Update(func(c *config.Config) error {
				var i int
				h, i = c.Find(args[0])
				if i < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				return applyFlags(h, f, cmd.Flags())
			})
			if err != nil {
				return err
			}
			fmt.Printf("Updated %q.\n", h.Name)
			teamEditHint(*h)
			return nil
		},
	}
	addHostFlags(cmd, &f)
	return cmd
}

func newRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a host",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, idx := cfg.Find(args[0]); idx < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			if !yes {
				fmt.Printf("Remove host %q? [y/N] ", args[0])
				resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(resp)) != "y" {
					fmt.Println("Cancelled.")
					return nil
				}
			}
			var removed config.Host
			err = cfg.Update(func(c *config.Config) error {
				h, idx := c.Find(args[0])
				if idx < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				removed = *h
				c.Remove(args[0])
				return nil
			})
			if err != nil {
				return err
			}
			if removed.Team != "" {
				fmt.Fprintf(os.Stderr, "%s was shared with a team: `meterm team push` will remove it for everyone.\n"+
					"To only drop it from this machine instead, run `meterm team ignore %s`.\n", removed.Name, removed.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	return cmd
}

func applyFlags(h *config.Host, f hostFlags, changed interface{ Changed(string) bool }) error {
	if changed.Changed("address") {
		h.Address = f.address
	}
	if changed.Changed("user") {
		h.User = f.user
	}
	if changed.Changed("users") {
		h.Users = config.SplitTags(f.users)
		if err := h.CheckConnection(); err != nil {
			return err
		}
	}
	if changed.Changed("port") {
		h.Port = f.port
	}
	if changed.Changed("identity") {
		h.IdentityFile = sshkey.ContractHome(f.identity)
	}
	if changed.Changed("group") {
		h.Group = f.group
	}
	if changed.Changed("tags") {
		h.Tags = config.SplitTags(f.tags)
	}
	if changed.Changed("env") {
		env, err := config.NormalizeEnv(f.env)
		if err != nil {
			return err
		}
		h.Env = env
	}
	if changed.Changed("proxy-jump") {
		h.ProxyJump = f.proxyJump
	}
	if changed.Changed("notes") {
		h.Notes = f.notes
	}
	if changed.Changed("forward-local") {
		fw, err := config.ParseLocalForward(f.forwardLocal)
		if err != nil {
			return err
		}
		h.AddForward(fw)
	}
	return nil
}

func nonEmptyHost(h *config.Host) *config.Host {
	if h.Name == "" && h.Address == "" {
		return nil
	}
	return h
}

// --- interactive wizard -----------------------------------------------

func runAddWizard(cfg *config.Config, existing *config.Host) error {
	return addWizard(cfg, existing, os.Stdin)
}

// clearValue, typed at an edit prompt, empties the field: a blank answer
// keeps what's there, so without it nothing could be taken out.
const clearValue = "-"

func addWizard(cfg *config.Config, existing *config.Host, in io.Reader) error {
	reader := bufio.NewReader(in)
	h := config.Host{}
	editing := existing != nil
	if editing {
		h = *existing
		fmt.Println("Enter keeps the value in [brackets]; " + clearValue + " clears it.")
	}

	prompt := func(label, cur string) string {
		if cur != "" {
			fmt.Printf("%s [%s]: ", label, cur)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		switch line {
		case "":
			return cur
		case clearValue:
			return ""
		}
		return line
	}

	h.Name = prompt("Name", h.Name)
	if h.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !editing {
		if _, idx := cfg.Find(h.Name); idx >= 0 {
			return fmt.Errorf("host %q already exists", h.Name)
		}
	}
	h.Address = prompt("Address (IP or hostname)", h.Address)
	if h.Address == "" {
		return fmt.Errorf("address is required")
	}
	h.User = prompt("User (empty = inherit)", h.User)
	h.Users = config.SplitTags(prompt("Other users people log in as (comma-separated; connect deploy@"+h.Name+")", strings.Join(h.Users, ",")))
	if portStr := prompt("Port (empty = inherit/22)", portOrBlank(h.Port)); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			h.Port = p
		}
	} else {
		h.Port = 0
	}
	h.IdentityFile = sshkey.ContractHome(prompt("Identity file (empty = inherit)", h.IdentityFile))

	groupPrompt := "Group (empty = none)"
	if len(cfg.Groups) > 0 {
		groupPrompt = fmt.Sprintf("Group (empty = none; available: %s)", strings.Join(cfg.SortedGroupNames(), ", "))
	}
	h.Group = prompt(groupPrompt, h.Group)

	h.Tags = config.SplitTags(prompt("Tags (comma-separated)", strings.Join(h.Tags, ",")))
	h.ProxyJump = prompt("Jump host (saved host name or user@host; empty = none)", h.ProxyJump)
	h.Notes = prompt("Notes", h.Notes)
	env, err := config.NormalizeEnv(prompt("Environment ("+config.EnvList+"; empty = inherit)", h.Env))
	if err != nil {
		return err
	}
	h.Env = env
	if err := h.CheckConnection(); err != nil {
		return err
	}

	// The prompts can take a while. Only what they asked about comes from the
	// answers: routes, tunnels and the rest stay as they are on disk now.
	err = cfg.Update(func(c *config.Config) error {
		if !editing {
			if _, idx := c.Find(h.Name); idx >= 0 {
				return fmt.Errorf("host %q already exists", h.Name)
			}
			c.Upsert(h)
			return nil
		}
		cur, idx := c.Find(existing.Name)
		if idx < 0 {
			return fmt.Errorf("host %q was removed while you were editing it, so nothing was saved", existing.Name)
		}
		updated := *cur
		updated.Name, updated.Address, updated.User, updated.Users = h.Name, h.Address, h.User, h.Users
		updated.Port, updated.IdentityFile, updated.Group, updated.Tags = h.Port, h.IdentityFile, h.Group, h.Tags
		updated.ProxyJump, updated.Notes, updated.Env = h.ProxyJump, h.Notes, h.Env
		c.Upsert(updated)
		return nil
	})
	if err != nil {
		return err
	}
	verb := "Added"
	if editing {
		verb = "Updated"
	}
	fmt.Printf("%s %q.\n", verb, h.Name)
	return nil
}

func portOrBlank(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}
