package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/team"
)

// pullInterval is how often autosync pulls a team's hosts while meterm is in use.
const pullInterval = 5 * time.Minute

type syncOpts struct {
	pull, push bool
	only       map[string]bool
	yes        bool
	upload     bool // also upload pending activity
	// prepare makes the changes this command is about, on a config just read.
	prepare func(*config.Config)
}

func teamShareCmd(teamFlag *string) *cobra.Command {
	var (
		tags, groups []string
		all, yes     bool
	)
	cmd := &cobra.Command{
		Use:   "share [host...]",
		Short: "Share saved hosts with the team",
		Long: "Marks hosts as belonging to the team and pushes them (after a preview). From then\n" +
			"on, activity on them goes to the team's log. identity_file stays on this machine.\n\n" +
			"Select hosts by name, --tag, --group or --all, like `meterm exec`:\n" +
			"  meterm team share WEB API\n" +
			"  meterm team share --tag acme",
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			if !t.Role.CanEditHosts() {
				fmt.Printf("You are %s in %q: these will be proposed, and the team sees them once an owner or admin approves.\n", t.Role, t.Name)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := selectHosts(cfg, args, tags, groups, all)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				return fmt.Errorf("no hosts matched (use names, --tag, --group, or --all)")
			}
			only := map[string]bool{}
			for _, sel := range hosts {
				h, _ := cfg.Find(sel.Name)
				if h.Team != "" && h.Team != t.ID {
					fmt.Fprintf(os.Stderr, "skipping %s: already shared with another team\n", h.Name)
					continue
				}
				only[h.Name] = true
			}
			if len(only) == 0 {
				return nil
			}
			share := func(c *config.Config) {
				for name := range only {
					if h, i := c.Find(name); i >= 0 {
						h.Team = t.ID
					}
				}
			}
			share(cfg)
			return runTeamSync(cmd.Context(), tc, t, cfg, syncOpts{push: true, only: only, yes: yes, prepare: share})
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "share hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "share hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "share every saved host (check the list first: personal and other clients' hosts included)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

func teamSyncCmd(teamFlag *string, use, short string, pull, push bool) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !push && len(args) > 0 {
				return fmt.Errorf("%s takes no host names", strings.Fields(use)[0])
			}
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			o := syncOpts{pull: pull, push: push, yes: yes, upload: pull && push}
			if len(args) > 0 {
				o.only = map[string]bool{}
				for _, a := range args {
					o.only[a] = true
				}
			}
			return runTeamSync(cmd.Context(), tc, t, cfg, o)
		},
	}
	if push {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	}
	return cmd
}

