package activity

import (
	"regexp"
	"strings"
)

// Actor types. ActorHuman is reserved: nothing writes it today, because nothing here
// can tell. Having no agent variable and a terminal doesn't make a person, as an agent
// can clear a variable and open a pty, so a person's shell says ActorUnknown.
const (
	ActorHuman   = "human"
	ActorAgent   = "agent"
	ActorUnknown = "unknown"
)

// SourceDeclared says the actor fields are what the process's environment claims.
// Any process can set or clear those variables. There's no "verified": nothing on this
// side of an ssh connection could earn it.
const SourceDeclared = "declared"

// KindClaudeCode is Claude Code's agent_kind.
const KindClaudeCode = "claude-code"

const maxKind = 32

// NoSessionEnv, set to anything, keeps agent_session_id out of the log, and so out of the
// team's copy. What kind of actor it was stays: that's what the log is for.
const NoSessionEnv = "METERM_NO_AGENT_SESSION"

// Actor is who an event claims to come from.
type Actor struct {
	Type, Kind, SessionID, Source string
}

var (
	// Letters and single hyphens: a name, and not much room for a token to pass as one.
	kindRE    = regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)
	sessionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// DetectActor reads who's asking from variables an agent's tools inherit. Claude Code
// sets CLAUDECODE=1, CLAUDE_CODE_SESSION_ID and AI_AGENT=claude-code_<version>_agent in
// the shells it starts; AI_AGENT is a convention other agents can use too. Only these
// variables are read, and each value is data: what isn't a plain short word is dropped,
// and nothing else from the environment gets near the log.
func DetectActor(getenv func(string) string) Actor {
	a := Actor{Type: ActorUnknown, Source: SourceDeclared}
	marker := getenv("AI_AGENT")
	switch {
	case getenv("CLAUDECODE") == "1", isClaudeMarker(marker):
		a.Type, a.Kind = ActorAgent, KindClaudeCode
		if s := getenv("CLAUDE_CODE_SESSION_ID"); sessionRE.MatchString(s) && Redact(s) == s && getenv(NoSessionEnv) == "" {
			a.SessionID = s
		}
	case marker != "":
		// Its first word, as in <kind>_<version>_agent. Anything else says only that
		// some agent is here.
		a.Type, a.Kind = ActorAgent, "other"
		if word, _, _ := strings.Cut(strings.ReplaceAll(marker, "/", "_"), "_"); len(word) <= maxKind && kindRE.MatchString(word) && Redact(marker) == marker {
			a.Kind = word
		}
	}
	return a
}

func isClaudeMarker(s string) bool {
	return strings.HasPrefix(s, "claude-code_") || strings.HasPrefix(s, "claude-code/")
}
