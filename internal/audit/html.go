package audit

import (
	"html/template"
	"io"
	"sort"
	"strings"
)

// htmlData is what the template actually renders — all computed ahead of
// time so the template itself stays declarative (no funcs, no method calls
// on exported types that might change shape later).
type htmlData struct {
	Title     string
	Generated string
	Hosts     []htmlHost
	Fails     int
	Unknowns  int
	Passes    int
	Owners    []htmlOwnerGroup
	Access    *htmlAccess
}

type htmlAccess struct {
	Days int
	Note string
	Rows []htmlAccessRow
}

type htmlAccessRow struct {
	Host, Who, Kinds, Last string
	Actions                int
}

type htmlHost struct {
	Name     string
	Exposure string
	OS       string
	Error    string
	HaveSudo bool
	Notes    []string
	Results  []Result
	Fails    int
	Unknowns int
	Passes   int
}

type htmlOwnerGroup struct {
	Owner   string
	Actions []htmlAction
}

type htmlAction struct {
	Host     string
	Severity string
	Title    string
	Fix      string
}

// RenderHTML renders a single-file, self-contained HTML report — no
// external CSS/JS/images — suitable for emailing or handing to a client who
// has no meterm installed. title is shown as the report's heading (e.g. a
// client or company name); pass "" for a generic title.
func RenderHTML(w io.Writer, r Report, title string) error {
	if title == "" {
		title = "Security baseline audit"
	}
	d := htmlData{
		Title:     title,
		Generated: r.Generated.Format("2006-01-02 15:04 MST"),
	}
	for _, h := range r.Hosts {
		hh := htmlHost{
			Name:     h.Host,
			Exposure: h.Exposure,
			OS:       h.OS,
			Error:    h.Error,
			HaveSudo: h.HaveSudo,
			Notes:    h.Notes,
			Results:  h.Results,
		}
		hh.Fails, hh.Unknowns, hh.Passes = h.Counts()
		d.Fails += hh.Fails
		d.Unknowns += hh.Unknowns
		d.Passes += hh.Passes
		d.Hosts = append(d.Hosts, hh)
	}
	d.Owners = htmlOwnerGroups(r)
	if a := r.Access; a != nil {
		d.Access = &htmlAccess{Days: a.Days, Note: a.Note}
		for _, row := range a.Rows {
			d.Access.Rows = append(d.Access.Rows, htmlAccessRow{
				Host: row.Host, Who: row.Who, Kinds: row.Kinds, Actions: row.Actions,
				Last: row.Last.Local().Format("2006-01-02 15:04"),
			})
		}
	}

	return htmlReportTmpl.Execute(w, d)
}

