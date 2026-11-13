package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"

	"meterm/internal/cellwidth"
	"meterm/internal/termsafe"
)

// The frame is drawn by hand, cell by cell, rather than by lipgloss: it
// measures text by graphemes and comes out a column short on Thai, which
// pushed the right border off its line. Every line here is exactly the
// terminal's width, whatever is on it, so the frame doesn't move as rows
// scroll past.

// styledBorder is a top or bottom border w cells wide with left and right
// set into it, ╭─left─────right─╮, styled once their widths are known.
// right goes first when it's tight.
func styledBorder(w int, open, close, left, right string, style func(left, right string) (string, string)) string {
	inner := w - 2
	lw, rw := cellwidth.Width(left), cellwidth.Width(right)
	if right != "" && 1+lw+2+rw+1 > inner {
		right, rw = "", 0
	}
	if left != "" && 1+lw+1 > inner {
		left = cellwidth.Fit(left, max(0, inner-2))
		lw = cellwidth.Width(left)
	}
	sl, sr := style(left, right)
	fill := func(n int) string { return faintStyle.Render(strings.Repeat("─", max(0, n))) }
	out := faintStyle.Render(open+"─") + sl
	used := 1 + lw
	if right != "" {
		out += fill(inner-used-rw-1) + sr + fill(1)
	} else {
		out += fill(inner - used)
	}
	return out + faintStyle.Render(close)
}

// frame is the box around the table: the title in its top border, the
// column header, the rows, the selected host's detail line, and the filter
// and position in its bottom border.
func (m model) frame() []string {
	w := m.width
	inner := w - 4
	side := faintStyle.Render("│")
	boxed := func(s string) string { return side + " " + s + " " + side }

	n := len(m.list.Items())
	shown := len(m.list.VisibleItems())
	title := fmt.Sprintf(" meterm %s ", displayVersion(Version))
	count := fmt.Sprintf("· %d %s ", n, plural(n, "host", "hosts"))
	if m.list.FilterState() != list.Unfiltered {
		count = fmt.Sprintf("· %d of %d %s ", shown, n, plural(n, "host", "hosts"))
	}
	sortText := " sort: " + m.sort.String() + " (s) "
	top := styledBorder(w, "╭", "╮", title+count, sortText, func(l, r string) (string, string) {
		if strings.HasPrefix(l, title) {
			return titleStyle.Padding(0).Render(title) + dimStyle.Render(strings.TrimPrefix(l, title)), dimStyle.Render(r)
		}
		return titleStyle.Padding(0).Render(l), dimStyle.Render(r)
	})

	lay := layoutFor(m.list.Items(), inner)
	out := []string{top, boxed(lay.header(m.sort))}

	rows := m.list.Height()
	visible := m.list.VisibleItems()
	start, end := m.list.Paginator.GetSliceBounds(len(visible))
	filtering := m.list.FilterState() != list.Unfiltered
	var body []string
	for idx := start; idx < end; idx++ {
		it, ok := visible[idx].(item)
		if !ok {
			continue
		}
		it.now = m.now()
		var matches []int
		if filtering {
			matches = m.list.MatchesForItem(idx)
		}
		body = append(body, it.row(lay, matches, idx == m.list.Index() && m.list.FilterState() != list.Filtering))
	}
	if len(body) == 0 {
		msg := "no hosts yet: press a to add one, or : and type import"
		switch {
		case m.loadErr != nil && n == 0:
			msg = "nothing read from the host file (see above)"
		case filtering:
			msg = "no host matches " + quote(m.list.FilterValue())
		}
		body = append(body, dimStyle.Render(cellwidth.FitPad(msg, inner)))
	}
	for len(body) < rows {
		body = append(body, strings.Repeat(" ", inner))
	}
	for _, r := range body[:rows] {
		out = append(out, boxed(r))
	}

	detail := ""
	if it, ok := m.list.SelectedItem().(item); ok {
		it.now = m.now()
		detail = it.detail(lay) // never empty: it starts with the health answer
	}
	out = append(out, boxed(dimStyle.Render(cellwidth.FitPad(detail, inner))))

	left, right := m.filterText(), ""
	if shown > 0 {
		right = fmt.Sprintf(" %d/%d ", m.list.Index()+1, shown)
	}
	out = append(out, styledBorder(w, "╰", "╯", left, right, func(l, r string) (string, string) {
		return selectedStyle.Render(l), dimStyle.Render(r)
	}))
	return out
}

