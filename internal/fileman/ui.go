package fileman

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Options configures a file manager session.
type Options struct {
	Left, Right       FS // left is normally this machine, right the host
	LeftDir, RightDir string
	// OnEvent is told about everything that changed something, for the
	// activity log: kind is upload, download, delete, rename or mkdir;
	// remote reports whether it happened on the host (Right).
	OnEvent func(kind, detail string, remote bool)
}

type mode int

const (
	modeNormal mode = iota
	modeInput
	modeConfirm
	modeBusy
)

type inputKind int

const (
	inputMkdir inputKind = iota
	inputRename
	inputFind
)

type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmOverwrite
)

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

	pendingPlan   *Plan
	pendingDelete []Entry

	status    string
	statusErr bool
	progress  *Progress
	cancel    context.CancelFunc
	events    chan tea.Msg
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

func (m Model) rows() int {
	r := m.h - 2 /*border*/ - 1 /*path*/ - 2 /*status+help*/
	if r < 1 {
		r = 1
	}
	return r
}

func (m *Model) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		if msg.err != nil {
			m.mode = modeNormal
			m.setStatus("copy: "+msg.err.Error(), true)
			return m, nil
		}
		if len(msg.plan.Files) == 0 && len(msg.plan.Dirs) == 0 {
			m.mode = modeNormal
			m.setStatus("nothing to copy", false)
			return m, nil
		}
		if n := msg.plan.Conflicts(); n > 0 {
			plan := msg.plan
			m.pendingPlan = &plan
			m.mode, m.confirmKind = modeConfirm, confirmOverwrite
			m.confirmText = fmt.Sprintf("%d of %d file(s) already exist on %s.  o = overwrite   s = skip those   esc = cancel", n, len(plan.Files), plan.Dst.Label())
			return m, nil
		}
		return m.startTransfer(msg.plan, false)

	case progressMsg:
		p := Progress(msg)
		m.progress = &p
		return m, waitFor(m.events)

	case transferDoneMsg:
		m.mode, m.progress, m.cancel = modeNormal, nil, nil
		dst := 1 - m.active
		for _, p := range m.panes {
			p.sel = map[string]bool{}
		}
		if msg.err != nil {
			if msg.err == context.Canceled {
				m.setStatus(fmt.Sprintf("cancelled after %d file(s); the unfinished file was removed", msg.res.Copied), true)
			} else {
				m.setStatus("copy failed: "+msg.err.Error(), true)
			}
		} else {
			s := fmt.Sprintf("copied %d file(s), %s to %s:%s", msg.res.Copied, HumanBytes(msg.res.Bytes), msg.plan.Dst.Label(), m.panes[dst].dir)
			if msg.res.Skipped > 0 {
				s += fmt.Sprintf("; kept %d existing", msg.res.Skipped)
			}
			if len(msg.plan.Skipped) > 0 {
				s += fmt.Sprintf("; skipped %d symlink(s)/special file(s)", len(msg.plan.Skipped))
			}
			m.setStatus(s, false)
		}
		if msg.res.Copied > 0 {
			m.report(msg.plan, msg.res)
		}
		return m, tea.Batch(m.load(0, m.panes[0].dir), m.load(1, m.panes[1].dir))

	case opDoneMsg:
		m.mode = modeNormal
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.status, false)
		}
		var cmds []tea.Cmd
		for _, i := range msg.panes {
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
				model, cmd = model.Update(singleKey(r))
				cmds = append(cmds, cmd)
			}
			return model, tea.Batch(cmds...)
		}
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeBusy:
			switch {
			case (msg.String() == "esc" || msg.String() == "ctrl+c") && m.cancel != nil:
				m.cancel()
				m.setStatus("cancelling…", true)
			case m.progress == nil:
				m.setStatus("busy — wait for it to finish", true)
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
	m.setStatus("", false)
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "ctrl+w":
		m.active = 1 - m.active
	case "j", "down":
		p.cursor++
	case "k", "up":
		p.cursor--
	case "ctrl+d", "pgdown":
		p.cursor += rows / 2
	case "ctrl+u", "pgup":
		p.cursor -= rows / 2
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
	case "a":
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
	case "R", "ctrl+r":
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
		m.mode = modeBusy
		m.setStatus("working out what to copy…", false)
		src, dst, dstDir := p.fs, other.fs, other.dir
		return m, func() tea.Msg {
			plan, err := MakePlan(context.Background(), src, items, dst, dstDir)
			return planMsg{plan: plan, err: err}
		}
	case "d", "f8", "delete":
		ts := p.targets()
		if len(ts) == 0 {
			return m, nil
		}
		m.pendingDelete = ts
		m.mode, m.confirmKind = modeConfirm, confirmDelete
		what := ts[0].Name
		if len(ts) > 1 {
			what = fmt.Sprintf("%d items", len(ts))
		}
		m.confirmText = fmt.Sprintf("Delete %s on %s (directories with everything inside)?  y = delete   any other key = cancel", what, p.fs.Label())
	case "r", "f6":
		if e, ok := p.current(); ok {
			m.mode, m.inputKind = modeInput, inputRename
			m.input.Prompt = "Rename to: "
			m.input.SetValue(e.Name)
			m.input.CursorEnd()
			m.input.Focus()
		}
	case "/":
		m.mode, m.inputKind = modeInput, inputFind
		m.input.Prompt = "Find: "
		m.input.SetValue("")
		m.input.Focus()
	case "n":
		m.findNext(m.lastFind, p.cursor+1)
	case "m", "f7":
		m.mode, m.inputKind = modeInput, inputMkdir
		m.input.Prompt = "New directory: "
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
		m.mode = modeNormal
		m.input.Blur()
		return m, nil
	case "enter":
		m.mode = modeNormal
		m.input.Blur()
		name := strings.TrimSpace(m.input.Value())
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			m.setStatus("not a valid name", true)
			return m, nil
		}
		i := m.active
		p := m.panes[i]
		fsys, dir := p.fs, p.dir
		remote := i == 1
		on := m.opts.OnEvent
		switch m.inputKind {
		case inputMkdir:
			p.focusOn = name
			return m, func() tea.Msg {
				target := fsys.Join(dir, name)
				if err := fsys.MkdirAll(target); err != nil {
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("mkdir: %w", err)}
				}
				if on != nil {
					on("mkdir", target, remote)
				}
				return opDoneMsg{panes: []int{i}, status: "created " + target}
			}
		case inputRename:
			e, ok := p.current()
			if !ok || e.Name == name {
				return m, nil
			}
			p.focusOn = name
			return m, func() tea.Msg {
				from, to := fsys.Join(dir, e.Name), fsys.Join(dir, name)
				if _, err := fsys.Lstat(to); err == nil {
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("rename: %s already exists", name)}
				}
				if err := fsys.Rename(from, to); err != nil {
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("rename: %w", err)}
				}
				if on != nil {
					on("rename", from+" -> "+to, remote)
				}
				return opDoneMsg{panes: []int{i}, status: "renamed to " + name}
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
			if match(strings.ToLower(v[idx].Name)) {
				p.cursor = idx
				p.clamp(m.rows())
				return
			}
		}
	}
}