func htmlOwnerGroups(r Report) []htmlOwnerGroup {
	byOwner := map[Owner][]htmlAction{}
	for _, h := range r.Hosts {
		for _, res := range h.Results {
			if res.Status != Fail {
				continue
			}
			byOwner[res.Owner] = append(byOwner[res.Owner], htmlAction{
				Host:     h.Host,
				Severity: strings.ToUpper(res.Severity.String()),
				Title:    res.Title,
				Fix:      res.Fix,
			})
		}
	}
	owners := make([]Owner, 0, len(byOwner))
	for o := range byOwner {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	groups := make([]htmlOwnerGroup, 0, len(owners))
	for _, o := range owners {
		acts := byOwner[o]
		sort.SliceStable(acts, func(i, j int) bool {
			if acts[i].Severity != acts[j].Severity {
				return acts[i].Severity > acts[j].Severity
			}
			return acts[i].Host < acts[j].Host
		})
		groups = append(groups, htmlOwnerGroup{Owner: string(o), Actions: acts})
	}
	return groups
}

var htmlReportTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
         margin: 0; padding: 2rem 1rem; background: #f6f7f9; color: #1a1d23; }
  .wrap { max-width: 860px; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin: 0 0 0.25rem; }
  .meta { color: #6b7280; font-size: 0.875rem; margin-bottom: 1.5rem; }
  .summary { display: flex; gap: 0.75rem; margin-bottom: 2rem; flex-wrap: wrap; }
  .stat { flex: 1; min-width: 110px; background: #fff; border: 1px solid #e5e7eb; border-radius: 8px;
          padding: 0.75rem 1rem; text-align: center; }
  .stat .n { font-size: 1.5rem; font-weight: 600; display: block; }
  .stat.fail .n { color: #b42318; }
  .stat.unknown .n { color: #8a6d00; }
  .stat.pass .n { color: #067647; }
  .stat .label { font-size: 0.75rem; color: #6b7280; text-transform: uppercase; letter-spacing: 0.03em; }
  .host { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px; padding: 1rem 1.25rem; margin-bottom: 1rem; }
  .host h2 { font-size: 1.05rem; margin: 0 0 0.25rem; }
  .host .sub { color: #6b7280; font-size: 0.8rem; margin-bottom: 0.75rem; }
  .row { padding: 0.4rem 0; border-top: 1px solid #f1f2f4; font-size: 0.9rem; }
  .row:first-of-type { border-top: none; }
  .badge { display: inline-block; font-size: 0.7rem; font-weight: 600; padding: 0.1rem 0.45rem;
           border-radius: 4px; margin-right: 0.5rem; vertical-align: middle; }
  .badge.fail { background: #fef3f2; color: #b42318; }
  .badge.unknown { background: #fff8e1; color: #8a6d00; }
  .badge.pass { background: #ecfdf3; color: #067647; }
  .detail { color: #6b7280; font-size: 0.85rem; margin: 0.15rem 0 0 1.75rem; }
  .error { color: #b42318; }
  .note { color: #6b7280; font-size: 0.8rem; margin: 0.25rem 0; }
  .owners { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px; padding: 1rem 1.25rem; }
  .owners h2 { font-size: 1.05rem; margin: 0 0 0.75rem; }
  .owners h3 { font-size: 0.9rem; margin: 1rem 0 0.4rem; text-transform: capitalize; }
  .action { font-size: 0.85rem; padding: 0.3rem 0; }
  .access { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px; padding: 1rem 1.25rem; margin-top: 1rem; overflow-x: auto; }
  .access h2 { font-size: 1.05rem; margin: 0 0 0.25rem; }
  .access table { border-collapse: collapse; width: 100%; font-size: 0.85rem; margin-top: 0.5rem; }
  .access th, .access td { text-align: left; padding: 0.35rem 0.5rem; border-top: 1px solid #f1f2f4; }
  .access th { color: #6b7280; font-weight: 600; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.03em; border-top: none; }
  .access td.n { text-align: right; font-variant-numeric: tabular-nums; }
  footer { text-align: center; color: #9ca3af; font-size: 0.75rem; margin-top: 2rem; }
</style>
</head>
<body>
<div class="wrap">
  <h1>{{.Title}}</h1>
  <div class="meta">Generated {{.Generated}} · read-only, nothing was changed on any host</div>

  <div class="summary">
    <div class="stat fail"><span class="n">{{.Fails}}</span><span class="label">to fix</span></div>
    <div class="stat unknown"><span class="n">{{.Unknowns}}</span><span class="label">could not check</span></div>
    <div class="stat pass"><span class="n">{{.Passes}}</span><span class="label">ok</span></div>
  </div>

  {{range .Hosts}}
  <div class="host">
    <h2>{{.Name}}</h2>
    <div class="sub">{{.Exposure}}{{if .OS}} · {{.OS}}{{end}}{{if and (not .Error) (not .HaveSudo)}} · no passwordless sudo: root-only checks reported as unknown{{end}}</div>
    {{if .Error}}
      <div class="row error">could not audit: {{.Error}}</div>
    {{else}}
      {{range .Notes}}<div class="note">{{.}}</div>{{end}}
      {{range .Results}}
      <div class="row">
        {{if eq (print .Status) "fail"}}<span class="badge fail">{{.Severity}}</span>
        {{else if eq (print .Status) "unknown"}}<span class="badge unknown">?</span>
        {{else}}<span class="badge pass">ok</span>{{end}}
        {{.Title}}
        {{if ne (print .Status) "pass"}}<div class="detail">{{.Detail}}</div>{{end}}
      </div>
      {{end}}
    {{end}}
  </div>
  {{end}}

  <div class="owners">
    <h2>Actions by owner</h2>
    {{if not .Owners}}<div class="note">Nothing to hand off.</div>{{end}}
    {{range .Owners}}
      <h3>{{.Owner}}</h3>
      {{range .Actions}}
      <div class="action">[{{.Severity}}] <strong>{{.Host}}</strong>: {{.Title}} → {{.Fix}}</div>
      {{end}}
    {{end}}
  </div>

  {{with .Access}}
  <div class="access">
    <h2>Who accessed these hosts</h2>
    <div class="note">Last {{.Days}} days, from the team activity log. Only work done through meterm is recorded.</div>
    {{if .Note}}<div class="note">{{.Note}}</div>{{end}}
    {{if .Rows}}
    <table>
      <tr><th>Host</th><th>Person</th><th>Actions</th><th>What</th><th>Last</th></tr>
      {{range .Rows}}<tr><td>{{.Host}}</td><td>{{.Who}}</td><td class="n">{{.Actions}}</td><td>{{.Kinds}}</td><td>{{.Last}}</td></tr>
      {{end}}
    </table>
    {{else if not .Note}}<div class="note">No recorded activity.</div>{{end}}
  </div>
  {{end}}

  <footer>Generated by meterm audit</footer>
</div>
</body>
</html>
`))
