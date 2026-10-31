package sshrun

import (
	"strings"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	for in, want := range map[string]Failure{
		"bcd@127.0.0.1: Permission denied (publickey).":         FailureAuth,
		"Received disconnect: Too many authentication failures": FailureAuth,
		"Host key verification failed.":                         FailureUnknownHost,
		"No ED25519 host key is known for [127.0.0.1]:22 and you have requested strict checking.\nHost key verification failed.": FailureUnknownHost,
		"@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @\nHost key verification failed.":                                    FailureHostKey,
		"Host key for 10.0.0.1 has changed and you have requested strict checking.\nHost key verification failed.":               FailureHostKey,
		"bind [127.0.0.1]:5432: Address already in use\nchannel_setup_fwd_listener_tcpip: cannot listen to port: 5432":           FailurePortInUse,
		"Could not request local forwarding.":                           FailurePortInUse,
		"Error: remote port forwarding failed for listen port 8080":     FailurePortInUse,
		"@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @":          FailureHostKey,
		"Unable to negotiate with 10.0.0.1 port 22: no matching cipher": FailureConfig,
		"ssh: connect to host 10.0.0.1 port 22: Connection refused":     FailureNone,
		"ssh: connect to host 10.0.0.1 port 22: Permission denied":      FailureNone,
		"Connection timed out during banner exchange":                   FailureNone,
		"client_loop: send disconnect: Broken pipe":                     FailureNone,
		"Permission denied, please try again.":                          FailureNone,
		"":                                                              FailureNone,
	} {
		if got := ClassifyFailure(in); got != want {
			t.Errorf("%q → %v, want %v", in, got, want)
		}
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	tail := &Tail{Max: 5}
	for _, s := range []string{"abc", "defgh", strings.Repeat("x", 3)} {
		if n, err := tail.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q → %d, %v", s, n, err)
		}
	}
	if got := tail.String(); got != "ghxxx" {
		t.Errorf("tail = %q", got)
	}
	tail.Reset()
	if tail.String() != "" {
		t.Error("reset kept something")
	}
}
