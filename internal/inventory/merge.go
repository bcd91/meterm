package inventory

import "meterm/internal/config"

// Outcome is what Merge did with one imported host.
type Outcome struct {
	Host    config.Host
	Action  string // "added", "exists" (name taken), "duplicate" (address saved as Other), "declined"
	Other   string
	Stopped bool // ask said stop: this host and the rest were left alone
}

// Ask decides about one host that could be added: add it, or not, or stop
// asking altogether.
type Ask func(h config.Host) (add, stop bool)

// Merge adds imported hosts to cfg (in memory) unless the name is taken or
// the address is already saved under another name — so importing a tailnet
// doesn't duplicate hosts saved by hand. ask may be nil to add everything.
// It returns one outcome per host it looked at, stopping early if asked.
func Merge(cfg *config.Config, imported []config.Host, ask Ask) []Outcome {
	byAddr := map[string]string{}
	for _, h := range cfg.Hosts {
		byAddr[h.Address] = h.Name
	}
	var out []Outcome
	for _, h := range imported {
		if _, idx := cfg.Find(h.Name); idx >= 0 {
			out = append(out, Outcome{Host: h, Action: "exists"})
			continue
		}
		if other, ok := byAddr[h.Address]; ok {
			out = append(out, Outcome{Host: h, Action: "duplicate", Other: other})
			continue
		}
		if ask != nil {
			add, stop := ask(h)
			if stop {
				out = append(out, Outcome{Host: h, Action: "declined", Stopped: true})
				return out
			}
			if !add {
				out = append(out, Outcome{Host: h, Action: "declined"})
				continue
			}
		}
		cfg.Upsert(h)
		byAddr[h.Address] = h.Name
		out = append(out, Outcome{Host: h, Action: "added"})
	}
	return out
}

// Added counts the hosts Merge added.
func Added(out []Outcome) int {
	n := 0
	for _, o := range out {
		if o.Action == "added" {
			n++
		}
	}
	return n
}
