package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"meterm/internal/status"
)

func TestWriteStatusJSONKeepsAnEmptyArray(t *testing.T) {
	var b bytes.Buffer
	if err := writeStatus(&b, statusReport{Schema: status.SchemaVersion, Entries: []status.Row{}}, "json"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if e, ok := got["entries"].([]any); !ok || len(e) != 0 {
		t.Errorf("entries = %#v", got["entries"])
	}
	if got["schema"] != float64(status.SchemaVersion) {
		t.Errorf("schema = %v", got["schema"])
	}
}

func TestWriteStatusText(t *testing.T) {
	rep := statusReport{Entries: []status.Row{{Entry: status.Entry{Kind: status.KindTunnel, Host: "box", Name: "web",
		State: status.Connected, Route: "office", Since: time.Now()}, PID: 7}}, Skipped: 1}
	var b bytes.Buffer
	if err := writeStatus(&b, rep, "text"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOST", "box", "tunnel", "web", "connected", "office", "newer meterm"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := writeStatus(&b, statusReport{}, "text"); err != nil || !strings.Contains(b.String(), "Nothing") {
		t.Errorf("empty: %q, %v", b.String(), err)
	}
}

// watchStatus prints the first report and then only what's different, and a
// read failing mid-way doesn't end it.
func TestWatchStatusPrintsChangesOnly(t *testing.T) {
	row := func(state string) statusReport {
		return statusReport{Schema: 1, Entries: []status.Row{{Entry: status.Entry{Host: "box", State: state}, PID: 1}}}
	}
	reads := []func() (statusReport, error){
		func() (statusReport, error) { return row("connecting"), nil },
		func() (statusReport, error) { return row("connecting"), nil },
		func() (statusReport, error) { return statusReport{}, errors.New("gone") },
		func() (statusReport, error) { return row("connected"), nil },
		func() (statusReport, error) { return row("connected"), nil },
		func() (statusReport, error) { return row("reconnecting"), nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	go func() {
		for range reads[1:] {
			tick <- time.Now()
		}
	}()
	i := 0
	read := func() (statusReport, error) {
		r := reads[i]
		i++
		if i == len(reads) {
			defer cancel()
		}
		return r()
	}
	var shown []string
	err := watchStatus(ctx, tick, read, func(rep statusReport) error {
		shown = append(shown, rep.Entries[0].State)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(shown, " "); got != "connecting connected reconnecting" {
		t.Errorf("shown = %q", got)
	}
}

func TestWatchStatusFailsAtTheStart(t *testing.T) {
	want := errors.New("unsafe dir")
	err := watchStatus(context.Background(), nil, func() (statusReport, error) { return statusReport{}, want }, nil)
	if !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
}
