package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"meterm/internal/activity"
	"meterm/internal/cellwidth"
	"meterm/internal/config"
	"meterm/internal/fileman"
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
	case errors.Is(err, fileman.ErrDropped):
		o.Note = "stopped" // on purpose: the person dropped a stalled connection
	case errors.Is(err, errCancelled):
		o.Note = "cancelled"
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
	// One argument is a line for the host's shell: its program is the first
	// word, and the rest is one argument however many words it has.
	line := len(command) == 1
	words := command
	if line {
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
	switch n := len(words) - 1; {
	case line && n > 0:
		return prog + " (shell line)"
	case !line && n > 0:
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
		full   bool
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
			"and a real event replaced by rubbish leaves a hole the chain shows.\n\n" +
			"On a terminal a long host name is cut to 24 columns (16 under 120) and a\n" +
			"detail that doesn't fit loses its middle. What it keeps: an outcome written\n" +
			"first, up to the first ':' or ';' (\"failed:\"), whole when it leaves room for\n" +
			"more, else its start; then the start and end of the rest. A cut detail writes\n" +
			"its production note short: ⚑asked (asked here), ⚑picker (asked in the picker),\n" +
			"⚑--yes, ⚑no prompt (stdin wasn't a terminal), ⚑ (production, nothing asked).\n" +
			"A TARGET column (user@address) is added from 140 columns. --full wraps the\n" +
			"detail instead, note as written, and always has TARGET. Piped or redirected,\n" +
			"nothing is cut. --host takes a name as meterm shows it.",
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

			if host != "" {
				if events, err = filterHost(events, host); err != nil {
					return err
				}
			}

			if format == "json" {
				shown := make([]activity.Event, 0, len(events))
				for _, e := range events {
					shown = append(shown, e.Clean())
				}
				if limit > 0 && len(shown) > limit {
					shown = shown[len(shown)-limit:]
				}
				return encodeJSON(os.Stdout, shown)
			}

			rows := activityRows(events, "")
			if limit > 0 && len(rows) > limit {
				rows = rows[len(rows)-limit:]
			}
			if len(rows) == 0 && host != "" {
				fmt.Printf("No activity recorded for %s.\n", exactName(host))
				return nil
			}
			if len(rows) == 0 {
				fmt.Println("No activity recorded yet.")
				return nil
			}
			printActivity(os.Stdout, rows, terminalWidth(os.Stdout), full)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only show actions on this host (the name as shown, or exactly)")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "show at most this many recent events (0 = all)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the hash chain instead of listing events")
	cmd.Flags().BoolVar(&repair, "repair", false, "move lines that aren't events aside, keep them, and record that")
	cmd.Flags().BoolVar(&full, "full", false, "show each detail in full, wrapped, instead of cut at the terminal's edge")
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
	s = strings.Join(strings.Fields(lbl(s)), " ")
	if len([]rune(s)) > max {
		return string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// filterHost is the events on the host called name: by its exact name, or
// by the name as meterm shows it, with config.Lookup's rule — a shown name
// two hosts share is an error naming both. Hosts that were removed or
// renamed are still in the log, so it goes by the log's names.
func filterHost(events []activity.Event, name string) ([]activity.Event, error) {
	match := func(e activity.Event) bool { return e.Host == name }
	if lbl(name) == name {
		var same []string
		seen := map[string]bool{}
		for _, e := range events {
			if lbl(e.Host) == name && !seen[e.Host] {
				seen[e.Host] = true
				same = append(same, strconv.Quote(e.Host))
			}
		}
		if len(same) > 1 {
			return nil, fmt.Errorf("%q is %d hosts in the log as meterm shows them (%s): use the exact name, as `meterm activity --format json` spells it",
				name, len(same), strings.Join(same, ", "))
		}
		match = func(e activity.Event) bool { return lbl(e.Host) == name }
	}
	var out []activity.Event
	for _, e := range events {
		if match(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// The HOST column is at most this wide on a terminal, so one long name
// doesn't push every detail off the line; narrower under 120 columns, where
// the year and the seconds' room goes to the detail too.
const (
	activityHostWidth       = 24
	activityHostWidthNarrow = 16
	activityNarrow          = 120
)

// A detail with fewer cells than activityDetailMin left on its row goes on
// the line below instead, indented activityBelowIndent.
const (
	activityDetailMin   = 24
	activityBelowIndent = 2
)

// activityTargetAt is the terminal width from which TARGET (user@address)
// gets a column: it tells apart two hosts that show the same name.
const activityTargetAt = 140

// printActivity writes rows as a table aligned by terminal cells. width is
// the terminal's; 0 means not a terminal, where nothing is cut. On a
// terminal HOST is capped and the detail gets the rest of the line, cut in
// the middle to it, unless full asks for it wrapped onto lines of its own.
// EXIT and TOOK are the outcome folded onto its start (activityRows).
//
// Under activityNarrow columns, ACTOR and EXIT/TOOK are left out when no row
// shown has anything in them, so the detail keeps its room; piped, or with
// a row that has them, they're there.
func printActivity(out io.Writer, rows []activityRow, width int, full bool) {
	withTarget := full || width >= activityTargetAt
	narrow := width > 0 && width < activityNarrow
	withActor, withEnd := !narrow, !narrow
	for _, row := range rows {
		if a := actorLabel(row.Event); a != "-" && a != "unknown" {
			withActor = true
		}
		if row.exit != "-" || row.took != "-" {
			withEnd = true
		}
	}
	head := []string{"TIME", "USER"}
	if withActor {
		head = append(head, "ACTOR")
	}
	head = append(head, "ACTION", "HOST")
	if withTarget {
		head = append(head, "TARGET")
	}
	if withEnd {
		head = append(head, "EXIT", "TOOK")
	}
	table := [][]string{append(head, "DETAIL")}
	hostW, timeFmt := activityHostWidth, "2006-01-02 15:04:05"
	if width > 0 && width < activityNarrow {
		hostW, timeFmt = activityHostWidthNarrow, "01-02 15:04:05"
	}
	for _, row := range rows {
		e := row.Clean()
		host := exactName(e.Host) // cleaned: what was hidden shows as �
		if width > 0 {
			host = cellwidth.Fit(host, hostW)
		}
		r := []string{e.Time.Local().Format(timeFmt), lbl(e.User)}
		if withActor {
			r = append(r, lbl(actorLabel(e)))
		}
		r = append(r, lbl(e.Action), host)
		if withTarget {
			r = append(r, lbl(e.Target))
		}
		if withEnd {
			r = append(r, row.exit, row.took)
		}
		table = append(table, append(r, strings.Join(strings.Fields(lbl(e.Detail)), " ")))
	}
	const gap = 2
	colW := make([]int, len(head))
	for _, r := range table {
		for c := range colW {
			colW[c] = max(colW[c], cellwidth.Width(r[c]))
		}
	}
	indent := 0
	for _, cw := range colW {
		indent += cw + gap
	}
	detailW := 0 // no limit
	below := false
	if width > 0 {
		detailW = width - indent
		if detailW < activityDetailMin {
			// Too few cells for an outcome and a note: the detail gets a
			// line of its own under its row.
			below, indent, detailW = true, activityBelowIndent, max(width-activityBelowIndent, 10)
		}
	}
	for _, r := range table {
		var b strings.Builder
		for c, cw := range colW {
			b.WriteString(r[c] + strings.Repeat(" ", cw-cellwidth.Width(r[c])+gap))
		}
		if below {
			fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
			b.Reset()
			b.WriteString(strings.Repeat(" ", indent))
		}
		d := r[len(r)-1]
		switch {
		case detailW == 0 || cellwidth.Width(d) <= detailW:
			fmt.Fprintln(out, strings.TrimRight(b.String()+d, " "))
		case full:
			lines := cellwidth.Wrap(d, detailW, 1<<20)
			fmt.Fprintln(out, b.String()+lines[0])
			for _, l := range lines[1:] {
				fmt.Fprintln(out, strings.Repeat(" ", indent)+l)
			}
		default:
			fmt.Fprintln(out, b.String()+cutMiddle(shortProdNote(d), detailW))
		}
	}
}

// prodNoteRE is the production note a start's detail carries first:
// "(production, picker asked) ls (+2 args)" (noteDetail).
// A host reached through production says "(via production, …)", a tunnel
// whose note says it leads there "(tunnel to production, …)".
var prodNoteRE = regexp.MustCompile(`\s?\((via |tunnel to )?production(?:, ([^)]*))?\)`)

// shortProdNote writes a detail's production note as ⚑ and a word or two,
// for a detail about to be cut: "(production, no prompt: not a terminal)"
// is too long to survive 20 cells, and losing it makes a row on production
// read like any other.
func shortProdNote(d string) string {
	d = prodNoteRE.ReplaceAllStringFunc(d, func(m string) string {
		sub := prodNoteRE.FindStringSubmatch(m)
		how := sub[2]
		switch how {
		case "picker asked":
			how = "picker"
		case "no prompt: not a terminal":
			how = "no prompt"
		case "named on the command line":
			how = "named"
		case "picker didn't ask":
			how = "not asked"
		}
		switch sub[1] {
		case "via ":
			how = "via " + how
		case "tunnel to ":
			how = "tunnel " + how
		}
		return " ⚑" + strings.TrimSpace(how)
	})
	return strings.TrimPrefix(d, " ")
}

// outcomeEnd is the index of the first ':' or ';' in s outside brackets,
// which ends a detail's outcome ("failed:", "exit 1 after 0.2s;"), or -1.
func outcomeEnd(s string) int {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
		case ':', ';':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// cutMiddle fits s to w cells by leaving out its middle. The outcome, up to
// the first ':' or ';', is kept whole when it leaves room for a little more;
// then as much of the start of the rest as fits, and its end: the last path
// segment (up to two thirds of the room) or else the last half. An outcome
// too long for that keeps its start.
func cutMiddle(s string, w int) string {
	if cellwidth.Width(s) <= w {
		return s
	}
	if w < 8 {
		return cellwidth.Fit(s, w)
	}
	if i := outcomeEnd(s); i > 0 {
		lead, rest := s[:i+1], s[i+1:]
		lw := cellwidth.Width(lead)
		switch room := w - lw; {
		case room < 4:
			return cellwidth.Fit(s, w)
		case room < 8:
			tail := strings.TrimPrefix(cellwidth.TruncLeft(rest, room-1), "…")
			return lead + "…" + rest[wholeMarkers(rest, len(rest)-len(tail), false):]
		default:
			return lead + cutEnds(rest, room)
		}
	}
	return cutEnds(s, w)
}

// cutEnds fits s, wider than w (8 or more), to w by leaving out its middle.
func cutEnds(s string, w int) string {
	tail := ""
	if i := strings.LastIndex(s, "/"); i >= 0 && cellwidth.Width(s[i+1:]) <= 2*w/3 {
		tail = s[i+1:]
	} else if i := strings.LastIndex(s, " "); i >= 0 && cellwidth.Width(s[i+1:]) <= w/2 {
		tail = s[i+1:]
	}
	if tail == "" {
		tail = strings.TrimPrefix(cellwidth.TruncLeft(s, w/2), "…")
	}
	tail = s[wholeMarkers(s, len(s)-len(tail), false):]
	head := cellwidth.Fit(s[:len(s)-len(tail)], w-cellwidth.Width(tail))
	if !strings.HasSuffix(head, "…") {
		head += "…"
		for cellwidth.Width(head)+cellwidth.Width(tail) > w {
			head = cellwidth.Fit(strings.TrimSuffix(head, "…"), cellwidth.Width(head)-1)
		}
	}
	kept := strings.TrimSuffix(head, "…")
	return s[:wholeMarkers(s, len(kept), true)] + "…" + tail
}

// wholeMarkers moves a cut at byte i of s out of any [REDACTED] it falls
// in: back to its start when the text before the cut is kept (left), on
// past its end when the text after it is. Half a marker reads as a value
// cut short, or as one that was never masked.
func wholeMarkers(s string, i int, left bool) int {
	const marker = "[REDACTED]"
	for at := 0; ; {
		j := strings.Index(s[at:], marker)
		if j < 0 {
			return i
		}
		a, b := at+j, at+j+len(marker)
		if a < i && i < b {
			if left {
				return a
			}
			return b
		}
		at = b
	}
}

// terminalWidth is f's width in columns when it's a terminal, 0 when it
// isn't (a pipe or a file, where nothing should be cut).
func terminalWidth(f *os.File) int {
	if !isTerminal(f) {
		return 0
	}
	if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
		return w
	}
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 0 {
		return w
	}
	return 80
}
