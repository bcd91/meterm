package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"

	"meterm/internal/cellwidth"
	"meterm/internal/config"
	"meterm/internal/termsafe"
)

// The picker is a table: one line per host, columns aligned by the cells a
// terminal gives them, and the columns that don't fit dropped from the
// lowest priority up, so a row never wraps.

// sortMode is the order of the rows; s cycles through them.
type sortMode int

const (
	sortRecent sortMode = iota // last connected first; never-connected after, by name
	sortName
	sortGroup // by group, ungrouped last, then by name
	sortEnv   // production, then staging, then the rest, then by name
	sortModes
)

func (s sortMode) String() string {
	return [...]string{"recent", "name", "group", "env"}[s]
}

// Column ids, in priority order after the name: the first to go when the
// terminal is narrow is the last here.
const (
	colEnv = iota
	colAddr
	colGroup
	colTags
	colLast
	numCols
)

var colTitle = [numCols]string{"ENV", "ADDRESS", "GROUP", "TAGS", "LAST"}

// colCap is the most a column takes however long its longest value; past
// it, values are cut with "…". colMin is the least the address is cut to
// before it's dropped instead.
var (
	colCap = [numCols]int{10, 34, 16, 24, 9}
	colMin = [numCols]int{8, 14, 5, 5, 8}
)

const (
	nameCap  = 28
	nameKeep = 20
	// rowPrefix is the cursor, the mark and the status, then a space.
	rowPrefix = 4
	colGap    = 2
)

// layout is the width of each column shown; 0 means it's dropped.
type layout struct {
	inner int // the row's width
	name  int
	cols  [numCols]int
}

// envLabel is the env badge, "" when the host has none. An env meterm
// doesn't know counts as production, flagged with a "?".
func (i item) envLabel() string {
	switch {
	case i.res.IsProduction() && i.envRaw != "":
		return "⚑prod?"
	case i.res.IsProduction():
		return "⚑prod"
	case i.res.Env == config.EnvStaging:
		return "⚑staging"
	}
	return i.res.Env // dev, test, or "" for none
}

// envNote is what the detail line says about the env when the badge can't:
// that there is none, or that it's a value meterm doesn't know.
func (i item) envNote() string {
	switch {
	case i.envRaw != "":
		return "env " + quoteASCII(termsafe.Label(i.envRaw)) + " not recognised: treated as production"
	case i.res.Env == "":
		return "env not set"
	}
	return ""
}

func quoteASCII(s string) string { return `"` + s + `"` }

// flags are the compact markers after the address: key installed, other
// routes, tunnels (red when one reaches production), team, and what team
// sync is waiting on. Each part is plain text and its style.
func (i item) flags() []styled {
	var out []styled
	if i.keyInstalled {
		out = append(out, styled{"⚷", keyStyle})
	}
	if n := len(i.host.Routes); n > 0 {
		out = append(out, styled{fmt.Sprintf("⇄%d", n), dimStyle})
	}
	tunnels, prod := 0, i.res.IsProduction()
	for _, f := range i.host.Forwards {
		if f.Name != "" {
			tunnels++
			prod = prod || f.MentionsProduction()
		}
	}
	if tunnels > 0 {
		st := dimStyle
		if prod {
			st = downStyle
		}
		out = append(out, styled{fmt.Sprintf("⇶%d", tunnels), st})
	}
	if i.team != "" {
		out = append(out, styled{"⧉", teamStyle})
	}
	switch {
	case i.attention == "":
	case i.conflict():
		out = append(out, styled{"‼", downStyle})
	default:
		out = append(out, styled{"↑", noticeStyle})
	}
	return out
}

// conflict says team sync is stuck on this host rather than just behind.
func (i item) conflict() bool {
	a := i.attention
	return strings.Contains(a, "conflict") || strings.Contains(a, "taken") || strings.Contains(a, "can't")
}

type styled struct {
	text  string
	style lipgloss.Style
}

func flagsText(fs []styled) string {
	parts := make([]string, len(fs))
	for k, f := range fs {
		parts[k] = f.text
	}
	return strings.Join(parts, " ")
}

func (i item) tagsText() string {
	tags := i.tags()
	for k, t := range tags {
		tags[k] = "#" + t
	}
	return strings.Join(tags, " ")
}

