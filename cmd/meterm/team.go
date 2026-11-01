package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/authkeys"
	"meterm/internal/config"
	"meterm/internal/team"
	"meterm/internal/termsafe"
)

// teamCtx bundles what most team subcommands need.
type teamCtx struct {
	dir   string
	be    *team.Router // nil when this machine has no team
	state team.State
}

// openTeamAny is openTeam for commands that need a team: it fails when
// this machine has none.
func openTeamAny() (*teamCtx, error) {
	tc, err := openTeam()
	if err != nil {
		return nil, err
	}
	if tc.be == nil {
		return nil, team.ErrNotSignedIn
	}
	return tc, nil
}

func openTeam() (*teamCtx, error) {
	dir, err := team.Dir()
	if err != nil {
		return nil, err
	}
	st, err := team.LoadState(dir)
	if err != nil {
		return nil, err
	}
	if team.ForgetCloud(dir) {
		fmt.Fprintln(os.Stderr, "meterm: removed the sign-in for meterm's old cloud teams from this machine (the cloud service is gone; teams live in Git).")
	}
	tc := &teamCtx{dir: dir, state: st}
	r := &team.Router{Git: map[string]*team.GitStore{}}
	for id, gt := range st.GitTeams {
		g, err := team.OpenGitStore(context.Background(), dir, gt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "meterm: team %q: %v\n", gt.Name, err)
			continue
		}
		r.Git[id] = g
	}
	if len(r.Git) > 0 {
		tc.be = r
	}
	return tc, nil
}

// current returns the team commands act on: --team if given, else the one
// chosen with `team use`.
func (tc *teamCtx) current(ctx context.Context, flag string) (team.Team, error) {
	if tc.be == nil {
		return team.Team{}, team.ErrNotSignedIn
	}
	teams, err := tc.be.Teams(ctx)
	if err != nil {
		return team.Team{}, err
	}
	want := flag
	if want == "" {
		want = tc.state.Current
	}
	if want == "" {
		if len(teams) == 1 {
			return teams[0], nil
		}
		return team.Team{}, errors.New("no team selected — run `meterm team use <name>` or pass --team")
	}
	t, err := pickTeam(teams, want)
	if err != nil {
		return team.Team{}, err
	}
	return t, nil
}

// pickTeam finds a team by ID, or by name.
func pickTeam(teams []team.Team, want string) (team.Team, error) {
	var byName []team.Team
	for _, t := range teams {
		if t.ID == want {
			return t, nil
		}
		if strings.EqualFold(t.Name, want) {
			byName = append(byName, t)
		}
	}
	switch len(byName) {
	case 0:
		return team.Team{}, fmt.Errorf("you are not in a team called %q (see `meterm team list`)", want)
	case 1:
		return byName[0], nil
	}
	var ids []string
	for _, t := range byName {
		ids = append(ids, t.ID)
	}
	return team.Team{}, fmt.Errorf("more than one team is called %q; use its ID: %s", want, strings.Join(ids, ", "))
}

func (tc *teamCtx) saveState() error { return team.SaveState(tc.dir, tc.state) }

// openTeamLocal is openTeamAny with no network and no writes beyond what reading a
// clone takes: it doesn't clone a team that's missing, or clear out old sign-ins.
func openTeamLocal() (*teamCtx, error) {
	dir, err := team.Dir()
	if err != nil {
		return nil, err
	}
	st, err := team.LoadState(dir)
	if err != nil {
		return nil, err
	}
	r := &team.Router{Git: map[string]*team.GitStore{}}
	var firstErr error
	for id, gt := range st.GitTeams {
		g, err := team.OpenLocalGitStore(dir, gt)
		if err != nil {
			firstErr = err
			continue
		}
		r.Git[id] = g
	}
	if len(r.Git) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, team.ErrNotSignedIn
	}
	return &teamCtx{dir: dir, state: st, be: r}, nil
}

func newTeamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Share hosts and an activity log with a team",
		Long: "Teams share connection details (address, user, port, group, tags, jump host, notes)\n" +
			"and the activity log of the hosts they share. Private keys and identity_file paths\n" +
			"never leave your machine, and activity on personal hosts is never uploaded.\n\n" +
			"Getting started, with the team kept in a Git repository you control:\n" +
			"  meterm team git add ACME git@github.com:acme/meterm-team.git\n" +
			"  meterm team share WEB API                # share existing hosts\n" +
			"  meterm team invite someone@example.com --role admin   # and give them repo access\n" +
			"  # they run: meterm team git add ACME git@github.com:acme/meterm-team.git\n" +
			"  meterm team push                       # share later edits (shows a preview first)\n\n" +
			"The repository's own access controls decide who can read and change the team:\n" +
			"add people as collaborators there.\n\n" +
			"Autosync (on by default): activity is uploaded right after each command, and the\n" +
			"team's hosts are pulled in the background at most every 5 minutes while you use\n" +
			"meterm. Your own edits are never pushed automatically — `meterm team push` does that.",
	}
	var teamFlag string
	cmd.PersistentFlags().StringVar(&teamFlag, "team", "", "team name or ID (default: the one chosen with 'meterm team use')")

	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List your teams", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				tc, err := openTeamAny()
				if err != nil {
					return err
				}
				teams, err := tc.be.Teams(cmd.Context())
				if err != nil {
					return err
				}
				if len(teams) == 0 {
					fmt.Println("No teams yet. Add one with `meterm team git add <name> <repo-url>`.")
					return nil
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "\tTEAM\tROLE\tREPOSITORY")
				for _, t := range teams {
					mark := ""
					if t.ID == tc.state.Current {
						mark = "*"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, t.Name, t.Role, tc.state.GitTeams[t.ID].Remote)
				}
				return w.Flush()
			},
		},
		&cobra.Command{
			Use: "use <name>", Short: "Choose the team other team commands act on", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				tc, err := openTeamAny()
				if err != nil {
					return err
				}
				t, err := tc.current(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if err := tc.updateState(func(s *team.State) error {
					s.Current = t.ID
					return nil
				}); err != nil {
					return err
				}
				fmt.Printf("Now using team %q (you are %s).\n", t.Name, t.Role)
				return nil
			},
		},
		&cobra.Command{
			Use: "members", Short: "List the team's members", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				tc, err := openTeamAny()
				if err != nil {
					return err
				}
				t, err := tc.current(cmd.Context(), teamFlag)
				if err != nil {
					return err
				}
				members, err := tc.be.Members(cmd.Context(), t.ID)
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "EMAIL\tROLE")
				for _, m := range members {
					fmt.Fprintf(w, "%s\t%s\n", m.Email, m.Role)
				}
				return w.Flush()
			},
		},
		teamInviteCmd(&teamFlag),
		teamRemoveCmd(&teamFlag),
		teamGitCmd(),
		teamShareCmd(&teamFlag),
		teamProposalsCmd(&teamFlag),
		teamApproveCmd(&teamFlag),
		teamRejectCmd(&teamFlag),
		teamSyncCmd(&teamFlag, "sync", "Pull the team's hosts, push your changes (after a preview), upload activity", true, true),
		teamSyncCmd(&teamFlag, "push [host...]", "Share your local changes to team hosts with the team (after a preview)", false, true),
		teamSyncCmd(&teamFlag, "pull", "Get the team's latest hosts (autosync does this for you)", true, false),
		teamStatusCmd(&teamFlag),
		teamResolveCmd(&teamFlag),
		teamIgnoreCmd(&teamFlag, true),
		teamIgnoreCmd(&teamFlag, false),
		teamAutosyncCmd(),
		teamBackgroundCmd(),
		teamLogCmd(&teamFlag),
		teamAuditsCmd(&teamFlag),
	)
	return cmd
}

func teamInviteCmd(teamFlag *string) *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use: "invite <email>...", Short: "Record people's role in the team (admins and owners); grant them repo access too", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			r := team.Role(role)
			if !r.Valid() {
				return fmt.Errorf("--role must be owner, admin or member")
			}
			for _, email := range args {
				if err := tc.be.Invite(cmd.Context(), t.ID, email, r); err != nil {
					if errors.Is(err, team.ErrForbidden) {
						return fmt.Errorf("invite %s: %w (you are %s)", email, err, t.Role)
					}
					return fmt.Errorf("invite %s: %w", email, err)
				}
				fmt.Printf("Added %s to %q as %s. Give them access to the repository, then they run `meterm team git add %q %s`.\n", email, t.Name, r, t.Name, tc.state.GitTeams[t.ID].Remote)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", string(team.RoleMember), "owner, admin or member")
	return cmd
}

