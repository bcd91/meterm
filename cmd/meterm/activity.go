package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/termsafe"
)

// recordActivity notes an action in the local activity log. Best-effort like
// history: a broken log warns but never blocks reaching the host.
func recordActivity(cfg *config.Config, h config.Host, action, detail string) (activity.Event, bool) {
	r := cfg.Resolve(h)
	target := r.Address
	if r.User != "" {
		target = r.User + "@" + r.Address
	}
	e, err := activity.Record(action, h.Name, target, detail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "meterm: warning: activity log not updated: %s\n", termsafe.Text(err.Error()))
		return e, false
	}
	activity.SendToSink(cfg.Activity.Sink)
	return e, true
}

// activityRun is an action on a host from the moment it's logged, so that how it ended
// can be logged too. The start goes in first, before anything reaches the host, and
// goes on being what the log says if this process never gets to end(): killed, or a
// machine losing power. A long `connect` shows from the start that it happened.
type activityRun struct {
	sink   string
	start  activity.Event
	logged bool
	began  time.Time
}

func beginActivity(cfg *config.Config, h config.Host, action, detail string) *activityRun {
	e, ok := recordActivity(cfg, h, action, detail)
	return &activityRun{sink: cfg.Activity.Sink, start: e, logged: ok, began: time.Now()}
}

// outcome logs o with no autosync, for when several end together.
func (a *activityRun) outcome(o activity.Outcome) {
	if a == nil || !a.logged {
		return
	}
	if _, err := activity.RecordOutcome(a.start, o); err != nil {
		fmt.Fprintf(os.Stderr, "meterm: warning: activity log not updated: %s\n", termsafe.Text(err.Error()))
		return
	}
	activity.SendToSink(a.sink)
}

// end logs how a command ended, and starts its upload: the start's own may still be
// running, and the wait for that is in `team background`.
func (a *activityRun) end(err error) {
	a.outcome(outcomeOf(err, time.Since(a.began)))
	kickAutosync(false, true)
}

// outcomeOf reads an ended command. err's text stays out, since it can quote a host.
func outcomeOf(err error, took time.Duration) activity.Outcome {
	o := activity.Outcome{Took: took}
	var exit *exec.ExitError
	switch {
	case err == nil:
		code := 0
		o.ExitCode = &code
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		code := exit.ExitCode()
		o.ExitCode = &code
	case errors.As(err, &exit):
		o.Note = "killed"
	default:
		o.Note = "failed"
	}
	return o
}

// runInherited is the package's runInherited with the outcome logged first, as that
// one can end the process.
func (a *activityRun) runInherited(c *exec.Cmd) error {
	err := c.Run()
	a.end(err)
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			finish(exit.ExitCode())
		}
		return err
	}
	return nil
}

// envWordRE is a leading NAME=value word, which says nothing about what ran and
// is where a secret would be.
var envWordRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandDetail is what the log says about a command run on a host. The log goes to
// the team and can't forget, and a command's arguments can hold anything: a pasted
// prompt, or a secret in a form redaction doesn't know. So by default it keeps what
// was run and not what with; logArgs asks for the arguments too, redacted as all
// detail is. One argument is a command line, whose first word is the program.
func commandDetail(command []string, logArgs bool) string {
	if logArgs {
		return strings.Join(command, " ")
	}
	words := command
	if len(words) == 1 {
		words = strings.Fields(words[0])
	}
	for len(words) > 0 && envWordRE.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	prog := words[0]
	if r := []rune(prog); len(r) > 64 {
		prog = string(r[:64]) + "…"
	}
	if n := len(words) - 1; n > 0 {
		return fmt.Sprintf("%s (+%d args)", prog, n)
	}
	return prog
}

