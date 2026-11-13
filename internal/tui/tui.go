// Package tui implements meterm's interactive host picker. Anything you can
// do on the command line, you can also reach from here: the built-in keys
// (connect, add, edit, delete, files) are shortcuts, and the ':' command
// palette re-execs the meterm binary with whatever subcommand you type, so
// there's nothing the CLI can do that the picker can't dispatch.
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"meterm/internal/cellwidth"
	"meterm/internal/config"
	"meterm/internal/health"
	"meterm/internal/history"
	"meterm/internal/prodask"
	"meterm/internal/termsafe"
)

type healthState = health.State

const (
	healthUnknown = health.Unknown // not checked yet
	healthUp      = health.Up
	healthDown    = health.Down
	healthSkipped = health.NotChecked // connect wouldn't try it (an unsafe host), so neither do we
	healthUnsure  = health.Unsure     // the port took the connection, no SSH greeting came
)

// The health checks run again in the background every recheckEvery, and a
// mark older than that is drawn dim. Coming back to the picker (focus)
// checks again once the last round is focusRecheck old.
const (
	recheckEvery = 5 * time.Minute
	focusRecheck = 30 * time.Second
	tickEvery    = 30 * time.Second // also keeps "2m ago" current
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

	dangerEdge = lipgloss.NewStyle().Foreground(colorDanger) // a question's box
	accentEdge = lipgloss.NewStyle().Foreground(colorAccent) // a picker's or the palette's
)

var noticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

var teamStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "90", Dark: "177"}) // violet

type item struct {
	host         config.Host
	res          config.Resolved
	status       healthState
	check        health.Result // the last answer: route, why not, when
	now          time.Time     // set when drawn, for the answer's age
	last         string
	keyInstalled bool
	marked       bool   // picked for `x` (exec on several hosts)
	team         string // name of the team the host is shared with; "" = personal
	envRaw       string // the env as written, when meterm doesn't know it (see Config.EnvUnrecognised)
	attention    string // what team sync is waiting on for this host, if anything
}

// target is user@address as the row shows it.
func (i item) target() string {
	if i.res.User != "" {
		return i.res.User + "@" + i.res.Address
	}
	return i.res.Address
}

// filterFields is what the filter searches, in order: FilterValue joins
// them with fieldSep, and a match position in it is mapped back onto the
// field it fell in, so the row can mark it without touching anything the
// picker styled itself. The address comes last: hostFilter matches it only
// as a plain substring.
func (i item) filterFields() []string {
	return []string{
		termsafe.Label(i.host.Name), termsafe.Label(i.host.Group), i.tagsText(), i.res.Env,
		termsafe.Label(i.host.Notes), termsafe.Label(strings.Join(i.logins(), " ")), termsafe.Label(i.team),
		termsafe.Label(i.target()),
	}
}

// logins are the accounts the row's user@ and the host's users name.
func (i item) logins() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, u := range append([]string{i.res.User}, i.host.Users...) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// tags are the host's tags, each made safe to show.
func (i item) tags() []string {
	out := make([]string, len(i.host.Tags))
	for k, t := range i.host.Tags {
		out[k] = termsafe.Label(t)
	}
	return out
}

// fieldSep separates the fields in FilterValue. termsafe.Label drops
// control characters, so no field can contain it.
const fieldSep = "\x1f"

const (
	fieldName = iota
	fieldGroup
	fieldTags
	fieldEnv
	fieldNotes
	fieldUsers
	fieldTeam
	fieldAddr
)

func (i item) FilterValue() string { return strings.Join(i.filterFields(), fieldSep) }

// fieldStart is where field f begins in FilterValue, in bytes.
func fieldStart(fields []string, f int) int {
	start := 0
	for _, prev := range fields[:f] {
		start += len(prev) + len(fieldSep)
	}
	return start
}

// mark renders text, which sits at byte offset start in FilterValue, in
// style, underlining the letters at the matched byte offsets (as the fuzzy
// filter reports them).
func mark(text string, start int, matches []int, style lipgloss.Style) string {
	var runes []int
	for _, at := range matches {
		if at >= start && at < start+len(text) {
			runes = append(runes, utf8.RuneCountInString(text[:at-start]))
		}
	}
	if len(runes) == 0 {
		return style.Render(text)
	}
	return lipgloss.StyleRunes(text, runes, style.Underline(true), style)
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

// rowDelegate tells the list each host takes one line. The picker draws
// the rows itself (see table.go), so Render is never asked for one.
type rowDelegate struct{}

func (rowDelegate) Height() int                                  { return 1 }
func (rowDelegate) Spacing() int                                 { return 0 }
func (rowDelegate) Update(tea.Msg, *list.Model) tea.Cmd          { return nil }
func (rowDelegate) Render(io.Writer, list.Model, int, list.Item) {}

type keyMap struct {
	Connect key.Binding
	Add     key.Binding
	Edit    key.Binding
	Rename  key.Binding
	Stats   key.Binding
	CopyID  key.Binding
	Keygen  key.Binding
	Delete  key.Binding
	Palette key.Binding
	Files   key.Binding
	Refresh key.Binding
	Sort    key.Binding
	Help    key.Binding
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

// keys is the picker's key map. A key means the same here as in the file
// manager (meterm files) wherever both have the idea: r reloads, R renames,
// d deletes, space marks, / filters or finds, esc clears, ? helps, q quits.
// The README's key table lists both screens side by side.
var keys = keyMap{
	Connect: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "connect")),
	Add:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add a host")),
	Edit:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
	Rename:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "rename")),
	Files:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "files")),
	Stats:   key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "stats")),
	CopyID:  key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install key")),
	Keygen:  key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "keygen")),
	Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
	Palette: key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "command")),
	Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Sort:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sort")),
	Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

	Tunnel: key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tunnel up")),
	Tmux:   key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "connect in tmux")),
	Exec:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "exec")),
	Mark:   key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "mark")),
	Audit:  key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "audit")),
	Logs:   key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "logs")),
	Routes: key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "route check")),
	Access: key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "who has access")),
	Team:   key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "team status")),
}

