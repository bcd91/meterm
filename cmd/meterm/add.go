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
	"meterm/internal/termsafe"
	"meterm/internal/tui"
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
			quietOnSuccess = true // the picker shows the new host
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
			if err := h.CheckPrintable(); err != nil {
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
			quietOnSuccess = true // the picker shows the change
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx, err := cfg.Lookup(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			name := h.Name // the real name, when args[0] is it as shown
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
			var before config.Host
			var envBefore, envAfter string
			err = cfg.Update(func(c *config.Config) error {
				var i int
				h, i = c.Find(name)
				if i < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				before, envBefore = *h, c.Env(*h)
				if err := applyFlags(h, f, cmd.Flags()); err != nil {
					return err
				}
				envAfter = c.Env(*h)
				return nil
			})
			if err != nil {
				return err
			}
			logEdit(cfg, before, *h, envBefore, envAfter)
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
			h, idx, err := cfg.Lookup(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			name := h.Name // the real name, when args[0] is it as shown
			prod := cfg.IsProduction(*h)
			users := cfg.JumpedBy(h.Name)
			if yes && prod {
				fmt.Fprintf(os.Stderr, "⚠ removing %s, a PRODUCTION host.\n", lbl(h.Name))
			}
			if yes && len(users) > 0 {
				fmt.Fprintf(os.Stderr, "⚠ %s\n", tui.JumpBrokenText(users))
			}
			if !yes {
				if prod {
					fmt.Printf("⚠ %s is a PRODUCTION host.\n", lbl(h.Name))
				}
				if len(users) > 0 {
					fmt.Printf("⚠ %s\n", tui.JumpBrokenText(users))
				}
				fmt.Printf("Remove host %q? [y/N] ", lbl(name))
				resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(resp)) != "y" {
					fmt.Println("Cancelled.")
					return nil
				}
			}
			var removed config.Host
			err = cfg.Update(func(c *config.Config) error {
				h, idx := c.Find(name)
				if idx < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				removed = *h
				c.Remove(name)
				return nil
			})
			if err != nil {
				return err
			}
			recordActivity(cfg, removed, "rm", tui.RemovedDetail(prod, removed.Team != ""))
			if removed.Team != "" {
				name := termsafe.Label(removed.Name)
				fmt.Fprintf(os.Stderr, "%s was shared with a team: `meterm team push` will remove it for everyone.\n"+
					"To only drop it from this machine instead, run `meterm team ignore %s`.\n", name, name)
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
	// Only what was given is checked: a field already saved with a control
	// character mustn't stop someone from fixing another one.
	given := config.Host{}
	for flag, set := range map[string]func(){
		"address":    func() { given.Address = h.Address },
		"user":       func() { given.User = h.User },
		"users":      func() { given.Users = h.Users },
		"identity":   func() { given.IdentityFile = h.IdentityFile },
		"group":      func() { given.Group = h.Group },
		"tags":       func() { given.Tags = h.Tags },
		"proxy-jump": func() { given.ProxyJump = h.ProxyJump },
		"notes":      func() { given.Notes = h.Notes },
	} {
		if changed.Changed(flag) {
			set()
		}
	}
	if err := given.CheckPrintable(); err != nil {
		return err
	}
	return checkConnectionFlags(h, changed)
}

// checkConnectionFlags refuses, when it's given, a connection value that ssh
// would read as something else: an address, user or jump that starts with
// "-" or holds a space, an identity file that starts with "-". connect
// refuses them too, but by then they're saved, and shared with a team.
func checkConnectionFlags(h *config.Host, changed interface{ Changed(string) bool }) error {
	checks := []struct {
		flag  string
		check func() error
	}{
		{"address", func() error { return config.CheckAddress(h.Address) }},
		{"user", func() error { return config.CheckUser(h.User) }},
		{"proxy-jump", func() error { return config.CheckJump(h.ProxyJump) }},
		{"identity", func() error {
			if strings.HasPrefix(h.IdentityFile, "-") {
				return fmt.Errorf("identity file %q starts with '-', which ssh would read as an option", h.IdentityFile)
			}
			return nil
		}},
	}
	for _, c := range checks {
		if !changed.Changed(c.flag) {
			continue
		}
		if err := c.check(); err != nil {
			return fmt.Errorf("--%s: %s", c.flag, lbl(err.Error()))
		}
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

	// A value saved with control characters in it (a hand-edited or old host
	// file) is shown cleaned, and Enter keeps it cleaned: keeping it as it is
	// would only fail the check at the end, after every prompt.
	var cleaned []string
	prompt := func(label, cur string) string {
		shown, dirty := cur, config.CheckText("", cur) != nil
		if dirty {
			shown = strings.Join(strings.Fields(termsafe.Label(cur)), " ")
		}
		switch {
		case dirty && shown != "":
			fmt.Printf("%s [%s] (cleaned: control characters left out; enter keeps it so): ", label, shown)
		case dirty:
			fmt.Printf("%s (cleaned: it was only control characters; enter leaves it empty): ", label)
		case cur != "":
			fmt.Printf("%s [%s]: ", label, shown)
		default:
			fmt.Printf("%s: ", label)
		}
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		switch line {
		case "":
			if dirty {
				cleaned = append(cleaned, strings.SplitN(label, " (", 2)[0])
			}
			return shown
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
	h.Users = config.SplitTags(prompt("Other users people log in as (comma-separated; connect deploy@"+termsafe.Label(h.Name)+")", strings.Join(h.Users, ",")))
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
	if editing {
		cleaned = append(cleaned, cleanUnasked(&h)...)
	}
	if len(cleaned) > 0 {
		fmt.Printf("Control characters cleaned from: %s.\n", strings.Join(cleaned, ", "))
		quietOnSuccess = false // from the picker, wait so this can be read
	}
	if err := h.CheckPrintable(); err != nil {
		return err
	}
	if err := h.CheckConnection(); err != nil {
		return err
	}
	if strings.HasPrefix(h.IdentityFile, "-") {
		return fmt.Errorf("identity file %q starts with '-', which ssh would read as an option", h.IdentityFile)
	}

	// The prompts can take a while. Only what they asked about comes from the
	// answers: routes, tunnels and the rest stay as they are on disk now.
	var before, after config.Host
	var envBefore, envAfter string
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
			return fmt.Errorf("host %q was removed while you were editing it, so nothing was saved", termsafe.Label(existing.Name))
		}
		if h.Name != existing.Name {
			if _, j := c.Find(h.Name); j >= 0 {
				return fmt.Errorf("a host called %s already exists", h.Name)
			}
		}
		updated := *cur
		updated.Name, updated.Address, updated.User, updated.Users = h.Name, h.Address, h.User, h.Users
		updated.Port, updated.IdentityFile, updated.Group, updated.Tags = h.Port, h.IdentityFile, h.Group, h.Tags
		updated.ProxyJump, updated.Notes, updated.Env = h.ProxyJump, h.Notes, h.Env
		cleanUnasked(&updated)
		before, after, envBefore = *cur, updated, c.Env(*cur)
		envAfter = c.Env(updated)
		// In place: Upsert goes by name, and a renamed host would be added
		// as a second one beside the old.
		c.Hosts[idx] = updated
		if h.Name != existing.Name {
			renameJumps(c, existing.Name, h.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if editing && h.Name != existing.Name {
		carryHistory(existing.Name, h.Name)
	}
	if editing {
		logEdit(cfg, before, after, envBefore, envAfter)
	}
	verb := "Added"
	if editing {
		verb = "Updated"
	}
	fmt.Printf("%s %q.\n", verb, h.Name)
	return nil
}

// cleanUnasked cleans what the edit prompts don't ask about (routes and
// tunnels) the way the prompts clean a value, and says what it changed.
func cleanUnasked(h *config.Host) []string {
	var out []string
	clean := func(what string, fields ...*string) {
		for _, f := range fields {
			if config.CheckText("", *f) != nil {
				*f = strings.Join(strings.Fields(termsafe.Label(*f)), " ")
				out = append(out, what)
			}
		}
	}
	h.Routes = append([]config.Route(nil), h.Routes...)
	for k := range h.Routes {
		rt := &h.Routes[k]
		clean("a route", &rt.Name, &rt.Address, &rt.ProxyJump)
	}
	h.Forwards = append([]config.Forward(nil), h.Forwards...)
	for k := range h.Forwards {
		fw := &h.Forwards[k]
		clean("a tunnel", &fw.Name, &fw.Note, &fw.RemoteHost)
	}
	return out
}

func portOrBlank(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

// editDetail is what the activity log says an edit changed: old→new for the
// fields that say where a session goes or how careful to be (name, env,
// address, user, port, jump), and "changed" for the rest. "" when nothing
// did. env is the one in effect, a group's included: production to dev
// turns the warnings off.
func editDetail(old, cur config.Host, envOld, envNew string) string {
	var parts []string
	show := func(s string) string {
		if s == "" {
			return "(none)"
		}
		return exactName(s)
	}
	pair := func(what, a, b string) {
		if a != b {
			parts = append(parts, what+" "+show(a)+"→"+show(b))
		}
	}
	pair("name", old.Name, cur.Name)
	pair("env", envOld, envNew)
	pair("address", old.Address, cur.Address)
	pair("user", old.User, cur.User)
	pair("port", portOrBlank(old.Port), portOrBlank(cur.Port))
	pair("jump", old.ProxyJump, cur.ProxyJump)
	for _, f := range []struct {
		what string
		a, b string
	}{
		{"users", strings.Join(old.Users, ","), strings.Join(cur.Users, ",")},
		{"identity", old.IdentityFile, cur.IdentityFile},
		{"group", old.Group, cur.Group},
		{"tags", strings.Join(old.Tags, ","), strings.Join(cur.Tags, ",")},
		{"notes", old.Notes, cur.Notes},
		{"tunnels", fmt.Sprint(old.Forwards), fmt.Sprint(cur.Forwards)},
		{"routes", fmt.Sprint(old.Routes), fmt.Sprint(cur.Routes)},
	} {
		if f.a != f.b {
			parts = append(parts, f.what+" changed")
		}
	}
	return strings.Join(parts, "; ")
}

// logEdit notes an edit in the activity log, when it changed anything.
func logEdit(cfg *config.Config, old, cur config.Host, envOld, envNew string) {
	if d := editDetail(old, cur, envOld, envNew); d != "" {
		recordActivity(cfg, cur, "edit", d)
	}
}
