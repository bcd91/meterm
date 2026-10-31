// Package agents reads what `claude agents --json` says on a host. All of it is
// remote output: it's checked field by field, kept to the fields known here, and
// cleaned before anything can show it.
package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"meterm/internal/termsafe"
)

// Schema is the version of the --format json report. It goes up when a field
// changes meaning; new fields don't need it to.
const Schema = 1

// RemoteCommand is all `meterm agents` ever runs on a host. It's a constant so
// that nothing from a host, a flag or the config can reach a remote shell.
//
// The login shell parses it first, and may be fish or csh, so everything is inside
// one pair of single quotes for sh. A non-login ssh often has no ~/.local/bin on
// PATH; those places go last, so that the claude this user would get anyway wins.
// A failing claude comes out as 1, because 255 has to keep meaning ssh's own
// failure, and 127 as "no claude" is decided here, not by claude.
const RemoteCommand = `sh -c 'PATH="$PATH:$HOME/.local/bin:$HOME/.claude/local:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin"; export PATH; ` +
	`command -v claude >/dev/null 2>&1 || exit 127; claude agents --json || exit 1'`

// ExitNoClaude is RemoteCommand's exit code when claude isn't there.
const ExitNoClaude = 127

// Limits on what's believed from a host.
const (
	MaxOutput = 1 << 20 // bytes of output; more is an error, never a partial list
	MaxAgents = 1000
	maxText   = 1024 // runes kept per string
)

// Agent is one session, with the fields meterm knows. A field the host didn't
// send stays out of the JSON.
type Agent struct {
	Cwd        string `json:"cwd,omitempty"`
	Kind       string `json:"kind,omitempty"`
	StartedAt  *int64 `json:"startedAt,omitempty"` // Unix milliseconds
	ID         string `json:"id,omitempty"`
	State      string `json:"state,omitempty"`
	PID        *int64 `json:"pid,omitempty"`
	Status     string `json:"status,omitempty"`
	WaitingFor string `json:"waitingFor,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Host is what one host said. Agents is null, not empty, when Error is set: "we
// don't know" must never read as "none". Reachable says the remote command ran.
type Host struct {
	Host      string  `json:"host"`
	Reachable bool    `json:"reachable"`
	Error     string  `json:"error"`
	Agents    []Agent `json:"agents"`
}

// Report is the --format json output.
type Report struct {
	Schema int    `json:"schema"`
	Hosts  []Host `json:"hosts"`
}

// Parse reads claude's output. Its errors are in meterm's own words and hold
// nothing the host sent, so they're safe to show as they are. The list it returns
// is never nil.
func Parse(raw []byte) ([]Agent, error) {
	if len(raw) > MaxOutput {
		return nil, errors.New("more output than meterm reads")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("not a JSON array")
	}
	if items == nil { // null
		return nil, errors.New("not a JSON array")
	}
	if len(items) > MaxAgents {
		return nil, fmt.Errorf("more than %d agents", MaxAgents)
	}
	out := make([]Agent, 0, len(items))
	for i, item := range items {
		a, err := parseAgent(item)
		if err != nil {
			return nil, fmt.Errorf("agent %d: %w", i+1, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// parseAgent goes through a map rather than a struct: encoding/json would also
// take "PID" for pid, and any other spelling a host cares to send.
func parseAgent(raw json.RawMessage) (Agent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Agent{}, errors.New("not a JSON object")
	}
	var a Agent
	for _, f := range []struct {
		name string
		to   *string
	}{
		{"cwd", &a.Cwd}, {"kind", &a.Kind}, {"id", &a.ID}, {"state", &a.State},
		{"status", &a.Status}, {"waitingFor", &a.WaitingFor}, {"sessionId", &a.SessionID}, {"name", &a.Name},
	} {
		if err := readString(fields, f.name, f.to); err != nil {
			return Agent{}, err
		}
	}
	var err error
	if a.StartedAt, err = readInt(fields, "startedAt"); err != nil {
		return Agent{}, err
	}
	if a.PID, err = readInt(fields, "pid"); err != nil {
		return Agent{}, err
	}
	return a, nil
}

func readString(fields map[string]json.RawMessage, name string, to *string) error {
	raw, ok := fields[name]
	if !ok || isNull(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s isn't a string", name)
	}
	*to = termsafe.Clip(termsafe.Text(s), maxText)
	return nil
}

// readInt takes whole numbers that aren't negative, and nothing a float64 would
// have rounded.
func readInt(fields map[string]json.RawMessage, name string) (*int64, error) {
	raw, ok := fields[name]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var n json.Number
	// json.Number would take the string "12" too.
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) || json.Unmarshal(raw, &n) != nil {
		return nil, fmt.Errorf("%s isn't a number", name)
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return nil, fmt.Errorf("%s isn't a whole number", name)
	}
	return &v, nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }
