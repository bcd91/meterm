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
)

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
	fromPicker := os.Getenv(fromTUIEnv) != "" || os.Getenv(prodask.PathEnv) != ""
	asked, why := prodask.Consume()
	if asked {
		return prodPickerAsked, true
	}
	if fromPicker {
		fmt.Fprintf(os.Stderr, "meterm: the picker's answer doesn't count here (%s)\n", why)
	}
	names := make([]string, len(prod))
	for i, h := range prod {
		names[i] = exactName(h.Name)
	}
	list := strings.Join(names, ", ")
	switch {
	case yes:
		fmt.Fprintf(os.Stderr, "⚠ %s on %d PRODUCTION host(s): %s.\n", what, len(prod), list)
		return prodYes, true
	case !stdinTerminal():
		fmt.Fprintf(os.Stderr, "⚠ %s on %d PRODUCTION host(s), not asking (stdin isn't a terminal): %s.\n", what, len(prod), list)
		return prodNoTerminal, true
	}
	fmt.Printf("⚠ %s reaches %d PRODUCTION host(s): %s.\n", what, len(prod), list)
	fmt.Print("Go on? Only y goes. [y/N] ")
	resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(resp) != "y" && strings.TrimSpace(resp) != "Y" {
		fmt.Println("Cancelled.")
		return "", false
	}
	return prodAsked, true
}

// productionOf is the production hosts among hosts.
func productionOf(cfg *config.Config, hosts []config.Host) []config.Host {
	var out []config.Host
	for _, h := range hosts {
		if cfg.IsProduction(h) {
			out = append(out, h)
		}
	}
	return out
}

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
