package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/authkeys"
	"meterm/internal/config"
	"meterm/internal/sshrun"
)

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "See which SSH keys can log in to your hosts, and revoke them",
		Long: "`keys list` reads authorized_keys on each host (root's and other users' too\n" +
			"when passwordless sudo works) and shows who can get in. `keys revoke` takes a\n" +
			"key away everywhere at once — the offboarding step that's easy to forget.",
	}
	cmd.AddCommand(newKeysListCmd(), newKeysRevokeCmd())
	return cmd
}

// scanKeys runs the read-only listing on every host, 10 at a time.
func scanKeys(cfg *config.Config, hosts []config.Host) []authkeys.HostKeys {
	out := make([]authkeys.HostKeys, len(hosts))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h config.Host) {
			defer wg.Done()
			defer func() { <-sem }()
			hk := authkeys.HostKeys{Host: h.Name}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			raw, err := runScript(ctx, cfg.Resolve(h), authkeys.ListScript)
			if err != nil {
				hk.Error = err.Error()
			} else {
				l := authkeys.Parse(raw)
				hk.Sudo, hk.Entries = l.Sudo, l.Entries
			}
			out[i] = hk
		}(i, h)
	}
	wg.Wait()
	return out
}

// hostRunner runs scripts on saved hosts by name.
func hostRunner(cfg *config.Config) authkeys.Runner {
	return func(ctx context.Context, name, script string) (string, error) {
		h, idx, err := cfg.Target(name)
		if err != nil {
			return "", err
		}
		if idx < 0 {
			return "", fmt.Errorf("no such host: %s", name)
		}
		return runScript(ctx, cfg.Resolve(*h), script)
	}
}

// runScript pipes script to `sh -s` on the host, unattended.
func runScript(ctx context.Context, r config.Resolved, script string) (string, error) {
	r.ExtraArgs = append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, r.ExtraArgs...)
	c := sshrun.SSHContext(ctx, r, "sh", "-s")
	c.Stdin = strings.NewReader(script)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return string(out), fmt.Errorf("%s", msg)
	}
	return string(out), nil
}

// localKeys is every public key this machine could log in with: the .pub
// files in ~/.ssh and whatever the agent holds. Revoking one of them is how
// you lock yourself out.
func localKeys() map[string]bool {
	fps := map[string]bool{}
	add := func(line string) {
		if k, err := authkeys.ParsePublicKey(line); err == nil {
			fps[k.Fingerprint] = true
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		pubs, _ := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
		for _, p := range pubs {
			if b, err := os.ReadFile(p); err == nil {
				add(strings.TrimSpace(string(b)))
			}
		}
	}
	if out, err := exec.Command("ssh-add", "-L").Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			add(l)
		}
	}
	return fps
}

func newKeysListCmd() *cobra.Command {
	var tags, groups []string
	var all, byKey bool
	var format string
	cmd := &cobra.Command{
		Use:   "list [host...]",
		Short: "List the keys in authorized_keys on hosts (read-only)",
		Long: "Shows every key that can log in to the selected hosts. Keys this machine\n" +
			"holds are marked \"you\". --by-key turns it around: each key once, with the\n" +
			"hosts it opens — the view to check before and after someone leaves.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := pickHostsOrAll(cfg, args, tags, groups, all)
			if err != nil {
				return err
			}
			res := scanKeys(cfg, hosts)
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			mine := localKeys()
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			if byKey {
				printKeysByKey(tw, res, mine)
			} else {
				fmt.Fprintln(tw, "HOST\tLOGIN AS\tTYPE\tFINGERPRINT\tCOMMENT\t")
				for _, hk := range res {
					if hk.Error != "" {
						fmt.Fprintf(tw, "%s\t-\t-\t-\terror: %s\t\n", hk.Host, hk.Error)
						continue
					}
					for _, e := range hk.Entries {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t\n", hk.Host, e.Owner, shortType(e.Type), e.Fingerprint, keyLabel(e, mine))
					}
				}
			}
			tw.Flush()
			noSudo := []string{}
			for _, hk := range res {
				if hk.Error == "" && !hk.Sudo {
					noSudo = append(noSudo, hk.Host)
				}
			}
			if len(noSudo) > 0 {
				fmt.Printf("\nOnly the login user's keys were readable on %s (no passwordless sudo): root's and other users' keys aren't shown.\n", strings.Join(noSudo, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "every saved host (the default when no host is named)")
	cmd.Flags().BoolVar(&byKey, "by-key", false, "one row per key, with the hosts it opens")
	cmd.Flags().StringVar(&format, "format", "text", "text or json")
	return cmd
}

func pickHostsOrAll(cfg *config.Config, names, tags, groups []string, all bool) ([]config.Host, error) {
	if len(names) == 0 && len(tags) == 0 && len(groups) == 0 {
		all = true
	}
	hosts, err := selectHosts(cfg, names, tags, groups, all)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no hosts matched")
	}
	return hosts, nil
}

func shortType(t string) string {
	return strings.TrimPrefix(strings.TrimPrefix(t, "ssh-"), "ecdsa-sha2-")
}

func keyLabel(e authkeys.Entry, mine map[string]bool) string {
	s := e.Comment
	if s == "" {
		s = "(no comment)"
	}
	if mine[e.Fingerprint] {
		s += "  [you]"
	}
	if e.Options != "" {
		s += "  {" + e.Options + "}"
	}
	return s
}

func printKeysByKey(tw *tabwriter.Writer, res []authkeys.HostKeys, mine map[string]bool) {
	type agg struct {
		e     authkeys.Entry
		hosts []string
	}
	by := map[string]*agg{}
	for _, hk := range res {
		for _, e := range hk.Entries {
			a, ok := by[e.Fingerprint]
			if !ok {
				a = &agg{e: e}
				by[e.Fingerprint] = a
			}
			where := hk.Host
			if !e.Own {
				where += "(" + e.Owner + ")"
			}
			a.hosts = append(a.hosts, where)
		}
	}
	list := make([]*agg, 0, len(by))
	for _, a := range by {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].hosts) != len(list[j].hosts) {
			return len(list[i].hosts) > len(list[j].hosts)
		}
		return list[i].e.Comment < list[j].e.Comment
	})
	fmt.Fprintln(tw, "KEY\tFINGERPRINT\tHOSTS\t")
	for _, a := range list {
		fmt.Fprintf(tw, "%s\t%s\t%d: %s\t\n", keyLabel(a.e, mine), a.e.Fingerprint, len(a.hosts), strings.Join(a.hosts, ", "))
	}
}

