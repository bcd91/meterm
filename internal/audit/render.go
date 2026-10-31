package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// HostReport is one host's audit: what it was told about the host, what
// was found, and (if the host couldn't be reached) why not.
type HostReport struct {
	Host     string       `json:"host"`
	Exposure string       `json:"exposure"`
	OS       string       `json:"os,omitempty"`
	Probed   map[int]bool `json:"probed,omitempty"` // port -> reachable from the auditing machine
	Results  []Result     `json:"results,omitempty"`
	Notes    []string     `json:"notes,omitempty"`
	Error    string       `json:"error,omitempty"`
	HaveSudo bool         `json:"have_sudo"`
}

// Report is the audit of several hosts.
type Report struct {
	Generated time.Time      `json:"generated"`
	Hosts     []HostReport   `json:"hosts"`
	Access    *AccessSummary `json:"access,omitempty"`
}

// AccessSummary is who worked on the reported hosts recently, from the team
// activity log — what an auditor asks right after "is it patched?".
type AccessSummary struct {
	Days int         `json:"days"`
	Rows []AccessRow `json:"rows"`
	// Note explains a missing or partial log (not signed in, hosts not
	// shared with a team, a fetch error).
	Note string `json:"note,omitempty"`
}

// AccessRow is one person's activity on one host.
type AccessRow struct {
	Host    string    `json:"host"`
	Who     string    `json:"who"`
	Actions int       `json:"actions"`
	Kinds   string    `json:"kinds"` // e.g. "connect ×12, exec ×3"
	Last    time.Time `json:"last"`
}

// Counts tallies a host's results by status.
func (h HostReport) Counts() (fails, unknowns, passes int) {
	for _, r := range h.Results {
		switch r.Status {
		case Fail:
			fails++
		case Unknown:
			unknowns++
		case Pass:
			passes++
		}
	}
	return
}

// WriteJSON renders the report as indented JSON, for feeding other tools.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func renderAccessText(b *strings.Builder, a *AccessSummary) {
	fmt.Fprintf(b, "\nWho accessed these hosts (last %d days, via meterm)\n", a.Days)
	if a.Note != "" {
		fmt.Fprintf(b, "  %s\n", a.Note)
	}
	if len(a.Rows) == 0 && a.Note == "" {
		b.WriteString("  No recorded activity.\n")
	}
	for _, r := range a.Rows {
		fmt.Fprintf(b, "  %-16s %-32s %4d  %-28s last %s\n", r.Host, r.Who, r.Actions, r.Kinds, r.Last.Local().Format("2006-01-02 15:04"))
	}
}

// RenderText renders the report for a terminal: one block per host, then the
// failures regrouped by the role who has to act on them.
func RenderText(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Security baseline audit — %s\n", r.Generated.Format("2006-01-02 15:04"))
	b.WriteString("Read-only: nothing was changed on any host.\n")

	for _, h := range r.Hosts {
		b.WriteString("\n")
		renderHost(&b, h)
	}
	renderHandoff(&b, r)
	if r.Access != nil {
		renderAccessText(&b, r.Access)
	}
	return b.String()
}

func renderHost(b *strings.Builder, h HostReport) {
	fmt.Fprintf(b, "== %s  [%s]", h.Host, h.Exposure)
	if h.OS != "" {
		fmt.Fprintf(b, "  %s", h.OS)
	}
	b.WriteString("\n")
	if h.Error != "" {
		fmt.Fprintf(b, "   could not audit: %s\n", h.Error)
		return
	}
	fails, unknowns, passes := h.Counts()
	fmt.Fprintf(b, "   %d to fix, %d could not be checked, %d ok\n", fails, unknowns, passes)
	if !h.HaveSudo {
		b.WriteString("   (no passwordless sudo here: checks that need root are reported as unknown, not as passing)\n")
	}
	for _, n := range h.Notes {
		fmt.Fprintf(b, "   note: %s\n", n)
	}
	if len(h.Probed) > 0 {
		var open, closed []string
		ports := make([]int, 0, len(h.Probed))
		for p := range h.Probed {
			ports = append(ports, p)
		}
		sort.Ints(ports)
		for _, p := range ports {
			if h.Probed[p] {
				open = append(open, fmt.Sprint(p))
			} else {
				closed = append(closed, fmt.Sprint(p))
			}
		}
		fmt.Fprintf(b, "   reachable from the auditing machine: %s; not reachable: %s\n", listOrNone(open), listOrNone(closed))
	}
	for _, r := range h.Results {
		switch r.Status {
		case Fail:
			fmt.Fprintf(b, "   ✗ [%s] %s\n", strings.ToUpper(r.Severity.String()), r.Title)
			fmt.Fprintf(b, "       %s\n", r.Detail)
		case Unknown:
			fmt.Fprintf(b, "   ? %s\n       %s\n", r.Title, r.Detail)
		case Pass:
			fmt.Fprintf(b, "   ✓ %s — %s\n", r.Title, r.Detail)
		}
	}
}

type action struct {
	host string
	Result
}

// renderHandoff lists every failure by the role who can act on it, so the
// report can be forwarded as-is to the right people.
func renderHandoff(b *strings.Builder, r Report) {
	byOwner := map[Owner][]action{}
	for _, h := range r.Hosts {
		for _, res := range h.Results {
			if res.Status == Fail {
				byOwner[res.Owner] = append(byOwner[res.Owner], action{h.Host, res})
			}
		}
	}
	if len(byOwner) == 0 {
		b.WriteString("\nNothing to hand off.\n")
		return
	}
	owners := make([]Owner, 0, len(byOwner))
	for o := range byOwner {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	b.WriteString("\n== ACTIONS BY OWNER ==\n")
	for _, o := range owners {
		acts := byOwner[o]
		sort.SliceStable(acts, func(i, j int) bool {
			if acts[i].Severity != acts[j].Severity {
				return acts[i].Severity > acts[j].Severity
			}
			return acts[i].host < acts[j].host
		})
		fmt.Fprintf(b, "\n%s:\n", o)
		for _, a := range acts {
			fmt.Fprintf(b, "  [%s] %s: %s\n        → %s\n", strings.ToUpper(a.Severity.String()), a.host, a.Title, a.Fix)
		}
	}
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
