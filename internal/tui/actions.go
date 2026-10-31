package tui

import (
	"fmt"
	"sort"
	"strings"

	"meterm/internal/config"
)

// Commands is meterm's command tree for the palette's tab completion: each
// top-level command mapped to its subcommands (nil when it has none).
// cmd/meterm fills it from cobra, so completion never drifts from the CLI.
var Commands = map[string][]string{}

// HostAttention reports, per host name, what team sync is waiting on
// ("not pushed", "conflict", ...). cmd/meterm sets it; it's re-read after
// every dispatched command, since a push or resolve changes the answer.
var HostAttention func() map[string]string

// TeamNames maps team IDs to names, so each row can say which team a host
// is shared with. cmd/meterm sets it.
var TeamNames func() map[string]string

// hostFirst lists the commands whose first argument must be a host, so the
// palette fills in the selected host when it's left out. Commands where the
// host is optional (audit, keys list, route check, ...) are deliberately not
// here: without a host they act on every host, and the palette must not
// quietly narrow that. Destructive ones (rm, keys revoke) aren't either.
var hostFirst = map[string]bool{
	"connect": true, "edit": true, "files": true, "stats": true,
	"copy-id": true, "logs": true, "logs list": true, "tunnel up": true,
}

// execKeepsLine gives exec what follows its -- as typed: a line typed here is a
// command line for the host, pipes and quotes included, and exec would quote separate
// arguments into plain text.
func execKeepsLine(value string, args []string) []string {
	if len(args) == 0 || args[0] != "exec" {
		return args
	}
	for i, a := range args {
		if a == "--" {
			if _, line, ok := strings.Cut(value, " -- "); ok && i+2 < len(args) {
				return append(append([]string(nil), args[:i+1]...), strings.TrimSpace(line))
			}
			return args
		}
	}
	return args
}

// withSelectedHost puts host into args when the command needs a host and
// none was given: `logs` becomes `logs <host>`, `exec -- uptime` becomes
// `exec <host> -- uptime`. Anything else comes back unchanged.
func withSelectedHost(args []string, host string, hostNames map[string]bool) []string {
	if host == "" || strings.HasPrefix(host, "-") || len(args) == 0 {
		return args
	}
	path := 1
	if len(args) > 1 && hostFirst[args[0]+" "+args[1]] {
		path = 2
	} else if !hostFirst[args[0]] && args[0] != "exec" {
		return args
	}
	rest := args[path:]
	if args[0] == "exec" {
		// exec picks hosts by name, --tag, --group or --all; only an exec with
		// none of those before the -- gets the selected host.
		if len(rest) > 0 && rest[0] != "--" {
			return args
		}
	} else if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && !strings.HasPrefix(rest[0], "/") {
		return args // a host is already there (or something we'd only guess at)
	} else {
		for _, a := range rest {
			if a == "--" {
				break
			}
			if !strings.HasPrefix(a, "-") && hostNames[a] {
				return args // a host after a flag: `logs -f web1`
			}
		}
	}
	out := append([]string(nil), args[:path]...)
	out = append(out, host)
	return append(out, rest...)
}

// completion is the palette's tab completion of the word being typed.
// It returns the new input value and, when the word is ambiguous, the
// candidates to show.
func complete(value string, cfg *config.Config) (string, []string) {
	if strings.ContainsAny(value, `'"`) {
		return value, nil // inside quoting: not a word we can reason about
	}
	fields := strings.Fields(value)
	cur := ""
	if len(fields) > 0 && !strings.HasSuffix(value, " ") {
		cur = fields[len(fields)-1]
		fields = fields[:len(fields)-1]
	}
	for _, f := range fields {
		if f == "--" {
			return value, nil // the remote command: not ours to complete
		}
	}
	if strings.HasPrefix(cur, "-") {
		return value, nil
	}

	var cands []string
	suffix := " "
	switch {
	case len(fields) == 0:
		for c := range Commands {
			cands = append(cands, c)
		}
	case len(fields) == 1 && len(Commands[fields[0]]) > 0:
		cands = Commands[fields[0]]
	case len(fields) >= 2 && fields[0] == "tunnel" && fields[1] == "up":
		for _, h := range cfg.Hosts {
			cands = append(cands, h.Name)
			for _, f := range h.Forwards {
				if f.Name != "" {
					cands = append(cands, h.Name+"/"+f.Name)
				}
			}
		}
	case fields[0] == "cp":
		if strings.Contains(cur, ":") {
			return value, nil // a remote path: we don't list remote files
		}
		for _, h := range cfg.Hosts {
			cands = append(cands, h.Name+":")
		}
		suffix = ""
	default:
		for _, h := range cfg.Hosts {
			cands = append(cands, h.Name)
		}
	}

	var match []string
	for _, c := range cands {
		if strings.HasPrefix(strings.ToLower(c), strings.ToLower(cur)) {
			match = append(match, c)
		}
	}
	sort.Strings(match)
	head := value[:len(value)-len(cur)]
	switch len(match) {
	case 0:
		return value, nil
	case 1:
		return head + match[0] + suffix, nil
	}
	lcp := match[0]
	for _, m := range match[1:] {
		for !strings.HasPrefix(strings.ToLower(m), strings.ToLower(lcp)) {
			lcp = lcp[:len(lcp)-1]
		}
	}
	if len(lcp) < len(cur) {
		lcp = cur
	}
	return head + lcp, match
}

// choice is one row of a picker (the tunnel picker): what it shows and the
// command it runs.
type choice struct {
	label string
	warn  bool // reaches production
	args  []string
}

// tunnelChoices is what the tunnel picker offers for h: each named tunnel,
// and all of them at once when there's more than one. prod says h itself is a
// production host, which makes every one of its tunnels a warning.
func tunnelChoices(h config.Host, prod bool) []choice {
	var out []choice
	for _, f := range h.Forwards {
		if f.Name == "" {
			continue
		}
		label := f.Name + "  " + forwardSpec(f)
		if f.Note != "" {
			label += "  " + f.Note
		}
		out = append(out, choice{
			label: printable(label),
			warn:  prod || f.MentionsProduction(),
			args:  []string{"tunnel", "up", h.Name + "/" + f.Name},
		})
	}
	if len(out) > 1 {
		warn := false
		for _, c := range out {
			warn = warn || c.warn
		}
		out = append(out, choice{label: "all of them", warn: warn, args: []string{"tunnel", "up", h.Name}})
	}
	return out
}

func forwardSpec(f config.Forward) string {
	switch f.Type {
	case "D":
		return fmt.Sprintf("-D %d", f.LocalPort)
	default:
		return fmt.Sprintf("-%s %d:%s:%d", f.Type, f.LocalPort, f.RemoteHost, f.RemotePort)
	}
}

// printable drops control characters, so text that came from a team (a
// note, a tunnel name) can't move the cursor or recolour the terminal.
// Team sync already refuses such text; this is the second lock.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// keyAction is the command a per-host shortcut runs for host.
func keyAction(k, host string) []string {
	switch k {
	case "A":
		return []string{"audit", host}
	case "R":
		return []string{"route", "check", host}
	case "w":
		return []string{"keys", "list", host}
	case "L":
		return []string{"logs", host}
	}
	return nil
}

// execPrefill is what `x` puts in the palette: exec on the marked hosts, or
// on the selected one when nothing is marked.
func execPrefill(marked []string, selected string) string {
	hosts := marked
	if len(hosts) == 0 {
		if selected == "" {
			return "exec "
		}
		hosts = []string{selected}
	}
	return "exec " + strings.Join(hosts, " ") + " -- "
}

// loginChoices is one row per account people log in to h as (its own
// user first); flags go after the host, e.g. --tmux.
func loginChoices(h config.Host, flags []string) []choice {
	first := h.Name + "  (its default user)"
	if h.User != "" {
		first = h.User + "@" + h.Name + "  (default)"
	}
	out := []choice{{label: printable(first), args: append([]string{"connect", h.Name}, flags...)}}
	for _, u := range h.Users {
		if u == h.User || u == "" {
			continue
		}
		t := u + "@" + h.Name
		out = append(out, choice{label: printable(t), args: append([]string{"connect", t}, flags...)})
	}
	return out
}

// shellish reports whether a palette line uses shell syntax the palette
// doesn't do: no shell runs here, so $(...), `...`, $VAR, a backslash or a
// leading ~/ reach meterm (and the host) exactly as typed — and if the host's
// shell expands them, it does so with the host's values, not this machine's.
func shellish(s string) bool {
	return strings.Contains(s, "$") || strings.Contains(s, "`") || strings.Contains(s, `\`) ||
		strings.Contains(s, " ~/") || strings.HasPrefix(s, "~/")
}