func newKeysRevokeCmd() *cobra.Command {
	var tags, groups, keyFiles, fps, comments []string
	var all, yes, force bool
	cmd := &cobra.Command{
		Use:   "revoke [host...] (--key file.pub | --fingerprint SHA256:… | --comment user@machine)",
		Short: "Remove a key from authorized_keys on every selected host",
		Long: "Finds the key on every selected host (all hosts when none are named),\n" +
			"shows exactly which lines go, and asks before changing anything. Each file\n" +
			"is backed up next to itself as authorized_keys.meterm-bak-<time> and\n" +
			"rewritten in place, keeping its owner and mode. Files of other users are\n" +
			"edited with `sudo -n`.\n\n" +
			"It refuses to remove a key this machine holds, or the last key in the\n" +
			"login user's own file, unless --force: either would lock you out.",
		Example: "  meterm keys revoke --comment bob@laptop\n" +
			"  meterm keys revoke --tag prod --key ~/offboarding/bob.pub\n" +
			"  meterm keys revoke --fingerprint SHA256:abc… --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			m := authkeys.Matcher{Comments: comments}
			for _, f := range keyFiles {
				b, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				for _, line := range strings.Split(string(b), "\n") {
					if strings.TrimSpace(line) == "" {
						continue
					}
					k, err := authkeys.ParsePublicKey(line)
					if err != nil {
						return fmt.Errorf("%s: %w", f, err)
					}
					m.Keys = append(m.Keys, k)
				}
			}
			for _, fp := range fps {
				if !strings.HasPrefix(fp, "SHA256:") {
					fp = "SHA256:" + fp
				}
				m.Keys = append(m.Keys, authkeys.PublicKey{Fingerprint: fp})
			}
			if len(m.Keys) == 0 && len(m.Comments) == 0 {
				return fmt.Errorf("say which key: --key, --fingerprint or --comment")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := pickHostsOrAll(cfg, args, tags, groups, all)
			if err != nil {
				return err
			}
			return revokeKeys(cfg, hosts, m, yes, force)
		},
	}
	cmd.Flags().StringArrayVar(&keyFiles, "key", nil, "public key file to revoke (.pub, or a file of several)")
	cmd.Flags().StringArrayVar(&fps, "fingerprint", nil, "SHA256 fingerprint to revoke, as `keys list` shows it")
	cmd.Flags().StringArrayVar(&comments, "comment", nil, "revoke keys with this exact comment, e.g. bob@laptop")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "every saved host (the default when no host is named)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	cmd.Flags().BoolVar(&force, "force", false, "allow revoking your own key or the last key in a file")
	return cmd
}

// revokeKeys removes the keys m matches from every host: scan, preview,
// refuse a lockout unless force, ask unless yes, then edit each file.
func revokeKeys(cfg *config.Config, hosts []config.Host, m authkeys.Matcher, yes, force bool) error {
	plan, problems := authkeys.Plan(scanKeys(cfg, hosts), m, localKeys())
	for _, p := range problems {
		fmt.Println("! " + p.Msg)
	}
	if len(plan) == 0 {
		fmt.Println("That key isn't on any of the selected hosts.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tFILE\tFINGERPRINT\tCOMMENT\t")
	n := 0
	for _, f := range plan {
		for _, e := range f.Entries {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t\n", f.Host, f.File, e.Fingerprint, e.Comment)
			n++
		}
	}
	tw.Flush()
	if p, blocked := authkeys.Blocked(problems); blocked && !force {
		return fmt.Errorf("not revoking: %s (use --force if you're sure)", p.Msg)
	}
	if !yes {
		fmt.Printf("\nRemove these %d key line(s) from %d file(s)? [y/N] ", n, len(plan))
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if r := strings.ToLower(strings.TrimSpace(resp)); r != "y" && r != "yes" {
			fmt.Println("Nothing changed.")
			return nil
		}
	}
	failed := 0
	for _, r := range authkeys.Apply(context.Background(), plan, timeoutRunner(hostRunner(cfg), 30*time.Second), time.Now().Format("20060102-150405")) {
		if r.Err != nil {
			failed++
			fmt.Printf("FAILED %s %s: %v\n", r.Host, r.File, r.Err)
			continue
		}
		fmt.Printf("revoked on %s (%s), backup %s\n", r.Host, r.File, r.Backup)
		if h, idx := cfg.Find(r.Host); idx >= 0 {
			recordActivity(cfg, *h, "keys-revoke", fmt.Sprintf("%s: %d key(s)", r.File, len(r.Entries)))
		}
	}
	kickAutosync(false, true)
	if failed > 0 {
		return fmt.Errorf("%d file(s) not changed", failed)
	}
	return nil
}

// timeoutRunner gives each script its own deadline.
func timeoutRunner(run authkeys.Runner, d time.Duration) authkeys.Runner {
	return func(ctx context.Context, host, script string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return run(ctx, host, script)
	}
}