// natural is how wide column c would like to be for this item.
func (i item) natural(c int) int {
	switch c {
	case colEnv:
		return cellwidth.Width(i.envLabel())
	case colAddr:
		w := cellwidth.Width(termsafe.Label(i.target()))
		if f := flagsText(i.flags()); f != "" {
			w += 1 + cellwidth.Width(f)
		}
		return w
	case colGroup:
		return cellwidth.Width(termsafe.Label(i.host.Group))
	case colTags:
		return cellwidth.Width(i.tagsText())
	case colLast:
		return cellwidth.Width(i.last)
	}
	return 0
}

// layoutFor fits the columns into inner cells. Widths come from every
// host, not just the ones on screen or matching a filter, so the columns
// don't move while scrolling or typing a filter.
func layoutFor(items []list.Item, inner int) layout {
	l := layout{inner: inner}
	var natural [numCols]int
	name := 0
	for _, li := range items {
		it, ok := li.(item)
		if !ok {
			continue
		}
		name = max(name, cellwidth.Width(shownName(it.host.Name)))
		for c := range natural {
			natural[c] = max(natural[c], it.natural(c))
		}
	}
	name = min(max(name, len("NAME")), nameCap)
	avail := inner - rowPrefix
	if avail < 1 {
		return l
	}
	// The name keeps up to nameKeep cells whatever else is shown; the
	// other columns come in at their full width in priority order while
	// they fit (the address may be cut to fit, nothing else is), and the
	// first that doesn't takes every lower one with it. What's left then
	// lets a long name grow.
	l.name = min(name, nameKeep, avail)
	used := l.name
	for c := range natural {
		if natural[c] == 0 {
			continue // nobody has one: no empty column
		}
		nat := min(max(natural[c], cellwidth.Width(colTitle[c])), colCap[c])
		w := nat
		if used+colGap+w > avail {
			if c != colAddr || used+colGap+colMin[c] > avail {
				break
			}
			w = avail - used - colGap
		}
		used += colGap + w
		l.cols[c] = w
		if w < nat {
			break
		}
	}
	l.name += min(avail-used, name-l.name)
	return l
}

// header is the column titles, the sorted column marked ▾.
func (l layout) header(s sortMode) string {
	sorted := map[sortMode]int{sortRecent: colLast, sortGroup: colGroup, sortEnv: colEnv}
	title := func(t string, mark bool) string {
		if mark {
			return t + "▾"
		}
		return t
	}
	line := strings.Repeat(" ", rowPrefix) + cellwidth.FitPad(title("NAME", s == sortName), l.name)
	for c := range l.cols {
		if l.cols[c] == 0 {
			continue
		}
		c2, ok := sorted[s]
		line += strings.Repeat(" ", colGap) + cellwidth.FitPad(title(colTitle[c], ok && c2 == c), l.cols[c])
	}
	return faintStyle.Bold(true).Render(cellwidth.FitPad(line, l.inner))
}

// row draws it as one line exactly l.inner cells wide, with the letters the
// filter matched underlined.
func (i item) row(l layout, matches []int, selected bool) string {
	var b strings.Builder
	used := 0
	put := func(plainW int, s string) {
		b.WriteString(s)
		used += plainW
	}
	if selected {
		put(1, selectedStyle.Render("›"))
	} else {
		put(1, " ")
	}
	if i.marked {
		put(1, selectedStyle.Render("✓"))
	} else {
		put(1, " ")
	}
	put(1, i.statusMark())
	put(1, " ")
	ns := nameStyle
	if selected {
		ns = selectedStyle
	}
	fields := i.filterFields()
	put(l.name, markFit(fields[fieldName], fieldStart(fields, fieldName), matches, ns, l.name))
	for c, w := range l.cols {
		if w == 0 {
			continue
		}
		put(colGap, "  ")
		switch c {
		case colEnv:
			st := noticeStyle
			switch {
			case i.res.IsProduction():
				st = downStyle
			case i.res.Env != config.EnvStaging:
				st = dimStyle // dev, test
			}
			put(w, st.Render(cellwidth.FitPad(i.envLabel(), w)))
		case colAddr:
			put(w, i.addrCell(w, matches))
		case colGroup:
			put(w, markFit(fields[fieldGroup], fieldStart(fields, fieldGroup), matches, groupStyle, w))
		case colTags:
			put(w, markFit(fields[fieldTags], fieldStart(fields, fieldTags), matches, tagStyle, w))
		case colLast:
			put(w, faintStyle.Render(cellwidth.FitPad(i.last, w)))
		}
	}
	if used < l.inner {
		b.WriteString(strings.Repeat(" ", l.inner-used))
	}
	return b.String()
}

