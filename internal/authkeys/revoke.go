package authkeys

import (
	"context"
	"fmt"
	"strings"
)

// HostKeys is what ListScript found on one host, or why it couldn't run.
type HostKeys struct {
	Host    string  `json:"host"`
	Error   string  `json:"error,omitempty"`
	Sudo    bool    `json:"sudo"`
	Entries []Entry `json:"keys"`
}

// FileRevoke is the lines to remove from one authorized_keys file.
type FileRevoke struct {
	Host    string
	File    string
	Own     bool // the login user's file: edited without sudo
	Entries []Entry
}

// Problem is something to tell the user before revoking. A lockout blocks
// the revoke unless forced.
type Problem struct {
	Msg     string
	Lockout bool
}

// Plan finds the lines m matches and anything that makes removing them
// unsafe: a key this machine holds (mine, by fingerprint), the last key in
// a login user's own file, or hosts that couldn't be checked fully.
func Plan(res []HostKeys, m Matcher, mine map[string]bool) ([]FileRevoke, []Problem) {
	var plan []FileRevoke
	var problems []Problem
	for _, hk := range res {
		if hk.Error != "" {
			problems = append(problems, Problem{Msg: fmt.Sprintf("%s: couldn't check (%s)", hk.Host, hk.Error)})
			continue
		}
		byFile := map[string]*FileRevoke{}
		total := map[string]int{}
		var order []string
		for _, e := range hk.Entries {
			total[e.File]++
			if !m.Match(e) {
				continue
			}
			f, ok := byFile[e.File]
			if !ok {
				f = &FileRevoke{Host: hk.Host, File: e.File, Own: e.Own}
				byFile[e.File] = f
				order = append(order, e.File)
			}
			f.Entries = append(f.Entries, e)
			if mine[e.Fingerprint] {
				problems = append(problems, Problem{Lockout: true,
					Msg: fmt.Sprintf("%s: %s is a key on this machine — revoking it can lock you out", hk.Host, e.Fingerprint)})
			}
		}
		for _, file := range order {
			f := byFile[file]
			if f.Own && len(f.Entries) == total[file] {
				problems = append(problems, Problem{Lockout: true,
					Msg: fmt.Sprintf("%s: it's the last key in %s, the file meterm logs in with", hk.Host, file)})
			}
			plan = append(plan, *f)
		}
		if !hk.Sudo {
			problems = append(problems, Problem{Msg: fmt.Sprintf("%s: no passwordless sudo, so only the login user's keys were checked", hk.Host)})
		}
	}
	return plan, problems
}

// Blocked is the first lockout among problems, if any.
func Blocked(problems []Problem) (Problem, bool) {
	for _, p := range problems {
		if p.Lockout {
			return p, true
		}
	}
	return Problem{}, false
}

// Runner pipes a script to `sh -s` on a host and returns what it printed.
type Runner func(ctx context.Context, host, script string) (string, error)

// Result is how revoking from one file went.
type Result struct {
	FileRevoke
	Backup string // where the file was copied first
	Err    error
}

// Apply removes the planned lines, one file at a time, backing each file up
// as <file>.meterm-bak-<stamp> first.
func Apply(ctx context.Context, plan []FileRevoke, run Runner, stamp string) []Result {
	out := make([]Result, 0, len(plan))
	for _, f := range plan {
		res := Result{FileRevoke: f, Backup: f.File + ".meterm-bak-" + stamp}
		var blobs []string
		for _, e := range f.Entries {
			blobs = append(blobs, e.Blob)
		}
		script, err := RevokeScript(f.File, blobs, !f.Own, stamp)
		if err == nil {
			var said string
			said, err = run(ctx, f.Host, script)
			if err == nil && !strings.Contains(said, "revoked") {
				err = fmt.Errorf("%s", strings.TrimSpace(said))
			}
		}
		res.Err = err
		out = append(out, res)
	}
	return out
}
