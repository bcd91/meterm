package fileman

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"meterm/internal/cellwidth"
	"meterm/internal/termsafe"
)

// Options configures a file manager session.
type Options struct {
	Left, Right       FS // left is normally this machine, right the host
	LeftDir, RightDir string
	// OnEvent is told about everything that changes something, for the
	// activity log: each operation's start before it touches anything,
	// any answer the person gives part way, and how it ended. See Event.
	// It may be called from other goroutines than the one running Update.
	OnEvent func(Event)
	// Abort drops the connection to the host. It's the way out when the
	// host stops answering mid-operation: a cancel can't reach a peer that
	// says nothing, and the SFTP calls waiting on it never return.
	Abort func()
	// Env is the host's environment (production, staging, dev, test or
	// ""): a production host's pane and its destructive questions say so.
	Env string
}

// Event is one entry about an operation, for the activity log.
type Event struct {
	// Op is the operation it belongs to: its start, the answers given part
	// way, and its end all have the same Op.
	Op int64
	// Kind is upload, download, copy, delete, rename or mkdir.
	Kind string
	// Outcome is "started", "asked" (an answer part way), or how it ended:
	// "ok", "failed", "partial", "cancelled", "stopped", "aborted",
	// "already gone" (a delete) or "already there" (a mkdir).
	Outcome string
	// Detail is what it is: for a start, what's about to happen; for an
	// end, what happened, in full.
	Detail string
	// More says an end tells something its start didn't (a file written
	// in place, an error, what was done before it stopped): worth keeping
	// in words, not just as an outcome.
	More bool
	// Remote says it happened on the host (Right).
	Remote bool
}

// Line is the event as one line: how it went, then what it was:
// "ok: upload a.txt -> /srv (1 file(s), 3 B)".
func (e Event) Line() string { return e.Outcome + ": " + e.Kind + " " + e.Detail }

// Ends says the event is how its operation ended.
func (e Event) Ends() bool { return e.Outcome != "started" && e.Outcome != "asked" }

// opSeq numbers operations within this process.
var opSeq atomic.Int64

func newOp() int64 { return opSeq.Add(1) }

type mode int

const (
	modeNormal mode = iota
	modeInput
	modeConfirm
	modeBusy
	modeHelp
	// modeNotice holds keys after a question went away under the person,
	// until enter or esc: the answer they were typing (y is also copy, and
	// a name is a string of commands) mustn't run as commands.
	modeNotice
)

type inputKind int

const (
	inputMkdir inputKind = iota
	inputRename
	inputFind
)

// busyKind is what a busy file manager is waiting for.
type busyKind int

const (
	busyNone busyKind = iota
	busyPlan
	busyCopy
	busyDelete
)

// stallAfter is how long without a byte before the progress line says so.
const stallAfter = 5 * time.Second

// rateWindow is how far back the transfer rate looks.
const rateWindow = 5 * time.Second

// tickEvery is how often a busy screen redraws when nothing else happens,
// so a stall shows even though no progress arrives.
var tickEvery = 500 * time.Millisecond

type sample struct {
	at   time.Time
	done int64
}

type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmOverwrite
	confirmInPlace
	confirmLoss
)

// asked is what an open question or name box is about, taken as it opened.
// Its answer acts on this alone, never on the panes as they are by then: a
// listing that lands meanwhile moves the cursor, or changes the directory.
type asked struct {
	pane    int // the pane it acts on
	fs      FS
	dir     string
	dirs    [2]string // each pane's directory as it opened
	watch   [2]bool   // the panes whose directory changing cancels it
	targets []Entry   // a delete's
	paths   []string  // their full paths
	from    string    // a rename's: the full path of what it renames
	name    string    // and its name
	initial string    // the name box's starting text
}

// dirChanged is why a question or a box went away under the person.
const dirChanged = "cancelled: the directory changed while asking"

type pane struct {
	fs      FS
	dir     string
	entries []Entry
	cursor  int
	offset  int
	sel     map[string]bool
	hidden  bool   // show dotfiles
	focusOn string // name to put the cursor on after the next load
	err     string
}

func (p *pane) visible() []Entry {
	if p.hidden {
		return p.entries
	}
	out := make([]Entry, 0, len(p.entries))
	for _, e := range p.entries {
		if !strings.HasPrefix(e.Name, ".") {
			out = append(out, e)
		}
	}
	return out
}

func (p *pane) current() (Entry, bool) {
	v := p.visible()
	if p.cursor < 0 || p.cursor >= len(v) {
		return Entry{}, false
	}
	return v[p.cursor], true
}

// targets are the selected entries, or the one under the cursor.
func (p *pane) targets() []Entry {
	var out []Entry
	for _, e := range p.visible() {
		if p.sel[e.Name] {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		if e, ok := p.current(); ok && e.Name != ".." {
			out = append(out, e)
		}
	}
	return out
}

func (p *pane) clamp(rows int) {
	n := len(p.visible())
	if p.cursor >= n {
		p.cursor = n - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	if rows < 1 {
		rows = 1
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+rows {
		p.offset = p.cursor - rows + 1
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

// Model is the file manager's Bubble Tea model.
type Model struct {
	opts   Options
	panes  [2]*pane
	active int
	w, h   int

	mode        mode
	input       textinput.Model
	inputKind   inputKind
	confirmKind confirmKind
	confirmText string

	lastFind string // for n (next match)

	// A destructive answer (y to delete, o to overwrite) only counts once
	// the question has been on screen a moment: "dy" typed as one burst
	// (its keys are handled together) is a slip, not a decision.
	confirmAt time.Time

	// ask is what the open question or name box was opened about.
	ask *asked
	// copyDirs are the panes' directories when c was pressed, for the
	// question the copy may come back with.
	copyDirs [2]string

	pendingPlan   *Plan
	pendingDelete []Entry
	// A copy stopped to ask about writing a file in place: what it was
	// asked, how it was running, and what it had done so far.
	pendingInPlace *InPlaceNeeded
	// Or it stopped to ask about replacing a file that would lose its
	// owner, group or hard links.
	pendingLoss *ReplaceLoses
	runOpts     RunOptions
	carry       Result

	// lastOp is what the latest operation did, in full, for the ? screen.
	lastOp []string

	status    string
	statusErr bool
	progress  *Progress
	cancel    context.CancelFunc
	events    chan tea.Msg

	busy       busyKind
	busyDetail string // what the busy operation is working on, for the log if it's abandoned
	busyRemote bool   // whether that's on the host
	busyOp     int64  // and which operation it is
	copyOp     int64  // the copy under way, or stopped to ask: from its start to its end
	cancelling bool
	// stopAfter stops a copy the gentle way (RunOptions.After): a file
	// written in place is let finish. stopping says it was asked for, so the
	// next esc stops at once.
	stopAfter  context.CancelFunc
	stopping   bool
	running    *Plan // the copy under way
	busySince  time.Time
	clock      time.Time // the latest time seen in a progress report or tick
	lastChange time.Time // when the byte count last moved
	samples    []sample
	tickID     int
	exitErr    error // why the session ended, when it wasn't q
	helpTop    int   // the ? screen's first line shown

	now func() time.Time
}

// New returns a model; Init loads both directories.
func New(o Options) Model {
	ti := textinput.New()
	ti.CharLimit = 255
	ti.Cursor.SetMode(cursor.CursorStatic) // no blink timer: the UI only redraws on input
	return Model{
		opts:  o,
		panes: [2]*pane{{fs: o.Left, dir: o.LeftDir, sel: map[string]bool{}}, {fs: o.Right, dir: o.RightDir, sel: map[string]bool{}}},
		input: ti,
		w:     100, h: 30,
		now: time.Now,
	}
}

// ExitError is why the session ended, when it ended other than by q.
func (m Model) ExitError() error { return m.exitErr }

type tickMsg struct {
	id int
	at time.Time
}

func tickCmd(id int) tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg{id: id, at: t} })
}

// startBusy enters busy mode for kind and returns a context that Esc
// cancels, plus the ticker that keeps the screen current meanwhile.
func (m *Model) startBusy(kind busyKind, detail string) (context.Context, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	now := m.now()
	m.mode, m.busy, m.busyDetail, m.cancel, m.cancelling = modeBusy, kind, detail, cancel, false
	m.busySince, m.clock, m.lastChange, m.samples = now, now, now, nil
	m.tickID++
	return ctx, tickCmd(m.tickID)
}

// mayWriteInPlace says the copy running may write a file in place, so a
// stop at once could leave one partly written.
func (m Model) mayWriteInPlace() bool {
	return m.busy == busyCopy && (len(m.runOpts.InPlace) > 0 || m.runOpts.OnLoss == LossInPlace)
}

// stopNowHint is what the next esc does once a gentle stop was asked for.
const stopNowHint = "stopping after the file written in place · esc again: stop now (it is left partly written)"

// endBusy leaves busy mode.
func (m *Model) endBusy() {
	if m.cancel != nil {
		m.cancel() // release the context
	}
	m.mode, m.busy, m.busyDetail, m.cancel, m.cancelling = modeNormal, busyNone, "", nil, false
	m.stopAfter, m.stopping = nil, false
	m.progress, m.running, m.samples = nil, nil, nil
	m.tickID++ // stops the ticker
}

// observe records the byte count at time at, for the rate and the stall.
func (m *Model) observe(at time.Time, done int64) {
	if at.Before(m.clock) {
		at = m.clock
	}
	m.clock = at
	if n := len(m.samples); n == 0 || done != m.samples[n-1].done {
		m.lastChange = at
	}
	m.samples = append(m.samples, sample{at, done})
	// Keep one sample at or before the window's start as the base.
	cut := 0
	for i, s := range m.samples {
		if !s.at.After(at.Add(-rateWindow)) {
			cut = i
		}
	}
	m.samples = m.samples[cut:]
}

// rate is bytes per second over the last rateWindow; ok is false until
// there's a second of data to go on.
func (m Model) rate() (float64, bool) {
	if len(m.samples) < 2 {
		return 0, false
	}
	first, last := m.samples[0], m.samples[len(m.samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs < 1 {
		return 0, false
	}
	return float64(last.done-first.done) / secs, true
}

// abort is the way out of an operation that won't stop: drop the
// connection, log what's known, and quit.
func (m Model) abort() (tea.Model, tea.Cmd) {
	if m.opts.Abort != nil {
		m.opts.Abort()
	}
	host := m.opts.Right.Label()
	var what string
	switch m.busy {
	case busyCopy:
		what = "a copy"
		if m.running != nil {
			done, temp := 0, ""
			if m.progress != nil {
				done, temp = m.progress.Files, m.progress.Temp
			}
			kind, remote := m.transferKind(*m.running)
			d := fmt.Sprintf("%s: connection dropped after %d of %d file(s)", m.transferSummary(*m.running), done, len(m.running.Files))
			if temp != "" {
				d += "; unfinished temporary file may remain: " + where(m.running.Dst, temp)
			}
			if m.progress != nil && m.progress.InPlace != "" {
				d += "; was writing in place, may be partly written: " + where(m.running.Dst, m.progress.InPlace)
			}
			m.emit(Event{Op: m.copyOp, Kind: kind, Outcome: "aborted", Detail: d, More: true, Remote: remote})
		}
	case busyDelete:
		what = "a delete"
		m.emit(Event{Op: m.busyOp, Kind: "delete", Outcome: "aborted", Detail: m.busyDetail + ": connection dropped; may be partly deleted", More: true, Remote: m.busyRemote})
	case busyPlan:
		what = "working out a copy"
	}
	m.exitErr = fmt.Errorf("stopped waiting for %s during %s: the connection was dropped", host, what)
	if m.progress != nil && m.progress.Temp != "" && m.running != nil {
		m.exitErr = fmt.Errorf("%w; an unfinished temporary file may be left at %s", m.exitErr, where(m.running.Dst, m.progress.Temp))
	}
	if m.progress != nil && m.progress.InPlace != "" && m.running != nil {
		m.exitErr = fmt.Errorf("%w; %s was being written in place and may be partly written", m.exitErr, where(m.running.Dst, m.progress.InPlace))
	}
	m.endBusy()
	return m, tea.Quit
}

func (m Model) emit(e Event) {
	if m.opts.OnEvent != nil {
		m.opts.OnEvent(e)
	}
}

type dirLoadedMsg struct {
	pane    int
	dir     string
	entries []Entry
	err     error
}

type planMsg struct {
	plan Plan
	err  error
}

type progressMsg Progress

type transferDoneMsg struct {
	res  Result
	err  error
	plan Plan
}

type opDoneMsg struct {
	panes  []int
	status string
	err    error
	focus  string   // name to put the cursor on, only when the operation worked
	detail []string // what it did, in full, for the ? screen
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))
}

func (m Model) load(i int, dir string) tea.Cmd {
	fsys := m.panes[i].fs
	return func() tea.Msg {
		es, err := fsys.ReadDir(dir)
		return dirLoadedMsg{pane: i, dir: dir, entries: es, err: err}
	}
}

// Below this the file manager says the terminal is too small rather than
// draw panes that don't fit.
const (
	minWidth  = 40
	minHeight = 10
)

func (m Model) tooSmall() bool { return m.w < minWidth || m.h < minHeight }

// rows is how many entries each pane shows: the screen less the borders,
// the path line, the status lines and the key line.
func (m Model) rows() int {
	r := m.h - 2 /*border*/ - 1 /*path*/ - len(m.statusLines()) - 1 /*keys*/
	if r < 1 {
		r = 1
	}
	return r
}

// Update handles msg, then keeps each pane's cursor on a row that's drawn:
// a message taking a second line takes a row from the panes.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if fm, ok := next.(Model); ok {
		for _, p := range fm.panes {
			p.clamp(fm.rows())
		}
	}
	return next, cmd
}

