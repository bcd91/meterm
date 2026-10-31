package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// recordActivity notes an action in the local activity log. Best-effort like
// history: a broken log warns but never blocks reaching the host.
func recordActivity(cfg *config.Config, h config.Host, action, detail string) {
	r := cfg.Resolve(h)
	target := r.Address
	if r.User != "" {
		target = r.User + "@" + r.Address
	}
	if err := activity.Record(action, h.Name, target, detail); err != nil {
		fmt.Fprintf(os.Stderr, "meterm: warning: activity log not updated: %v\n", err)
	}
}

// envWordRE is a leading NAME=value word, which says nothing about what ran and
// is where a secret would be.
var envWordRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandDetail is what the log says about a command run on a host. The log goes to
// the team and can't forget, and a command's arguments can hold anything: a pasted
// prompt, or a secret in a form redaction doesn't know. So by default it keeps what
// was run and not what with; logArgs asks for the arguments too, redacted as all
// detail is. One argument is a command line, whose first word is the program.
func commandDetail(command []string, logArgs bool) string {
	if logArgs {
		return strings.Join(command, " ")
	}
	words := command
	if len(words) == 1 {
		words = strings.Fields(words[0])
	}
	for len(words) > 0 && envWordRE.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	prog := words[0]
	if r := []rune(prog); len(r) > 64 {
		prog = string(r[:64]) + "…"
	}
	if n := len(words) - 1; n > 0 {
		return fmt.Sprintf("%s (+%d args)", prog, n)
	}
	return prog
}

func newActivityCmd() *cobra.Command {
	var (
		host   string
		limit  int
		format string
		verify bool
	)
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show what meterm was used to do on saved hosts (local, tamper-evident log)",
		Long: "Every connect, exec, cp, sync and sftp is appended to a local log where each\n" +
			"entry carries the hash of the one before it, so editing or removing an old entry\n" +
			"is detectable with --verify. Only actions taken through meterm are recorded;\n" +
			"a plain `ssh` is not.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := activity.DefaultDir()
			if err != nil {
				return err
			}
			l, err := activity.Open(dir)
			if err != nil {
				return err
			}
			events, err := l.All()
			if err != nil {
				return err
			}

			if verify {
				if broken := activity.Verify(events); broken != nil {
					return broken
				}
				fmt.Printf("OK: %d event(s), chain intact\n", len(events))
				return nil
			}

			var shown []activity.Event
			for _, e := range events {
				if host == "" || e.Host == host {
					shown = append(shown, e)
				}
			}
			if limit > 0 && len(shown) > limit {
				shown = shown[len(shown)-limit:]
			}

			switch format {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if shown == nil {
					shown = []activity.Event{}
				}
				return enc.Encode(shown)
			case "text", "":
			default:
				return fmt.Errorf("unknown --format %q (use text or json)", format)
			}

			if len(shown) == 0 {
				fmt.Println("No activity recorded yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tUSER\tACTION\tHOST\tDETAIL")
			for _, e := range shown {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					e.Time.Local().Format("2006-01-02 15:04:05"), e.User, e.Action, e.Host, oneLine(e.Detail, 60))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only show actions on this saved host")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "show at most this many recent events (0 = all)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the hash chain instead of listing events")
	return cmd
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		return string([]rune(s)[:max-1]) + "…"
	}
	return s
}
