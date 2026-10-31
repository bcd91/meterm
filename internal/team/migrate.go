package team

import (
	"context"

	"meterm/internal/config"
)

// A team that was moved here from meterm's old cloud service records the
// cloud team's ID (migrated_from in meterm-team.yaml). A machine that still
// has hosts saved under that ID adopts them when it adds the Git team.

// MigratedFrom is the cloud team this Git team was moved from, if any.
func (g *GitStore) MigratedFrom(ctx context.Context) (string, error) {
	if err := g.read(ctx); err != nil {
		return "", err
	}
	tf, err := g.teamFile()
	return tf.MigratedFrom, err
}

// Adopt moves this machine's side of a migrated team over: hosts saved
// under fromID now belong to toID, and the sync bookkeeping starts from
// remote (the Git team's hosts), so nothing looks changed that isn't.
// It returns how many hosts moved; cfg and st still need saving.
func Adopt(cfg *config.Config, st *State, fromID, toID string, remote []SharedHost) int {
	// migrated_from comes from the repository, so anyone who can push sets
	// it. Only an old cloud team's ID counts: naming another Git team (or
	// nothing, which personal hosts carry) would pull this machine's hosts
	// from there into this team, to be pushed into it.
	if fromID == "" || IsGitTeam(fromID) || fromID == toID {
		return 0
	}
	n := 0
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Team == fromID {
			cfg.Hosts[i].Team = toID
			n++
		}
	}
	// Only hosts this machine actually has start from the remote copy. A
	// member who never had the cloud team's hosts here must not get them in
	// the base: with a base and no local copy, sync reads every host as
	// "removed here" — and a push would delete them all from the team.
	have := map[string]bool{}
	for _, h := range cfg.Hosts {
		if h.Team == toID {
			have[h.Name] = true
		}
	}
	base := Base{}
	for _, h := range remote {
		if have[h.Name] {
			base[h.Name] = h
		}
	}
	if st.Bases == nil {
		st.Bases = map[string]Base{}
	}
	st.Bases[toID] = base
	delete(st.Bases, fromID)
	if ig, ok := st.Ignored[fromID]; ok {
		st.Ignored[toID] = ig
		delete(st.Ignored, fromID)
	}
	delete(st.Attention, fromID)
	delete(st.Attention, toID) // recomputed by the next sync
	delete(st.LastPull, fromID)
	delete(st.TeamNames, fromID)
	if st.Current == fromID {
		st.Current = toID
	}
	return n
}