func (m *Model) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

// capture is what a question or box about pane i is opened on: that pane,
// and the directory each side shows now.
func (m Model) capture(i int) *asked {
	p := m.panes[i]
	a := &asked{pane: i, fs: p.fs, dir: p.dir, dirs: [2]string{m.panes[0].dir, m.panes[1].dir}}
	a.watch[i] = true
	return a
}

// asking says a question or a name box is open (finding isn't asking).
func (m Model) asking() bool {
	return m.mode == modeConfirm || m.mode == modeInput && m.inputKind != inputFind
}

// dropQuestion closes the open question or name box unanswered, saying
// why. A copy that stopped to ask stops there.
func (m Model) dropQuestion(why string) Model {
	if m.mode == modeConfirm && (m.confirmKind == confirmInPlace || m.confirmKind == confirmLoss) {
		next, _ := m.stopAtInPlace(why)
		return next.(Model)
	}
	if m.mode == modeInput {
		m.input.Blur()
	}
	m.mode, m.pendingPlan, m.pendingDelete, m.ask = modeNormal, nil, nil, nil
	m.setStatus(why, true)
	return m
}

// shortWhere is where(fsys, dir) for a question: the side whole, the path
// cut from the left to a third of the screen, so its end shows and the
// question's choices still fit.
func (m Model) shortWhere(fsys FS, dir string) string {
	w := where(fsys, dir)
	side := strings.TrimSuffix(w, dir)
	return side + cellwidth.TruncLeft(dir, max(12, m.w/3))
}

