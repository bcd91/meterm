package team

import (
	"testing"

	"meterm/internal/config"
)

func TestAdoptMovesHostsAndBookkeeping(t *testing.T) {
	cfg := emptyCfg(t)
	cfg.Upsert(config.Host{Name: "web1", Address: "10.0.0.1", Team: "cloud1"})
	cfg.Upsert(config.Host{Name: "mine", Address: "10.0.0.2"})
	st := State{Current: "cloud1", Bases: map[string]Base{"cloud1": {"web1": {Name: "web1", Rev: 7}}},
		Ignored: map[string][]string{"cloud1": {"noisy"}}}
	n := Adopt(cfg, &st, "cloud1", "git:repo", []SharedHost{{Name: "web1", Address: "10.0.0.1", Rev: 1}})
	if n != 1 {
		t.Errorf("moved %d", n)
	}
	if h, _ := cfg.Find("web1"); h.Team != "git:repo" {
		t.Errorf("web1 team = %q", h.Team)
	}
	if h, _ := cfg.Find("mine"); h.Team != "" {
		t.Errorf("unshared host moved to %q", h.Team)
	}
	if st.Current != "git:repo" || st.Bases["git:repo"]["web1"].Rev != 1 || st.Bases["cloud1"] != nil || st.Ignored["git:repo"][0] != "noisy" {
		t.Errorf("state = %+v", st)
	}
}

// migrated_from is written by whoever can push to the repository: it must
// never move personal hosts, or another Git team's, into this team.
func TestAdoptRefusesGitAndEmptySources(t *testing.T) {
	for _, from := range []string{"", "git:https://example.com/other", "git:repo"} {
		cfg := emptyCfg(t)
		cfg.Upsert(config.Host{Name: "mine", Address: "10.0.0.2"})
		cfg.Upsert(config.Host{Name: "other", Address: "10.0.0.3", Team: "git:https://example.com/other"})
		st := State{}
		if n := Adopt(cfg, &st, from, "git:repo", []SharedHost{{Name: "mine"}, {Name: "other"}}); n != 0 {
			t.Errorf("from %q moved %d hosts", from, n)
		}
		if h, _ := cfg.Find("mine"); h.Team != "" {
			t.Errorf("from %q: personal host moved to %q", from, h.Team)
		}
		if h, _ := cfg.Find("other"); h.Team != "git:https://example.com/other" {
			t.Errorf("from %q: other team's host moved to %q", from, h.Team)
		}
	}
}
