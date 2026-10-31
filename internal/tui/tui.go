// Package tui implements meterm's interactive host picker. Anything you can
// do on the command line, you can also reach from here: the built-in keys
// (connect, add, edit, delete, files) are shortcuts, and the ':' command
// palette re-execs the meterm binary with whatever subcommand you type, so
// there's nothing the CLI can do that the picker can't dispatch.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"meterm/internal/config"
	"meterm/internal/health"
	"meterm/internal/history"
)

type healthState int

const (
	healthUnknown healthState = iota // not checked yet
	healthUp
	healthDown
	healthSkipped // has a proxy_jump: a direct dial would just be wrong, so we don't try
)

// Palette. AdaptiveColor picks a light- or dark-terminal-appropriate value
// automatically, so the picker doesn't come out washed-out or illegible
// depending on the user's terminal theme.
var (
	colorAccent  = lipgloss.AdaptiveColor{Light: "25", Dark: "39"}   // blue
	colorGroup   = lipgloss.AdaptiveColor{Light: "94", Dark: "214"}  // amber
	colorMuted   = lipgloss.AdaptiveColor{Light: "244", Dark: "243"} // gray
	colorFaint   = lipgloss.AdaptiveColor{Light: "247", Dark: "238"} // fainter gray, for chrome
	colorSuccess = lipgloss.AdaptiveColor{Light: "28", Dark: "78"}   // green
	colorDanger  = lipgloss.AdaptiveColor{Light: "160", Dark: "203"} // red
	colorText    = lipgloss.AdaptiveColor{Light: "236", Dark: "255"} // near-fg

	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(colorAccent).Padding(0, 1)
	tagStyle      = lipgloss.NewStyle().Foreground(colorMuted)
	groupStyle    = lipgloss.NewStyle().Foreground(colorGroup)
	selectedStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(colorMuted)
	faintStyle    = lipgloss.NewStyle().Foreground(colorFaint)
	helpStyle     = lipgloss.NewStyle().Foreground(colorFaint)
	upStyle       = lipgloss.NewStyle().Foreground(colorSuccess)
	downStyle     = lipgloss.NewStyle().Foreground(colorDanger)
	keyStyle      = lipgloss.NewStyle().Foreground(colorSuccess)
	nameStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorText)

	frameStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorFaint).Padding(0, 1)
	dangerBox  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorDanger).Padding(1, 3)
	paletteBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(0, 1)
)

var noticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

var teamStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "90", Dark: "177"}) // violet

// renderLegend is the key to the list's markers, keeping as many whole
// entries as fit in width rather than cutting one off mid-word.
func renderLegend(width int) string {
	parts := []string{
		upStyle.Render("●") + faintStyle.Render(" up"),
		downStyle.Render("●") + faintStyle.Render(" down"),
		faintStyle.Render("○ checking"),
		faintStyle.Render("◌ via jump"),
		keyStyle.Render("⚷") + faintStyle.Render(" key"),
		faintStyle.Render("⇄ routes"),
		faintStyle.Render("⇶ tunnels"),
		teamStyle.Render("⧉") + faintStyle.Render(" team"),
		noticeStyle.Render("↑") + faintStyle.Render(" unpushed"),
		downStyle.Render("✗") + faintStyle.Render(" conflict"),
	}
	sep := faintStyle.Render("  ")
	out := faintStyle.Render(" ")
	for i, p := range parts {
		next := out
		if i > 0 {
			next += sep
		}
		next += p
		if lipgloss.Width(next) > width {
			break
		}
		out = next
	}
	return out
}

type item struct {
	host         config.Host
	res          config.Resolved
	status       healthState
	last         string
	keyInstalled bool
	marked       bool   // picked for `x` (exec on several hosts)
	team         string // name of the team the host is shared with; "" = personal
	attention    string // what team sync is waiting on for this host, if anything
}

func (i item) Title() string {
	dot := dimStyle.Render("○") // still checking
	switch i.status {
	case healthUp:
		dot = upStyle.Render("●")
	case healthDown:
		dot = downStyle.Render("●")
	case healthSkipped:
		dot = dimStyle.Render("◌") // via a jump host — a direct dial would be misleading, so not checked
	}

	label := dot + " " + nameStyle.Render(i.host.Name)
	if i.marked {
		label = selectedStyle.Render("✓") + " " + label
	}
	if i.keyInstalled {
		label += " " + keyStyle.Render("⚷")
	}
	label += i.badges()
	if i.host.Group != "" {
		label += " " + groupStyle.Render("["+i.host.Group+"]")
	}

	target := i.res.Address
	if i.res.User != "" {
		target = i.res.User + "@" + i.res.Address
	}
	return label + dimStyle.Render("  "+target)
}

// badges are the compact markers after a host's name: how many other routes
// it has, how many tunnels (red when one reaches production), and whether
// team sync is waiting on it.
func (i item) badges() string {
	var b []string
	if n := len(i.host.Routes); n > 0 {
		b = append(b, dimStyle.Render(fmt.Sprintf("⇄%d", n)))
	}
	tunnels, prod := 0, i.res.IsProduction()
	if prod {
		b = append(b, downStyle.Render("⚑prod"))
	}
	for _, f := range i.host.Forwards {
		if f.Name != "" {
			tunnels++
			prod = prod || f.MentionsProduction()
		}
	}
	if tunnels > 0 {
		style := dimStyle
		if prod {
			style = downStyle
		}
		b = append(b, style.Render(fmt.Sprintf("⇶%d", tunnels)))
	}
	switch {
	case i.attention == "":
	case strings.Contains(i.attention, "conflict") || strings.Contains(i.attention, "taken") || strings.Contains(i.attention, "can't"):
		b = append(b, downStyle.Render("✗"))
	default:
		b = append(b, noticeStyle.Render("↑"))
	}
	if i.team != "" {
		b = append(b, teamStyle.Render("⧉"+printable(i.team)))
	}
	if len(b) == 0 {
		return ""
	}
	return " " + strings.Join(b, " ")
}

func (i item) Description() string {
	var parts []string
	if i.last != "" {
		parts = append(parts, faintStyle.Render(i.last))
	}
	if len(i.host.Tags) > 0 {
		var chips []string
		for _, t := range i.host.Tags {
			chips = append(chips, "#"+t)
		}
		parts = append(parts, tagStyle.Render(strings.Join(chips, " ")))
	}
	if jump := jumpText(i.res); jump != "" {
		parts = append(parts, dimStyle.Render(jump))
	}
	if len(i.host.Users) > 0 {
		parts = append(parts, dimStyle.Render("users: "+strings.Join(i.host.Logins(), ", ")))
	}
	if i.attention != "" {
		parts = append(parts, noticeStyle.Render("team: "+i.attention))
	}
	if i.host.Notes != "" {
		parts = append(parts, printable(i.host.Notes))
	}
	if len(parts) == 0 {
		return faintStyle.Render("no history yet")
	}
	return strings.Join(parts, faintStyle.Render("  ·  "))
}

// jumpText renders the actual jump chain (host names, or the raw literal
// spec for hops that aren't themselves saved hosts) instead of a generic
// "via a jump host" placeholder.
func jumpText(r config.Resolved) string {
	var hops []string
	for _, j := range r.ProxyJump {
		hops = append(hops, j.Name)
	}
	spec := strings.Join(hops, ",")
	if r.ProxyJumpRaw != "" {
		if spec != "" {
			spec += ","
		}
		spec += r.ProxyJumpRaw
	}
	if spec == "" {
		return ""
	}
	return "⇢ " + spec
}

func (i item) FilterValue() string {
	return i.host.Name + " " + i.host.Address + " " + i.host.Group + " " + strings.Join(i.host.Tags, " ") + " " + i.team
}

func newDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()
	// A left accent bar on the selected row (matching bubbles' own default
	// shape) rather than plain padding, so the current selection reads at
	// a glance instead of just being slightly bolder text.
	d.Styles.SelectedTitle = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(colorAccent).
		Foreground(colorAccent).
		Bold(true).
		Padding(0, 0, 0, 1)
	d.Styles.SelectedDesc = d.Styles.SelectedTitle.Foreground(colorMuted).Bold(false)
	return d
}

type keyMap struct {
	Connect key.Binding
	Add     key.Binding
	Edit    key.Binding
	Stats   key.Binding
	CopyID  key.Binding
	Keygen  key.Binding
	Delete  key.Binding
	Palette key.Binding
	Files   key.Binding
	Refresh key.Binding
	Quit    key.Binding

	Tunnel key.Binding
	Tmux   key.Binding
	Exec   key.Binding
	Mark   key.Binding
	Audit  key.Binding
	Logs   key.Binding
	Routes key.Binding
	Access key.Binding
	Team   key.Binding
}

var keys = keyMap{
	Connect: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "connect")),
	Add:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
	Edit:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
	Files:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "files")),
	Stats:   key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "stats")),
	CopyID:  key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install key")),
	Keygen:  key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "keygen")),
	Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
	Palette: key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "command")),
	Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

	// Lowercase l, k, u and the like are the list's own movement keys, so
	// the newer shortcuts that would clash use capitals.
	Tunnel: key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tunnel up")),
	Tmux:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "connect in tmux")),
	Exec:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "exec")),
	Mark:   key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "mark for exec")),
	Audit:  key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "audit")),
	Logs:   key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "logs")),
	Routes: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "route check")),
	Access: key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "who has access")),
	Team:   key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "team status")),
}

type mode int

const (
	modeList mode = iota
	modeConfirmDelete
	modePalette
	modePick // choosing one of picks (the tunnel picker)
)

type execDoneMsg struct{ err error }
type healthResultMsg struct {
	name  string
	state healthState
}

type model struct {
	cfgPath  string
	selfPath string
	cfg      *config.Config
	hist     history.History
	status   map[string]healthState

	list    list.Model
	palette textinput.Model
	mode    mode
	target  string // host pending delete confirmation

	width, height int

	// notice is a one-line reminder shown under the legend (e.g. team
	// changes waiting to be pushed); empty hides the line.
	notice string
	listW  int // last width handed to list.SetSize; used to hard-clamp its rendered output

	marked    map[string]bool   // hosts picked with space, for `x`
	attention map[string]string // per host: what team sync is waiting on
	teams     map[string]string // team ID → name
	picks     []choice          // modePick's rows
	pickIdx   int
	pickTitle string
	flash     string   // one-off message shown in place of the legend until the next key
	hint      []string // the palette's completion candidates
	afterExec string   // palette text to open with once the dispatched command returns
	warned    string   // palette text already warned about (shell syntax): Enter again runs it
}

// New builds the picker for the host inventory at cfgPath. selfPath is the
// meterm binary to re-exec for anything beyond the built-in shortcuts.
func New(cfgPath, selfPath string) model {
	cfg, _ := config.Load(cfgPath)
	if cfg == nil {
		cfg = &config.Config{}
	}
	hpath, _ := history.Path()
	hist, _ := history.Load(hpath)
	if hist == nil {
		hist = history.History{}
	}

	m := model{
		cfgPath:  cfgPath,
		selfPath: selfPath,
		cfg:      cfg,
		hist:     hist,
		status:   map[string]healthState{},
		marked:   map[string]bool{},
	}
	m.loadAttention()

	l := list.New(m.buildItems(), newDelegate(), 0, 0)
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	// Deliberately short: bubbles' ShortHelpView truncates with "…" when
	// content overflows its width, but if even the ellipsis doesn't fit at
	// the point of overflow it gives up truncating and dumps every
	// remaining binding unbounded — which is exactly what having ~13
	// items here triggered (the help line, and with it the whole framed
	// box, stopped respecting the terminal width). Keep this to a handful
	// of essentials and put the rest behind '?' (FullHelpView), which
	// wraps into columns instead and doesn't hit the same edge case.
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{keys.Connect, keys.Add, keys.Delete, keys.Palette}
	}
	l.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{keys.Connect, keys.Add, keys.Edit, keys.Files, keys.Stats, keys.CopyID, keys.Keygen, keys.Delete, keys.Refresh, keys.Palette,
			keys.Tunnel, keys.Tmux, keys.Exec, keys.Mark, keys.Audit, keys.Logs, keys.Routes, keys.Access, keys.Team}
	}
	m.list = l
	m.retitle()

	p := textinput.New()
	p.Prompt = "› "
	p.PromptStyle = selectedStyle
	p.Placeholder = "tab completes · a missing host is the selected one · e.g. logs, exec -- uptime, team status"
	m.palette = p

	return m
}