// boxPrompt is a name box's prompt: what it does, and where, cut from the
// left to leave room to type.
func (m *Model) boxPrompt(what string, a *asked) {
	m.input.Prompt = what + " in " + termsafe.Label(m.shortWhere(a.fs, a.dir)) + ": "
	m.input.Width = max(1, m.w-cellwidth.Width(m.input.Prompt)-1)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		for _, p := range m.panes {
			p.clamp(m.rows())
		}
		return m, nil

	case dirLoadedMsg:
		p := m.panes[msg.pane]
		if msg.err != nil {
			p.err = msg.err.Error()
			m.setStatus(fmt.Sprintf("%s: %v", p.fs.Label(), msg.err), true)
			return m, nil
		}
		changed := p.dir != msg.dir
		if changed && m.ask != nil && m.asking() && m.ask.watch[msg.pane] && m.ask.dirs[msg.pane] != msg.dir {
			m = m.dropQuestion(dirChanged)
			m.mode = modeNotice
			p = m.panes[msg.pane]
		}
		p.dir, p.entries, p.err = msg.dir, msg.entries, ""
		if changed {
			p.sel = map[string]bool{}
			p.cursor, p.offset = 0, 0
		}
		// keep selections that still exist
		for name := range p.sel {
			found := false
			for _, e := range p.entries {
				if e.Name == name {
					found = true
				}
			}
			if !found {
				delete(p.sel, name)
			}
		}
		if p.focusOn != "" {
			for i, e := range p.visible() {
				if e.Name == p.focusOn {
					p.cursor = i
				}
			}
			p.focusOn = ""
		}
		p.clamp(m.rows())
		return m, nil

	case planMsg:
		m.endBusy()
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				m.setStatus("copy cancelled", false)
			} else {
				m.setStatus("copy: "+msg.err.Error(), true)
			}
			return m, nil
		}
		if len(msg.plan.Files) == 0 && len(msg.plan.Dirs) == 0 {
			m.setStatus("nothing to copy", false)
			return m, nil
		}
		if n := msg.plan.Conflicts(); n > 0 {
			if m.copyDirs != [2]string{m.panes[0].dir, m.panes[1].dir} {
				m.setStatus(dirChanged, true) // while it was being worked out
				return m, nil
			}
			plan := msg.plan
			m.pendingPlan = &plan
			m.ask = m.capture(m.active)
			m.ask.dirs, m.ask.watch = m.copyDirs, [2]bool{true, true}
			m.mode, m.confirmKind = modeConfirm, confirmOverwrite
			m.confirmAt = m.now()
			var names []string
			for _, f := range plan.Files {
				if f.Exists {
					name := relTo(plan.Dst, plan.DstDir, f.Dst)
					if f.LinkTo != "" {
						name += " (-> " + relTo(plan.Dst, plan.Dst.Dir(f.Dst), f.LinkTo) + ", the file it points to will be overwritten)"
					}
					names = append(names, name)
				}
			}
			m.confirmText = fmt.Sprintf("%s%d of %d file(s) already exist in %s: %s.  o = overwrite   s = skip those   esc = cancel",
				m.prodWarning(plan.Dst), n, len(plan.Files), m.shortWhere(plan.Dst, plan.DstDir), firstFew(names, 3))
			return m, nil
		}
		m.logStart(msg.plan, false)
		return m.startTransfer(msg.plan, RunOptions{}, Result{})

	case progressMsg:
		if m.busy != busyCopy {
			return m, nil
		}
		p := Progress(msg)
		m.progress = &p
		m.observe(p.At, p.Done)
		return m, waitFor(m.events)

	case tickMsg:
		if msg.id != m.tickID || m.mode != modeBusy {
			return m, nil // a ticker from an operation that has finished
		}
		if m.progress != nil {
			m.observe(msg.at, m.progress.Done)
		} else if msg.at.After(m.clock) {
			m.clock = msg.at
		}
		return m, tickCmd(m.tickID)

	case transferDoneMsg:
		opts := m.runOpts
		msg.res = m.carry.Add(msg.res)
		m.endBusy()
		for _, p := range m.panes {
			p.sel = map[string]bool{}
		}
		var need *InPlaceNeeded
		if errors.As(msg.err, &need) {
			plan := msg.plan
			m.pendingPlan, m.pendingInPlace, m.runOpts, m.carry = &plan, need, opts, msg.res
			m.ask = m.capture(m.active)
			m.ask.watch = [2]bool{true, true}
			m.mode, m.confirmKind = modeConfirm, confirmInPlace
			m.confirmAt = m.now()
			m.confirmText = fmt.Sprintf("%sReplacing %s: %v.  i = write in place (not atomic: a failure leaves a partial file)   s = skip this file   any other key = stop",
				m.prodWarning(plan.Dst), where(plan.Dst, need.Target), need)
			return m, tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))
		}
		var loses *ReplaceLoses
		if errors.As(msg.err, &loses) {
			plan := msg.plan
			m.pendingPlan, m.pendingLoss, m.runOpts, m.carry = &plan, loses, opts, msg.res
			m.ask = m.capture(m.active)
			m.ask.watch = [2]bool{true, true}
			m.mode, m.confirmKind = modeConfirm, confirmLoss
			m.confirmAt = m.now()
			m.confirmText = m.prodWarning(plan.Dst) + lossQuestion(loses)
			return m, tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))
		}
		var partial *PartialWrite
		if msg.err != nil {
			var s string
			switch {
			case errors.Is(msg.err, context.Canceled):
				s = fmt.Sprintf("%s; %d of %d file(s) copied before that", stopNote(msg.err), msg.res.Copied, len(msg.plan.Files))
			case errors.As(msg.err, &partial):
				s = fmt.Sprintf("copy stopped after %d file(s): %v", msg.res.Copied, partial)
			default:
				s = fmt.Sprintf("copy failed after %d of %d file(s): %v", msg.res.Copied, len(msg.plan.Files), msg.err)
			}
			if n := len(msg.res.Files); n > 0 {
				rel := make([]string, n)
				for k, f := range msg.res.Files {
					rel[k] = relTo(msg.plan.Dst, msg.plan.DstDir, f)
				}
				s += "; already copied: " + firstFew(rel, 3) + " (? for all)"
			}
			if n := len(msg.res.Leftovers); n > 0 {
				s += "; unfinished temporary file left: " + firstFew(msg.res.Leftovers, 2)
			}
			m.setStatus(s, true)
		} else {
			s := fmt.Sprintf("copied %d file(s), %s to %s:%s", msg.res.Copied, HumanBytes(msg.res.Bytes), msg.plan.Dst.Label(), msg.plan.DstDir)
			if n := len(msg.res.InPlace); n > 0 {
				s += fmt.Sprintf("; %d written in place, not atomically", n)
			}
			if n := len(msg.res.Followed); n > 0 {
				rel := make([]string, n)
				for k, f := range msg.res.Followed {
					rel[k] = relTo(msg.plan.Dst, msg.plan.DstDir, f.Link) + " -> " + relTo(msg.plan.Dst, msg.plan.Dst.Dir(f.Link), f.Target)
				}
				s += fmt.Sprintf("; overwrote the file %s points to: %s", plural(n, "a link", "links"), firstFew(rel, 3))
			}
			if n := len(msg.res.Lost); n > 0 {
				s += fmt.Sprintf("; %d replaced losing owner, group or hard links, as agreed", n)
			}
			if msg.res.Skipped > 0 {
				s += fmt.Sprintf("; kept %d existing", msg.res.Skipped)
			}
			if n := len(msg.res.Leftovers); n > 0 {
				s += "; unfinished temporary file left: " + firstFew(msg.res.Leftovers, 2)
			}
			if len(msg.plan.Skipped) > 0 {
				s += fmt.Sprintf("; skipped %d symlink(s)/special file(s)", len(msg.plan.Skipped))
			}
			// A warning is kept on screen like an error, until dismissed.
			if w := warningsText(msg.res.Warnings); w != "" {
				s += "; " + w
			}
			m.setStatus(s, len(msg.res.Warnings) > 0 || len(msg.res.Leftovers) > 0)
		}
		// Its end is always logged: the start was.
		d := m.report(msg.plan, msg.res, msg.err)
		if msg.res.Copied > 0 || msg.err != nil || len(msg.plan.Dirs) > 0 || len(msg.res.Warnings) > 0 {
			m.lastOp = detailLines(d)
		}
		return m, tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))

	case opDoneMsg:
		m.endBusy()
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.status, false)
		}
		if msg.detail != nil {
			m.lastOp = msg.detail
		}
		var cmds []tea.Cmd
		for _, i := range msg.panes {
			if msg.focus != "" { // set only by an operation that worked
				m.panes[i].focusOn = msg.focus
			}
			cmds = append(cmds, m.load(i, m.panes[i].dir))
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		// Keys typed faster than the terminal is read arrive as one message
		// ("Gc", or "yq" at a prompt): each letter is its own command,
		// except while typing a name.
		if m.mode != modeInput && msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			var cmds []tea.Cmd
			var model tea.Model = m
			for _, r := range msg.Runes {
				var cmd tea.Cmd
				model, cmd = model.(Model).update(singleKey(r))
				cmds = append(cmds, cmd)
			}
			return model, tea.Batch(cmds...)
		}
		if m.tooSmall() && m.mode != modeBusy {
			// Nothing on screen says what a key would act on: only the way
			// out works, and a question waiting for an answer is a no.
			switch {
			case msg.String() == "ctrl+c", msg.String() == "q" && m.mode != modeInput:
				return m, tea.Quit
			case m.mode == modeConfirm:
				if m.confirmKind == confirmInPlace || m.confirmKind == confirmLoss {
					return m.stopAtInPlace("the terminal is too small to show the question")
				}
				m = m.dropQuestion("cancelled: the terminal is too small to show the question")
			}
			return m, nil
		}
		// A key typed straight after Esc can reach us in the same read,
		// which bubbletea takes for alt+key. Nothing here binds alt, so
		// it's taken apart again: Esc, then the key.
		if m.mode == modeNormal && msg.Alt && msg.Type == tea.KeyRunes {
			first, c1 := m.update(tea.KeyMsg{Type: tea.KeyEsc})
			plain := msg
			plain.Alt = false
			second, c2 := first.(Model).update(plain)
			return second, tea.Batch(c1, c2)
		}
		switch m.mode {
		case modeNotice:
			switch msg.String() {
			case "enter", "esc":
				m.mode = modeNormal
				m.setStatus("", false)
			case "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		case modeInput:
			return m.updateInput(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeHelp:
			page := max(1, m.h-3)
			switch msg.String() {
			case "j", "down":
				m.helpTop++
			case "k", "up":
				m.helpTop--
			case "pgdown", "ctrl+d", " ":
				m.helpTop += page
			case "pgup", "ctrl+u":
				m.helpTop -= page
			case "g", "home":
				m.helpTop = 0
			case "G", "end":
				m.helpTop = len(m.helpLines())
			case "ctrl+c":
				return m, tea.Quit
			default:
				m.mode = modeNormal // any other key closes it
			}
			m.helpTop = max(0, min(m.helpTop, len(m.helpLines())-(m.h-2)))
			return m, nil
		case modeBusy:
			switch k := msg.String(); {
			case (k == "esc" || k == "ctrl+c") && !m.cancelling:
				// A copy that may write a file in place (the person agreed
				// to it) stops the gentle way first: a file going to a
				// temporary name stops at once, and one written in place is
				// let finish, as stopping it would leave it partly written.
				// The next esc stops at once.
				if m.stopAfter != nil && !m.stopping && m.mayWriteInPlace() {
					m.stopAfter()
					m.stopping = true
					return m, nil
				}
				if m.cancel != nil {
					m.cancel()
				}
				m.cancelling = true
			case k == "esc" || k == "ctrl+c":
				// The cancel hasn't come back: whatever it's waiting on isn't
				// answering. Drop the connection and leave.
				return m.abort()
			}
			return m, nil
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updateNormal(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.panes[m.active]
	rows := m.rows()
	if !m.statusErr {
		m.setStatus("", false) // an error stays until dismissed or replaced
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		// Esc clears one thing at a time: a message, then the selection.
		if m.status != "" {
			m.setStatus("", false)
		} else {
			p.sel = map[string]bool{}
		}
	case "?":
		m.mode, m.helpTop = modeHelp, 0
	case "tab", "ctrl+w":
		m.active = 1 - m.active
	case "j", "down":
		p.cursor++
	case "k", "up":
		p.cursor--
	case "pgdown":
		p.cursor += rows
	case "pgup":
		p.cursor -= rows
	case "ctrl+d":
		p.cursor += max(1, rows/2)
	case "ctrl+u":
		p.cursor -= max(1, rows/2)
	case "g", "home":
		p.cursor = 0
	case "G", "end":
		p.cursor = len(p.visible()) - 1
	case "l", "right", "enter":
		if e, ok := p.current(); ok && e.IsDir {
			return m, m.load(m.active, p.fs.Join(p.dir, e.Name))
		}
	case "h", "left", "backspace":
		parent := p.fs.Dir(p.dir)
		if parent != p.dir {
			p.focusOn = p.fs.Base(p.dir)
			return m, m.load(m.active, parent)
		}
	case "~":
		if home, err := p.fs.Home(); err == nil {
			return m, m.load(m.active, home)
		}
	case " ", "insert":
		if e, ok := p.current(); ok {
			if p.sel[e.Name] {
				delete(p.sel, e.Name)
			} else {
				p.sel[e.Name] = true
			}
			p.cursor++
		}
	case "*":
		if len(p.sel) > 0 {
			p.sel = map[string]bool{}
		} else {
			for _, e := range p.visible() {
				p.sel[e.Name] = true
			}
		}
	case ".":
		p.hidden = !p.hidden
		p.cursor = 0
		if !p.hidden {
			// What's selected is what's shown: a hidden file mustn't be
			// counted in the title, nor deleted or copied unseen.
			for name := range p.sel {
				if strings.HasPrefix(name, ".") {
					delete(p.sel, name)
				}
			}
		}
	case "r", "ctrl+r":
		return m, tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))
	case "c", "f5", "y":
		ts := p.targets()
		if len(ts) == 0 {
			return m, nil
		}
		other := m.panes[1-m.active]
		items := make([]string, len(ts))
		for i, e := range ts {
			items[i] = p.fs.Join(p.dir, e.Name)
		}
		m.copyDirs = [2]string{m.panes[0].dir, m.panes[1].dir}
		ctx, tick := m.startBusy(busyPlan, strings.Join(items, ", "))
		src, dst, dstDir := p.fs, other.fs, other.dir
		return m, tea.Batch(func() tea.Msg {
			plan, err := MakePlan(ctx, src, items, dst, dstDir)
			return planMsg{plan: plan, err: err}
		}, tick)
	case "d", "f8", "delete":
		ts := p.targets()
		if len(ts) == 0 {
			return m, nil
		}
		m.pendingDelete = ts
		m.ask = m.capture(m.active)
		m.ask.targets = ts
		for _, e := range ts {
			m.ask.paths = append(m.ask.paths, p.fs.Join(p.dir, e.Name))
		}
		m.mode, m.confirmKind = modeConfirm, confirmDelete
		m.confirmAt = m.now()
		m.confirmText = fmt.Sprintf("%sDelete %s in %s?  y = delete   any other key = cancel", m.prodWarning(p.fs), deleteWhat(ts), m.shortWhere(p.fs, p.dir))
	case "R", "f6":
		if e, ok := p.current(); ok && e.Name != ".." {
			m.ask = m.capture(m.active)
			m.ask.from, m.ask.name = p.fs.Join(p.dir, e.Name), e.Name
			m.mode, m.inputKind = modeInput, inputRename
			if termsafe.Label(e.Name) != e.Name || strings.TrimSpace(e.Name) != e.Name {
				// The box would show a cleaned name, which enter would
				// then rename it to: start empty instead.
				m.boxPrompt("Rename (this name has hidden characters; type the new name)", m.ask)
			} else {
				m.ask.initial = e.Name
				m.boxPrompt("Rename", m.ask)
			}
			m.input.SetValue(m.ask.initial)
			m.input.CursorEnd()
			m.input.Focus()
		}
	case "/":
		m.mode, m.inputKind = modeInput, inputFind
		m.input.Prompt, m.input.Width = "Find: ", 0
		m.input.SetValue("")
		m.input.Focus()
	case "n":
		m.findNext(m.lastFind, p.cursor+1)
	case "m", "f7":
		m.ask = m.capture(m.active)
		m.mode, m.inputKind = modeInput, inputMkdir
		m.boxPrompt("New directory", m.ask)
		m.input.SetValue("")
		m.input.Focus()
	}
	p.clamp(rows)
	return m, nil
}

func (m Model) updateInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inputKind == inputFind {
		switch k.String() {
		case "esc", "enter":
			m.mode = modeNormal
			m.input.Blur()
			m.lastFind = m.input.Value()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		m.findNext(m.input.Value(), 0)
		return m, cmd
	}
	switch k.String() {
	case "esc":
		m.mode, m.ask = modeNormal, nil
		m.input.Blur()
		return m, nil
	case "enter":
		a := m.ask
		m.mode, m.ask = modeNormal, nil
		m.input.Blur()
		if m.inputKind == inputRename && m.input.Value() == a.initial {
			m.setStatus("unchanged", false)
			return m, nil
		}
		name := strings.TrimSpace(m.input.Value())
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			m.setStatus("not a valid name", true)
			return m, nil
		}
		i := a.pane
		fsys, dir := a.fs, a.dir
		remote := i == 1
		on := m.opts.OnEvent
		switch m.inputKind {
		case inputMkdir:
			return m, func() tea.Msg {
				target := fsys.Join(dir, name)
				// A directory already there isn't made, nor logged: nothing
				// changes.
				if e, lerr := fsys.Lstat(target); lerr == nil && e.IsDir {
					return opDoneMsg{panes: []int{i}, status: name + " already exists; nothing was created", focus: name}
				}
				op := newOp()
				// Before it's asked for: a session that dies meanwhile
				// still shows it was.
				if on != nil {
					on(Event{Op: op, Kind: "mkdir", Outcome: "started", Detail: target, Remote: remote})
				}
				// Mkdir, not MkdirAll: only a directory this made is
				// logged as made.
				if err := fsys.Mkdir(target); err != nil {
					if e, lerr := fsys.Lstat(target); lerr == nil && e.IsDir {
						if on != nil {
							on(Event{Op: op, Kind: "mkdir", Outcome: "already there", Detail: target + ": already a directory; nothing was created", More: true, Remote: remote})
						}
						return opDoneMsg{panes: []int{i}, status: name + " already exists; nothing was created", focus: name}
					} else if lerr == nil {
						err = fmt.Errorf("%s already exists and isn't a directory", name)
					}
					if on != nil {
						on(Event{Op: op, Kind: "mkdir", Outcome: "failed", Detail: target + ": " + cause(err).Error(), More: true, Remote: remote})
					}
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("mkdir: %w", err)}
				}
				if on != nil {
					on(Event{Op: op, Kind: "mkdir", Outcome: "ok", Detail: target, Remote: remote})
				}
				return opDoneMsg{panes: []int{i}, status: "created " + target, focus: name}
			}
		case inputRename:
			if a.name == name {
				m.setStatus("unchanged", false)
				return m, nil
			}
			return m, func() tea.Msg {
				from, to := a.from, fsys.Join(dir, name)
				op := newOp()
				if on != nil {
					on(Event{Op: op, Kind: "rename", Outcome: "started", Detail: from + " -> " + to, Remote: remote})
				}
				// Rename itself refuses an existing target; a separate look
				// first would leave a gap for something to appear in.
				if err := fsys.Rename(from, to); err != nil {
					if _, lerr := fsys.Lstat(to); errors.Is(err, fs.ErrExist) || lerr == nil {
						err = fmt.Errorf("%s already exists", name)
					}
					if on != nil {
						on(Event{Op: op, Kind: "rename", Outcome: "failed", Detail: from + " -> " + to + ": " + cause(err).Error(), More: true, Remote: remote})
					}
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("rename: %w", err)}
				}
				if on != nil {
					on(Event{Op: op, Kind: "rename", Outcome: "ok", Detail: from + " -> " + to, Remote: remote})
				}
				return opDoneMsg{panes: []int{i}, status: "renamed to " + name, focus: name}
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// findNext moves the cursor to the first entry from index `from` whose name
// starts with q, or failing that contains it (case-insensitive), wrapping.
func (m *Model) findNext(q string, from int) {
	q = strings.ToLower(q)
	if q == "" {
		return
	}
	p := m.panes[m.active]
	v := p.visible()
	for _, match := range []func(string) bool{
		func(n string) bool { return strings.HasPrefix(n, q) },
		func(n string) bool { return strings.Contains(n, q) },
	} {
		for i := 0; i < len(v); i++ {
			idx := (from + i) % len(v)
			if match(strings.ToLower(v[idx].Name)) || match(strings.ToLower(termsafe.Label(v[idx].Name))) {
				p.cursor = idx
				p.clamp(m.rows())
				return
			}
		}
	}
}

// confirmSettle is how long a question must have been up before a
// destructive answer to it counts.
const confirmSettle = 150 * time.Millisecond

func (m Model) updateConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := strings.ToLower(k.String())
	destructive := (m.confirmKind == confirmDelete && key == "y") || (m.confirmKind == confirmOverwrite && key == "o") ||
		(m.confirmKind == confirmInPlace && key == "i") ||
		(m.confirmKind == confirmLoss && (key == "r" || key == "i" && m.pendingLoss.InPlaceOK))
	if destructive && m.now().Sub(m.confirmAt) < confirmSettle {
		return m, nil // typed before the question could have been read: keep asking
	}
	a := m.ask // what the question was asked about; the answer acts on that
	m.ask = nil
	switch m.confirmKind {
	case confirmLoss:
		loses, plan, opts := m.pendingLoss, m.pendingPlan, m.runOpts
		choice := map[string]LossChoice{"r": LossReplace, "s": LossSkip}[key]
		if key == "i" && loses.InPlaceOK {
			choice = LossInPlace
		}
		if choice == LossAsk {
			return m.stopAtInPlace("")
		}
		m.pendingPlan, m.pendingLoss = nil, nil
		// The person's decision goes in the log before a byte is written.
		kind, remote := m.transferKind(*plan)
		said := map[LossChoice]string{
			LossInPlace: "chose to write in place (keeps owner, group and hard links; not atomic)",
			LossReplace: "chose to replace",
			LossSkip:    "chose to skip",
		}[choice]
		m.emit(Event{Op: m.copyOp, Kind: kind, Outcome: "asked", Detail: fmt.Sprintf("%s: %s for %s (replacing it: %s); the same for any other such file in this copy", m.transferSummary(*plan), said, loses.Where, loses.what()), Remote: remote})
		opts.OnLoss, opts.From = choice, loses.Index
		return m.startTransfer(*plan, opts, m.carry)
	case confirmInPlace:
		need, plan, opts := m.pendingInPlace, m.pendingPlan, m.runOpts
		switch key {
		case "i":
			m.pendingPlan, m.pendingInPlace = nil, nil
			// The person's decision goes in the log before a byte is written.
			kind, remote := m.transferKind(*plan)
			m.emit(Event{Op: m.copyOp, Kind: kind, Outcome: "asked", Detail: fmt.Sprintf("%s: agreed to write in place (not atomic): %s; %s", m.transferSummary(*plan), where(plan.Dst, need.Target), need.Error()), Remote: remote})
			in := map[string]bool{}
			for k := range opts.InPlace {
				in[k] = true
			}
			in[need.Target] = true
			opts.InPlace, opts.From = in, need.Index
			return m.startTransfer(*plan, opts, m.carry)
		case "s":
			m.pendingPlan, m.pendingInPlace = nil, nil
			carry := m.carry
			carry.Skipped++
			opts.From = need.Index + 1
			return m.startTransfer(*plan, opts, carry)
		}
		return m.stopAtInPlace("")
	case confirmOverwrite:
		plan := m.pendingPlan
		m.pendingPlan = nil
		switch key {
		case "o":
			m.logStart(*plan, true)
			return m.startTransfer(*plan, RunOptions{Overwrite: true}, Result{})
		case "s":
			m.logStart(*plan, false)
			return m.startTransfer(*plan, RunOptions{}, Result{})
		}
		m.mode = modeNormal
		m.setStatus("copy cancelled", false)
		return m, nil
	case confirmDelete:
		ts := a.targets
		m.pendingDelete, m.ask = nil, nil
		if key != "y" {
			m.mode = modeNormal
			m.setStatus("delete cancelled", false)
			return m, nil
		}
		i := a.pane
		fsys, dir := a.fs, a.dir
		remote := i == 1
		on := m.opts.OnEvent
		m.panes[i].sel = map[string]bool{}
		all := a.paths
		ctx, tick := m.startBusy(busyDelete, strings.Join(all, ", "))
		op := newOp()
		m.busyRemote, m.busyOp = remote, op
		// Say what's about to go before any of it does: if this session
		// dies part way, the log still shows what was asked for.
		if on != nil {
			on(Event{Op: op, Kind: "delete", Outcome: "started", Detail: strings.Join(all, ", "), Remote: remote})
		}
		return m, tea.Batch(func() tea.Msg {
			var names, gone, removed []string // the items finished, those already gone, and every path removed
			for n, target := range all {
				inside := 0
				wasGone, err := removeTree(ctx, fsys, target, func(p string) {
					removed = append(removed, p)
					if p != target {
						inside++
					}
				})
				if err == nil {
					if wasGone {
						gone = append(gone, target)
					} else {
						names = append(names, target)
					}
					continue
				}
				outcome := "failed"
				d := "failed on " + target + ": " + err.Error()
				status := fmt.Sprintf("delete %s stopped: %s", fsys.Base(target), err)
				if errors.Is(err, context.Canceled) {
					outcome = "cancelled"
					d = "cancelled at " + target
					status = fmt.Sprintf("delete cancelled after %d of %d item(s)", len(names)+len(gone), len(ts))
				}
				if len(removed) > 0 {
					outcome = "partial" // something went before it stopped
				}
				d = strings.Join(all, ", ") + ": " + d
				if len(names) > 0 {
					d += "; deleted: " + strings.Join(names, ", ")
				}
				if len(gone) > 0 {
					d += "; already gone: " + strings.Join(gone, ", ")
				}
				if inside > 0 {
					d += fmt.Sprintf("; %d %s inside %s removed before it stopped", inside, plural(inside, "entry", "entries"), target)
				}
				if len(removed) > 0 {
					d += "; removed: " + capList(removed, logCap)
					rel := make([]string, len(removed))
					for k, p := range removed {
						rel[k] = relTo(fsys, dir, p)
					}
					status += fmt.Sprintf("; removed %d before that: %s (see ? or the log for all)", len(removed), firstFew(rel, 3))
				} else {
					status += "; nothing was removed"
				}
				if rest := all[n+1:]; len(rest) > 0 {
					d += "; not reached: " + strings.Join(rest, ", ")
				}
				if on != nil {
					on(Event{Op: op, Kind: "delete", Outcome: outcome, Detail: d, More: true, Remote: remote})
				}
				detail := []string{"Delete on " + fsys.Label() + ": " + status, "", "Failed on: " + target, "  " + err.Error(), "",
					fmt.Sprintf("Removed before it stopped (%d):", len(removed))}
				for k, p := range removed {
					if k == logCap {
						detail = append(detail, fmt.Sprintf("  +%d more", len(removed)-logCap))
						break
					}
					detail = append(detail, "  "+p)
				}
				if rest := all[n+1:]; len(rest) > 0 {
					detail = append(detail, "", "Not reached:")
					for _, p := range rest {
						detail = append(detail, "  "+p)
					}
				}
				return opDoneMsg{panes: []int{i}, err: errors.New(status), detail: detail}
			}
			end := Event{Op: op, Kind: "delete", Outcome: "ok", Detail: strings.Join(names, ", "), Remote: remote}
			status := fmt.Sprintf("deleted %d item(s)", len(names))
			if len(gone) > 0 {
				end.Detail += "; already gone: " + strings.Join(gone, ", ")
				end.More = true
				status += fmt.Sprintf("; %d already gone", len(gone))
			}
			if len(names) == 0 {
				end.Outcome, end.Detail = "already gone", strings.Join(gone, ", ")
				status = fmt.Sprintf("nothing deleted: %s already gone", plural(len(gone), "it was", fmt.Sprintf("all %d were", len(gone))))
			}
			if on != nil {
				on(end)
			}
			detail := []string{fmt.Sprintf("Deleted on %s (%d item(s)):", fsys.Label(), len(names))}
			for _, p := range names {
				detail = append(detail, "  "+p)
			}
			if len(gone) > 0 {
				detail = append(detail, "", "Already gone:")
				for _, p := range gone {
					detail = append(detail, "  "+p)
				}
			}
			return opDoneMsg{panes: []int{i}, status: status, detail: detail}
		}, tick)
	}
	return m, nil
}

// deleteWhat names what a delete would remove, by kind: "the file a.txt",
// "the directory d and everything inside it", "2 files and 1 directory
// (with everything inside it)".
func deleteWhat(ts []Entry) string {
	if len(ts) == 1 {
		e := ts[0]
		switch {
		case e.Link:
			return "the symlink " + e.Name + " (not what it leads to)"
		case e.IsDir:
			return "the directory " + e.Name + " and everything inside it"
		}
		return "the file " + e.Name
	}
	var files, dirs, links int
	for _, e := range ts {
		switch {
		case e.Link:
			links++
		case e.IsDir:
			dirs++
		default:
			files++
		}
	}
	var parts []string
	for _, c := range []struct {
		n         int
		one, many string
	}{{files, "file", "files"}, {dirs, "directory", "directories"}, {links, "symlink", "symlinks"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, plural(c.n, c.one, c.many)))
		}
	}
	s := parts[0]
	if len(parts) > 1 {
		s = strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	if dirs > 0 {
		s += " (with everything inside " + plural(dirs, "it", "them") + ")"
	}
	return s
}

