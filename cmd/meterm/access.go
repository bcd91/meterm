package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"meterm/internal/activity"
	"meterm/internal/audit"
	"meterm/internal/config"
	"meterm/internal/team"
)

// teamAccess builds the "who accessed these hosts" section of a report from
// the activity logs of whichever teams the hosts are shared with. It never
// fails the report: problems become the summary's Note.
func teamAccess(ctx context.Context, hosts []config.Host, days int) *audit.AccessSummary {
	sum := &audit.AccessSummary{Days: days}
	byTeam := map[string]map[string]bool{}
	for _, h := range hosts {
		if h.Team == "" {
			continue
		}
		if byTeam[h.Team] == nil {
			byTeam[h.Team] = map[string]bool{}
		}
		byTeam[h.Team][h.Name] = true
	}
	if len(byTeam) == 0 {
		sum.Note = "None of these hosts is shared with a team, so there is no shared access log for them (meterm team share)."
		return sum
	}
	tc, err := openTeam()
	if err != nil || tc.be == nil {
		sum.Note = "This machine has no team, so the access log was left out (meterm team git add)."
		return sum
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	var events []team.TeamEvent
	ids := make([]string, 0, len(byTeam))
	for id := range byTeam {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		evs, err := tc.be.ListEvents(ctx, id, team.EventFilter{Since: since})
		if err != nil {
			sum.Note = fmt.Sprintf("Couldn't read the access log for some hosts: %v", err)
			continue
		}
		for _, e := range evs {
			if byTeam[id][e.Host] {
				events = append(events, e)
			}
		}
	}
	sum.Rows = accessRows(events)
	return sum
}

// accessRows groups events by host and person: how many actions of each
// kind, and when the last one was.
func accessRows(events []team.TeamEvent) []audit.AccessRow {
	type key struct{ host, who string }
	rows := map[key]*audit.AccessRow{}
	kinds := map[key]map[string]int{}
	for _, e := range events {
		if e.Action == activity.ActionOutcome {
			continue // how an action ended isn't another action
		}
		k := key{e.Host, e.UploaderEmail}
		r := rows[k]
		if r == nil {
			r = &audit.AccessRow{Host: e.Host, Who: e.UploaderEmail}
			rows[k] = r
			kinds[k] = map[string]int{}
		}
		r.Actions++
		kinds[k][e.Action]++
		if e.Time.After(r.Last) {
			r.Last = e.Time
		}
	}
	out := make([]audit.AccessRow, 0, len(rows))
	for k, r := range rows {
		names := make([]string, 0, len(kinds[k]))
		for n := range kinds[k] {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool {
			if kinds[k][names[i]] != kinds[k][names[j]] {
				return kinds[k][names[i]] > kinds[k][names[j]]
			}
			return names[i] < names[j]
		})
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s ×%d", n, kinds[k][n])
		}
		r.Kinds = strings.Join(parts, ", ")
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Actions > out[j].Actions
	})
	return out
}