// Version is shown in the title bar, so "which meterm are you on?" has an
// answer on screen. cmd/meterm sets it from the build.
var Version = "dev"

// retitle refreshes the list's title bar with the current host count, so
// it stays accurate across add/delete/import without a full re-New.
func (m *model) retitle() {
	n := len(m.list.Items())
	noun := "hosts"
	if n == 1 {
		noun = "host"
	}
	m.list.Title = fmt.Sprintf("meterm %s  ·  %d %s", displayVersion(Version), n, noun)
}

// displayVersion is "v0.15.0" for a release, "dev" for a local build.
func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return "dev"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

func (m *model) buildItems() []list.Item {
	hosts := m.hist.SortByRecency(m.cfg.Hosts)
	items := make([]list.Item, len(hosts))
	for i, h := range hosts {
		last := ""
		keyInstalled := false
		if e, ok := m.hist[h.Name]; ok {
			last = history.FormatAgo(e.Last)
			keyInstalled = e.KeyInstalled
		}
		items[i] = item{host: h, res: m.cfg.Resolve(h), status: m.status[h.Name], last: last, keyInstalled: keyInstalled,
			marked: m.marked[h.Name], attention: m.attention[h.Name], team: m.teams[h.Team]}
	}
	return items
}

func (m *model) loadAttention() {
	m.attention, m.teams = nil, nil
	if HostAttention != nil {
		m.attention = HostAttention()
	}
	if TeamNames != nil {
		m.teams = TeamNames()
	}
}

// markedNames is the marked hosts, in list order.
func (m model) markedNames() []string {
	var out []string
	for _, it := range m.list.Items() {
		if iv, ok := it.(item); ok && m.marked[iv.host.Name] {
			out = append(out, iv.host.Name)
		}
	}
	return out
}

func (m model) hostNames() map[string]bool {
	out := make(map[string]bool, len(m.cfg.Hosts))
	for _, h := range m.cfg.Hosts {
		out[h.Name] = true
	}
	return out
}

// refreshItems rebuilds the rows in place, keeping the selection.
func (m *model) refreshItems() {
	selected := m.selectedName()
	m.list.SetItems(m.buildItems())
	m.reselect(selected)
}

// openPalette shows the command palette holding value.
func (m *model) openPalette(value string) tea.Cmd {
	m.mode = modePalette
	m.palette.SetValue(value)
	m.palette.CursorEnd()
	m.palette.Focus()
	m.hint = nil
	m.resizeList()
	return textinput.Blink
}

func (m model) selectedName() string {
	if it, ok := m.list.SelectedItem().(item); ok {
		return it.host.Name
	}
	return ""
}

func (m *model) reselect(name string) {
	if name == "" {
		return
	}
	for i, it := range m.list.Items() {
		if iv, ok := it.(item); ok && iv.host.Name == name {
			m.list.Select(i)
			return
		}
	}
}

// resizeList sizes the list to leave exactly enough room for the frame
// (rounded border + padding) and legend line drawn around it in View —
// and, in palette mode, the bordered input box below that — so the whole
// composed view fits the terminal exactly with no overflow or scrolling.
func (m *model) resizeList() {
	const frameH = 4 // border left+right (2) + horizontal padding (2)
	const frameV = 2 // border top+bottom
	legendV := 1
	if m.notice != "" {
		legendV++
	}
	v := frameV + legendV
	if m.mode == modePalette {
		v += 2 // palette box's own border top+bottom
		v++    // the line under the input: what will run, or the completions
	}
	w, h := m.width-frameH, m.height-v
	if w < 20 {
		w = 20
	}
	if h < 3 {
		h = 3
	}
	m.list.SetSize(w, h)
	m.listW = w
}

// reload re-reads the inventory and history from disk (a dispatched command
// may have changed either) and kicks off a fresh round of health checks.
func (m *model) reload() tea.Cmd {
	if cfg, err := config.Load(m.cfgPath); err == nil {
		m.cfg = cfg
	}
	if hpath, err := history.Path(); err == nil {
		if h, err := history.Load(hpath); err == nil {
			m.hist = h
		}
	}
	m.loadAttention()
	for name := range m.marked {
		if _, idx := m.cfg.Find(name); idx < 0 {
			delete(m.marked, name)
		}
	}
	selected := m.selectedName()
	m.list.SetItems(m.buildItems())
	m.retitle()
	m.reselect(selected)
	return tea.Batch(m.healthCmds()...)
}