// startTransfer runs plan with opts; carry is what earlier runs of the same
// plan did (one that stopped to ask, and goes on).
func (m Model) startTransfer(plan Plan, opts RunOptions, carry Result) (tea.Model, tea.Cmd) {
	ctx, tick := m.startBusy(busyCopy, "")
	after, stopAfter := context.WithCancel(ctx)
	m.stopAfter, m.stopping = stopAfter, false
	m.running = &plan
	m.runOpts, m.carry = opts, carry
	run := opts // After is this run's alone: runOpts goes on to a resumed run
	run.After = after
	m.progress = &Progress{Total: plan.TotalBytes, FilesTotal: len(plan.Files), Started: m.busySince, At: m.busySince}
	m.observe(m.busySince, 0)
	ch := make(chan tea.Msg, 16)
	m.events = ch
	go func() {
		res, err := RunWith(ctx, plan, run, func(p Progress) {
			select {
			case ch <- progressMsg(p):
			default: // the UI is behind; drop this update rather than slow the copy
			}
		})
		ch <- transferDoneMsg{res: res, err: err, plan: plan}
	}()
	return m, tea.Batch(waitFor(ch), tick)
}

// stopAtInPlace ends a copy that stopped to ask about writing in place,
// with no: the file is left as it was, and the copy goes no further.
func (m Model) stopAtInPlace(why string) (tea.Model, tea.Cmd) {
	plan, res := *m.pendingPlan, m.carry
	var err error
	if need := m.pendingInPlace; need != nil {
		err = fmt.Errorf("%s not copied, left as it was: %w", where(plan.Dst, need.Target), need)
	} else {
		loses := m.pendingLoss
		err = fmt.Errorf("%s not copied, left as it was: %w", loses.Where, loses)
	}
	m.mode, m.pendingPlan, m.pendingInPlace, m.pendingLoss, m.carry, m.runOpts, m.ask = modeNormal, nil, nil, nil, Result{}, RunOptions{}, nil
	s := fmt.Sprintf("stopped: %v; %d of %d file(s) copied before that", err, res.Copied, len(plan.Files))
	if why != "" {
		s = why + "; " + s
	}
	m.setStatus(s, true)
	m.lastOp = detailLines(m.reportAs("stopped", plan, res, err))
	return m, nil
}