// runTeamSync is the foreground sync: it previews anything that would change
// the team for everyone and asks first, then applies, saves and reports.
func runTeamSync(ctx context.Context, tc *teamCtx, t team.Team, cfg *config.Config, o syncOpts) error {
	unlock, ok, err := team.Lock(tc.dir, 15*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("another team sync is running; try again in a moment")
	}
	defer unlock()
	if tc.state, err = team.LoadState(tc.dir); err != nil {
		return err
	}
	base := tc.state.Bases[t.ID]
	ignored := tc.state.IgnoredSet(t.ID)

	if o.push {
		plan, err := team.Sync(ctx, tc.be, t.ID, t.Role, cfg, base, team.Options{Pull: o.pull, Push: true, Only: o.only, Ignored: ignored, DryRun: true})
		if err != nil {
			return err
		}
		var outgoing []team.Change
		proposing := false
		for _, c := range plan.Changes {
			if c.Outcome == team.Pushed || c.Outcome == team.PushedRemoval || c.Outcome == team.Proposed {
				outgoing = append(outgoing, c)
				proposing = proposing || c.Outcome == team.Proposed
			}
		}
		if len(outgoing) > 0 {
			remote, err := tc.be.ListHosts(ctx, t.ID)
			if err != nil {
				return err
			}
			if proposing {
				fmt.Printf("This proposes, for an owner or admin of %q to approve:\n", t.Name)
			} else {
				fmt.Printf("This changes %q for everyone in the team:\n", t.Name)
			}
			printPushPreview(outgoing, cfg, remote)
			if !o.yes {
				if !isatty.IsTerminal(os.Stdin.Fd()) {
					return errors.New("not a terminal: re-run with --yes to confirm")
				}
				ans, _ := promptLine("Push these changes? [y/N] ")
				if strings.ToLower(ans) != "y" {
					fmt.Println("Nothing pushed.")
					return nil
				}
			}
		}
	}

	// The question can take a while, and the network does too, and something can
	// change the host file in the meantime. So this works from the file as it is
	// now, and if it has moved on by the time this one is done, goes again from
	// there: what was already pushed comes up as the same on both sides.
	var res team.Result
	for attempt := 0; ; attempt++ {
		fresh, err := loadConfig()
		if err != nil {
			return err
		}
		if o.prepare != nil {
			o.prepare(fresh)
		}
		cfg = fresh
		res, err = team.Sync(ctx, tc.be, t.ID, t.Role, cfg, base, team.Options{Pull: o.pull, Push: o.push, Only: o.only, Ignored: ignored})
		if err != nil {
			return err
		}
		err = cfg.Save()
		if err == nil {
			break
		}
		if !errors.Is(err, config.ErrChanged) || attempt == 2 {
			return err
		}
	}
	tc.state.Bases[t.ID] = res.Base
	tc.state.Attention[t.ID] = team.NeedingAttention(res.Changes)
	tc.state.TeamNames[t.ID] = t.Name
	if o.pull {
		tc.state.LastPull[t.ID] = time.Now()
	}
	if err := tc.saveState(); err != nil {
		return err
	}

	for _, c := range res.Changes {
		line := fmt.Sprintf("  %-16s %s", c.Outcome, c.Host)
		if c.Detail != "" {
			line += " — " + c.Detail
		}
		fmt.Println(line)
	}
	if len(res.Changes) == 0 {
		fmt.Printf("Hosts for %q are up to date.\n", t.Name)
	}

	if o.upload {
		dir, err := activity.DefaultDir()
		if err != nil {
			return err
		}
		l, err := activity.Open(dir)
		if err != nil {
			return err
		}
		n, err := team.UploadActivity(ctx, tc.be, t.ID, cfg, l)
		if err != nil {
			return fmt.Errorf("hosts synced, but uploading activity failed (it stays queued): %w", err)
		}
		if n > 0 {
			fmt.Printf("Uploaded %d activity event(s).\n", n)
		}
	}
	if n := team.Problems(res.Changes); n > 0 {
		return fmt.Errorf("%d host(s) need attention — see `meterm team status`", n)
	}
	return nil
}

// printPushPreview shows, field by field, what each outgoing change does to
// the team's copy — the place to catch a typo before everyone gets it.
func printPushPreview(outgoing []team.Change, cfg *config.Config, remote []team.SharedHost) {
	byName := map[string]team.SharedHost{}
	for _, h := range remote {
		byName[h.Name] = h
	}
	for _, c := range outgoing {
		old, existed := byName[c.Host]
		if _, here := cfg.Find(c.Host); c.Outcome == team.PushedRemoval || (c.Outcome == team.Proposed && here < 0) {
			fmt.Printf("  - remove %s (%s) from the team\n", c.Host, old.Address)
			continue
		}
		h, _ := cfg.Find(c.Host)
		cur := team.SharedFromHost(*h)
		if !existed {
			fmt.Printf("  + add %s (%s)\n", c.Host, cur.Describe())
			continue
		}
		fmt.Printf("  ~ update %s\n", c.Host)
		for _, d := range team.Diff(old, cur) {
			fmt.Printf("      %s\n", d)
		}
	}
}

