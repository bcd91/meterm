package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/health"
	"meterm/internal/history"
	"meterm/internal/team"
)

// listRow is the --format json shape for one host: a stable, documented
// schema so scripts and AI agents can rely on field names across releases
// instead of parsing the tabwriter columns meant for a terminal.
//
// LastConnected is RFC 3339 (matching `history --format json`), omitted
// entirely when the host has never been connected to — check for the key's
// absence rather than any particular string value.
type listRow struct {
	Name          string   `json:"name"`
	Target        string   `json:"target"`
	Route         string   `json:"route,omitempty"` // the route used (list --check), when not the host's own address
	Group         string   `json:"group,omitempty"`
	Team          string   `json:"team,omitempty"` // the team's name; empty for a personal host
	Tags          []string `json:"tags,omitempty"`
	Env           string   `json:"env,omitempty"` // the host's, or else its group's; absent when neither says
	Jump          string   `json:"jump,omitempty"`
	LastConnected string   `json:"last_connected,omitempty"`
	Status        string   `json:"status,omitempty"`

	lastHuman string
	routeNote string `json:"-"` // "never" / "8d ago" — text output only
}

func newListCmd() *cobra.Command {
	var group, tag, teamName, format string
	var check bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List saved hosts",
		Long: "Lists every saved host, or a filtered subset.\n\n" +
			"Use --format json for scripting or AI agents: it prints a JSON array of\n" +
			"objects with stable field names (name, target, group, tags, jump,\n" +
			"last_connected, and status when --check is given) instead of the\n" +
			"human-oriented table.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json, got %q", format)
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			kickAutosync(true, false)

			names := teamNames()
			var hosts []config.Host
			for _, h := range cfg.SortedHosts() {
				if teamName != "" && !strings.EqualFold(names[h.Team], teamName) && h.Team != teamName {
					continue
				}
				if group != "" && h.Group != group {
					continue
				}
				if tag != "" && !hasTag(h.Tags, tag) {
					continue
				}
				hosts = append(hosts, h)
			}

			if !check {
				cfg.SetRoutePicker(nil) // listing shouldn't wait on the network
			}
			var status map[string]string
			if check {
				status = checkHosts(cfg, hosts)
			}

			hpath, _ := history.Path()
			hist, _ := history.Load(hpath)

			rows := make([]listRow, 0, len(hosts))
			for _, h := range hosts {
				r := cfg.Resolve(h)
				target := r.Address
				if r.User != "" {
					target = r.User + "@" + r.Address
				}
				routeNote := r.Route
				if routeNote == "" && len(h.Routes) > 0 {
					routeNote = fmt.Sprintf("+%d route", len(h.Routes))
				}
				jump := r.ProxyJumpRaw
				for _, j := range r.ProxyJump {
					if jump != "" {
						jump += ","
					}
					jump += j.Name
				}
				last := "never"
				var lastRFC3339 string
				if e, ok := hist[h.Name]; ok {
					last = history.FormatAgo(e.Last)
					lastRFC3339 = e.Last.Format("2006-01-02T15:04:05Z07:00")
				}
				rows = append(rows, listRow{
					Name:          h.Name,
					Target:        target,
					Route:         r.Route,
					routeNote:     routeNote,
					Group:         h.Group,
					Team:          names[h.Team],
					Tags:          h.Tags,
					Env:           cfg.Env(h),
					Jump:          jump,
					LastConnected: lastRFC3339,
					Status:        status[h.Name],
					lastHuman:     last,
				})
			}

			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			showEnv := false
			for _, row := range rows {
				showEnv = showEnv || row.Env != ""
			}
			header := "NAME\tTARGET\tTEAM\tGROUP\tTAGS\tJUMP"
			if showEnv {
				header += "\tENV"
			}
			header += "\tLAST CONNECTED"
			if check {
				header = "STATUS\t" + header
			}
			fmt.Fprintln(w, header)
			for _, row := range rows {
				tgt := row.Target
				if row.routeNote != "" {
					tgt += " (" + row.routeNote + ")"
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", row.Name, tgt, row.Team, row.Group, strings.Join(row.Tags, ","), row.Jump)
				if showEnv {
					line += "\t" + row.Env
				}
				if check {
					line = row.Status + "\t" + line
				}
				fmt.Fprintln(w, line+"\t"+row.lastHuman)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if n := teamNotice(); n != "" {
				fmt.Fprintln(os.Stderr, "\n⚠ "+n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "only show hosts in this group")
	cmd.Flags().StringVar(&tag, "tag", "", "only show hosts with this tag")
	cmd.Flags().StringVar(&teamName, "team", "", "only show hosts shared with this team (its name)")
	cmd.Flags().BoolVar(&check, "check", false, "TCP-ping each host's port before listing (adds latency)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

// checkHosts probes every host's port concurrently and returns "up"/"down",
// keyed by host name. Hosts behind a proxy_jump are reported as "via-jump"
// instead of being dialed directly — from outside that jump host's network
// a direct dial almost always fails even when the host is perfectly
// reachable, so a bare "down" there would just be wrong.
func checkHosts(cfg *config.Config, hosts []config.Host) map[string]string {
	result := make(map[string]string, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h config.Host) {
			defer wg.Done()
			r := cfg.Resolve(h)
			mark := "down"
			switch {
			case len(r.ProxyJump) > 0 || r.ProxyJumpRaw != "":
				mark = "via-jump"
			case health.Check(r.Address, r.Port, health.DefaultTimeout):
				mark = "up"
			}
			mu.Lock()
			result[h.Name] = mark
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return result
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// teamNames maps team IDs to their names, for showing which team a host is
// shared with.
func teamNames() map[string]string {
	out := map[string]string{}
	dir, err := team.Dir()
	if err != nil {
		return out
	}
	st, err := team.LoadState(dir)
	if err != nil {
		return out
	}
	for id, gt := range st.GitTeams {
		out[id] = gt.Name
	}
	for id, n := range st.TeamNames {
		if out[id] == "" {
			out[id] = n
		}
	}
	return out
}