func (m model) healthCmds() []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.cfg.Hosts))
	cfg := m.cfg
	for _, h := range m.cfg.Hosts {
		h := h
		r := cfg.Resolve(h)
		if len(r.ProxyJump) > 0 || r.ProxyJumpRaw != "" {
			// A direct dial from here would almost always fail even when the
			// host is perfectly reachable through its jump host, so a red
			// dot here would just be wrong. Leave it explicitly unchecked.
			cmds = append(cmds, func() tea.Msg {
				return healthResultMsg{name: h.Name, state: healthSkipped}
			})
			continue
		}
		cmds = append(cmds, func() tea.Msg {
			state := healthDown
			if health.Check(r.Address, r.Port, health.DefaultTimeout) {
				state = healthUp
			}
			return healthResultMsg{name: h.Name, state: state}
		})
	}
	return cmds
}

// dispatch re-execs meterm as a subprocess with args, handing it the
// terminal (tea.ExecProcess pauses the picker while it runs) so any
// interactive command — a wizard, an ssh/sftp session, an exec fan-out —
// behaves exactly as it would run standalone.
//
// The config path travels as $METERM_CONFIG rather than a --config flag:
// several subcommands (connect, sync) set DisableFlagParsing so they can
// pass their own args straight through to ssh/rsync, which means cobra
// never strips out a leading --config <path> — it would land as a raw
// positional argument (e.g. a hostname of literally "--config"). The env
// var is read the same way regardless of how any given subcommand parses
// its flags.
func (m model) dispatch(args ...string) tea.Cmd {
	c := buildDispatchCmd(m.selfPath, m.cfgPath, args)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return execDoneMsg{err: err}
	})
}

// buildDispatchCmd constructs the subprocess dispatch() hands to
// tea.ExecProcess. Split out as a pure function so the exact argv/env it
// produces can be asserted on in tests without spawning a real process.
func buildDispatchCmd(selfPath, cfgPath string, args []string) *exec.Cmd {
	c := exec.Command(selfPath, args...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "METERM_CONFIG=") || strings.HasPrefix(e, "METERM_FROM_TUI=") {
			continue
		}
		env = append(env, e)
	}
	c.Env = append(env, "METERM_CONFIG="+cfgPath, "METERM_FROM_TUI=1")
	return c
}