func teamStatusCmd(teamFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "What's waiting to be pushed or pulled, conflicts, and queued activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			t, err := tc.current(ctx, *teamFlag)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			plan, err := team.Sync(ctx, tc.be, t.ID, t.Role, cfg, tc.state.Bases[t.ID],
				team.Options{Pull: true, Push: true, Ignored: tc.state.IgnoredSet(t.ID), DryRun: true})
			if err != nil {
				return err
			}

			fmt.Printf("Team %q — you are %s\n", t.Name, t.Role)
			groups := []struct {
				title string
				match func(team.Outcome) bool
			}{
				{"Your changes, not pushed yet (meterm team push):", func(o team.Outcome) bool { return o == team.Pushed || o == team.PushedRemoval }},
				{"Team changes, not pulled yet (meterm team pull):", func(o team.Outcome) bool { return o == team.Pulled || o == team.PulledRemoval }},
				{"Needs you (meterm team resolve <host> --theirs|--mine):", func(o team.Outcome) bool { return o == team.Conflict || o == team.NameTaken }},
				{"Can't push with your role:", func(o team.Outcome) bool { return o == team.Forbidden }},
				{"Waiting for an owner or admin (meterm team proposals; push again after edits):", func(o team.Outcome) bool { return o == team.Proposed }},
			}
			any := false
			for _, g := range groups {
				var lines []string
				for _, c := range plan.Changes {
					if g.match(c.Outcome) {
						verb := string(c.Outcome)
						switch c.Outcome {
						case team.Pushed:
							verb = "changed"
						case team.PushedRemoval:
							verb = "removed here"
						case team.PulledRemoval:
							verb = "removed by team"
						}
						lines = append(lines, fmt.Sprintf("  %-16s %s", verb, c.Host))
					}
				}
				if len(lines) > 0 {
					any = true
					fmt.Println(g.title)
					fmt.Println(strings.Join(lines, "\n"))
				}
			}
			if !any {
				fmt.Println("Hosts: in sync.")
			}

			if dir, err := activity.DefaultDir(); err == nil {
				if l, err := activity.Open(dir); err == nil {
					pending, _ := l.Pending(t.ID)
					shared := map[string]bool{}
					for _, h := range cfg.Hosts {
						if h.Team == t.ID {
							shared[h.Name] = true
						}
					}
					n := 0
					for _, e := range pending {
						if shared[e.Host] {
							n++
						}
					}
					fmt.Printf("Activity: %d event(s) waiting to upload.\n", n)
				}
			}
			if ign := tc.state.Ignored[t.ID]; len(ign) > 0 {
				fmt.Printf("Ignored here: %s (meterm team unignore <host>)\n", strings.Join(ign, ", "))
			}
			auto := "on"
			if !tc.state.AutosyncOn() {
				auto = "off"
			}
			last := "never"
			if lp, ok := tc.state.LastPull[t.ID]; ok {
				last = humanAgo(time.Since(lp))
			}
			fmt.Printf("Autosync: %s, last pull %s.\n", auto, last)
			if tc.state.LastError != "" {
				fmt.Printf("Last background error: %s\n", tc.state.LastError)
			}
			return nil
		},
	}
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func teamResolveCmd(teamFlag *string) *cobra.Command {
	var theirs, mine bool
	cmd := &cobra.Command{
		Use:   "resolve <host>",
		Short: "Settle a conflict or name-taken: --theirs takes the team's copy, --mine pushes yours",
		Long: "--theirs replaces your copy with the team's (your identity_file is kept). Use it\n" +
			"for name-taken when your personal host is the same machine as the team's.\n" +
			"--mine pushes your copy over the team's (admins and owners).\n" +
			"If they're different machines that just share a name, rename yours instead:\n" +
			"  meterm rename <host> <new-name>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if theirs == mine {
				return errors.New("choose one: --theirs or --mine")
			}
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			unlock, ok, err := team.Lock(tc.dir, 15*time.Second)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("another team sync is running; try again in a moment")
			}
			defer unlock()
			if tc.state, err = team.LoadState(tc.dir); err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			base, err := team.Resolve(cmd.Context(), tc.be, t.ID, t.Role, cfg, tc.state.Bases[t.ID], args[0], theirs)
			if err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			tc.state.Bases[t.ID] = base
			var keep []team.Change
			for _, c := range tc.state.Attention[t.ID] {
				if c.Host != args[0] {
					keep = append(keep, c)
				}
			}
			tc.state.Attention[t.ID] = keep
			if err := tc.saveState(); err != nil {
				return err
			}
			if theirs {
				fmt.Printf("%s now matches the team's copy.\n", args[0])
			} else {
				fmt.Printf("Pushed your %s to %q.\n", args[0], t.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&theirs, "theirs", false, "take the team's copy")
	cmd.Flags().BoolVar(&mine, "mine", false, "push your copy over the team's")
	return cmd
}

func teamIgnoreCmd(teamFlag *string, ignore bool) *cobra.Command {
	use, short := "ignore <host>...", "Stop receiving team hosts on this machine (removes them here only)"
	if !ignore {
		use, short = "unignore <host>...", "Receive previously ignored team hosts again"
	}
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			unlock, ok, err := team.Lock(tc.dir, 15*time.Second)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("another team sync is running; try again in a moment")
			}
			if tc.state, err = team.LoadState(tc.dir); err != nil {
				unlock()
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				unlock()
				return err
			}
			set := tc.state.IgnoredSet(t.ID)
			for _, name := range args {
				if ignore {
					set[name] = true
					delete(tc.state.Bases[t.ID], name)
				} else {
					delete(set, name)
				}
			}
			list := make([]string, 0, len(set))
			for n := range set {
				list = append(list, n)
			}
			sort.Strings(list)
			tc.state.Ignored[t.ID] = list
			err = cfg.Update(func(c *config.Config) error {
				for _, name := range args {
					if h, i := c.Find(name); ignore && i >= 0 && h.Team == t.ID {
						c.Remove(name)
					}
				}
				return nil
			})
			if err == nil {
				err = tc.saveState()
			}
			unlock()
			if err != nil {
				return err
			}
			if ignore {
				fmt.Printf("Ignoring %s on this machine; the team still has %s.\n", strings.Join(args, ", "), map[bool]string{true: "it", false: "them"}[len(args) == 1])
				return nil
			}
			return runTeamSync(cmd.Context(), tc, t, cfg, syncOpts{pull: true})
		},
	}
}

func teamAutosyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "autosync [on|off]",
		Short:     "Turn background upload of activity and pulling of hosts on or off",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeam()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if tc.state.AutosyncOn() {
					fmt.Println("Autosync is on.")
				} else {
					fmt.Println("Autosync is off.")
				}
				return nil
			}
			on := args[0] == "on"
			if !on && args[0] != "off" {
				return errors.New("use on or off")
			}
			if err := tc.updateState(func(s *team.State) error {
				s.Autosync = &on
				return nil
			}); err != nil {
				return err
			}
			fmt.Printf("Autosync %s.\n", args[0])
			return nil
		},
	}
}

// teamBackgroundCmd is what kickAutosync starts: a short-lived, detached
// process that pulls hosts and uploads activity for every team, then exits.
// It never pushes and never prompts.
func teamBackgroundCmd() *cobra.Command {
	var hosts, upload bool
	cmd := &cobra.Command{
		Use:    "background",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Nobody is watching a background sync: git must fail rather than
			// pop up a sign-in window (Git Credential Manager on Windows) or
			// wait for a password nobody will type.
			os.Setenv("GCM_INTERACTIVE", "never")
			os.Setenv("GIT_TERMINAL_PROMPT", "0")
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			tc, err := openTeam()
			if err != nil || tc.be == nil {
				return err
			}
			unlock, ok, err := team.Lock(tc.dir, 0)
			if err != nil || !ok {
				return err // busy: another sync is already doing this
			}
			defer unlock()
			if tc.state, err = team.LoadState(tc.dir); err != nil {
				return err
			}
			bgErr := backgroundSync(ctx, tc, hosts, upload)
			tc.state.LastError = ""
			if bgErr != nil {
				tc.state.LastError = time.Now().Format("2006-01-02 15:04") + ": " + bgErr.Error()
			}
			return tc.saveState()
		},
	}
	cmd.Flags().BoolVar(&hosts, "hosts", false, "pull every team's hosts")
	cmd.Flags().BoolVar(&upload, "activity", false, "upload queued activity")
	return cmd
}