// listKeys are the movement keys the list itself handles: j/k and arrows,
// g/G, a page with PgUp/PgDn (ctrl+u/ctrl+d, half a page, are the
// picker's own, as in the file manager). h, l, b, f, u and d (bubbles'
// defaults for paging) are left free for the picker's own keys, and the
// list's own quit and help keys are off: esc never quits.
func listKeys() list.KeyMap {
	k := list.DefaultKeyMap()
	k.PrevPage = key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up"))
	k.NextPage = key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "page down"))
	k.ShowFullHelp = key.NewBinding(key.WithDisabled())
	k.CloseFullHelp = key.NewBinding(key.WithDisabled())
	k.Quit = key.NewBinding(key.WithDisabled())
	k.ForceQuit = key.NewBinding(key.WithDisabled())
	return k
}

type mode int

const (
	modeList mode = iota
	modeConfirmDelete
	modePalette
	modePick       // choosing one of picks (the tunnel or login picker)
	modeConfirmRun // asking before running something on a production host
	modeHelp       // the ? screen
)

type execDoneMsg struct{ err error }

// healthResultMsg is one host's check. key is what was checked (its
// addresses and routes) and round which round of checks asked: an answer
// for an older round, or for an address the host no longer has, is dropped.
type healthResultMsg struct {
	name  string
	key   string
	round int
	res   health.Result
}

// tickMsg comes every tickEvery: it redraws the ages and, when due, checks
// every host again.
type tickMsg struct{}

func tick() tea.Cmd { return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{} }) }

type model struct {
	cfgPath  string
	selfPath string
	cfg      *config.Config
	loadErr  error // why the host file couldn't be read; cfg is then the last good one, or empty
	hist     history.History
	checks   map[string]health.Result // each host's last answer, by name
	round    int                      // the round of checks under way; answers to others are dropped
	roundAt  time.Time                // when it started

	list    list.Model
	palette textinput.Model
	mode    mode
	target  string // host pending delete confirmation

	width, height int

	// notice is a one-line reminder shown under the footer (e.g. team
	// changes waiting to be pushed); empty hides the line.
	notice string

	marked        map[string]bool   // hosts picked with space, for `x`
	attention     map[string]string // per host: what team sync is waiting on
	teams         map[string]string // team ID → name
	picks         []choice          // modePick's rows
	pickIdx       int
	pickTitle     string
	flash         string            // one-off message shown in the footer until the next key
	flashNeutral  bool              // the message is news, not a warning (a session that ended)
	lastFlash     string            // the last message, kept for the ? screen
	sort          sortMode          // the rows' order; s cycles it
	helpTop       int               // the ? screen's first line shown
	hint          []string          // the palette's completion candidates
	afterExec     string            // palette text to open with once the dispatched command returns
	pendingSelect string            // host to put the cursor on when the re-run filter's rows arrive
	pendingDown   bool              // and then move one row down (after marking)
	warned        string            // palette text already warned about (shell syntax): Enter again runs it
	refs          map[string]string // ‹...› references the picker wrote into the palette → the exact host name (see typedName)

	running     []string // the command last dispatched, for saying how it ended
	pending     []string // modeConfirmRun: the command waiting for a yes
	pendingProd []string // and the production hosts it reaches

	outcomeDir string // where a dispatched connect says its session ran (see outcomeFile)

	lastDispatch *exec.Cmd // the command dispatch last started, for tests
	prodToken    string    // the token file handed to the running command, removed once it ends

	askedAt time.Time        // when the question on screen (production, delete) appeared
	now     func() time.Time // the clock; tests replace it
}