func (m Model) updateConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := strings.ToLower(k.String())
	switch m.confirmKind {
	case confirmOverwrite:
		plan := m.pendingPlan
		m.pendingPlan = nil
		switch key {
		case "o":
			return m.startTransfer(*plan, true)
		case "s":
			return m.startTransfer(*plan, false)
		}
		m.mode = modeNormal
		m.setStatus("copy cancelled", false)
		return m, nil
	case confirmDelete:
		ts := m.pendingDelete
		m.pendingDelete = nil
		if key != "y" {
			m.mode = modeNormal
			m.setStatus("delete cancelled", false)
			return m, nil
		}
		i := m.active
		p := m.panes[i]
		fsys, dir := p.fs, p.dir
		remote := i == 1
		on := m.opts.OnEvent
		p.sel = map[string]bool{}
		m.mode = modeBusy
		m.setStatus("deleting…", false)
		return m, func() tea.Msg {
			var names []string
			for _, e := range ts {
				target := fsys.Join(dir, e.Name)
				if err := RemoveAll(context.Background(), fsys, target); err != nil {
					return opDoneMsg{panes: []int{i}, err: fmt.Errorf("delete %s: %w", e.Name, err)}
				}
				names = append(names, target)
			}
			if on != nil {
				on("delete", strings.Join(names, ", "), remote)
			}
			return opDoneMsg{panes: []int{i}, status: fmt.Sprintf("deleted %d item(s)", len(ts))}
		}
	}
	return m, nil
}

func (m Model) startTransfer(plan Plan, overwrite bool) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mode = modeBusy
	m.progress = &Progress{Total: plan.TotalBytes, FilesTotal: len(plan.Files), Started: time.Now()}
	ch := make(chan tea.Msg, 16)
	m.events = ch
	go func() {
		res, err := Run(ctx, plan, overwrite, func(p Progress) {
			select {
			case ch <- progressMsg(p):
			default: // the UI is behind; drop this update rather than slow the copy
			}
		})
		ch <- transferDoneMsg{res: res, err: err, plan: plan}
	}()
	return m, waitFor(ch)
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

// report tells OnEvent what a finished copy did.
func (m Model) report(plan Plan, res Result) {
	if m.opts.OnEvent == nil {
		return
	}
	kind, remote := "download", true
	switch {
	case plan.Dst == m.opts.Right:
		kind = "upload"
	case plan.Src != m.opts.Right:
		kind, remote = "copy", false
	}
	var tops []string
	for _, it := range plan.Items {
		if len(tops) == 5 {
			tops = append(tops, fmt.Sprintf("+%d more", len(plan.Items)-5))
			break
		}
		tops = append(tops, plan.Src.Base(it))
	}
	detail := fmt.Sprintf("%s -> %s (%d file(s), %s)", strings.Join(tops, ", "), plan.DstDir, res.Copied, HumanBytes(res.Bytes))
	m.opts.OnEvent(kind, detail, remote)
}

