package agents

// The neutral event format: what an agent is doing, in words that aren't Claude Code's,
// so that what reads this doesn't change when an agent's own words do, or when another
// agent is added. docs/agent-events.md is the contract.

const (
	EventWorking    = "working"
	EventNeedsInput = "needs_input"
	EventDone       = "done"
	EventFailed     = "failed"
	EventIdle       = "idle"
	EventUnknown    = "unknown"
)

const (
	// ConfidenceHigh: a value in the table said so, and nothing contradicted it.
	ConfidenceHigh = "high"
	// ConfidenceLow: the event is a guess between values that disagree, or unknown.
	ConfidenceLow = "low"
)

const (
	KindClaudeCode     = "claude-code"
	SourceClaudeAgents = "claude agents --json"
)

// stateEvents and statusEvents are the values of claude agents' `state` and `status`
// that are known. Everything else is unknown: a word that isn't here may be a new one
// that means the agent is stuck, so it can't be taken for quiet. `stopped` is left out
// on purpose, as it says neither that the work was done nor that it failed.
var (
	stateEvents = map[string]string{
		"working": EventWorking,
		"running": EventWorking,
		"blocked": EventNeedsInput,
		"done":    EventDone,
		"failed":  EventFailed,
		"error":   EventFailed,
	}
	statusEvents = map[string]string{
		"busy":    EventWorking,
		"waiting": EventNeedsInput,
		"idle":    EventIdle,
	}
)

// Classify is the one place Claude Code's words become neutral ones. `state` is what a
// background agent reports and says more than `status`, which a finished one can still
// give as idle, so it goes first. `waitingFor` means it's waiting, however it's put.
func Classify(state, status, waitingFor string) (event, confidence string) {
	if waitingFor != "" {
		if state == "done" || state == "failed" || state == "error" {
			return stateEvents[state], ConfidenceLow // over, and still says it's waiting
		}
		return EventNeedsInput, ConfidenceHigh
	}
	fromState, stateKnown := stateEvents[state]
	fromStatus, statusKnown := statusEvents[status]
	switch {
	case state != "" && !stateKnown, status != "" && !statusKnown:
		return EventUnknown, ConfidenceLow
	case stateKnown && statusKnown:
		if statusAgrees(fromState, fromStatus) {
			return fromState, ConfidenceHigh
		}
		return fromState, ConfidenceLow
	case stateKnown:
		return fromState, ConfidenceHigh
	case statusKnown:
		return fromStatus, ConfidenceHigh
	}
	return EventUnknown, ConfidenceLow
}

// statusAgrees: idle is what a finished agent's status says, so it doesn't contradict
// done or failed.
func statusAgrees(fromState, fromStatus string) bool {
	if fromStatus == EventIdle {
		return fromState == EventDone || fromState == EventFailed
	}
	return fromState == fromStatus
}