// lossQuestion asks what to do with a file that replacing would change for
// someone else.
func lossQuestion(l *ReplaceLoses) string {
	q := fmt.Sprintf("Replacing %s: %s.  ", l.Where, l.what())
	if l.InPlaceOK {
		q += "i = write in place (keeps owner, group and hard links; not atomic)   "
	} else {
		q += "(it can't be written in place: not writable)   "
	}
	becomes := "owner/group stay"
	if l.NewOwner != "" {
		becomes = "owner/group become " + l.NewOwner
	}
	links := ""
	if l.Links > 1 {
		links = ", hard links split"
	}
	return q + fmt.Sprintf("r = replace (%s%s)   s = skip   any other key = stop  (the answer holds for the rest of this copy)", becomes, links)
}

// prodWarning starts a destructive question about fsys when it's a
// production host.
func (m Model) prodWarning(fsys FS) string {
	if fsys == m.opts.Right && m.opts.Env == "production" {
		return "⚠ PRODUCTION " + fsys.Label() + ": "
	}
	return ""
}

// relTo is p as a path under base, or p itself when it isn't under base.
func relTo(fsys FS, base, p string) string {
	if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(fsys.Join(base, "x"), "x")); ok && rest != "" {
		return rest
	}
	return p
}

// firstFew is the first n items, and how many more there are.
func firstFew(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(", +%d more", len(items)-n)
}