// splitArgs is a minimal shell-word tokenizer for the command palette:
// whitespace-separated, with '...' or "..." for values containing spaces.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.healthCmds()...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The box around the input takes 4 columns (border and padding) and
		// the cursor one more.
		m.palette.Width = msg.Width - lipgloss.Width(m.palette.Prompt) - 5
		m.resizeList()
		return m, nil

	case execDoneMsg:
		cmd := m.reload()
		if next := m.afterExec; next != "" {
			m.afterExec = ""
			return m, tea.Batch(cmd, m.openPalette(next))
		}
		return m, cmd

	case healthResultMsg:
		m.status[msg.name] = msg.state
		for i, it := range m.list.Items() {
			if iv, ok := it.(item); ok && iv.host.Name == msg.name {
				iv.status = m.status[msg.name]
				m.list.SetItem(i, iv)
				break
			}
		}
		return m, nil

	case tea.KeyMsg:
		m.flash = ""
		switch m.mode {
		case modePick:
			switch msg.String() {
			case "up", "k", "shift+tab":
				if m.pickIdx > 0 {
					m.pickIdx--
				}
			case "down", "j", "tab":
				if m.pickIdx < len(m.picks)-1 {
					m.pickIdx++
				}
			case "enter":
				m.mode = modeList
				return m, m.dispatch(m.picks[m.pickIdx].args...)
			case "esc", "q":
				m.mode = modeList
			}
			return m, nil

		case modeConfirmDelete:
			switch msg.String() {
			case "y", "Y":
				target := m.target
				_ = m.cfg.Update(func(c *config.Config) error {
					c.Remove(target)
					return nil
				})
				m.list.SetItems(m.buildItems())
				m.retitle()
			}
			m.mode = modeList
			m.target = ""
			return m, nil

		case modePalette:
			switch msg.Type {
			case tea.KeyEsc:
				m.mode = modeList
				m.resizeList()
				return m, nil
			case tea.KeyEnter:
				if v := m.palette.Value(); shellish(v) && m.warned != v {
					m.warned = v // first Enter: the warning under the prompt; the second runs it
					return m, nil
				}
				m.warned = ""
				m.mode = modeList
				m.resizeList()
				args := m.paletteArgs()
				if len(args) == 0 {
					return m, nil
				}
				return m, m.dispatch(args...)
			case tea.KeyTab:
				v, cands := complete(m.palette.Value(), m.cfg)
				m.palette.SetValue(v)
				m.palette.CursorEnd()
				m.hint = cands
				return m, nil
			}
			m.hint = nil
			var cmd tea.Cmd
			m.palette, cmd = m.palette.Update(msg)
			return m, cmd

		default: // modeList
			if m.list.FilterState() == list.Filtering {
				break // let the filter input consume keys first
			}
			if msg.String() == "esc" && len(m.marked) > 0 {
				m.marked = map[string]bool{} // esc drops the marks before it quits
				m.refreshItems()
				return m, nil
			}
			switch {
			case key.Matches(msg, keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, keys.Mark):
				if name := m.selectedName(); name != "" {
					if m.marked[name] {
						delete(m.marked, name)
					} else {
						m.marked[name] = true
					}
					m.refreshItems()
					m.list.CursorDown()
				}
				return m, nil
			case key.Matches(msg, keys.Exec):
				return m, m.openPalette(execPrefill(m.markedNames(), m.selectedName()))
			case key.Matches(msg, keys.Team):
				return m, m.dispatch("team", "status")
			case key.Matches(msg, keys.Audit), key.Matches(msg, keys.Routes), key.Matches(msg, keys.Access):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch(keyAction(msg.String(), name)...)
				}
			case key.Matches(msg, keys.Logs):
				if name := m.selectedName(); name != "" {
					// List what's there first, then hand back a palette ready
					// for the file to tail.
					m.afterExec = "logs " + name + " -f /var/log/"
					return m, m.dispatch(keyAction("L", name)...)
				}
			case key.Matches(msg, keys.Tunnel):
				if it, ok := m.list.SelectedItem().(item); ok {
					picks := tunnelChoices(it.host, it.res.IsProduction())
					switch len(picks) {
					case 0:
						m.flash = "no tunnels on " + it.host.Name + " — add one with: tunnel add " + it.host.Name + " <name> -L port:host:port --note \"...\""
					case 1:
						return m, m.dispatch(picks[0].args...)
					default:
						m.mode, m.picks, m.pickIdx = modePick, picks, 0
						m.pickTitle = "Open a tunnel on " + it.host.Name
					}
				}
				return m, nil
			case key.Matches(msg, keys.Connect), key.Matches(msg, keys.Tmux):
				it, ok := m.list.SelectedItem().(item)
				if !ok {
					break
				}
				var flags []string
				if key.Matches(msg, keys.Tmux) {
					flags = []string{"--tmux"}
				}
				picks := loginChoices(it.host, flags)
				if len(picks) <= 1 {
					return m, m.dispatch(append([]string{"connect", it.host.Name}, flags...)...)
				}
				m.mode, m.picks, m.pickIdx = modePick, picks, 0
				m.pickTitle = "Log in to " + it.host.Name + " as"
				return m, nil
			case key.Matches(msg, keys.Add):
				return m, m.dispatch("add")
			case key.Matches(msg, keys.Edit):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("edit", name)
				}
			case key.Matches(msg, keys.Files):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("files", name)
				}
			case key.Matches(msg, keys.Stats):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("stats", name)
				}
			case key.Matches(msg, keys.CopyID):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("copy-id", name)
				}
			case key.Matches(msg, keys.Keygen):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("keygen", "--for", name)
				}
			case key.Matches(msg, keys.Delete):
				if name := m.selectedName(); name != "" {
					m.mode = modeConfirmDelete
					m.target = name
				}
				return m, nil
			case key.Matches(msg, keys.Refresh):
				return m, m.reload()
			case key.Matches(msg, keys.Palette):
				return m, m.openPalette("")
			}
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	switch m.mode {
	case modeConfirmDelete:
		content := fmt.Sprintf("Delete host %s?\n\n%s",
			nameStyle.Render(m.target),
			helpStyle.Render("y = yes    any other key = cancel"))
		box := dangerBox.Render(content)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	case modePick:
		var rows []string
		for i, c := range m.picks {
			line := "  " + c.label
			if c.warn {
				line += "  " + downStyle.Render("⚠ PRODUCTION")
			}
			if i == m.pickIdx {
				line = selectedStyle.Render("›") + line[1:]
			}
			rows = append(rows, line)
		}
		content := nameStyle.Render(m.pickTitle) + "\n\n" + strings.Join(rows, "\n") + "\n\n" +
			helpStyle.Render("↑/↓ choose    enter = go    esc = cancel")
		box := paletteBox.Padding(1, 2).MaxWidth(m.width).Render(content)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	case modePalette:
		return frameStyle.Render(m.clampedListView()) + "\n" + m.clampedLegend() + "\n" + lipgloss.NewStyle().MaxWidth(m.width).Render(paletteBox.Render(m.palette.View())) + "\n" + m.paletteHint()
	default:
		return frameStyle.Render(m.clampedListView()) + "\n" + m.clampedLegend()
	}
}