func newActivityCmd() *cobra.Command {
	var (
		host   string
		limit  int
		format string
		verify bool
		repair bool
	)
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show what meterm was used to do on saved hosts (a local record of use)",
		Long: "Every connect, exec, cp, sync and sftp is appended to a local log where each\n" +
			"entry carries the hash of the one before it, so --verify notices an edited or\n" +
			"removed entry. It's a record of use: anything running as this user can rewrite the\n" +
			"whole of it, and only actions taken through meterm are in it, a plain `ssh` not.\n\n" +
			"--format json prints an array of events: see docs/export.md. With --verify it prints\n" +
			"{\"schema\":1,\"ok\",\"events\",\"broken\",\"sink\"}.\n\n" +
			"--verify exits 1 if the chain doesn't hold, and 2 if it does and activity.sink is failing\n" +
			"or differs. A last line cut short by a crash is a note, not a failure.\n\n" +
			"--repair moves lines that aren't events to a damaged-* file beside the log, and records\n" +
			"that as an event. It can't make a log hold that doesn't: an edited event stays broken,\n" +
			"and a real event replaced by rubbish leaves a hole the chain shows.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "" && format != "json" {
				return fmt.Errorf("unknown --format %q (use text or json)", format)
			}
			dir, err := activity.DefaultDir()
			if err != nil {
				return err
			}
			l := activity.Reader(dir) // looking at the log doesn't start one
			if repair {
				if verify || format == "json" {
					return fmt.Errorf("--repair stands alone")
				}
				return repairActivity(l)
			}
			if verify {
				return verifyActivity(l, format)
			}
			events, err := l.All()
			if err != nil {
				return err
			}

			if format == "json" {
				var shown []activity.Event
				for _, e := range events {
					if host == "" || e.Host == host {
						shown = append(shown, e.Clean())
					}
				}
				if limit > 0 && len(shown) > limit {
					shown = shown[len(shown)-limit:]
				}
				if shown == nil {
					shown = []activity.Event{}
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(shown)
			}

			rows := activityRows(events, host)
			if limit > 0 && len(rows) > limit {
				rows = rows[len(rows)-limit:]
			}
			if len(rows) == 0 {
				fmt.Println("No activity recorded yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tUSER\tACTOR\tACTION\tHOST\tEXIT\tTOOK\tDETAIL")
			for _, r := range rows {
				e := r.Clean()
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.User,
					actorLabel(e), e.Action, e.Host, r.exit, r.took, oneLine(e.Detail, 60))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only show actions on this saved host")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "show at most this many recent events (0 = all)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the hash chain instead of listing events")
	cmd.Flags().BoolVar(&repair, "repair", false, "move lines that aren't events aside, keep them, and record that")
	return cmd
}

// Exit code of `activity --verify` when the chain holds and its copy doesn't. 1 stays
// what it was: the chain is broken, or nothing could be read.
const activitySinkTrouble = 2

// brokenJSON is where a chain stops holding.
type brokenJSON struct {
	Line   int    `json:"line"`
	Seq    int64  `json:"seq"`
	Reason string `json:"reason"`
}

// activityVerifyReport is `activity --verify --format json`.
type activityVerifyReport struct {
	Schema int         `json:"schema"`
	OK     bool        `json:"ok"`
	Events int         `json:"events"`
	Broken *brokenJSON `json:"broken"`
	// TornTail is a last line cut short by a crash or a full disk. Nothing whole is missing,
	// so it doesn't affect ok, and the next event moves it aside. Repairs are the seqs of
	// events saying bytes were moved out of the log.
	TornTail *tornJSON            `json:"torn_tail"`
	Repairs  []int64              `json:"repairs"`
	Sink     *activity.SinkReport `json:"sink"`
}

type tornJSON struct {
	Line  int `json:"line"`
	Bytes int `json:"bytes"`
}

func verifyActivity(l *activity.Log, format string) error {
	n, broken, err := l.VerifyFile()
	if err != nil {
		return err
	}
	rep := activityVerifyReport{Schema: 1, Events: n, OK: broken == nil, Repairs: []int64{}}
	if broken != nil {
		rep.Broken = &brokenJSON{Line: broken.Line, Seq: broken.Seq, Reason: termsafe.Text(broken.Reason)}
	}
	if notes, err := l.Notes(); err == nil {
		if notes.TornLine != 0 {
			rep.TornTail = &tornJSON{Line: notes.TornLine, Bytes: notes.TornBytes}
		}
		if notes.Repairs != nil {
			rep.Repairs = notes.Repairs
		}
	}
	// config.Load, not loadConfig: this only reads. A host file that can't be read says
	// nothing about the chain, and does mean no sink can be named.
	if path, err := resolveCfgPath(); err == nil {
		if cfg, err := config.Load(path); err != nil {
			fmt.Fprintf(os.Stderr, "meterm: can't say how the sink is: %s\n", termsafe.Text(err.Error()))
		} else if cfg.Activity.Sink != "" {
			s := l.CheckSink(cfg.Activity.Sink)
			s.Target, s.LastError = termsafe.Text(s.Target), termsafe.Text(s.LastError)
			rep.Sink = &s
		}
	}
	if format == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
			return err
		}
	} else {
		if rep.OK {
			fmt.Printf("OK: %d event(s), chain intact\n", n)
			if n == 0 {
				fmt.Println("That's all a log with nothing in it can say: if its whole directory was deleted, nothing here shows it. A copy elsewhere would.")
			}
		}
		switch {
		case rep.TornTail != nil && rep.OK:
			fmt.Printf("Note: line %d is torn, %d bytes cut short by a crash or a full disk. Nothing whole is missing; the next event moves it aside, or `meterm activity --repair` does now.\n", rep.TornTail.Line, rep.TornTail.Bytes)
		case rep.TornTail != nil:
			fmt.Printf("Note: line %d is also cut short, %d bytes.\n", rep.TornTail.Line, rep.TornTail.Bytes)
		}
		if len(rep.Repairs) > 0 {
			fmt.Printf("Note: this log has been repaired or found changed %d time(s), most recently at seq %d: see `meterm activity`, and any damaged-* files beside the log.\n", len(rep.Repairs), rep.Repairs[len(rep.Repairs)-1])
		}
		if rep.Sink != nil {
			writeSinkReport(os.Stdout, *rep.Sink)
		}
	}
	switch {
	case broken != nil:
		return broken
	case rep.Sink != nil && !rep.Sink.Healthy():
		fmt.Fprintf(os.Stderr, "meterm: the chain holds, and its copy needs a look: %s\n", rep.Sink.State)
		finish(activitySinkTrouble)
	}
	return nil
}