func backgroundSync(ctx context.Context, tc *teamCtx, hosts, upload bool) error {
	teams, err := tc.be.Teams(ctx)
	if err != nil {
		return err
	}
	path, err := resolveCfgPath()
	if err != nil {
		return err
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, t := range teams {
		tc.state.TeamNames[t.ID] = t.Name
		if hosts {
			keep(backgroundPull(ctx, tc, t, path))
		}
		if upload {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			dir, err := activity.DefaultDir()
			if err != nil {
				return err
			}
			l, err := activity.Open(dir)
			if err != nil {
				return err
			}
			_, err = team.UploadActivity(ctx, tc.be, t.ID, cfg, l)
			keep(err)
		}
	}
	return firstErr
}

// backgroundPull pulls one team's hosts, but only saves if nobody edited
// hosts.yaml while it was talking to the server — otherwise it would undo
// their edit. It just tries again next time.
func backgroundPull(ctx context.Context, tc *teamCtx, t team.Team, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	res, err := team.Sync(ctx, tc.be, t.ID, t.Role, cfg, tc.state.Bases[t.ID],
		team.Options{Pull: true, Ignored: tc.state.IgnoredSet(t.ID)})
	if err != nil {
		return err
	}
	tc.state.Attention[t.ID] = team.NeedingAttention(res.Changes)
	changed := false
	for _, c := range res.Changes {
		if c.Outcome == team.Pulled || c.Outcome == team.PulledRemoval {
			changed = true
		}
	}
	if changed {
		if err := cfg.Save(); err != nil {
			if errors.Is(err, config.ErrChanged) {
				return nil // edited meanwhile; base stays put, so next pull redoes this
			}
			return err
		}
	}
	tc.state.Bases[t.ID] = res.Base
	tc.state.LastPull[t.ID] = time.Now()
	return nil
}

// kickAutosync starts a detached `meterm team background` if this machine is
// signed in to a team and autosync is on. It returns immediately; nothing it
// does can slow down or fail the command that called it. hosts asks for a
// pull, which is throttled to pullInterval.
func kickAutosync(hosts, upload bool) {
	if os.Getenv("METERM_NO_AUTOSYNC") != "" {
		return
	}
	dir, err := team.Dir()
	if err != nil {
		return
	}
	st, err := team.LoadState(dir)
	if err != nil || !st.AutosyncOn() {
		return
	}
	if len(st.GitTeams) == 0 {
		return
	}
	if hosts && len(st.LastPull) > 0 {
		recent := true
		for _, t := range st.LastPull {
			if time.Since(t) > pullInterval {
				recent = false
			}
		}
		if recent {
			hosts = false
		}
	}
	if !hosts && !upload {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	args := []string{"team", "background"}
	if hosts {
		args = append(args, "--hosts")
	}
	if upload {
		args = append(args, "--activity")
	}
	if p, err := resolveCfgPath(); err == nil {
		args = append(args, "--config", p)
	}
	c := exec.Command(self, args...)
	c.Env = append(os.Environ(), "METERM_NO_AUTOSYNC=1", fromTUIEnv+"=") // the child must never kick again or pause for enter
	detach(c)
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}

// teamNotice is the one-line reminder shown by list and the picker when a
// team has something waiting for a person, or "" when there's nothing.
// hostAttention is, per host, what team sync is waiting on ("not pushed",
// "conflict", ...), for the picker's badges.
func hostAttention() map[string]string {
	dir, err := team.Dir()
	if err != nil {
		return nil
	}
	st, err := team.LoadState(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, cs := range st.Attention {
		for _, c := range cs {
			var what string
			switch c.Outcome {
			case team.Pending, team.PendingRemoval:
				what = "not pushed"
			case team.Conflict:
				what = "conflict"
			case team.NameTaken:
				what = "name taken"
			case team.Forbidden:
				what = "can't push"
			default:
				continue
			}
			if prev := out[c.Host]; prev == "" || prev == "not pushed" {
				out[c.Host] = what // a problem outranks a plain unpushed change
			}
		}
	}
	return out
}

func teamNotice() string {
	dir, err := team.Dir()
	if err != nil {
		return ""
	}
	// No session check: a Git-only team member has no cloud session but
	// still has unpushed changes worth a reminder.
	st, err := team.LoadState(dir)
	if err != nil {
		return ""
	}
	var parts []string
	ids := make([]string, 0, len(st.Attention))
	for id := range st.Attention {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		counts := map[string]int{}
		for _, c := range st.Attention[id] {
			switch c.Outcome {
			case team.Pending, team.PendingRemoval:
				counts["not pushed"]++
			case team.Conflict:
				counts["conflict"]++
			case team.NameTaken:
				counts["name taken"]++
			case team.Forbidden:
				counts["can't push"]++
			}
		}
		if len(counts) == 0 {
			continue
		}
		var bits []string
		for _, k := range []string{"not pushed", "conflict", "name taken", "can't push"} {
			if counts[k] > 0 {
				bits = append(bits, fmt.Sprintf("%d %s", counts[k], k))
			}
		}
		name := st.TeamNames[id]
		if name == "" {
			name = id
		}
		parts = append(parts, fmt.Sprintf("%s: %s", name, strings.Join(bits, ", ")))
	}
	if len(parts) == 0 {
		if st.LastError != "" {
			return "team autosync failed — see `meterm team status`"
		}
		return ""
	}
	return "team " + strings.Join(parts, "; ") + " — run `meterm team status`"
}

// teamEditHint reminds that edits to a team host stay local until pushed.
func teamEditHint(h config.Host) {
	if h.Team != "" {
		fmt.Fprintf(os.Stderr, "%s is shared with a team: this change stays on this machine until `meterm team push`.\n", h.Name)
	}
}

// updateTeamState changes state.json under the team's lock, from what's in the
// file now, so a sync running next to this keeps what it wrote.
func updateTeamState(dir string, fn func(*team.State) error) (team.State, error) {
	unlock, ok, err := team.Lock(dir, 15*time.Second)
	if err != nil {
		return team.State{}, err
	}
	if !ok {
		return team.State{}, errors.New("another team sync is running; try again in a moment")
	}
	defer unlock()
	st, err := team.LoadState(dir)
	if err != nil {
		return st, err
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	return st, team.SaveState(dir, st)
}

func (tc *teamCtx) updateState(fn func(*team.State) error) error {
	st, err := updateTeamState(tc.dir, fn)
	if err != nil {
		return err
	}
	tc.state = st
	return nil
}
