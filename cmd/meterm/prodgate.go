package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"meterm/internal/config"
	"meterm/internal/prodask"
)

// stdinTerminal says a question can be asked; tests replace it.
var stdinTerminal = func() bool { return isTerminal(os.Stdin) }

// Notes the activity log keeps about how a run on production went ahead.
const (
	prodAsked       = "production, asked"
	prodPickerAsked = "production, picker asked"
	prodYes         = "production, --yes"
	prodNoTerminal  = "production, no prompt: not a terminal"
	prodNamed       = "production, named on the command line"
	prodNotAsked    = "production, picker didn't ask"
)

// pickerAnswer is the picker's production answer for this process, taken
// (prodask.Consume) before any command runs, so every command the picker
// started uses up its token whether it logs anything or not, and its
// children never inherit it. nil until taken; tests call the gate without
// taking it, and then each call consumes on its own.
var pickerAnswer *pickerAnswerT

type pickerAnswerT struct {
	from, asked bool
	why         string
}

// takePickerAnswer consumes the picker's token, if any, for this process.
func takePickerAnswer() {
	a := consumePicker()
	pickerAnswer = &a
}

func consumePicker() pickerAnswerT {
	from := os.Getenv(fromTUIEnv) != "" || os.Getenv(prodask.PathEnv) != ""
	asked, why := prodask.Consume()
	return pickerAnswerT{from: from, asked: asked, why: why}
}

// pickerAsked is the picker's answer: the one taken for this process, or,
// when none was (tests), a fresh Consume.
func pickerAsked() pickerAnswerT {
	if pickerAnswer != nil {
		return *pickerAnswer
	}
	return consumePicker()
}

// productionGate asks before what's about to run (what) reaches the
// production hosts in prod, y only. It doesn't ask when the picker that
// started this command asked already and left a one-use token saying so
// (prodask), nor with --yes. METERM_FROM_TUI alone skips nothing: any child
// of the picker has it. When stdin isn't a terminal nothing can answer: it
// goes ahead, says so on stderr, and the note says so in the log. It returns
// that note ("" when nothing was production) and whether to go on. A tool,
// not a guard: a script that means it isn't stopped, and a same-user
// process can forge the token.
func productionGate(prod []config.Host, what string, yes bool) (string, bool) {
	if len(prod) == 0 {
		return "", true
	}
	return gateWith(pickerAsked(), prod, what, yes)
}

// tunnelGate is productionGate for opening tunnel f on h: when h is neither
// production nor reached through it but f's name or note says it leads to
// production, the question says that, not that h is production, and the
// note says "tunnel to production". Otherwise it's productionGate's.
func tunnelGate(cfg *config.Config, h config.Host, f config.Forward, what string, yes bool) (string, bool) {
	switch {
	case cfg.TouchesProduction(h):
		note, ok := productionGate([]config.Host{h}, what, yes)
		if note != "" && !cfg.IsProduction(h) {
			note = "via " + note
		}
		return note, ok
	case !f.MentionsProduction():
		return "", true
	}
	note, ok := gateWith(pickerAsked(), nil, what, yes)
	if note != "" {
		note = "tunnel to " + note
	}
	return note, ok
}

// gateWith asks about prod, or, when prod is empty, about a tunnel that
// leads to production from a host that isn't.
func gateWith(p pickerAnswerT, prod []config.Host, what string, yes bool) (string, bool) {
	if p.asked {
		return prodPickerAsked, true
	}
	if p.from {
		fmt.Fprintf(os.Stderr, "meterm: the picker's answer doesn't count here (%s)\n", p.why)
	}
	names := make([]string, len(prod))
	for i, h := range prod {
		names[i] = exactName(h.Name)
	}
	where := fmt.Sprintf("%d PRODUCTION host(s): %s", len(prod), strings.Join(names, ", "))
	if len(prod) == 0 {
		where = "PRODUCTION: the tunnel leads there"
	}
	switch {
	case yes:
		fmt.Fprintf(os.Stderr, "⚠ %s reaches %s.\n", what, where)
		return prodYes, true
	case !stdinTerminal():
		fmt.Fprintf(os.Stderr, "⚠ %s reaches %s; not asking (stdin isn't a terminal).\n", what, where)
		return prodNoTerminal, true
	}
	fmt.Printf("⚠ %s reaches %s.\n", what, where)
	fmt.Print("Go on? Only y goes. [y/N] ")
	resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(resp) != "y" && strings.TrimSpace(resp) != "Y" {
		return "", false // the caller returns errCancelled, which says so
	}
	return prodAsked, true
}