// New builds the picker for the host inventory at cfgPath. selfPath is the
// meterm binary to re-exec for anything beyond the built-in shortcuts.
func New(cfgPath, selfPath string) model {
	// A host file that doesn't parse is shown as an error, not as an empty
	// inventory: an empty list invites adding hosts over a file meterm
	// couldn't read.
	cfg, loadErr := config.Load(cfgPath)
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
		loadErr:  loadErr,
		hist:     hist,
		checks:   map[string]health.Result{},
		marked:   map[string]bool{},
		refs:     map[string]string{},
		now:      time.Now,
	}
	m.loadAttention()

	l := list.New(m.buildItems(), rowDelegate{}, 0, 0)
	// The picker draws its own title, filter line, header and footer around
	// the rows (see frame in View); the list keeps the rows, the cursor, the
	// pages and the filter.
	l.SetShowTitle(false)
	l.SetShowFilter(false)
	l.SetShowStatusBar(false)
	l.SetShowPagination(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Filter = hostFilter
	l.KeyMap = listKeys()
	l.DisableQuitKeybindings()
	l.FilterInput.Prompt = "/"
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
	rows := make([]item, len(hosts))
	for i, h := range hosts {
		last := ""
		keyInstalled := false
		if e, ok := m.hist[h.Name]; ok {
			last = history.FormatAgo(e.Last)
			keyInstalled = e.KeyInstalled
		}
		rows[i] = item{host: h, res: m.cfg.Resolve(h), status: m.checks[h.Name].State, check: m.checks[h.Name], last: last, keyInstalled: keyInstalled,
			marked: m.marked[h.Name], attention: m.attention[h.Name], team: m.teams[h.Team], envRaw: m.cfg.EnvUnrecognised(h)}
	}
	sortItems(rows, m.sort)
	items := make([]list.Item, len(rows))
	for i, r := range rows {
		items[i] = r
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
	out := make(map[string]bool, 2*len(m.cfg.Hosts))
	for _, h := range m.cfg.Hosts {
		out[h.Name] = true
		out[termsafe.Label(h.Name)] = true // typed as it shows
	}
	return out
}

// refreshItems rebuilds the rows in place, keeping the selection. The
// command it returns re-runs an applied filter over the new rows: dropped,
// a filtered list comes back empty.
func (m *model) refreshItems() tea.Cmd {
	selected := m.selectedName()
	cmd := m.list.SetItems(m.buildItems())
	m.reselect(selected)
	return cmd
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

// reselect moves the cursor to the host called name. Select counts rows
// as shown, so under a filter it's the filtered rows that are searched;
// while the filter is still being re-run over new rows there are none, and
// the cursor moves once its result arrives.
func (m *model) reselect(name string) {
	m.pendingSelect, m.pendingDown = "", false
	if name == "" {
		return
	}
	for i, it := range m.list.VisibleItems() {
		if iv, ok := it.(item); ok && iv.host.Name == name {
			m.list.Select(i)
			return
		}
	}
	if m.list.FilterState() != list.Unfiltered {
		m.pendingSelect = name
	}
}

// Below this the picker says the terminal is too small rather than draw
// something that doesn't fit.
const (
	minWidth  = 40
	minHeight = 10
)

// tooSmall says the terminal is known to be too small; before the first
// size message it isn't known at all.
func (m model) tooSmall() bool {
	return m.width > 0 && (m.width < minWidth || m.height < minHeight)
}

// chromeHeight is how many lines the frame takes around the rows: the
// banner, the top border with the title, the column header, the detail
// line, the bottom border with the filter, then the footer (one line of
// keys, or a message of up to two), the notice, and in palette mode the
// input box and the line under it in place of the footer.
func (m model) chromeHeight() int {
	h := len(m.bannerLines()) + 4
	if m.mode == modePalette {
		h += 3 + 1
	} else {
		h += len(m.footerLines())
	}
	if m.notice != "" {
		h++
	}
	return h
}

// resizeList gives the list exactly the rows left once the frame is drawn,
// so the whole screen fits the terminal with nothing scrolling off.
func (m *model) resizeList() {
	m.list.SetSize(max(1, m.width-4), max(1, m.height-m.chromeHeight()))
}

// reload re-reads the inventory and history from disk (a dispatched command
// may have changed either) and kicks off a fresh round of health checks.
// Every host shows as checking until its new result is in, so an old
// answer can't pass for a fresh one.
func (m *model) reload() tea.Cmd {
	m.checks = map[string]health.Result{}
	// When the file stops parsing, the last list read stays on screen with
	// the error above it.
	if cfg, err := config.Load(m.cfgPath); err == nil {
		m.cfg, m.loadErr = cfg, nil
	} else {
		m.loadErr = err
	}
	if m.width > 0 {
		m.resizeList() // the error banner comes or goes
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
	refilter := m.list.SetItems(m.buildItems())
	m.retitle()
	m.reselect(selected)
	return tea.Batch(append(m.newRound(), refilter)...)
}

// dialHealth is the health check's dial (an SSH greeting); tests swap it
// out to stay off the network.
var dialHealth health.Dialer = health.Dial

// healthCmds checks every host the way connect would reach it (see
// health.CheckHost), as the round under way: each route's first hop, which
// for a host behind a jump host is the jump host.
func (m model) healthCmds() []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.cfg.Hosts))
	cfg, round, now := m.cfg, m.round, m.now
	dial := dialHealth // read here, not in the commands, which run on their own goroutines
	last := lastRoutes(m.cfgPath)
	for _, h := range m.cfg.Hosts {
		h := h
		// Worked out here: the commands run on their own goroutines, and the
		// config may change under them (a delete, a reload).
		key, plan := hostKey(cfg, h), health.PlanHost(cfg, h, last)
		cmds = append(cmds, func() tea.Msg {
			return healthResultMsg{name: h.Name, key: key, round: round, res: plan.Run(dial, health.DefaultTimeout, now)}
		})
	}
	return cmds
}

// newRound starts a round of checks; answers to the one before are dropped.
func (m *model) newRound() []tea.Cmd {
	m.round++
	m.roundAt = m.now()
	return m.healthCmds()
}

// hostKey is what a host's check depends on: every way to reach it.
func hostKey(cfg *config.Config, h config.Host) string {
	var b strings.Builder
	for _, rt := range h.Candidates() {
		rt := rt
		r := cfg.ResolveVia(h, &rt)
		fmt.Fprintf(&b, "%s=%s:%d>%s|", rt.Name, r.Address, r.Port, jumpText(r))
	}
	return b.String()
}