// detailLines splits an activity-log line into lines for the ? screen: a
// clause a line, and a long list an item a line. The first clause, how it
// went and what it was, stays whole.
func detailLines(d string) []string {
	out := []string{"Copy:"}
	for k, clause := range strings.Split(d, "; ") {
		if k == 0 {
			out = append(out, "  "+clause)
			continue
		}
		head, list, ok := strings.Cut(clause, ": ")
		if ok && strings.Contains(list, ", ") {
			out = append(out, "  "+head+":")
			for _, it := range strings.Split(list, ", ") {
				out = append(out, "    "+it)
			}
			continue
		}
		out = append(out, "  "+clause)
	}
	return out
}

// singleKey turns one rune of a burst back into the key it was, including
// the control keys a burst can carry (tab, enter, esc, backspace).
func singleKey(r rune) tea.KeyMsg {
	switch r {
	case '\t':
		return tea.KeyMsg{Type: tea.KeyTab}
	case '\r', '\n':
		return tea.KeyMsg{Type: tea.KeyEnter}
	case 0x1b:
		return tea.KeyMsg{Type: tea.KeyEsc}
	case 0x7f, 0x08:
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case ' ':
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func waitFor(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// transferKind is how the activity log names a copy in this direction.
func (m Model) transferKind(plan Plan) (kind string, remote bool) {
	kind, remote = "download", true
	switch {
	case plan.Dst == m.opts.Right:
		kind = "upload"
	case plan.Src != m.opts.Right:
		kind, remote = "copy", false
	}
	return kind, remote
}

// transferSummary is "a.txt, b -> /dir" for the activity log.
func (m Model) transferSummary(plan Plan) string {
	var tops []string
	for _, it := range plan.Items {
		if len(tops) == 5 {
			tops = append(tops, fmt.Sprintf("+%d more", len(plan.Items)-5))
			break
		}
		tops = append(tops, plan.Src.Base(it))
	}
	return strings.Join(tops, ", ") + " -> " + plan.DstDir
}

// logStart tells OnEvent a copy is starting, before a byte is written: if
// the session dies part way, the log still shows what was under way. It
// opens the copy's operation, which its end closes.
func (m *Model) logStart(plan Plan, replace bool) {
	m.copyOp = newOp()
	kind, remote := m.transferKind(plan)
	d := fmt.Sprintf("%s (%d file(s), %s", m.transferSummary(plan), len(plan.Files), HumanBytes(plan.TotalBytes))
	if n := plan.Conflicts(); n > 0 {
		var names []string
		for _, f := range plan.Files {
			if f.Exists {
				name := f.Dst
				if f.LinkTo != "" {
					name += " (-> " + f.LinkTo + ")"
				}
				names = append(names, name)
			}
		}
		if replace {
			d += fmt.Sprintf(", replacing %d: %s", n, capList(names, logCap))
		} else {
			d += fmt.Sprintf(", keeping %d existing", n)
		}
	}
	m.emit(Event{Op: m.copyOp, Kind: kind, Outcome: "started", Detail: d + ")", Remote: remote})
}

// report tells OnEvent what a copy did, finished or not, and returns what
// it said.
func (m Model) report(plan Plan, res Result, err error) string {
	return m.reportAs("", plan, res, err)
}

// reportAs is report, saying outcome when it isn't "" (a copy stopped by
// the person's no).
func (m Model) reportAs(outcome string, plan Plan, res Result, err error) string {
	kind, remote := m.transferKind(plan)
	var partial *PartialWrite
	if outcome == "" {
		switch {
		case err == nil:
			outcome = "ok"
		case errors.As(err, &partial):
			outcome = "partial"
		case errors.Is(err, context.Canceled):
			outcome = "cancelled"
		default:
			outcome = "failed"
		}
	}
	d := fmt.Sprintf("%s (%d file(s), %s)", m.transferSummary(plan), res.Copied, HumanBytes(res.Bytes))
	if err != nil {
		d = fmt.Sprintf("%s (%d of %d file(s), %s)", m.transferSummary(plan), res.Copied, len(plan.Files), HumanBytes(res.Bytes))
		if outcome == "failed" || outcome == "stopped" {
			d += ": " + err.Error()
		}
	}
	if err != nil && len(res.Files) > 0 {
		d += "; copied before it stopped: " + capList(res.Files, logCap)
	}
	if res.Replaced > 0 {
		d += fmt.Sprintf("; replaced %d existing: %s", res.Replaced, capList(res.ReplacedPaths, logCap))
	}
	if len(res.InPlace) > 0 {
		d += "; written in place, not atomically: " + capList(res.InPlace, logCap)
	}
	if len(res.Lost) > 0 {
		d += "; replaced as agreed, losing: " + capList(res.Lost, logCap)
	}
	if res.Skipped > 0 {
		d += fmt.Sprintf("; kept %d existing", res.Skipped)
	}
	if w := warningsText(res.Warnings); w != "" {
		d += "; " + w
	}
	if len(res.Leftovers) > 0 {
		d += "; unfinished temporary files left: " + capList(res.Leftovers, logCap)
	}
	switch {
	case errors.As(err, &partial):
		d += "; stopped: " + partial.Error()
	case errors.Is(err, context.Canceled):
		d += "; " + stopNote(err)
	}
	more := err != nil || res.Skipped > 0 || len(res.InPlace) > 0 || len(res.Lost) > 0 || len(res.Warnings) > 0 || len(res.Leftovers) > 0
	m.emit(Event{Op: m.copyOp, Kind: kind, Outcome: outcome, Detail: d, More: more, Remote: remote})
	return outcome + ": " + kind + " " + d
}

// stopNote says what a cancel left of the file it stopped in: unchanged,
// not created, or partly written, and any temporary file left behind.
func stopNote(err error) string {
	var u *Unchanged
	var partial *PartialWrite
	switch {
	case errors.As(err, &partial):
		return "cancelled: " + partial.Where + " may be partly written"
	case errors.As(err, &u):
		s := "cancelled: " + u.Where + " unchanged, the unfinished copy discarded"
		if u.New {
			s = "cancelled: " + u.Where + " not created, the unfinished copy discarded"
		}
		if u.Leftover != "" {
			s = "cancelled: " + u.Where + " unchanged; the unfinished copy is still at " + u.Leftover + " (" + u.LeftoverErr.Error() + ")"
			if u.New {
				s = "cancelled: " + u.Where + " not created; couldn't remove the unfinished new file " + u.Leftover + " (" + u.LeftoverErr.Error() + ")"
			}
		}
		return s
	}
	return "cancelled between files: none was being written"
}

// warningsText sums up what went wrong without stopping a copy: the first
// warning in full, and how many more.
func warningsText(ws []string) string {
	switch len(ws) {
	case 0:
		return ""
	case 1:
		return ws[0]
	}
	return fmt.Sprintf("%s (and %d more file(s) the same way)", ws[0], len(ws)-1)
}

// logCap is how many paths the activity log lists for one operation.
const logCap = 200

// capList joins up to max items, then says how many more there were.
func capList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", +%d more", len(items)-max)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- view ------------------------------------------------------------------

// Colours readable on dark and light backgrounds alike, deepened when the
// terminal is known to be light (see darkBackground). The cursor is drawn
// in reverse video, which works on any theme.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "25", Dark: "33"}   // blue
	colorLink   = lipgloss.AdaptiveColor{Light: "30", Dark: "37"}   // teal
	colorSel    = lipgloss.AdaptiveColor{Light: "130", Dark: "172"} // amber
	colorMuted  = lipgloss.AdaptiveColor{Light: "242", Dark: "245"} // grey
	colorDanger = lipgloss.AdaptiveColor{Light: "160", Dark: "203"} // red
	colorOK     = lipgloss.AdaptiveColor{Light: "28", Dark: "71"}   // green

	borderActive   = lipgloss.NewStyle().Foreground(colorAccent)
	borderInactive = lipgloss.NewStyle().Foreground(colorMuted)
	pathStyle      = lipgloss.NewStyle().Bold(true)
	dirStyle       = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	linkStyle      = lipgloss.NewStyle().Foreground(colorLink)
	selStyle       = lipgloss.NewStyle().Foreground(colorSel).Bold(true)
	cursorStyle    = lipgloss.NewStyle().Reverse(true)
	faint          = lipgloss.NewStyle().Foreground(colorMuted)
	errStyle       = lipgloss.NewStyle().Foreground(colorDanger)
	okStyle        = lipgloss.NewStyle().Foreground(colorOK)
)

// darkBackground reads $COLORFGBG ("fg;bg", set by rxvt, Konsole and
// others) without asking the terminal anything. Background colours 7 and
// 9-15 are light; anything else, or nothing, is taken as dark.
func darkBackground(colorfgbg string) bool {
	parts := strings.Split(colorfgbg, ";")
	bg := parts[len(parts)-1]
	switch bg {
	case "7", "9", "10", "11", "12", "13", "14", "15":
		return false
	}
	return true
}

func (m Model) View() string {
	if m.tooSmall() {
		return cellwidth.Fit(fmt.Sprintf("too small: %d×%d, need %d×%d", m.w, m.h, minWidth, minHeight), m.w) +
			"\n" + faint.Render(cellwidth.Fit("q quits", m.w))
	}
	if m.mode == modeHelp {
		return m.viewFullHelp()
	}
	paneW := m.w / 2
	left := m.viewPane(0, paneW)
	right := m.viewPane(1, m.w-paneW)
	// Joined line by line: lipgloss would measure (and pad) each line by
	// its own idea of width, which is wrong for Thai.
	lines := make([]string, len(left))
	for i := range left {
		lines[i] = left[i] + right[i]
	}
	return strings.Join(lines, "\n") + "\n" + strings.Join(m.statusLines(), "\n") + "\n" + m.viewHelp()
}

// viewPane draws one pane, border included, as lines exactly width columns
// wide.
func (m Model) viewPane(i, width int) []string {
	p := m.panes[i]
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	rows := m.rows()
	// One header line, then exactly rows() entries: clamp() scrolls by the
	// same count, so the cursor row is always one that gets drawn. An error
	// goes in the header rather than taking an entry's row.
	header := m.viewHeader(i, inner)
	if p.err != "" {
		header = errStyle.Render(cellwidth.FitPad("⚠ "+termsafe.Label(p.err), inner))
	}
	body := []string{header}
	v := p.visible()
	for r := 0; r < rows && p.offset+r < len(v); r++ {
		idx := p.offset + r
		body = append(body, m.viewRow(p, v[idx], inner, idx == p.cursor && i == m.active, idx == p.cursor))
	}
	for len(body) < rows+1 {
		body = append(body, strings.Repeat(" ", inner))
	}
	// The active pane's border is heavy: which side keys act on shows in
	// any terminal, colour or not.
	style, b := borderInactive, [6]string{"╭", "─", "╮", "│", "╰", "╯"}
	if i == m.active {
		style, b = borderActive, [6]string{"┏", "━", "┓", "┃", "┗", "┛"}
	}
	side := style.Render(b[3])
	out := []string{style.Render(b[0] + strings.Repeat(b[1], inner) + b[2])}
	for _, l := range body {
		out = append(out, side+l+side)
	}
	return append(out, style.Render(b[4]+strings.Repeat(b[1], inner)+b[5]))
}

// viewHeader is a pane's first line, width columns wide: the host's
// environment badge, the host, the path and the selection count. The path
// is what gives way when it doesn't fit (from the left, so its end shows);
// the host never does, short of not fitting on its own.
func (m Model) viewHeader(i, width int) string {
	p := m.panes[i]
	badge, badgeStyle := "", pathStyle
	if p.fs == m.opts.Right {
		switch m.opts.Env {
		case "production":
			badge, badgeStyle = "⚑PROD ", errStyle.Bold(true)
		case "staging":
			badge, badgeStyle = "⚑staging ", selStyle
		}
	}
	host := termsafe.Label(p.fs.Label()) + ":"
	path := termsafe.Label(p.dir)
	count := ""
	if n := p.selected(); n > 0 {
		count = fmt.Sprintf("  [%d selected]", n)
	}
	fixed := cellwidth.Width(badge) + cellwidth.Width(host)
	if fixed+cellwidth.Width(count)+4 > width {
		count = "" // the count gives way before the path is squeezed to nothing
	}
	room := width - fixed - cellwidth.Width(count)
	if room < 1 {
		// Even the host doesn't fit with its badge: keep the badge whole.
		host = cellwidth.Fit(host, max(1, width-cellwidth.Width(badge)))
		path, room = "", 0
	}
	path = cellwidth.TruncLeft(path, max(0, room))
	title := faint
	if i == m.active {
		title = pathStyle.Foreground(colorAccent)
	}
	out := badgeStyle.Render(badge) + title.Render(host+path) + count
	used := cellwidth.Width(badge) + cellwidth.Width(host) + cellwidth.Width(path) + cellwidth.Width(count)
	if used > width { // a badge wider than the pane
		return pathStyle.Render(cellwidth.FitPad(badge+host+path, width))
	}
	return out + strings.Repeat(" ", width-used)
}

// selected counts the selected entries that are shown.
func (p *pane) selected() int {
	n := 0
	for _, e := range p.visible() {
		if p.sel[e.Name] {
			n++
		}
	}
	return n
}

// viewRow draws one entry exactly width columns wide.
func (m Model) viewRow(p *pane, e Entry, width int, cursor, cursorInactive bool) string {
	mark := " "
	if p.sel[e.Name] {
		mark = "*"
	}
	name := termsafe.Label(e.Name)
	if e.IsDir {
		name += "/"
	}
	if e.Link {
		name += "@"
	}
	size := ""
	if !e.IsDir {
		size = HumanBytes(e.Size)
	}
	date := ""
	if width >= 50 {
		date = e.ModTime.Format("2006-01-02 15:04")
	}
	right := size
	if date != "" {
		right = fmt.Sprintf("%9s  %s", size, date)
	}
	nameW := width - 2 - len(right) - 1 // right is ASCII
	if nameW < 4 {
		nameW, right = width-3, ""
	}
	// The active pane's cursor has the marker (and reverse video); the
	// other pane's is only underlined and dim, so the two can't be taken
	// for each other.
	pointer := " "
	if cursor {
		pointer = "›"
	}
	line := mark + pointer + cellwidth.FitPad(name, nameW) + " " + right
	var st lipgloss.Style
	switch {
	case cursor:
		return cursorStyle.Render(line)
	case p.sel[e.Name]:
		st = selStyle
	case e.IsDir:
		st = dirStyle
	case e.Link:
		st = linkStyle
	default:
		st = lipgloss.NewStyle()
	}
	if cursorInactive {
		st = st.Underline(true).Faint(true)
	}
	return st.Render(line)
}

// statusLines is the status area: one line, or two when a message or a
// question needs them (cut with "…" past that; the log has the whole of
// what was done).
func (m Model) statusLines() []string {
	wrap := func(s string, st lipgloss.Style) []string {
		lines := cellwidth.Wrap(s, max(1, m.w), 2)
		for k, l := range lines {
			lines[k] = st.Render(l)
		}
		return lines
	}
	switch {
	case m.mode == modeConfirm:
		// A question may take a third line: it names what it would replace.
		lines := cellwidth.Wrap(termsafe.Label(m.confirmText), max(1, m.w), 3)
		for k, l := range lines {
			lines[k] = selStyle.Render(l)
		}
		return lines
	case m.mode == modeNotice:
		return wrap(termsafe.Label(m.status)+"  (enter or esc: go on)", errStyle)
	case m.mode == modeInput || m.mode == modeBusy || m.progress != nil:
		return []string{m.viewStatus()}
	case m.status != "":
		st := okStyle
		s := termsafe.Label(m.status)
		if m.statusErr {
			st = errStyle
			s += "  (esc dismisses)"
		}
		return wrap(s, st)
	}
	return []string{""}
}

func (m Model) viewStatus() string {
	switch {
	case m.mode == modeInput:
		return m.input.View()
	case m.mode == modeConfirm:
		return selStyle.Render(cellwidth.Fit(termsafe.Label(m.confirmText), m.w))
	case m.mode == modeBusy && m.progress == nil:
		s := map[busyKind]string{busyPlan: "working out what to copy…", busyDelete: "deleting…"}[m.busy]
		if secs := int(m.clock.Sub(m.busySince).Seconds()); secs >= 2 {
			s += fmt.Sprintf(" %ds", secs)
		}
		if m.cancelling {
			return errStyle.Render(cellwidth.Fit("cancelling… esc or ctrl+c again drops the connection and quits   "+s, m.w))
		}
		return cellwidth.Fit(s+"  (esc cancels)", m.w)
	case m.progress != nil:
		p := m.progress
		pct := 0.0
		if p.Total > 0 {
			pct = float64(p.Done) / float64(p.Total)
		}
		barW := 20
		filled := min(barW, max(0, int(pct*float64(barW))))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
		name := ""
		if p.File != "" {
			name = termsafe.Label(m.panes[m.active].fs.Base(p.File)) + "  "
		}
		rate := ""
		if r, ok := m.rate(); ok {
			rate = " " + HumanBytes(int64(r)) + "/s"
		}
		s := fmt.Sprintf("%s%s %3.0f%%  %s / %s %s  file %d/%d",
			name, bar, pct*100, HumanBytes(p.Done), HumanBytes(p.Total), rate, min(p.Files+1, p.FilesTotal), p.FilesTotal)
		// What needs attention goes first, so a narrow terminal still shows it.
		var warn []string
		switch {
		case m.cancelling:
			warn = append(warn, "cancelling… esc or ctrl+c again drops the connection and quits")
		case m.stopping:
			warn = append(warn, stopNowHint)
		}
		if idle := m.clock.Sub(m.lastChange); idle >= stallAfter {
			warn = append(warn, fmt.Sprintf("no data for %ds", int(idle.Seconds())))
		}
		if len(warn) > 0 {
			w := cellwidth.Fit(strings.Join(warn, " · ")+"   ", m.w)
			return errStyle.Render(w) + cellwidth.Fit(s, max(0, m.w-cellwidth.Width(w)))
		}
		return cellwidth.Fit(s+"  (esc cancels)", m.w)
	case m.status != "":
		st := okStyle
		if m.statusErr {
			st = errStyle
		}
		s := termsafe.Label(m.status)
		if m.statusErr {
			s += "  (esc dismisses)"
		}
		return st.Render(cellwidth.Fit(s, m.w))
	}
	return ""
}

// viewHelp is the one-line key reminder: as many keys as fit, most used
// first, and always where to find the rest.
func (m Model) viewHelp() string {
	// Only the keys that do something now: while a copy runs, q waits.
	switch {
	case m.mode == modeBusy && m.cancelling:
		return faint.Render(cellwidth.Fit("esc or ctrl+c again: drop the connection and quit", m.w))
	case m.mode == modeBusy && m.stopping:
		return faint.Render(cellwidth.Fit("esc or ctrl+c again: stop now, leaving the file written in place partly written", m.w))
	case m.mode == modeBusy:
		return faint.Render(cellwidth.Fit("esc or ctrl+c cancel   (other keys wait until this is done)", m.w))
	case m.mode == modeConfirm:
		// The question names its keys too, but a long one is cut: these
		// always show.
		keys := map[confirmKind]string{
			confirmDelete:    "y delete   any other key cancels",
			confirmOverwrite: "o overwrite   s skip those   esc cancel",
			confirmInPlace:   "i write in place   s skip this file   any other key stops",
			confirmLoss:      "r replace   s skip   any other key stops",
		}[m.confirmKind]
		if m.confirmKind == confirmLoss && m.pendingLoss != nil && m.pendingLoss.InPlaceOK {
			keys = "i write in place   " + keys
		}
		return faint.Render(cellwidth.Fit(keys, m.w))
	case m.mode == modeNotice:
		return faint.Render(cellwidth.Fit("enter or esc: go on   (other keys wait, so an answer typed already does nothing)", m.w))
	case m.mode == modeInput && m.inputKind == inputFind:
		return faint.Render(cellwidth.Fit("enter or esc: done finding", m.w))
	case m.mode == modeInput:
		return faint.Render(cellwidth.Fit("enter ok   esc cancel", m.w))
	}
	keys := []string{"tab side", "space select", "c copy", "d delete", "R rename", "m mkdir", "/ find", "hjkl move", "* all", ". hidden", "~ home", "r reload"}
	tail := "? help  q quit"
	line := ""
	for _, k := range keys {
		if cellwidth.Width(line+k+"  "+tail) > m.w {
			break
		}
		line += k + "  "
	}
	return faint.Render(cellwidth.Fit(line+tail, m.w))
}

// fullHelp is the ? screen. The keys mean what they mean in the host
// picker wherever both have the idea (r reloads, R renames, d deletes,
// space marks, / finds, esc clears, ? helps, q quits).
var fullHelp = []string{
	"Keys",
	"  tab, ctrl+w        switch side",
	"  j k, up down       move",
	"  g G, home end      top, bottom",
	"  pgup pgdn          a page up, down",
	"  ctrl+u ctrl+d      half a page up, down",
	"  l, enter, right    open a directory",
	"  h, backspace, left go up a directory",
	"  space, insert      select or unselect, and move down",
	"  *                  select all / none",
	"  c, y, F5           copy to the other side (asks before replacing files)",
	"  d, F8, delete      delete (asks first; directories with everything inside)",
	"  R, F6              rename (refuses a name that's taken)",
	"  m, F7              new directory",
	"  /, n               find by name, next match",
	"  .                  show hidden files",
	"  ~                  home directory",
	"  r, ctrl+r          reload both sides",
	"  esc                dismiss a message, then clear the selection",
	"  ?                  this help",
	"  q, ctrl+c          quit",
	"",
	"While a copy or delete runs: esc or ctrl+c cancels; if the host has",
	"stopped answering, press it again to drop the connection and quit.",
	"A file you agreed to have written in place is let finish first (esc",
	"again stops it at once, leaving it partly written). meterm never",
	"removes an existing file it is overwriting.",
	"",
	"A host marked ⚑PROD is production: its questions before a delete or an",
	"overwrite say PRODUCTION. ⚑staging marks a staging host.",
	"",
	"Where no temporary file can be written next to a file being replaced",
	"(its directory is read-only), you're asked before it's written in",
	"place, which isn't atomic: a failure part way leaves a partial file.",
	"",
	"A replacement keeps the old file's permissions, and its owner and group",
	"where they can be given (as root, or a group you're in). If they can't,",
	"or the file has other hard links, you're asked once per copy: write in",
	"place (keeps them; not atomic), replace (they change, links split), or",
	"skip. A host doesn't say how many hard links a file has, so there a",
	"replacement splits them unasked. ACLs and extended attributes are never",
	"carried over by a replacement. A file whose permissions can't be set on",
	"its replacement (the server refuses) is left as it was, rather than",
	"replaced by one more open.",
	"",
	"Uploads, downloads, deletes and renames on the host are in the",
	"activity log (meterm activity), in full.",
}

// helpLines is the ? screen: the keys, then what the last operation did.
func (m Model) helpLines() []string {
	if len(m.lastOp) == 0 {
		return fullHelp
	}
	out := append(append([]string(nil), fullHelp...), "", "Last operation (also in the activity log):")
	for _, l := range m.lastOp {
		out = append(out, "  "+termsafe.Label(l))
	}
	return out
}

func (m Model) viewFullHelp() string {
	inner := m.w - 4
	rows := m.h - 2
	help := m.helpLines()
	top := min(m.helpTop, max(0, len(help)-rows))
	title := " meterm files: keys "
	right := ""
	if len(help) > rows {
		right = fmt.Sprintf(" %d-%d of %d · j/k scroll ", top+1, min(top+rows, len(help)), len(help))
	}
	border := func(open, close, left, right string) string {
		fill := m.w - 2 - 1 - cellwidth.Width(left) - cellwidth.Width(right)
		if fill < 1 {
			right, fill = "", m.w-2-1-cellwidth.Width(left)
		}
		if fill < 0 {
			left, fill = cellwidth.Fit(left, m.w-3), 0
			fill = m.w - 3 - cellwidth.Width(left)
		}
		if right != "" {
			return faint.Render(open+"─") + pathStyle.Render(left) + faint.Render(strings.Repeat("─", fill-1)) + faint.Render(right) + faint.Render("─"+close)
		}
		return faint.Render(open+"─") + pathStyle.Render(left) + faint.Render(strings.Repeat("─", fill)+close)
	}
	lines := []string{border("╭", "╮", title, right)}
	for k := 0; k < rows; k++ {
		l := ""
		if top+k < len(help) {
			l = help[top+k]
		}
		lines = append(lines, faint.Render("│")+" "+cellwidth.FitPad(l, inner)+" "+faint.Render("│"))
	}
	lines = append(lines, border("╰", "╯", " any other key closes ", ""))
	return strings.Join(lines, "\n")
}

// RunUI starts the file manager full-screen and returns when the person quits.
func RunUI(o Options) error {
	// Decide the background up front. Left to itself, lipgloss asks the
	// terminal (OSC 11) and waits up to 5 seconds for an answer some
	// terminals never send, reading stdin at the same time as Bubble Tea —
	// which shows up as keys ignored and the UI freezing for seconds.
	lipgloss.SetHasDarkBackground(darkBackground(os.Getenv("COLORFGBG")))
	final, err := tea.NewProgram(New(o), tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(Model); ok {
		return fm.ExitError()
	}
	return nil
}