// productionOf is the hosts among hosts that are production or are reached
// through it (config.TouchesProduction).
func productionOf(cfg *config.Config, hosts []config.Host) []config.Host {
	var out []config.Host
	for _, h := range hosts {
		if cfg.TouchesProduction(h) {
			out = append(out, h)
		}
	}
	return out
}

// loginKey tells logins apart: deploy@API and API are two.
func loginKey(h config.Host) string { return h.User + "@" + h.Name }

// productionNotes decides how a command on hosts goes ahead where it touches
// production, and returns the note each host's start event carries ("" for
// one that doesn't touch production) and whether to go on. Hosts in named
// (by loginKey) were named on the command line, which is the person asking
// for them: they aren't asked about again. The others (swept in by --all,
// --tag or --group, or an rsync --delete) are asked about, y only, unless
// the picker that started this command asked already, or --yes. A host that
// isn't production itself but is reached through it gets the same note with
// "via " in front.
func productionNotes(cfg *config.Config, hosts []config.Host, named map[string]bool, what string, yes bool) (func(config.Host) string, bool) {
	none := func(config.Host) string { return "" }
	prod := productionOf(cfg, hosts)
	if len(prod) == 0 {
		return none, true
	}
	p := pickerAsked()
	var swept []config.Host
	for _, h := range prod {
		if !named[loginKey(h)] {
			swept = append(swept, h)
		}
	}
	namedNote, sweptNote := "", ""
	if len(swept) < len(prod) {
		namedNote = namedHow(p)
	}
	if len(swept) > 0 {
		var ok bool
		if sweptNote, ok = gateWith(p, swept, what, yes); !ok {
			return none, false
		}
	}
	isProd := map[string]bool{}
	for _, h := range prod {
		isProd[loginKey(h)] = true
	}
	return func(h config.Host) string {
		if !isProd[loginKey(h)] {
			return ""
		}
		note := namedNote
		if !named[loginKey(h)] {
			note = sweptNote
		}
		if !cfg.IsProduction(h) {
			note = "via " + note
		}
		return note
	}, true
}

// namedHow is the note for production a person named on the command line:
// the picker asked, the picker started this without asking (said once on
// stderr), or the command line itself.
func namedHow(p pickerAnswerT) string {
	switch {
	case p.asked:
		return prodPickerAsked
	case p.from:
		if !warnedPicker {
			warnedPicker = true
			fmt.Fprintf(os.Stderr, "meterm: the picker's answer doesn't count here (%s)\n", p.why)
		}
		return prodNotAsked
	}
	return prodNamed
}

var warnedPicker bool

// namedNote is the production note for a command on the one host h that the
// command line named: "" when it doesn't touch production. leads says a
// tunnel it opens leads to production (its name or note says so) when h
// itself is neither production nor reached through it.
func namedNote(cfg *config.Config, h config.Host, leads bool) string {
	prefix := ""
	switch {
	case cfg.IsProduction(h):
	case cfg.TouchesProduction(h):
		prefix = "via "
	case leads:
		prefix = "tunnel to "
	default:
		return ""
	}
	return prefix + namedHow(pickerAsked())
}

// namedOnly is the named set for a command that names its one host.
func namedOnly(h config.Host) map[string]bool { return map[string]bool{loginKey(h): true} }

// tookText is how long something took, for the log: tenths under ten
// seconds (most commands), whole seconds and minutes after.
func tookText(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return shortDuration(d)
}

// withNote puts a note after what it is about: "failed: files /srv (production, --yes)".
func withNote(outcome, note string) string {
	if note == "" {
		return outcome
	}
	return outcome + " (" + note + ")"
}

// noteDetail is a start event's detail with its production note first, where a
// cut detail still shows it: "(production, --yes) ls (+2 args)". How the run
// ended goes in its own outcome event, not here.
func noteDetail(note, detail string) string {
	switch {
	case note == "":
		return detail
	case detail == "":
		return "(" + note + ")"
	}
	return "(" + note + ") " + detail
}