// ---- view ------------------------------------------------------------------

var (
	borderActive   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("39"))
	borderInactive = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	pathStyle      = lipgloss.NewStyle().Bold(true)
	dirStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	linkStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	selStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	cursorStyle    = lipgloss.NewStyle().Reverse(true)
	faint          = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
)

func (m Model) View() string {
	paneW := m.w / 2
	left := m.viewPane(0, paneW)
	right := m.viewPane(1, m.w-paneW)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	return body + "\n" + m.viewStatus() + "\n" + m.viewHelp()
}

func (m Model) viewPane(i, width int) string {
	p := m.panes[i]
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	rows := m.rows()
	title := p.fs.Label() + ":" + p.dir
	if n := len(p.sel); n > 0 {
		title += fmt.Sprintf("  [%d selected]", n)
	}
	// One header line, then exactly rows() entries: clamp() scrolls by the
	// same count, so the cursor row is always one that gets drawn. An error
	// goes in the header rather than taking an entry's row.
	header := pathStyle.Render(truncLeft(title, inner))
	if p.err != "" {
		header = errStyle.Render(ansi.Truncate("⚠ "+p.err, inner, "…"))
	}
	lines := []string{header}
	v := p.visible()
	for r := 0; r < rows && p.offset+r < len(v); r++ {
		idx := p.offset + r
		lines = append(lines, m.viewRow(p, v[idx], inner, idx == p.cursor && i == m.active, idx == p.cursor))
	}
	for len(lines) < rows+1 {
		lines = append(lines, "")
	}
	style := borderInactive
	if i == m.active {
		style = borderActive
	}
	return style.Width(inner).Render(strings.Join(lines, "\n"))
}

func (m Model) viewRow(p *pane, e Entry, width int, cursor, cursorInactive bool) string {
	mark := " "
	if p.sel[e.Name] {
		mark = "*"
	}
	name := e.Name
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
	nameW := width - 2 - lipgloss.Width(right) - 1
	if nameW < 4 {
		nameW, right = width-2, ""
	}
	name = ansi.Truncate(name, nameW, "…")
	pad := nameW - lipgloss.Width(name)
	if pad < 0 {
		pad = 0
	}
	line := mark + " " + name + strings.Repeat(" ", pad) + " " + right
	switch {
	case cursor:
		return cursorStyle.Render(line)
	case p.sel[e.Name]:
		return selStyle.Render(line)
	case e.IsDir:
		return dirStyle.Render(line)
	case e.Link:
		return linkStyle.Render(line)
	case cursorInactive:
		return faint.Render(line)
	}
	return line
}

func (m Model) viewStatus() string {
	switch {
	case m.mode == modeInput:
		return m.input.View()
	case m.mode == modeConfirm:
		return selStyle.Render(ansi.Truncate(m.confirmText, m.w, "…"))
	case m.progress != nil:
		p := m.progress
		pct := 0.0
		if p.Total > 0 {
			pct = float64(p.Done) / float64(p.Total)
		}
		barW := 20
		filled := int(pct * float64(barW))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
		name := ""
		if p.File != "" {
			name = m.panes[m.active].fs.Base(p.File) + "  "
		}
		s := fmt.Sprintf("%s%s %3.0f%%  %s / %s  %s/s  file %d/%d  (esc cancels)",
			name, bar, pct*100, HumanBytes(p.Done), HumanBytes(p.Total), HumanBytes(int64(p.Rate())), min(p.Files+1, p.FilesTotal), p.FilesTotal)
		return ansi.Truncate(s, m.w, "…")
	case m.status != "":
		st := okStyle
		if m.statusErr {
			st = errStyle
		}
		return st.Render(ansi.Truncate(m.status, m.w, "…"))
	}
	return ""
}

func (m Model) viewHelp() string {
	h := "tab switch  hjkl move  / find  space select  a all  c copy→  d delete  r rename  m mkdir  . hidden  ~ home  R reload  q quit"
	return faint.Render(ansi.Truncate(h, m.w, "…"))
}

// truncLeft keeps the end of a long path, which is the part that matters.
func truncLeft(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[1:]
	}
	return "…" + string(r)
}

// RunUI starts the file manager full-screen and returns when the person quits.
func RunUI(o Options) error {
	// Decide the background up front. Left to itself, lipgloss asks the
	// terminal (OSC 11) and waits up to 5 seconds for an answer some
	// terminals never send, reading stdin at the same time as Bubble Tea —
	// which shows up as keys ignored and the UI freezing for seconds.
	lipgloss.SetHasDarkBackground(true)
	_, err := tea.NewProgram(New(o), tea.WithAltScreen()).Run()
	return err
}