// filterText is the filter as the bottom border shows it: being typed,
// with a cursor; applied, with how to clear it; nothing when there's none.
func (m model) filterText() string {
	in := m.list.FilterInput
	v := termsafe.Label(in.Value())
	switch m.list.FilterState() {
	case list.Filtering:
		pos := min(in.Position(), len([]rune(v)))
		r := []rune(v)
		return " /" + string(r[:pos]) + "▏" + string(r[pos:]) + " "
	case list.FilterApplied:
		return " /" + v + " · esc clears "
	}
	return ""
}

func quote(s string) string { return "“" + termsafe.Label(s) + "”" }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// footerLines is the line under the frame: a message when there is one,
// wrapped to two lines at most (the whole of it stays in ? until the next),
// otherwise the keys that fit, always ending with where the rest are.
func (m model) footerLines() []string {
	if m.flash != "" {
		lines := cellwidth.Wrap(" "+termsafe.Label(m.flash), max(1, m.width), 2)
		st := noticeStyle
		if m.flashNeutral {
			st = dimStyle
		}
		for k, l := range lines {
			lines[k] = st.Render(l)
		}
		return lines
	}
	var hints []string
	tail := "? help  q quit"
	switch {
	case m.list.FilterState() == list.Filtering:
		// Every letter, ? and q included, goes into the filter.
		hints = []string{"type to filter", "enter keep filter", "esc cancel"}
		tail = "ctrl+c quit"
	case m.list.FilterState() == list.FilterApplied:
		hints = []string{"enter connect", "esc clear filter", "space mark", "x exec"}
	case len(m.marked) > 0:
		hints = []string{fmt.Sprintf("x exec on %d marked", len(m.marked)), "esc clear marks", "space mark"}
	default:
		hints = []string{"enter connect", "/ filter", "space mark", "x exec", "f files", "s sort", ": command"}
	}
	line := " "
	for _, h := range hints {
		if cellwidth.Width(line+h+"  "+tail) > m.width {
			break
		}
		line += h + "  "
	}
	return []string{faintStyle.Render(cellwidth.Fit(line+tail, m.width))}
}

// keysChanged is the top of the ? screen: keys that moved in v0.24, so a
// hand that remembers the old ones finds out here. Its title names the
// version: the build's own version says nothing about when keys moved.
var keysChanged = []string{
	"Keys changed in v0.24 (from v0.23)",
	"  h l b u ← →\tno longer turn the page: pgup pgdn do",
	"  ctrl+u ctrl+d\thalf a page (new)",
	"  M\tconnect in tmux (was m)",
	"  C\troute check (was R)",
	"  R\trename (new)",
	"  esc\tclears the filter, then the marks; it no longer quits (q does)",
	"  y\tanswers a production or delete question only once it has been up 150ms",
	"",
}

// pickerHelp is the ? screen: every key, what the marks mean. A line is
// text, or a key line, "  key\twhat it does", which the ? screen lays out
// in two columns, wrapping the description under itself.
var pickerHelp = []string{
	"Keys",
	"  enter\tconnect (asks which login when the host has several)",
	"  M\tconnect in tmux on the host (a dropped line loses nothing)",
	"  j k ↑ ↓\tmove",
	"  g G home end\ttop, bottom",
	"  pgup pgdn\ta page",
	"  ctrl+u ctrl+d\thalf a page",
	"  /\tfilter: name, group, tags, env, notes, user, team; the address only as typed (/192.0.2.1)",
	"  esc\tclear the filter, then the marks; never quits",
	"  space\tmark (for x)",
	"  x\texec on the marked hosts",
	"  s\tsort: recent, name, group, env",
	"  a\tadd a host",
	"  e\tedit",
	"  R\trename",
	"  d\tdelete (asks first)",
	"  f\tfiles",
	"  t\topen a tunnel",
	"  S\tstats",
	"  L\tlogs",
	"  A\taudit",
	"  C\troute check",
	"  w\twho has access",
	"  T\tteam status",
	"  i\tinstall key",
	"  K\tmake a key for the host",
	"  r\trefresh: re-read the host file, check every host again",
	"  :\tcommand palette (any meterm command; tab completes)",
	"  ?\tthis help",
	"  q ctrl+c\tquit",
	"",
	"Marks",
	"  ● up   ◐ its jump host up, the host itself not checked   ✕ down   ? checking, or unsure (port open, no SSH greeting)   ◌ not checked",
	"  checked by the saved routes connect tries: each route's first hop, the jump host if there is one, dialled directly (not through your ssh_config: a Host alias or ProxyCommand there isn't followed); again every 5 minutes, and when you come back; a dim mark is older than that",
	"  the line under the table says how and when: up via vpn, 2m ago · via bastion: down: connection refused, 30s ago · via bastion: bastion up, the host itself not checked",
	"  ✓ marked   ⚑prod ⚑staging   ⚷ key installed   ⇄2 other routes",
	"  ⇶1 tunnels (red: one reaches production)   ⧉ shared with a team",
	"  ↑ team change not pushed   ‼ team conflict (see T)",
	"",
	"Anything that runs on a production host asks first, and so does a delete: only y answers yes, and only once the question has been up 150ms; a key typed sooner is ignored and the question stays.",
}

// helpKeyCol is where a key line's description starts.
const helpKeyCol = 16

// wrapHelp wraps one help line to w cells. A key line keeps its key on the
// left and wraps its description under itself; when that would leave the
// description too little room, it goes on the lines below the key.
func wrapHelp(l string, w int) []string {
	if key, desc, ok := strings.Cut(l, "\t"); ok {
		key = cellwidth.FitPad(key, helpKeyCol-1) + " "
		if cellwidth.Width(key)+cellwidth.Width(desc) <= w {
			return []string{key + desc}
		}
		if w-helpKeyCol >= 16 {
			var out []string
			for k, d := range cellwidth.Wrap(desc, w-helpKeyCol, 100) {
				if k == 0 {
					out = append(out, key+d)
				} else {
					out = append(out, strings.Repeat(" ", helpKeyCol)+d)
				}
			}
			return out
		}
		out := []string{strings.TrimRight(key, " ")}
		for _, d := range cellwidth.Wrap(desc, max(1, w-6), 100) {
			out = append(out, "      "+d)
		}
		return out
	}
	if cellwidth.Width(l) <= w {
		return []string{l}
	}
	indent := len(l) - len(strings.TrimLeft(l, " "))
	var out []string
	for _, d := range cellwidth.Wrap(strings.TrimSpace(l), max(1, w-indent), 100) {
		out = append(out, strings.Repeat(" ", indent)+d)
	}
	return out
}

func (m model) helpLines() []string {
	w := max(1, m.width-4)
	var lines []string
	for _, l := range append(append([]string(nil), keysChanged...), pickerHelp...) {
		lines = append(lines, wrapHelp(l, w)...)
	}
	if m.loadErr != nil {
		lines = append(lines, "", "Host file")
		for _, l := range cellwidth.Wrap(m.bannerText(), max(1, m.width-6), 20) {
			lines = append(lines, "  "+l)
		}
	}
	if m.lastFlash != "" {
		lines = append(lines, "", "Last message")
		for _, l := range cellwidth.Wrap(termsafe.Label(m.lastFlash), max(1, m.width-6), 20) {
			lines = append(lines, "  "+l)
		}
	}
	return lines
}

// helpView draws the ? screen in the same frame, scrolled to helpTop.
func (m model) helpView() string {
	w := m.width
	inner := w - 4
	rows := m.height - 2
	all := m.helpLines()
	top := min(m.helpTop, max(0, len(all)-rows))
	right := ""
	if len(all) > rows {
		right = fmt.Sprintf(" %d-%d of %d · j/k scroll ", top+1, min(top+rows, len(all)), len(all))
	}
	out := []string{styledBorder(w, "╭", "╮", " meterm keys ", right, func(l, r string) (string, string) {
		return titleStyle.Padding(0).Render(l), dimStyle.Render(r)
	})}
	side := faintStyle.Render("│")
	for k := 0; k < rows; k++ {
		l := ""
		if top+k < len(all) {
			l = all[top+k]
		}
		out = append(out, side+" "+cellwidth.FitPad(l, inner)+" "+side)
	}
	out = append(out, styledBorder(w, "╰", "╯", " any other key closes ", "", func(l, r string) (string, string) {
		return dimStyle.Render(l), r
	}))
	return strings.Join(out, "\n")
}
