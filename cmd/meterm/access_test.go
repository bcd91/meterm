package main

import (
	"testing"
	"time"

	"meterm/internal/activity"
	"meterm/internal/team"
)

func TestAccessRowsGroupByHostAndPerson(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ev := func(host, who, action string, h int) team.TeamEvent {
		return team.TeamEvent{Event: activity.Event{Host: host, Action: action, Time: t0.Add(time.Duration(h) * time.Hour)}, UploaderEmail: who}
	}
	rows := accessRows([]team.TeamEvent{
		ev("WEB", "a@x", "connect", 1), ev("WEB", "a@x", "connect", 5), ev("WEB", "a@x", "exec", 2),
		ev("WEB", "b@x", "cp", 3), ev("API", "a@x", "connect", 4),
	})
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Host != "API" || rows[1].Who != "a@x" || rows[1].Actions != 3 {
		t.Errorf("order/grouping wrong: %+v", rows)
	}
	if rows[1].Kinds != "connect ×2, exec ×1" || !rows[1].Last.Equal(t0.Add(5*time.Hour)) {
		t.Errorf("WEB/a@x = %+v", rows[1])
	}
}