// lastRoutes is the route each host was last reached by, which connect tries
// first (internal/route keeps it next to the host file).
func lastRoutes(cfgPath string) map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "routes.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
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
func (m *model) dispatch(args ...string) tea.Cmd {
	return m.dispatchWith(false, args)
}

// dispatchAsked dispatches args after the person said y to the picker's
// production question, with a one-use token (prodask) that lets the child
// skip asking again. Without one (it couldn't be written) the child asks.
func (m *model) dispatchAsked(args ...string) tea.Cmd {
	return m.dispatchWith(true, args)
}

func (m *model) dispatchWith(asked bool, args []string) tea.Cmd {
	m.running = args
	c := buildDispatchCmd(m.selfPath, m.cfgPath, args)
	if p := m.outcomeFile(); p != "" {
		c.Env = append(c.Env, SessionOutcomeEnv+"="+p)
		if asked {
			if env, err := prodask.Issue(m.outcomeDir); err == nil {
				c.Env = append(c.Env, env...)
				m.prodToken = strings.TrimPrefix(env[0], prodask.PathEnv+"=")
			}
		}
	}
	m.lastDispatch = c
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return execDoneMsg{err: err}
	})
}

// run dispatches args, but first asks when they reach a production host,
// or when warn says so (a tunnel whose note says it leads to production).
func (m *model) run(warn bool, args ...string) tea.Cmd {
	prod := productionTargets(args, m.cfg)
	if len(prod) == 0 && !warn {
		return m.dispatch(args...)
	}
	m.mode, m.pending, m.pendingProd = modeConfirmRun, args, prod
	m.askedAt = m.now()
	return nil
}

// ConfirmSettle is how long a question that guards production or a delete
// must have been on screen before a key answers it. Keys that arrive sooner
// were typed before it could be read (a double Enter, a y typed ahead) and
// are ignored: the question stays. The file manager uses the same 150ms
// (internal/fileman confirmSettle).
const ConfirmSettle = 150 * time.Millisecond

// settling says the question on screen is too new for a key to answer it.
func (m model) settling() bool {
	return m.now().Sub(m.askedAt) < ConfirmSettle
}

// reason is err for a one-line message: the cause first, since the line
// is cut at the screen's edge and a long path would push it off.
func reason(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error() + " (" + pathErr.Op + " " + pathErr.Path + ")"
	}
	return err.Error()
}

// buildDispatchCmd constructs the subprocess dispatch() hands to
// tea.ExecProcess. Split out as a pure function so the exact argv/env it
// produces can be asserted on in tests without spawning a real process.
func buildDispatchCmd(selfPath, cfgPath string, args []string) *exec.Cmd {
	c := exec.Command(selfPath, args...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "METERM_CONFIG=") || strings.HasPrefix(e, "METERM_FROM_TUI=") ||
			strings.HasPrefix(e, prodask.PathEnv+"=") || strings.HasPrefix(e, prodask.ValueEnv+"=") {
			continue
		}
		env = append(env, e)
	}
	c.Env = append(env, "METERM_CONFIG="+cfgPath, "METERM_FROM_TUI=1")
	return c
}

// splitArgs is a minimal shell-word tokenizer for the command palette:
// whitespace-separated, with '...' or "..." for values containing spaces,
// taken as typed (backslashes and all). The one exception is a reference the
// picker itself wrote (typedName) for a host name it can't show as it is:
// ‹...› outside quotes that matches an entry in refs exactly is replaced by
// the name it stands for. Anything else between ‹ and › is just text.
func splitArgs(s string, refs map[string]string) []string {
	var args []string
	var cur strings.Builder
	var quote byte
	word := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			word = true
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 || word {
				args = append(args, cur.String())
				cur.Reset()
			}
			word = false
		default:
			if tok := refAt(s, i); tok != "" {
				if name, ok := refs[tok]; ok {
					word = true
					cur.WriteString(name)
					i += len(tok) - 1
					continue
				}
			}
			word = true
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 || word {
		args = append(args, cur.String())
	}
	return args
}

func (m model) Init() tea.Cmd {
	return tea.Batch(append(m.healthCmds(), tick())...)
}