// statusMark is the health check's answer; each has its own shape, so it
// reads without colour too.
func (i item) statusMark() string {
	up, down := upStyle, downStyle
	if i.staleAt(i.now) {
		up, down = dimStyle, dimStyle // older than a round: may no longer be true
	}
	switch i.status {
	case healthUp:
		if i.check.Via != "" {
			// Only the jump host answered: past it nothing is checked
			// without logging in, so the host itself may be down.
			return up.Render("◐")
		}
		return up.Render("●")
	case healthDown:
		return down.Render("✕")
	case healthSkipped:
		return dimStyle.Render("◌") // not checked: connect wouldn't try it
	}
	return dimStyle.Render("?") // not checked yet, or unsure: no SSH greeting

}

// staleAt says the answer is older than a round of checks at now.
func (i item) staleAt(now time.Time) bool {
	return !i.check.At.IsZero() && !now.IsZero() && now.Sub(i.check.At) > recheckEvery
}

// healthText is the health answer for the detail line: how, why not, when.
func (i item) healthText() string {
	ago := ""
	if !i.check.At.IsZero() && !i.now.IsZero() {
		ago = ", " + agoText(i.now.Sub(i.check.At))
	}
	switch i.status {
	case healthUp, healthDown, healthUnsure:
		state := "up"
		switch i.status {
		case healthDown:
			why := i.check.Reason
			if why == "" {
				why = "no answer"
			}
			state = "down: " + termsafe.Label(why)
		case healthUnsure:
			state = "unsure: " + termsafe.Label(i.check.Reason)
		}
		// What answered, or didn't, is the first hop: past a jump host
		// nothing is checked, so the answer is the jump host's.
		switch rt, via := termsafe.Label(i.check.Route), termsafe.Label(i.check.Via); {
		case via != "" && i.status == healthUp && rt != "":
			return "via " + via + " (route " + rt + "): " + via + " up, the host itself not checked" + ago
		case via != "" && i.status == healthUp:
			return "via " + via + ": " + via + " up, the host itself not checked" + ago
		case via != "" && rt != "":
			return "via " + via + " (route " + rt + "): " + state + ago
		case via != "":
			return "via " + via + ": " + state + ago
		case rt != "" && i.status == healthUp:
			return "up via " + rt + ago
		case rt != "":
			return "route " + rt + ": " + state + ago
		}
		return state + ago
	case healthSkipped:
		if i.check.Reason != "" {
			return "not checked: " + termsafe.Label(i.check.Reason)
		}
		return "not checked"
	}
	return "checking…"
}