// repairActivity is `activity --repair`.
func repairActivity(l *activity.Log) error {
	res, err := l.Repair()
	if err != nil {
		return err
	}
	if len(res.Lines) == 0 {
		fmt.Println("Nothing to repair: every line is an event.")
	} else {
		fmt.Printf("Moved %d damaged line(s) to %s beside the log, and recorded that as seq %d.\n", len(res.Lines), termsafe.Text(res.SavedTo), res.EventSeq)
	}
	_, broken, err := l.VerifyFile()
	if err != nil {
		return err
	}
	if broken != nil {
		return fmt.Errorf("the log still doesn't hold, and repair can't change that: %w", broken)
	}
	fmt.Println("The chain holds.")
	return nil
}

func writeSinkReport(w io.Writer, s activity.SinkReport) {
	where := s.Kind
	if s.Target != "" {
		where = s.Target
	}
	fmt.Fprintf(w, "Sink %s: %s, sent through seq %d", where, s.State, s.SentSeq)
	if s.Copy == "compared" {
		fmt.Fprintf(w, "; copy compared, %d difference(s)", len(s.Differences))
	} else {
		fmt.Fprintf(w, "; copy %s", s.Copy)
	}
	fmt.Fprintln(w)
	if s.FailingSince != nil {
		fmt.Fprintf(w, "  failing since %s\n", s.FailingSince.Local().Format("2006-01-02 15:04:05"))
	}
	if s.LastError != "" {
		fmt.Fprintf(w, "  last error: %s\n", s.LastError)
	}
	for i, d := range s.Differences {
		if i == 5 {
			fmt.Fprintf(w, "  and %d more\n", len(s.Differences)-i)
			break
		}
		fmt.Fprintf(w, "  seq %d: %s\n", d.Seq, d.Reason)
	}
}

// activityRow is an event as the table shows it: an outcome goes on the line of what
// it's the outcome of, or stands alone if that line is gone. "-" under EXIT is an end
// nobody recorded, or one with no exit code to give.
type activityRow struct {
	activity.Event
	exit, took string
}

func activityRows(events []activity.Event, host string) []activityRow {
	var rows []activityRow
	bySeq := map[int64]int{}
	for _, e := range events {
		if host != "" && e.Host != host {
			continue
		}
		if i, ok := bySeq[e.RefSeq]; ok && e.Action == activity.ActionOutcome && e.RefSeq != 0 {
			rows[i].exit, rows[i].took = outcomeCells(e)
			continue
		}
		r := activityRow{Event: e, exit: "-", took: "-"}
		if e.Action == activity.ActionOutcome {
			r.exit, r.took = outcomeCells(e)
		} else {
			bySeq[e.Seq] = len(rows)
		}
		rows = append(rows, r)
	}
	return rows
}

func outcomeCells(e activity.Event) (exit, took string) {
	exit, took = "-", "-"
	if e.ExitCode != nil {
		exit = fmt.Sprint(*e.ExitCode)
	}
	if e.DurationMS != nil {
		took = (time.Duration(*e.DurationMS) * time.Millisecond).Round(100 * time.Millisecond).String()
	}
	return exit, took
}

func actorLabel(e activity.Event) string {
	switch {
	case e.ActorType == "":
		return "-"
	case e.AgentKind != "":
		return e.ActorType + ":" + e.AgentKind
	}
	return e.ActorType
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		return string([]rune(s)[:max-1]) + "…"
	}
	return s
}
