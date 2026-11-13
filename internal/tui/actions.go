package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/termsafe"
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
func complete(value string, cfg *config.Config, refs map[string]string) (string, []string) {
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
			cands = append(cands, typedName(h.Name, refs))
			for _, f := range h.Forwards {
				if f.Name != "" {
					cands = append(cands, typedName(h.Name+"/"+f.Name, refs))
				}
			}
		}
	case fields[0] == "cp":
		if strings.Contains(cur, ":") {
			return value, nil // a remote path: we don't list remote files
		}
		for _, h := range cfg.Hosts {
			cands = append(cands, typedName(h.Name, refs)+":")
		}
		suffix = ""
	default:
		for _, h := range cfg.Hosts {
			cands = append(cands, typedName(h.Name, refs))
		}
	}

	var match []string
	for _, c := range cands {
		low := strings.ToLower(c)
		if strings.HasPrefix(low, strings.ToLower(cur)) || strings.HasPrefix(low, "‹"+strings.ToLower(cur)) {
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
			label: termsafe.Label(label),
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

// keyAction is the command a per-host shortcut runs for host.
func keyAction(k, host string) []string {
	switch k {
	case "A":
		return []string{"audit", host}
	case "C":
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
func execPrefill(marked []string, selected string, refs map[string]string) string {
	hosts := marked
	if len(hosts) == 0 {
		if selected == "" {
			return "exec "
		}
		hosts = []string{selected}
	}
	shown := make([]string, len(hosts))
	for k, h := range hosts {
		shown[k] = typedName(h, refs)
	}
	return "exec " + strings.Join(shown, " ") + " -- "
}

// typedName is a host's name as the picker writes it into the palette, so
// that what Enter runs is that host and no other: the name itself when it's
// plain, otherwise a reference ‹...› (the name with its hidden characters
// spelled out, e.g. ‹zw\u200bsp›) recorded in refs, which splitArgs swaps
// back for the exact name. A name with hidden characters is never shown as
// the name it looks like: "zwsp" and "zw\u200bsp" are two hosts, and a
// cleaned name would run on the wrong one (see config.Lookup). Typed text
// is never decoded: only a reference the picker wrote stands for a name.
func typedName(name string, refs map[string]string) string {
	if name != "" && termsafe.Label(name) == name && !strings.ContainsAny(name, " '\"\\‹›") {
		return name
	}
	tok := refToken(name)
	if refs != nil {
		refs[tok] = name
	}
	return tok
}

// refToken is the reference typedName writes for name: one token, no
// spaces, quotes, ‹ or › inside, different for every name.
func refToken(name string) string {
	q := strconv.Quote(name)
	q = q[1 : len(q)-1]
	q = strings.ReplaceAll(q, "‹", `\u2039`)
	q = strings.ReplaceAll(q, "›", `\u203a`)
	q = strings.ReplaceAll(q, " ", `\x20`)
	q = strings.ReplaceAll(q, `\"`, `\x22`)
	q = strings.ReplaceAll(q, "'", `\x27`)
	return "‹" + q + "›"
}

// refAt is the ‹...› starting at s[i], or "" when there's none.
func refAt(s string, i int) string {
	if !strings.HasPrefix(s[i:], "‹") {
		return ""
	}
	j := strings.Index(s[i+len("‹"):], "›")
	if j < 0 {
		return ""
	}
	return s[i : i+len("‹")+j+len("›")]
}

// exactArg is an argument as the picker shows it: as it is when that's
// what it is, Go-quoted when it holds a character that wouldn't show.
func exactArg(a string) string {
	if termsafe.Label(a) == a {
		return a
	}
	return strconv.Quote(a)
}

// exactLine is args as the picker shows a command it will run.
func exactLine(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = exactArg(a)
	}
	return strings.Join(out, " ")
}

// loginChoices is one row per account people log in to h as (its own
// user first); flags go after the host, e.g. --tmux. prod marks every row:
// any login to a production host is one.
func loginChoices(h config.Host, flags []string, prod bool) []choice {
	first := h.Name + "  (its default user)"
	if h.User != "" {
		first = h.User + "@" + h.Name + "  (default)"
	}
	out := []choice{{label: termsafe.Label(first), warn: prod, args: append([]string{"connect", h.Name}, flags...)}}
	for _, u := range h.Users {
		if u == h.User || u == "" {
			continue
		}
		t := u + "@" + h.Name
		out = append(out, choice{label: termsafe.Label(t), warn: prod, args: append([]string{"connect", t}, flags...)})
	}
	return out
}

// shellish reports whether a palette line uses shell syntax the palette
// doesn't do: no shell runs here, so $(...), `...`, $VAR, a backslash or a
// leading ~/ reach meterm (and the host) exactly as typed — and if the host's
// shell expands them, it does so with the host's values, not this machine's.
func shellish(s string, refs map[string]string) bool {
	head, tail, cut := strings.Cut(s, " -- ")
	s = withoutRefs(head, refs)
	if cut {
		s += " -- " + tail // the host's command: its quotes are the host's
	}
	return strings.Contains(s, "$") || strings.Contains(s, "`") || strings.Contains(s, `\`) ||
		strings.Contains(s, " ~/") || strings.HasPrefix(s, "~/")
}

// withoutRefs is s with each reference the picker wrote (typedName) left
// out: a host name it spelled out isn't shell syntax.
func withoutRefs(s string, refs map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if tok := refAt(s, i); tok != "" {
			if _, ok := refs[tok]; ok {
				i += len(tok) - 1
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// localCommands are the commands that stay on this machine: they read or
// change meterm's own files, or only dial a port, and run nothing on a host.
// Anything not listed here is taken to run something on the hosts it names.
var localCommands = map[string]bool{
	"activity": true, "add": true, "edit": true, "rm": true, "remove": true, "delete": true,
	"rename": true, "group": true, "import": true, "export": true, "history": true,
	"list": true, "ls": true, "report": true, "team": true, "keygen": true, "status": true,
	"help": true, "version": true, "completion": true, "wake": true,
	"route add": true, "route list": true, "route ls": true, "route rm": true, "route check": true,
	"tunnel add": true, "tunnel list": true, "tunnel ls": true, "tunnel rm": true,
	"tunnel disable": true, "audit audits": true,
	// tunnel enable isn't: it keeps a tunnel open from every login on, so on
	// a production host it asks, as t (a one-off tunnel up) does.
}

// productionTargets is the hosts a command line would reach that are
// production or are reached through it (config.TouchesProduction):
// hosts it names (as name, user@name, name:path or name/tunnel), and for
// exec, the ones --all, --tag or --group pick. A command that reaches hosts
// but names none (audit, keys list) covers every host. Commands that stay
// on this machine reach none.
func productionTargets(args []string, cfg *config.Config) []string {
	if len(args) == 0 || localCommands[args[0]] || (len(args) > 1 && localCommands[args[0]+" "+args[1]]) {
		return nil
	}
	byName := map[string]config.Host{}
	for _, h := range cfg.Hosts {
		byName[h.Name] = h
	}
	picked := map[string]bool{}
	named := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if args[0] == "exec" {
			flag, val, hasVal := strings.Cut(a, "=")
			if (flag == "--tag" || flag == "--group") && !hasVal && i+1 < len(args) {
				i++
				val, hasVal = args[i], true
			}
			switch {
			case a == "--all":
				for _, h := range cfg.Hosts {
					picked[h.Name] = true
				}
				named = true
				continue
			case flag == "--tag" && hasVal:
				for _, h := range cfg.Hosts {
					for _, t := range h.Tags {
						if t == val {
							picked[h.Name] = true
						}
					}
				}
				named = true
				continue
			case flag == "--group" && hasVal:
				for _, h := range cfg.Hosts {
					if h.Group == val {
						picked[h.Name] = true
					}
				}
				named = true
				continue
			}
		}
		name := a
		if at := strings.LastIndex(name, "@"); at >= 0 {
			name = name[at+1:]
		}
		if c := strings.IndexAny(name, ":/"); c >= 0 {
			name = name[:c]
		}
		if _, ok := byName[name]; ok {
			picked[name] = true
			named = true
		} else if h, i, _ := cfg.Lookup(name); i >= 0 {
			picked[h.Name] = true // named as shown: a name with control characters
			named = true
		}
	}
	if !named {
		for _, h := range cfg.Hosts {
			picked[h.Name] = true
		}
	}
	var out []string
	for _, h := range cfg.Hosts {
		if picked[h.Name] && cfg.TouchesProduction(h) {
			out = append(out, h.Name)
		}
	}
	return out
}

// RemovedDetail is what the activity log says about a host removed from the
// inventory; `meterm rm` and the picker's delete both write it.
func RemovedDetail(prod, team bool) string {
	d := "removed from the host file"
	switch {
	case prod && team:
		d += " (a production host, shared with a team)"
	case prod:
		d += " (a production host)"
	case team:
		d += " (shared with a team)"
	}
	return d
}

// logRemoved notes in the activity log that h was deleted from the picker.
func logRemoved(cfg *config.Config, h config.Host) error {
	r := cfg.Resolve(h)
	target := r.Address
	if r.User != "" {
		target = r.User + "@" + r.Address
	}
	if _, err := activity.Record("rm", h.Name, target, RemovedDetail(cfg.IsProduction(h), h.Team != "")); err != nil {
		return err
	}
	activity.SendToSink(cfg.Activity.Sink)
	return nil
}

// isSession says args is an interactive ssh session (connect and its
// aliases), whose exit code is the remote shell's unless it's ssh's 255.
func isSession(args []string) bool {
	return len(args) > 1 && (args[0] == "connect" || args[0] == "c" || args[0] == "ssh")
}

// SessionOutcomeEnv names a file connect writes to once a session it ran
// has ended (not when meterm or ssh failed first): both exit 1 alike, and
// only one of them is a failure.
const SessionOutcomeEnv = "METERM_SESSION_OUTCOME"

// outcomeFile is where a dispatched connect says its session ran: a file in
// a directory of the picker's own, made on first use; "" when it can't be.
func (m *model) outcomeFile() string {
	if m.outcomeDir == "" {
		d, err := os.MkdirTemp("", "meterm-tui-")
		if err != nil {
			return ""
		}
		m.outcomeDir = d
	}
	return filepath.Join(m.outcomeDir, "session")
}

// sessionRan reads, and removes, what the last dispatched connect left: true
// when it says its session ran to an end.
func (m *model) sessionRan() bool {
	if m.outcomeDir == "" {
		return false
	}
	p := filepath.Join(m.outcomeDir, "session")
	b, err := os.ReadFile(p)
	_ = os.Remove(p)
	return err == nil && isSession(m.running) && strings.TrimSpace(string(b)) != ""
}

// JumpBrokenText says what deleting a jump host does to the hosts that go
// through it (config.JumpedBy): `meterm rm` and the picker both say it.
func JumpBrokenText(users []string) string {
	shown := make([]string, len(users))
	for i, u := range users {
		shown[i] = exactArg(u)
	}
	more := ""
	if len(shown) > 8 {
		shown, more = shown[:8], fmt.Sprintf(" and %d more", len(users)-8)
	}
	return fmt.Sprintf("%d host(s) jump through it: %s%s. Their jump will be broken: it will name a host meterm no longer has, "+
		"which ssh looks up by itself (DNS, ~/.ssh/config), maybe a different machine. Edit them first.",
		len(users), strings.Join(shown, ", "), more)
}

// tunnelLeadsToProduction says args is tunnel up or enable of a saved tunnel
// whose name or note says it reaches production, which the tunnel picker
// (t) asks about whatever the host's env.
func tunnelLeadsToProduction(args []string, cfg *config.Config) bool {
	if len(args) < 3 || args[0] != "tunnel" || (args[1] != "up" && args[1] != "enable") {
		return false
	}
	for _, a := range args[2:] {
		name, tun, ok := strings.Cut(a, "/")
		if !ok {
			continue
		}
		if h, i, _ := cfg.Lookup(name); i >= 0 {
			for _, f := range h.Forwards {
				if f.Name == tun && f.MentionsProduction() {
					return true
				}
			}
		}
	}
	return false
}

// shownName is a host's name as a row shows it: as it is when that's what
// it is, otherwise spelled out and quoted (exactArg), as `meterm list` does,
// so two names that differ only by a hidden character don't look alike.
func shownName(name string) string { return exactArg(name) }