// Update handles msg, then sizes the list to whatever room the frame now
// leaves it: a message in the footer, the palette or the error banner
// coming or going all change that.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm := next.(model)
	if nm.flash != "" {
		nm.lastFlash = nm.flash
	}
	if nm.width > 0 {
		nm.resizeList()
	}
	return nm, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The box around the input takes 4 columns (border and padding) and
		// the cursor one more.
		m.palette.Width = msg.Width - lipgloss.Width(m.palette.Prompt) - 5
		m.resizeList()
		return m, nil

	case execDoneMsg:
		if m.prodToken != "" {
			_ = os.Remove(m.prodToken) // the command didn't need it: it must not outlive it
			m.prodToken = ""
		}
		cmd := m.reload()
		next := m.afterExec
		m.afterExec = ""
		if msg.err != nil {
			// The command printed its own error before handing the screen
			// back; this keeps the outcome in view.
			what := "meterm " + strings.Join(m.running, " ")
			var exitErr *exec.ExitError
			ran := m.sessionRan()
			switch {
			case errors.As(msg.err, &exitErr) && ran && exitErr.ExitCode() > 0 && exitErr.ExitCode() != 255:
				// connect ends with the remote shell's code: the session ran,
				// and its last command's status is news, not a failure.
				m.flash = fmt.Sprintf("session on %s ended, exit %d", termsafe.Label(m.running[1]), exitErr.ExitCode())
				m.flashNeutral = true
			case errors.As(msg.err, &exitErr) && isSession(m.running) && exitErr.ExitCode() == 255:
				m.flash = what + ": ssh exit 255 (connection lost, or never made)"
			case errors.As(msg.err, &exitErr):
				m.flash = fmt.Sprintf("%s failed: exit status %d", what, exitErr.ExitCode())
			default:
				m.flash = what + " failed: " + msg.err.Error()
			}
			return m, cmd
		}
		m.sessionRan() // clear what it left
		if next != "" {
			return m, tea.Batch(cmd, m.openPalette(next))
		}
		return m, cmd

	case healthResultMsg:
		h, idx := m.cfg.Find(msg.name)
		if msg.round != m.round || idx < 0 || hostKey(m.cfg, *h) != msg.key {
			return m, nil // asked before r, an edit or a newer round: not this host's answer now
		}
		m.checks[msg.name] = msg.res
		for i, it := range m.list.Items() {
			if iv, ok := it.(item); ok && iv.host.Name == msg.name {
				iv.status, iv.check = msg.res.State, msg.res
				return m, m.list.SetItem(i, iv)
			}
		}
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if m.now().Sub(m.roundAt) >= recheckEvery {
			cmds = append(cmds, m.newRound()...) // the old marks stay until the new come
		}
		return m, tea.Batch(cmds...)

	case tea.FocusMsg:
		if m.now().Sub(m.roundAt) >= focusRecheck {
			return m, tea.Batch(m.newRound()...)
		}
		return m, nil

	case tea.KeyMsg:
		m.flash, m.flashNeutral = "", false
		if m.tooSmall() {
			// Nothing on screen says what a key would act on, so only the
			// way out works, and a question waiting for a yes is a no.
			switch {
			case msg.String() == "ctrl+c", msg.String() == "q" && (m.mode == modeList || m.mode == modeHelp):
				return m, tea.Quit
			case m.mode == modeConfirmRun, m.mode == modeConfirmDelete, m.mode == modePick:
				m.mode, m.pending, m.pendingProd, m.target, m.afterExec = modeList, nil, nil, "", ""
				m.flash = "cancelled: the terminal is too small to show what it would do"
			}
			return m, nil
		}
		// A key typed straight after Esc can reach us in the same read as
		// it, which bubbletea takes for alt+key. Nothing in the list binds
		// alt, so it's taken apart again: Esc, then the key.
		if m.mode == modeList && msg.Alt && msg.Type == tea.KeyRunes {
			first, c1 := m.update(tea.KeyMsg{Type: tea.KeyEsc})
			plain := msg
			plain.Alt = false
			second, c2 := first.(model).update(plain)
			return second, tea.Batch(c1, c2)
		}
		switch m.mode {
		case modeHelp:
			switch msg.String() {
			case "j", "down":
				m.helpTop++
			case "k", "up":
				m.helpTop--
			case "pgdown", " ":
				m.helpTop += max(1, m.height-2)
			case "pgup":
				m.helpTop -= max(1, m.height-2)
			case "ctrl+d":
				m.helpTop += max(1, (m.height-2)/2)
			case "ctrl+u":
				m.helpTop -= max(1, (m.height-2)/2)
			case "g", "home":
				m.helpTop = 0
			case "G", "end":
				m.helpTop = len(m.helpLines())
			case "ctrl+c":
				return m, tea.Quit
			default:
				m.mode = modeList
			}
			m.helpTop = max(0, min(m.helpTop, len(m.helpLines())-(m.height-2)))
			return m, nil

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
				pick := m.picks[m.pickIdx]
				return m, m.run(pick.warn, pick.args...)
			case "esc", "q":
				m.mode = modeList
			}
			return m, nil

		case modeConfirmRun:
			// Only y goes ahead. Enter cancels like any other key: it's the
			// key people press without reading, and the one a double press
			// sends twice.
			if m.settling() {
				return m, nil
			}
			args := m.pending
			m.mode, m.pending, m.pendingProd = modeList, nil, nil
			switch msg.String() {
			case "y", "Y":
				return m, m.dispatchAsked(args...)
			}
			m.afterExec = ""
			m.flash = "cancelled: nothing was run (only y goes ahead)"
			return m, nil

		case modeConfirmDelete:
			if m.settling() {
				return m, nil
			}
			switch msg.String() {
			case "y", "Y":
				target := m.target
				m.mode, m.target = modeList, ""
				var removed config.Host
				found := false
				err := m.cfg.Update(func(c *config.Config) error {
					if h, idx := c.Find(target); idx >= 0 {
						removed, found = *h, true
					}
					c.Remove(target)
					return nil
				})
				if err != nil {
					m.flash = "couldn't delete " + target + ": " + reason(err)
					return m, nil
				}
				if found {
					if err := logRemoved(m.cfg, removed); err != nil {
						m.flash = "deleted " + termsafe.Label(target) + ", but the activity log wasn't updated: " + reason(err)
					}
				}
				cmd := m.list.SetItems(m.buildItems())
				m.retitle()
				m.mode, m.target = modeList, ""
				return m, cmd
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
				if v := m.palette.Value(); shellish(v, m.refs) && m.warned != v {
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
				return m, m.run(tunnelLeadsToProduction(args, m.cfg), args...)
			case tea.KeyTab:
				v, cands := complete(m.palette.Value(), m.cfg, m.refs)
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
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			if m.list.FilterState() == list.Filtering {
				break // let the filter input consume keys first
			}
			if k := msg.String(); k == "ctrl+d" || k == "ctrl+u" {
				// Half a page, as in the file manager; pgup and pgdn are a whole one.
				for n := 0; n < max(1, m.list.Height()/2); n++ {
					if k == "ctrl+d" {
						m.list.CursorDown()
					} else {
						m.list.CursorUp()
					}
				}
				return m, nil
			}
			if msg.String() == "esc" {
				// Esc clears, one thing at a time, and never quits: the
				// filter first (the list does that), then the marks.
				if m.list.FilterState() == list.FilterApplied {
					break
				}
				if len(m.marked) > 0 {
					m.marked = map[string]bool{}
					return m, m.refreshItems()
				}
				return m, nil
			}
			if key.Matches(msg, keys.Add) || key.Matches(msg, keys.Edit) || key.Matches(msg, keys.Delete) || key.Matches(msg, keys.Rename) {
				if why := m.blocked(); why != "" {
					m.flash = why
					return m, nil
				}
			}
			switch {
			case key.Matches(msg, keys.Quit):
				return m, tea.Quit
			case key.Matches(msg, keys.Help):
				m.mode, m.helpTop = modeHelp, 0
				return m, nil
			case key.Matches(msg, keys.Sort):
				m.sort = (m.sort + 1) % sortModes
				return m, m.refreshItems()
			case key.Matches(msg, keys.Rename):
				if name := m.selectedName(); name != "" {
					return m, m.openPalette("rename " + typedName(name, m.refs) + " ")
				}
				return m, nil
			case key.Matches(msg, keys.Mark):
				if name := m.selectedName(); name != "" {
					if m.marked[name] {
						delete(m.marked, name)
					} else {
						m.marked[name] = true
					}
					cmd := m.refreshItems()
					if m.pendingSelect != "" {
						m.pendingDown = true // once the filter's rows are back
					} else {
						m.list.CursorDown()
					}
					return m, cmd
				}
				return m, nil
			case key.Matches(msg, keys.Exec):
				return m, m.openPalette(execPrefill(m.markedNames(), m.selectedName(), m.refs))
			case key.Matches(msg, keys.Team):
				return m, m.dispatch("team", "status")
			case key.Matches(msg, keys.Audit), key.Matches(msg, keys.Routes), key.Matches(msg, keys.Access):
				if name := m.selectedName(); name != "" {
					return m, m.run(false, keyAction(msg.String(), name)...)
				}
			case key.Matches(msg, keys.Logs):
				if name := m.selectedName(); name != "" {
					// List what's there first, then hand back a palette ready
					// for the file to tail.
					m.afterExec = "logs " + name + " -f /var/log/"
					return m, m.run(false, keyAction("L", name)...)
				}
			case key.Matches(msg, keys.Tunnel):
				if it, ok := m.list.SelectedItem().(item); ok {
					picks := tunnelChoices(it.host, it.res.IsProduction())
					switch len(picks) {
					case 0:
						name := termsafe.Label(it.host.Name)
						m.flash = "no tunnels on " + name + " — add one with: tunnel add " + name + " <name> -L port:host:port --note \"...\""
					case 1:
						return m, m.run(picks[0].warn, picks[0].args...)
					default:
						m.mode, m.picks, m.pickIdx = modePick, picks, 0
						m.pickTitle = "Open a tunnel on " + termsafe.Label(it.host.Name)
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
				picks := loginChoices(it.host, flags, it.res.IsProduction())
				if len(picks) <= 1 {
					return m, m.run(false, append([]string{"connect", it.host.Name}, flags...)...)
				}
				m.mode, m.picks, m.pickIdx = modePick, picks, 0
				m.pickTitle = "Log in to " + termsafe.Label(it.host.Name) + " as"
				return m, nil
			case key.Matches(msg, keys.Add):
				return m, m.dispatch("add")
			case key.Matches(msg, keys.Edit):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("edit", name)
				}
			case key.Matches(msg, keys.Files):
				if name := m.selectedName(); name != "" {
					return m, m.run(false, "files", name)
				}
			case key.Matches(msg, keys.Stats):
				if name := m.selectedName(); name != "" {
					return m, m.run(false, "stats", name)
				}
			case key.Matches(msg, keys.CopyID):
				if name := m.selectedName(); name != "" {
					return m, m.run(false, "copy-id", name)
				}
			case key.Matches(msg, keys.Keygen):
				if name := m.selectedName(); name != "" {
					return m, m.dispatch("keygen", "--for", name)
				}
			case key.Matches(msg, keys.Delete):
				if name := m.selectedName(); name != "" {
					m.mode = modeConfirmDelete
					m.target = name
					m.askedAt = m.now()
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
	if _, ok := msg.(list.FilterMatchesMsg); ok && m.pendingSelect != "" {
		down := m.pendingDown
		m.reselect(m.pendingSelect)
		if down {
			m.list.CursorDown()
		}
	}
	return m, cmd
}

// bannerLines is the error about an unreadable host file, wrapped to the
// terminal (three lines at most); none when the file read fine.
func (m model) bannerLines() []string {
	if m.loadErr == nil {
		return nil
	}
	lines := cellwidth.Wrap(m.bannerText(), max(m.width, 1), 3)
	for k, l := range lines {
		lines[k] = downStyle.Bold(true).Render(l)
	}
	return lines
}

// bannerText is the whole of the banner; ? shows it when the screen can't.
// What to do comes first, since a narrow screen cuts the end.
func (m model) bannerText() string {
	return "⚠ meterm can't read its host file: fix it in an editor, then press r. Nothing is saved to it " +
		"meanwhile, and the list shows what was last read, if anything. " + termsafe.Label(loadErrText(m.loadErr, m.cfgPath))
}

// loadErrText puts what's wrong with the host file before its path: a long
// path pushed "yaml: line 3: …" past the banner's last line.
func loadErrText(err error, path string) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "parse config "+path+": "); ok {
		return rest + " (in " + path + ")"
	}
	return msg
}

// blocked says why a key that would change the host file is refused, or
// "" when it may go ahead.
func (m model) blocked() string {
	if m.loadErr == nil {
		return ""
	}
	return "the host file can't be read (see above): fix it, then press r"
}

func (m model) View() string {
	if m.width == 0 {
		return "" // no size yet
	}
	if m.tooSmall() {
		return cellwidth.Fit(fmt.Sprintf("too small: %d×%d, need %d×%d", m.width, m.height, minWidth, minHeight), m.width) +
			"\n" + faintStyle.Render(cellwidth.Fit("q quits", m.width))
	}
	return m.view()
}

func (m model) view() string {
	switch m.mode {
	case modeConfirmDelete:
		paras := []string{"Delete host " + nameStyle.Render(exactArg(m.target)) + "?"}
		if h, idx := m.cfg.Find(m.target); idx >= 0 {
			if m.cfg.IsProduction(*h) {
				paras = append([]string{downStyle.Bold(true).Render("⚠ PRODUCTION host")}, paras...)
			}
			if users := m.cfg.JumpedBy(h.Name); len(users) > 0 {
				paras = append(paras, noticeStyle.Render(JumpBrokenText(users)))
			}
			if h.Team != "" {
				paras = append(paras, noticeStyle.Render("It's shared with a team: `meterm team push` will remove it for everyone. "+
					"To drop it from this machine only, cancel and run `meterm team ignore "+termsafe.Label(h.Name)+"`."))
			}
		}
		paras = append(paras, helpStyle.Render("y = delete    any other key = cancel"))
		return m.modal(dangerEdge, 64, paras)
	case modeConfirmRun:
		what := "This leads to production."
		switch n := len(m.pendingProd); {
		case n == 1:
			what = "This runs on " + nameStyle.Render(exactArg(m.pendingProd[0])) + ", a production host."
		case n > 1:
			names := m.pendingProd
			more := ""
			if n > 5 {
				names, more = names[:5], fmt.Sprintf(" and %d more", n-5)
			}
			what = fmt.Sprintf("This runs on %d production hosts: %s%s.", n, exactLine(names), more)
		}
		return m.modal(dangerEdge, 64, []string{downStyle.Bold(true).Render("⚠ PRODUCTION"), what,
			dimStyle.Render("↵ meterm " + exactLine(m.pending)),
			helpStyle.Render("y = go    any other key = cancel")})
	case modePick:
		// As many choices as fit, the selected one always among them.
		w := min(64, m.width-2) - 2
		n := min(len(m.picks), max(1, m.height-4))
		first := max(0, min(m.pickIdx-n/2, len(m.picks)-n))
		var rows []string
		for i := first; i < first+n; i++ {
			c := m.picks[i]
			line := "  " + c.label
			if c.warn {
				line += "  " + downStyle.Render("⚠ PRODUCTION")
			}
			if i == m.pickIdx {
				line = selectedStyle.Render("›") + line[1:]
			}
			rows = append(rows, ansi.Truncate(line, w, "…"))
		}
		return m.modal(accentEdge, 64, []string{nameStyle.Render(m.pickTitle), strings.Join(rows, "\n"),
			helpStyle.Render("↑/↓ choose    enter = go    esc = cancel")})
	case modeHelp:
		return m.helpView()
	}
	lines := append(m.bannerLines(), m.frame()...)
	if m.mode == modePalette {
		inner := m.width - 4
		lines = append(lines, accentEdge.Render("╭"+strings.Repeat("─", inner+2)+"╮"),
			accentEdge.Render("│")+" "+fitCells(m.palette.View(), inner)+" "+accentEdge.Render("│"),
			accentEdge.Render("╰"+strings.Repeat("─", inner+2)+"╯"), m.paletteHint())
	} else {
		lines = append(lines, m.footerLines()...)
	}
	if m.notice != "" {
		lines = append(lines, noticeStyle.Render(cellwidth.Fit(" ⚠ "+termsafe.Label(m.notice), m.width)))
	}
	return strings.Join(lines, "\n")
}

// modal centres a box holding paras, as roomy as the terminal allows. A
// view taller than the terminal loses its top (bubbletea keeps the bottom),
// so when even a tight box doesn't fit, the first paragraph (what this is)
// and the last (the keys) stay whole and the ones between are cut. The box
// is drawn and placed by hand, measured in cells: lipgloss counts Thai a
// column short, and its padding pushed a line with ำ past the terminal's
// edge.
func (m model) modal(edge lipgloss.Style, maxW int, paras []string) string {
	boxW := min(maxW+2, m.width)
	for _, try := range []struct {
		v, h int
		sep  string
	}{{1, 3, "\n\n"}, {0, 1, "\n\n"}, {0, 1, "\n"}} {
		lines := wrapBlock(strings.Join(paras, try.sep), boxW-2-2*try.h)
		if len(lines)+2+2*try.v <= m.height {
			return m.place(edge, lines, try.v, try.h)
		}
	}
	textW := boxW - 4
	head, tail := wrapBlock(paras[0], textW), wrapBlock(paras[len(paras)-1], textW)
	budget := max(0, m.height-2-len(head)-len(tail))
	var mid []string
	if len(paras) > 2 {
		mid = wrapBlock(strings.Join(paras[1:len(paras)-1], "\n"), textW)
	}
	if len(mid) > budget {
		mid = mid[:budget]
		if budget > 0 {
			mid[budget-1] = fitCells(ansi.Truncate(mid[budget-1], textW-1, "")+"…", textW)
		}
	}
	return m.place(edge, append(append(head, mid...), tail...), 0, 1)
}

// wrapBlock wraps styled text to w cells a line, each line exactly w.
func wrapBlock(s string, w int) []string {
	var out []string
	for _, l := range strings.Split(lipgloss.NewStyle().Width(w).Render(s), "\n") {
		out = append(out, fitCells(l, w))
	}
	return out
}

// fitCells makes a styled line exactly w cells wide as the terminal counts
// them, trimming the padding lipgloss added by its own count first.
func fitCells(l string, w int) string {
	cw := cellwidth.Width(ansi.Strip(l))
	for cw > w && strings.HasSuffix(l, " ") {
		l, cw = l[:len(l)-1], cw-1
	}
	if cw > w {
		l = ansi.Truncate(l, max(0, w-1), "") + "…"
		cw = cellwidth.Width(ansi.Strip(l))
	}
	return l + strings.Repeat(" ", max(0, w-cw))
}

// place draws lines (each the same width) in a rounded box with v blank
// lines and h spaces of padding, centred on the screen.
func (m model) place(edge lipgloss.Style, lines []string, v, h int) string {
	inner := 2 * h
	if len(lines) > 0 {
		inner += cellwidth.Width(ansi.Strip(lines[0]))
	}
	side := edge.Render("│")
	blank := side + strings.Repeat(" ", inner) + side
	box := []string{edge.Render("╭" + strings.Repeat("─", inner) + "╮")}
	for k := 0; k < v; k++ {
		box = append(box, blank)
	}
	pad := strings.Repeat(" ", h)
	for _, l := range lines {
		box = append(box, side+pad+l+pad+side)
	}
	for k := 0; k < v; k++ {
		box = append(box, blank)
	}
	box = append(box, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	left := strings.Repeat(" ", max(0, (m.width-inner-2)/2))
	out := make([]string, max(0, (m.height-len(box))/2))
	for _, l := range box {
		out = append(out, left+l)
	}
	return strings.Join(out, "\n")
}

// paletteArgs is what Enter in the palette will run.
func (m model) paletteArgs() []string {
	value := strings.TrimSpace(m.palette.Value())
	if value == "" {
		return nil
	}
	return withSelectedHost(execKeepsLine(value, splitArgs(value, m.refs)), m.selectedName(), m.hostNames())
}

// paletteHint is the line under the palette: the completions after a tab,
// otherwise the exact command Enter will run, so a filled-in host is never
// a surprise.
func (m model) paletteHint() string {
	clamp := func(s string) string { return cellwidth.Fit(s, m.width) }
	if len(m.hint) > 0 {
		return faintStyle.Render(clamp(" " + termsafe.Label(strings.Join(m.hint, "  "))))
	}
	if v := m.palette.Value(); shellish(v, m.refs) {
		msg := " ⚠ not a shell: $(…), $VAR, \\ and ~/ go to the host as typed, not expanded here — for that, run it in a terminal"
		if m.warned == v {
			msg += " · enter again to run it anyway"
		}
		return noticeStyle.Render(clamp(msg))
	}
	args := m.paletteArgs()
	if len(args) == 0 {
		return faintStyle.Render(clamp(" tab completes commands and hosts"))
	}
	return faintStyle.Render(clamp(" ↵ meterm " + exactLine(args)))
}

// Run launches the picker over the inventory at cfgPath until the user
// quits. notice, if not empty, is shown as a reminder under the legend.
func Run(cfgPath, notice string) error {
	selfPath, err := os.Executable()
	if err != nil {
		return err
	}
	// Asked here, before Bubble Tea takes the terminal, and only here: the
	// other commands never ask (see package earlyterm).
	lipgloss.SetHasDarkBackground(darkBackground())
	m := New(cfgPath, selfPath)
	m.notice = notice
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
	final, err := p.Run()
	if fm, ok := final.(model); ok && fm.outcomeDir != "" {
		_ = os.RemoveAll(fm.outcomeDir)
	}
	return err
}
