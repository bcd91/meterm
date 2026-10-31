package sshrun

import "strings"

// Failure is why ssh gave up, as far as its stderr says. OpenSSH's messages
// aren't translated, so they can be matched as they are.
type Failure int

const (
	// FailureNone is anything that may pass by itself, such as a network that
	// isn't there yet.
	FailureNone Failure = iota
	// FailureAuth means the host answered and refused every key.
	FailureAuth
	// FailureHostKey means the host's key isn't the one that's trusted: it changed.
	FailureHostKey
	// FailureUnknownHost means there's no trusted key yet, and nobody was there
	// to accept one or the answer was no. That says nothing against the route.
	FailureUnknownHost
	// FailurePortInUse means a forward's port couldn't be bound. It may pass, when
	// whatever has the port lets go of it.
	FailurePortInUse
	// FailureConfig means ssh can't work with these settings, on any attempt.
	FailureConfig
)

// failureMarks are matched in order. "Permission denied (" is ssh's last word
// on authentication; "Permission denied, please try again." isn't, and a local
// "connect to ... : Permission denied" is the network's doing. ssh ends with
// "Host key verification failed" both for a changed key and for one it has never
// seen, so the changed key's own words have to be looked for first.
var failureMarks = []struct {
	mark string
	kind Failure
}{
	{"REMOTE HOST IDENTIFICATION HAS CHANGED", FailureHostKey},
	{"has changed and you have requested strict checking", FailureHostKey},
	{"Host key verification failed", FailureUnknownHost},
	{"Could not request local forwarding", FailurePortInUse},
	{"remote port forwarding failed", FailurePortInUse},
	{"Address already in use", FailurePortInUse},
	{"Permission denied (", FailureAuth},
	{"Too many authentication failures", FailureAuth},
	{"Unable to negotiate with", FailureConfig},
	{"Bad configuration option", FailureConfig},
	{"Bad owner or permissions on", FailureConfig},
}

// ClassifyFailure reads what ssh wrote to stderr.
func ClassifyFailure(stderr string) Failure {
	for _, m := range failureMarks {
		if strings.Contains(stderr, m.mark) {
			return m.kind
		}
	}
	return FailureNone
}

// String says what's wrong, to finish "meterm: giving up on web1: ".
func (f Failure) String() string {
	switch f {
	case FailureAuth:
		return "it refused the key"
	case FailureHostKey:
		return "its host key doesn't match the trusted one"
	case FailureUnknownHost:
		return "its host key isn't trusted yet; run `meterm connect` on it once to check and accept it"
	case FailurePortInUse:
		return "a port it forwards is already in use"
	case FailureConfig:
		return "ssh can't use these settings"
	}
	return "no connection"
}

// Tail is an io.Writer that keeps only the last Max bytes written, for reading
// a long-running ssh's last words without holding everything it ever said.
type Tail struct {
	Max int
	buf []byte
}

// Write never fails, so it can sit in an io.MultiWriter next to the terminal.
func (t *Tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.Max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.Max:]...)
	}
	return len(p), nil
}

// Reset forgets what was written.
func (t *Tail) Reset() { t.buf = t.buf[:0] }

func (t *Tail) String() string { return string(t.buf) }