// agoText is a short age: 30s ago, 2m ago, 3h ago.
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(max(0, d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// addrCell is user@address and the flags after it, w cells wide. The
// flags give way first when it's tight.
func (i item) addrCell(w int, matches []int) string {
	fields := i.filterFields()
	target := fields[fieldAddr]
	start := fieldStart(fields, fieldAddr)
	fs := i.flags()
	fw := cellwidth.Width(flagsText(fs))
	if fw == 0 || cellwidth.Width(target)+1+fw > w && w-1-fw < 12 {
		return markFit(target, start, matches, dimStyle, w)
	}
	tw := min(cellwidth.Width(target), w-1-fw)
	out := markFit(target, start, matches, dimStyle, tw)
	for _, f := range fs {
		out += " " + f.style.Render(f.text)
	}
	return out + strings.Repeat(" ", max(0, w-tw-1-fw))
}

// markFit is mark for a field cut to w cells and padded to exactly w.
func markFit(text string, start int, matches []int, style lipgloss.Style, w int) string {
	fitted := cellwidth.Fit(text, w)
	pad := strings.Repeat(" ", max(0, w-cellwidth.Width(fitted)))
	if fitted == text {
		return mark(text, start, matches, style) + pad
	}
	if w <= 0 {
		return ""
	}
	kept := strings.TrimSuffix(fitted, "…")
	return mark(kept, start, matches, style) + style.Render("…") + pad
}

// detail is the line under the table about the selected host: whatever the
// row has no room for, then its jump hosts, logins, team and notes.
func (i item) detail(l layout) string {
	var parts []string
	if q := exactArg(i.host.Name); q != i.host.Name {
		// Another host may have the name it looks like.
		parts = append(parts, "name has hidden characters: "+q)
	}
	if m := config.MixedScripts(i.host.Name); m != "" {
		parts = append(parts, "name mixes "+m+" letters: it can look like another host's")
	}
	if l.cols[colAddr] == 0 {
		parts = append(parts, termsafe.Label(i.target()))
	}
	parts = append(parts, i.healthText())
	if l.cols[colEnv] == 0 && i.envLabel() != "" {
		parts = append(parts, i.envLabel())
	}
	if n := i.envNote(); n != "" {
		parts = append(parts, n)
	}
	switch {
	case i.groupMissing:
		// Shown even when the column is: the column can't say it's gone.
		parts = append(parts, "group "+termsafe.Label(i.host.Group)+" doesn't exist: nothing inherited")
	case l.cols[colGroup] == 0 && i.host.Group != "":
		parts = append(parts, "["+termsafe.Label(i.host.Group)+"]")
	}
	if jump := jumpText(i.res); jump != "" {
		parts = append(parts, termsafe.Label(jump))
	}
	if len(i.host.Users) > 0 {
		parts = append(parts, "users: "+termsafe.Label(strings.Join(i.host.Logins(), ", ")))
	}
	if i.team != "" {
		t := "team " + termsafe.Label(i.team)
		if i.attention != "" {
			t += " (" + termsafe.Label(i.attention) + ")"
		}
		parts = append(parts, t)
	}
	if l.cols[colTags] == 0 && len(i.host.Tags) > 0 {
		parts = append(parts, i.tagsText())
	}
	if l.cols[colLast] == 0 && i.last != "" {
		parts = append(parts, "last connected "+i.last) // not the check's age
	}
	if i.host.Notes != "" {
		parts = append(parts, termsafe.Label(i.host.Notes))
	}
	return strings.Join(parts, "  ·  ")
}

// hostFilter is the picker's filter. Each searchable field is matched
// fuzzily on its own, so letters spread across a name, a group and a note
// don't add up to a match; the address matches only as typed (a plain
// substring), so "web1" doesn't pick up every 192.0.2.1x. Fuzzy matches
// come first, best first; address-only matches after them.
func hostFilter(term string, targets []string) []list.Rank {
	type hit struct {
		rank  list.Rank
		score int
		fuzzy bool
	}
	var hits []hit
	low := asciiLower(term)
	for idx, t := range targets {
		fields := strings.Split(t, fieldSep)
		at := 0
		var best *hit
		for f, text := range fields {
			if f == fieldAddr {
				if k := strings.Index(asciiLower(text), low); k >= 0 && best == nil && term != "" {
					ms := make([]int, len(term))
					for n := range ms {
						ms[n] = at + k + n
					}
					best = &hit{rank: list.Rank{Index: idx, MatchedIndexes: ms}}
				}
			} else if found := fuzzy.Find(term, []string{text}); len(found) == 1 {
				if best == nil || !best.fuzzy || found[0].Score > best.score {
					ms := make([]int, len(found[0].MatchedIndexes))
					for n, m := range found[0].MatchedIndexes {
						ms[n] = at + m
					}
					best = &hit{rank: list.Rank{Index: idx, MatchedIndexes: ms}, score: found[0].Score, fuzzy: true}
				}
			}
			at += len(text) + len(fieldSep)
		}
		if term == "" {
			best = &hit{rank: list.Rank{Index: idx}, fuzzy: true}
		}
		if best != nil {
			hits = append(hits, *best)
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].fuzzy != hits[b].fuzzy {
			return hits[a].fuzzy
		}
		return hits[a].score > hits[b].score
	})
	out := make([]list.Rank, len(hits))
	for k, h := range hits {
		out[k] = h.rank
	}
	return out
}

// asciiLower lowers A-Z only, so byte offsets stay put.
func asciiLower(s string) string {
	b := []byte(s)
	for k, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[k] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// sortItems puts items in the order s asks for. They arrive in recency
// order, which every other order keeps among equals.
func sortItems(items []item, s sortMode) {
	byName := func(a, b item) bool { return strings.ToLower(a.host.Name) < strings.ToLower(b.host.Name) }
	envRank := func(i item) int {
		switch {
		case i.res.IsProduction():
			return 0
		case i.res.Env == config.EnvStaging:
			return 1
		}
		return 2
	}
	switch s {
	case sortName:
		sort.SliceStable(items, func(a, b int) bool { return byName(items[a], items[b]) })
	case sortGroup:
		sort.SliceStable(items, func(a, b int) bool {
			ga, gb := items[a].host.Group, items[b].host.Group
			if (ga == "") != (gb == "") {
				return gb == ""
			}
			if ga != gb {
				return strings.ToLower(ga) < strings.ToLower(gb)
			}
			return byName(items[a], items[b])
		})
	case sortEnv:
		sort.SliceStable(items, func(a, b int) bool {
			if ra, rb := envRank(items[a]), envRank(items[b]); ra != rb {
				return ra < rb
			}
			return byName(items[a], items[b])
		})
	}
}