func teamRemoveCmd(teamFlag *string) *cobra.Command {
	var revoke, yes bool
	cmd := &cobra.Command{
		Use:   "remove <email>",
		Short: "Take someone out of the team (owners), or leave it (your own email)",
		Long: "Removes the member: they lose access to the team's hosts, activity log and\n" +
			"audits at once. It does not touch the servers themselves — their SSH key\n" +
			"still opens every host it was installed on. --revoke-keys follows up by\n" +
			"removing keys whose comment is their email from authorized_keys on every\n" +
			"team host (with a preview and a confirmation, like `meterm keys revoke`).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, err := openTeamAny()
			if err != nil {
				return err
			}
			t, err := tc.current(cmd.Context(), *teamFlag)
			if err != nil {
				return err
			}
			email := strings.ToLower(strings.TrimSpace(args[0]))
			members, err := tc.be.Members(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			var target *team.Member
			for i := range members {
				if members[i].Email == email {
					target = &members[i]
				}
			}
			if target == nil {
				return fmt.Errorf("%s isn't a member of %q", email, t.Name)
			}
			me, err := tc.be.IdentityFor(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			self := target.UID == me.UID
			if !yes {
				what := fmt.Sprintf("Remove %s (%s) from %q?", email, target.Role, t.Name)
				if self {
					what = fmt.Sprintf("Leave %q?", t.Name)
				}
				fmt.Print(what + " [y/N] ")
				resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if r := strings.ToLower(strings.TrimSpace(resp)); r != "y" && r != "yes" {
					fmt.Println("Nothing changed.")
					return nil
				}
			}
			if err := tc.be.RemoveMember(cmd.Context(), t.ID, target.UID); err != nil {
				if errors.Is(err, team.ErrForbidden) {
					return fmt.Errorf("only an owner can remove people (you are %s)", t.Role)
				}
				return err
			}
			if self {
				fmt.Printf("You left %q.\n", t.Name)
				return nil
			}
			fmt.Printf("Removed %s from %q.\n", email, t.Name)
			fmt.Println("Also take away their access to the repository on your Git host — that's what actually locks them out.")
			if !revoke {
				fmt.Printf("Their SSH key still works on the servers. To take it away:\n  meterm keys revoke --comment %s   (or --key their.pub)\n", email)
				return nil
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			var hosts []config.Host
			for _, h := range cfg.Hosts {
				if h.Team == t.ID {
					hosts = append(hosts, h)
				}
			}
			if len(hosts) == 0 {
				fmt.Println("No team hosts on this machine to revoke keys on (run `meterm team pull` first).")
				return nil
			}
			fmt.Printf("\nLooking for keys commented %s on %d team host(s)…\n", email, len(hosts))
			return revokeKeys(cfg, hosts, authkeys.Matcher{Comments: []string{email}}, false, false)
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke-keys", false, "also remove their SSH keys (commented with their email) from every team host")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before removing from the team")
	return cmd
}

func teamLogCmd(teamFlag *string) *cobra.Command {
	var (
		host    string
		user    string
		limit   int
		since   time.Duration
		format  string
		verify  bool
		noFetch bool
	)
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show the team's activity log",
		Long: "Fetches the team's repository and lists what's in it. --no-fetch reads this\n" +
			"machine's clone as it is, with no network: what it last saw.\n\n" +
			"--verify checks the whole log instead of listing it, and compares this machine's\n" +
			"activity log with the team's copy of it, which finds events removed or changed\n" +
			"here even if the local chain was rebuilt to match. Exits 1 if anything's wrong.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" && format != "csv" {
				return fmt.Errorf("--format must be text, json or csv")
			}
			ctx := cmd.Context()
			open := openTeamAny
			if noFetch {
				open = openTeamLocal
				ctx = team.WithNoFetch(ctx)
			}
			tc, err := open()
			if err != nil {
				return err
			}
			t, err := tc.current(ctx, *teamFlag)
			if err != nil {
				return err
			}
			if verify {
				return verifyTeamLog(ctx, tc, t, format)
			}
			f := team.EventFilter{Host: host, Limit: limit}
			if user != "" {
				f.Limit = 0 // filter by person here, then apply the limit
			}
			if since > 0 {
				f.Since = time.Now().Add(-since)
			}
			events, err := tc.be.ListEvents(ctx, t.ID, f)
			if err != nil {
				return err
			}
			if user != "" {
				var mine []team.TeamEvent
				for _, e := range events {
					if strings.EqualFold(e.UploaderEmail, user) {
						mine = append(mine, e)
					}
				}
				events = mine
				if limit > 0 && len(events) > limit {
					events = events[len(events)-limit:]
				}
			}
			plain := make([]activity.Event, len(events))
			for i, e := range events {
				plain[i] = e.Event
			}
			broken := activity.VerifyShared(plain)

			if format == "csv" {
				if err := writeTeamLogCSV(os.Stdout, events); err != nil {
					return err
				}
				if broken != nil {
					return fmt.Errorf("integrity check failed: %v", broken)
				}
				return nil
			}
			if format == "json" {
				return writeTeamLogJSON(os.Stdout, os.Stderr, events)
			}
			if len(events) == 0 {
				fmt.Println("No activity yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tWHO\tACTION\tHOST\tDETAIL")
			for _, e := range events {
				c := e.Event.Clean()
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.Time.Local().Format("2006-01-02 15:04:05"), termsafe.Text(e.UploaderEmail), c.Action, c.Host, oneLine(c.Detail, 60))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if broken != nil {
				return fmt.Errorf("integrity check failed: %v", broken)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only this host")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "most recent N (0 = all, up to 2000)")
	cmd.Flags().DurationVar(&since, "since", 0, "only the last duration, e.g. 24h or 720h")
	cmd.Flags().StringVar(&user, "user", "", "only this person (their account email)")
	cmd.Flags().StringVar(&format, "format", "text", "text, json or csv (csv: for spreadsheets and auditors)")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the whole log, and this machine's log against the team's copy, instead of listing")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "use this machine's clone as it is: no network, and only what it last saw")
	return cmd
}

// teamVerifyReport is `team log --verify --format json`.
type teamVerifyReport struct {
	Schema int  `json:"schema"`
	OK     bool `json:"ok"`
	// Events is how many are in the team's log, and Broken the first that doesn't hold.
	Events int         `json:"events"`
	Broken *brokenJSON `json:"broken"`
	// ExtGaps counts places where a newer event's back-pointer leads to an event with no
	// exit code or actor to check, and this machine's events that have them here and not in
	// the team's copy: uploaded by an older meterm, or stripped.
	ExtGaps int `json:"ext_gaps"`
	// Device is this machine's chain. Compared is how many of the team's events are from
	// it, and Differences where this machine's log has lost or changed them.
	Device      string                `json:"device"`
	Compared    int                   `json:"compared"`
	Differences []activity.Difference `json:"differences"`
	// NotInTeamCopy counts events here, on hosts shared now, that the upload has gone past and
	// the team hasn't got. They may be from before the host was shared, or they may have been
	// removed there, which can't be told apart, so this is a note and ok doesn't depend on it.
	NotInTeamCopy int `json:"not_in_team_copy"`
}

func verifyTeamLog(ctx context.Context, tc *teamCtx, t team.Team, format string) error {
	events, err := tc.be.ListEvents(ctx, t.ID, team.EventFilter{})
	if err != nil {
		return err
	}
	plain := make([]activity.Event, len(events))
	for i, e := range events {
		plain[i] = e.Event
	}
	check := activity.CheckShared(plain)
	rep := teamVerifyReport{Schema: 1, Events: len(events), ExtGaps: check.ExtGaps, Differences: []activity.Difference{}}
	if b := check.First; b != nil {
		rep.Broken = &brokenJSON{Line: b.Line, Seq: b.Seq, Reason: termsafe.Text(b.Reason)}
	}
	if dir, err := activity.DefaultDir(); err == nil {
		l := activity.Reader(dir)
		rep.Device = termsafe.Text(l.Device())
		local, err := l.All()
		if err != nil {
			return err
		}
		inTeam := map[int64]bool{}
		for _, e := range plain {
			if e.Device == l.Device() {
				rep.Compared++
				inTeam[e.Seq] = true
			}
		}
		if d := activity.CompareCopy(local, plain, l.Device()); d != nil {
			rep.Differences = d
		}
		// This machine can say more of its own events: the team's copy lacks what's here.
		noExt := map[int64]bool{}
		for _, e := range plain {
			if e.Device == l.Device() && e.ExtHash == "" {
				noExt[e.Seq] = true
			}
		}
		for _, e := range local {
			if e.ExtHash != "" && noExt[e.Seq] {
				rep.ExtGaps++
			}
		}
		if path, err := resolveCfgPath(); err == nil {
			if cfg, err := config.Load(path); err == nil {
				shared := map[string]bool{}
				for _, h := range cfg.Hosts {
					if h.Team == t.ID {
						shared[h.Name] = true
					}
				}
				sent, _ := l.UploadedSeq(t.ID)
				for _, e := range local {
					if e.Seq <= sent && shared[e.Host] && !inTeam[e.Seq] {
						rep.NotInTeamCopy++
					}
				}
			}
		}
	}
	rep.OK = rep.Broken == nil && len(rep.Differences) == 0
	if format == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Printf("Team log: %d event(s).\n", rep.Events)
		if rep.ExtGaps > 0 {
			fmt.Printf("%d gap(s) in what vouches for exit codes and actors: events uploaded by an older meterm, or stripped. Not treated as tampering.\n", rep.ExtGaps)
		}
		if rep.Device == "" {
			fmt.Println("This machine has no activity log to compare. If one was deleted with its directory, nothing here can tell.")
		} else {
			fmt.Printf("This machine (device %s): %d event(s) in the team's copy, %d differ from the log here.\n", rep.Device, rep.Compared, len(rep.Differences))
		}
		if rep.NotInTeamCopy > 0 {
			fmt.Printf("%d event(s) here on shared hosts aren't in the team's copy: from before a host was shared, or removed there.\n", rep.NotInTeamCopy)
		}
		for i, d := range rep.Differences {
			if i == 5 {
				fmt.Printf("  and %d more\n", len(rep.Differences)-i)
				break
			}
			fmt.Printf("  seq %d: %s\n", d.Seq, d.Reason)
		}
	}
	switch {
	case rep.Broken != nil:
		return fmt.Errorf("team log broken at seq %d: %s", rep.Broken.Seq, rep.Broken.Reason)
	case len(rep.Differences) > 0:
		return fmt.Errorf("this machine's activity log doesn't match the team's copy: %d event(s) lost or changed here", len(rep.Differences))
	}
	return nil
}

// teamLogRow is an event as `team log --format json` gives it. Verified says this event
// checks out by itself and against the ones next to it, and Problem says why not.
type teamLogRow struct {
	team.TeamEvent
	Verified bool   `json:"verified"`
	Problem  string `json:"integrity_problem,omitempty"`
}

// writeTeamLogJSON is the export, which has to stay usable whatever's in the log. That log
// is append-only and comes from everyone, so one bad line would otherwise fail this for the
// whole team for good. So every event comes out and says for itself whether it holds, a
// problem is said on stderr, and the exit code stays 0: `team log --verify` is the gate.
func writeTeamLogJSON(out, errOut io.Writer, events []team.TeamEvent) error {
	plain := make([]activity.Event, len(events))
	for i, e := range events {
		plain[i] = e.Event
	}
	check := activity.CheckShared(plain)
	rows := make([]teamLogRow, len(events))
	for i, e := range events {
		why := check.Problems[i]
		e.Event = e.Event.Clean()
		e.UploaderUID, e.UploaderEmail = termsafe.Text(e.UploaderUID), termsafe.Text(e.UploaderEmail)
		rows[i] = teamLogRow{TeamEvent: e, Verified: why == "", Problem: termsafe.Text(why)}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return err
	}
	if check.First != nil {
		fmt.Fprintf(errOut, "meterm: warning: %d event(s) in the team log don't check out, marked verified: false; the first: %s\n",
			len(check.Problems), termsafe.Text(check.First.Error()))
	}
	return nil
}

func writeTeamLogCSV(out io.Writer, events []team.TeamEvent) error {
	w := csv.NewWriter(out)
	// One place a row goes through, so no column can miss csvCell.
	row := func(cells ...string) {
		for i, c := range cells {
			cells[i] = csvCell(c)
		}
		_ = w.Write(cells)
	}
	// verified means what it means in the JSON: this event, and it against the ones next to it.
	plain := make([]activity.Event, len(events))
	for i, e := range events {
		plain[i] = e.Event
	}
	problems := activity.CheckShared(plain).Problems
	row("time", "who", "local_user", "action", "host", "target", "detail", "device", "seq", "hash", "verified")
	for i, e := range events {
		row(e.Time.UTC().Format(time.RFC3339), e.UploaderEmail, e.User, e.Action, e.Host, e.Target, e.Detail,
			e.Device, strconv.FormatInt(e.Seq, 10), e.Hash, strconv.FormatBool(problems[i] == ""))
	}
	w.Flush()
	return w.Error()
}

// csvCell makes a cell fit for a terminal and a spreadsheet. These come from a log that other
// people, and whatever they ran, wrote to. Spreadsheets skip spaces, read fullwidth forms as
// the plain ones, and take | for a command, so what counts is the first thing that isn't blank.
func csvCell(s string) string {
	s = termsafe.Clip(termsafe.Text(s), 4096)
	first := strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == termsafe.Replacement })
	if first != "" && strings.ContainsRune("=+-@|＝＋－＠｜", []rune(first)[0]) {
		return "'" + s
	}
	return s
}

// stdinLines is shared so consecutive prompts fed from a pipe don't lose
// lines to separate buffers.
var stdinLines = bufio.NewReader(os.Stdin)

func promptLine(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := stdinLines.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