// clampedLegend hard-caps the status legend to the terminal width, the same
// defensive reasoning as clampedListView: nothing rendered outside the
// framed box's own width-aware layout should be trusted to self-limit.
func (m model) clampedLegend() string {
	clamp := lipgloss.NewStyle().MaxWidth(m.width)
	if m.flash != "" {
		legend := clamp.Render(noticeStyle.Render(" " + m.flash))
		if m.notice != "" {
			legend += "\n" + clamp.Render(noticeStyle.Render(" ⚠ "+m.notice))
		}
		return legend
	}
	if m.notice != "" {
		return clamp.Render(renderLegend(m.width)) + "\n" + clamp.Render(noticeStyle.Render(" ⚠ "+m.notice))
	}
	return clamp.Render(renderLegend(m.width))
}

// paletteArgs is what Enter in the palette will run.
func (m model) paletteArgs() []string {
	value := strings.TrimSpace(m.palette.Value())
	if value == "" {
		return nil
	}
	return withSelectedHost(execKeepsLine(value, splitArgs(value)), m.selectedName(), m.hostNames())
}

// paletteHint is the line under the palette: the completions after a tab,
// otherwise the exact command Enter will run, so a filled-in host is never
// a surprise.
func (m model) paletteHint() string {
	clamp := lipgloss.NewStyle().MaxWidth(m.width)
	if len(m.hint) > 0 {
		return clamp.Render(faintStyle.Render(" " + strings.Join(m.hint, "  ")))
	}
	if v := m.palette.Value(); shellish(v) {
		msg := " ⚠ not a shell: $(…), $VAR, \\ and ~/ go to the host as typed, not expanded here — for that, run it in a terminal"
		if m.warned == v {
			msg += " · enter again to run it anyway"
		}
		return clamp.Render(noticeStyle.Render(msg))
	}
	args := m.paletteArgs()
	if len(args) == 0 {
		return clamp.Render(faintStyle.Render(" tab completes commands and hosts"))
	}
	return clamp.Render(faintStyle.Render(" ↵ meterm " + strings.Join(args, " ")))
}

// clampedListView hard-caps every line of the list's own rendering to the
// exact width it was given. Needed because bubbles' help.ShortHelpView (and
// FullHelpView) truncate with "…" only when there's room left for the
// ellipsis at the point content overflows — miss that by even one column
// and it gives up truncating and dumps every remaining key binding
// unbounded, which at certain width/content combinations blew the whole
// framed box past the terminal's actual width. MaxWidth is a belt-and-
// braces backstop against that regardless of which mode or width triggers it.
func (m model) clampedListView() string {
	return lipgloss.NewStyle().MaxWidth(m.listW).Render(m.list.View())
}

// Run launches the picker over the inventory at cfgPath until the user
// quits. notice, if not empty, is shown as a reminder under the legend.
func Run(cfgPath, notice string) error {
	selfPath, err := os.Executable()
	if err != nil {
		return err
	}
	m := New(cfgPath, selfPath)
	m.notice = notice
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}
