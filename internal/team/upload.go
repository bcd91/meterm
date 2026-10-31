package team

import (
	"context"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// UploadActivity sends teamID the local events on hosts that team shares
// (per cfg), then moves that team's cursor past everything it looked at.
// Events on personal hosts, or hosts shared with a different team, never
// leave the machine. Returns how many events were sent.
func UploadActivity(ctx context.Context, b Backend, teamID string, cfg *config.Config, log *activity.Log) (int, error) {
	pending, err := log.Pending(teamID)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	shared := map[string]bool{}
	for _, h := range cfg.Hosts {
		if h.Team == teamID {
			shared[h.Name] = true
		}
	}
	var send []activity.Event
	for _, e := range pending {
		if shared[e.Host] {
			send = append(send, e)
		}
	}
	if len(send) > 0 {
		if err := b.AppendEvents(ctx, teamID, send); err != nil {
			return 0, err
		}
	}
	return len(send), log.MarkUploaded(teamID, pending[len(pending)-1].Seq)
}
