package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		state, status, waiting, event, confidence string
	}{
		// What the real thing was seen to send.
		{"", "idle", "", EventIdle, ConfidenceHigh},
		{"", "busy", "", EventWorking, ConfidenceHigh},
		{"done", "idle", "", EventDone, ConfidenceHigh},
		{"", "busy", "input needed", EventNeedsInput, ConfidenceHigh},
		{"", "waiting", "dialog open", EventNeedsInput, ConfidenceHigh},
		{"", "waiting", "", EventNeedsInput, ConfidenceHigh},
		{"working", "busy", "", EventWorking, ConfidenceHigh},
		{"blocked", "waiting", "", EventNeedsInput, ConfidenceHigh},
		{"failed", "", "", EventFailed, ConfidenceHigh},
		{"failed", "idle", "", EventFailed, ConfidenceHigh},
		{"error", "", "", EventFailed, ConfidenceHigh},
		{"running", "", "", EventWorking, ConfidenceHigh},
		// Disagreement isn't confidence.
		{"done", "busy", "", EventDone, ConfidenceLow},
		{"working", "idle", "", EventWorking, ConfidenceLow},
		{"done", "idle", "input needed", EventDone, ConfidenceLow},
		// Anything else is unknown, and never idle.
		{"", "", "", EventUnknown, ConfidenceLow},
		{"", "paused", "", EventUnknown, ConfidenceLow},
		{"stopped", "idle", "", EventUnknown, ConfidenceLow},
		{"weird", "idle", "", EventUnknown, ConfidenceLow},
		{"done", "weird", "", EventUnknown, ConfidenceLow},
		{"", "IDLE", "", EventUnknown, ConfidenceLow},
		{"", "idle ", "", EventUnknown, ConfidenceLow},
		{"", "\x1b]0;idle\x07", "", EventUnknown, ConfidenceLow},
		{"", "idle‮", "", EventUnknown, ConfidenceLow},
		{"$(idle)", "", "", EventUnknown, ConfidenceLow},
	} {
		event, confidence := Classify(c.state, c.status, c.waiting)
		if event != c.event || confidence != c.confidence {
			t.Errorf("Classify(%q, %q, %q) = %s/%s, want %s/%s", c.state, c.status, c.waiting, event, confidence, c.event, c.confidence)
		}
	}
}

// The enumeration is closed: whatever a host sends, the fields hold only meterm's words.
func TestNeutralFieldsNeverHoldHostText(t *testing.T) {
	allowed := map[string]bool{EventWorking: true, EventNeedsInput: true, EventDone: true, EventFailed: true, EventIdle: true, EventUnknown: true}
	hostile := []string{"\x1b]52;c;aGk=\x07", "idle\n", "<script>", "needs_input;rm -rf /", strings.Repeat("idle", 1000), "working\u0000", "done‮", "Idle"}
	for _, a := range hostile {
		for _, b := range hostile {
			raw, _ := json.Marshal([]map[string]string{{"state": a, "status": b, "event": "idle", "agent_kind": "evil", "source": "evil", "confidence": "high"}})
			got, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			g := got[0]
			if !allowed[g.Event] || g.AgentKind != KindClaudeCode || g.Source != SourceClaudeAgents || (g.Confidence != ConfidenceHigh && g.Confidence != ConfidenceLow) {
				t.Fatalf("%+v", g)
			}
			if g.Event != EventUnknown {
				t.Fatalf("hostile %q/%q became %s", a, b, g.Event)
			}
		}
	}
}

func TestParseFillsTheNeutralFields(t *testing.T) {
	got, err := Parse([]byte(`[{"status":"busy"},{}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Event != EventWorking || got[0].AgentKind != "claude-code" || got[0].Source != "claude agents --json" || got[0].Confidence != "high" {
		t.Errorf("%+v", got[0])
	}
	if got[1].Event != EventUnknown || got[1].Confidence != "low" {
		t.Errorf("an agent that says nothing: %+v", got[1])
	}
	// Additive: schema 1's fields are still there, under the same names.
	b, _ := json.Marshal(Agent{Status: "busy"})
	if !strings.Contains(string(b), `"status":"busy"`) {
		t.Errorf("%s", b)
	}
}
