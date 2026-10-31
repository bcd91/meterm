package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/history"
	"meterm/internal/safefile"
	"meterm/internal/sshrun"
	"meterm/internal/termsafe"
)

// `meterm facts`: what meterm already knows about each host, from its own files, in one
// documented shape, so that nothing else has to read those files. It reads and nothing more:
// no network, no route picked or remembered, no autosync, nothing written. So it can be
// polled. docs/export.md is the contract.

const factsSchema = 1

// factsMaxText cuts each string. What's in these files is the user's own, but a server's
// software string and a team's host names come from elsewhere.
const factsMaxText = 512

type factsReport struct {
	Schema    int         `json:"schema"`
	Generated time.Time   `json:"generated"`
	Sources   factSources `json:"sources"`
	Hosts     []factsHost `json:"hosts"`
}

// factSources says how reading each file went: ok, missing or unreadable. A missing history
// isn't the same as an unreadable one, and neither is a host with no connections.
type factSources struct {
	History  string `json:"history"`
	Routes   string `json:"routes"`
	ServerOS string `json:"server_os"`
}

type factsHost struct {
	Name       string       `json:"name"`
	Target     string       `json:"target"` // user@address, as it resolves with no route picked
	Port       int          `json:"port"`
	Group      string       `json:"group"`
	Tags       []string     `json:"tags"`
	Env        string       `json:"env"` // production, staging, dev, test, or empty when unset
	Production bool         `json:"production"`
	ViaProd    bool         `json:"touches_production"`
	TeamShared bool         `json:"team_shared"`
	ProxyJump  string       `json:"proxy_jump"`
	Routes     []factsRoute `json:"routes"`
	LastRoute  string       `json:"last_route"` // the route that worked last; empty when none is known
	// History: null and 0 when no connection is on record here.
	LastConnected *time.Time `json:"last_connected"`
	Connections   int        `json:"connections"`
	// Server is from the version a server announces before login, when meterm has had reason
	// to look. Null when it hasn't.
	Server *factsServer `json:"server"`
}

type factsRoute struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	ProxyJump string `json:"proxy_jump"`
}

type factsServer struct {
	Software string    `json:"software"`
	Windows  bool      `json:"windows"`
	Checked  time.Time `json:"checked"`
	// Stale says the host's address or route changed since this was checked.
	Stale bool `json:"stale"`
}

func newFactsCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "facts",
		Short: "Export what meterm knows about each host: history, routes, server OS (local, read-only)",
		Long: "Reads the host file, connection history, the last route that worked and what's known\n" +
			"of each server, and prints them with a schema number. It stays on this machine and\n" +
			"writes nothing. Identity files, extra args and notes stay out.\n\n" +
			"The format is documented in docs/export.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json")
			}
			path, err := resolveCfgPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path) // not loadConfig: its route picker writes
			if err != nil {
				return err
			}
			rep := buildFacts(cfg, path, time.Now())
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tTARGET\tENV\tLAST CONNECTED\tCOUNT\tLAST ROUTE\tSERVER")
			for _, h := range rep.Hosts {
				last := "-"
				if h.LastConnected != nil {
					last = h.LastConnected.Local().Format("2006-01-02 15:04")
				}
				server := "-"
				if h.Server != nil {
					server = h.Server.Software
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", h.Name, h.Target, h.Env, last, h.Connections, h.LastRoute, server)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

func factText(s string) string { return termsafe.Clip(termsafe.Text(s), factsMaxText) }

func buildFacts(cfg *config.Config, cfgFile string, now time.Time) factsReport {
	rep := factsReport{Schema: factsSchema, Generated: now.UTC().Truncate(time.Second), Hosts: []factsHost{}}

	hist := history.History{}
	rep.Sources.History = "missing"
	if p, err := history.Path(); err == nil {
		if _, err := os.Stat(p); err == nil {
			rep.Sources.History = "ok"
			if hist, err = history.Load(p); err != nil {
				hist, rep.Sources.History = history.History{}, "unreadable"
			}
		}
	}
	lastRoutes := map[string]string{}
	rep.Sources.Routes = readFactFile(filepath.Join(filepath.Dir(cfgFile), "routes.json"), &lastRoutes)
	servers := map[string]serverOSEntry{}
	rep.Sources.ServerOS = readFactFile(filepath.Join(filepath.Dir(cfgFile), "server-os.json"), &servers)

	for _, h := range cfg.Hosts {
		r := cfg.Resolve(h)
		target := r.Address
		if r.User != "" {
			target = r.User + "@" + r.Address
		}
		f := factsHost{
			Name: factText(h.Name), Target: factText(target), Port: r.Port, Group: factText(h.Group), Tags: []string{},
			Env: factText(cfg.Env(h)), Production: cfg.IsProduction(h), ViaProd: cfg.TouchesProduction(h),
			TeamShared: h.Team != "", ProxyJump: factText(h.ProxyJump), Routes: []factsRoute{},
			LastRoute: factText(lastRoutes[h.Name]),
		}
		for _, t := range r.Tags {
			f.Tags = append(f.Tags, factText(t))
		}
		for _, rt := range h.Routes {
			f.Routes = append(f.Routes, factsRoute{Name: factText(rt.Name), Address: factText(rt.Address), Port: rt.Port, ProxyJump: factText(rt.ProxyJump)})
		}
		if e, ok := hist[h.Name]; ok {
			last := e.Last.UTC()
			f.LastConnected, f.Connections = &last, e.Count
		}
		if e, ok := servers[h.Name]; ok {
			f.Server = &factsServer{Software: factText(e.Software), Windows: sshrun.IsWindowsServer(e.Software), Checked: e.Checked.UTC(), Stale: e.Target != checkTarget(r)}
		}
		rep.Hosts = append(rep.Hosts, f)
	}
	return rep
}

// readFactFile fills v from the JSON at path, and says how that went.
func readFactFile(path string, v any) string {
	b, err := safefile.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return "missing"
	case err != nil:
		return "unreadable"
	case json.Unmarshal(b, v) != nil:
		return "unreadable"
	}
	return "ok"
}
