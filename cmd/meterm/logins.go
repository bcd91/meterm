package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/logins"
	"meterm/internal/termsafe"
)

// `meterm logins`: who the host says logged in, set beside what this machine's log says.
// It's the far side's word next to this side's, for finding logins meterm doesn't know of.
//
// It writes no activity entry, for the reason `agents` doesn't: it's a look, one constant
// that changes nothing, safe to run on a schedule, and that log can't forget. Each look
// shows in the host's own sshd log instead.

func newLoginsCmd() *cobra.Command {
	var flags lookFlags
	cmd := &cobra.Command{
		Use:   "logins [host...] [--tag t] [--group g]",
		Short: "List who logged in to hosts, and whether meterm's log knows of it (read-only, not logged)",
		Long: "Asks each selected host for `last -F -i -w -n 500`, and nothing else: the command is\n" +
			"fixed, read-only and takes no sudo. Each login is set beside this machine's activity\n" +
			"log: matched says there's an event on that host, for that account, within two\n" +
			"minutes of it. Hosts are chosen, and production refused unless named, as with `agents`,\n" +
			"and so are the extra_args a host can have.\n\n" +
			"What it can't see:\n" +
			"- wtmp mostly holds sessions with a terminal. `ssh host command`, scp and sftp, and so\n" +
			"  `meterm exec`, often leave nothing there. Those need the host's auth log, and root.\n" +
			"- How someone logged in, which is also in that log.\n" +
			"- Other people's meterm: not matched means not in this machine's log. It doesn't mean\n" +
			"  an intruder, and matched doesn't mean it was that event, only that one was close.\n" +
			"- Anything root removed from wtmp.\n\n" +
			"--format json prints {\"schema\":1,\"hosts\":[{\"host\",\"reachable\",\"error\",\"since\",\n" +
			"\"logins\",\"unparsed\",\"truncated\"}]}: see docs/export.md. logins is null when error is\n" +
			"set.\n\n" +
			"Exit codes:\n" +
			"  0  every host answered\n" +
			"  1  nothing was asked: bad flags, no selection, or an unknown host\n" +
			"  2  the report is on stdout, and at least one host has an error in it",
		RunE: func(cmd *cobra.Command, args []string) error {
			jobs, _, err := flags.jobs("logins", args)
			if err != nil {
				return err
			}
			var events []activity.Event
			if dir, err := activity.DefaultDir(); err == nil {
				if events, err = activity.Reader(dir).All(); err != nil {
					fmt.Fprintf(os.Stderr, "meterm: can't read the activity log, so nothing will match: %s\n", termsafe.Text(err.Error()))
				}
			}
			sshConfig, removeSSHConfig, err := writeAgentSSHConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			rep := runLogins(ctx, jobs, sshLookQuery(sshConfig, flags.timeout, logins.RemoteCommand, logins.MaxOutput), events, flags.timeout, flags.parallel)
			removeSSHConfig()
			if err := writeLogins(os.Stdout, rep, flags.format); err != nil {
				return err
			}
			failed := 0
			for _, h := range rep.Hosts {
				if h.Error != "" {
					failed++
				}
			}
			if failed > 0 {
				fmt.Fprintf(os.Stderr, "meterm: %d of %d host(s) didn't answer\n", failed, len(rep.Hosts))
				finish(agentsIncomplete)
			}
			return nil
		},
	}
	flags.add(cmd)
	return cmd
}

func runLogins(ctx context.Context, jobs []agentJob, query agentQuery, events []activity.Event, timeout time.Duration, parallel int) logins.Report {
	rep := logins.Report{Schema: logins.Schema, Hosts: make([]logins.Host, len(jobs))}
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rep.Hosts[i] = askLogins(ctx, job, query, events, timeout)
		}()
	}
	wg.Wait()
	return rep
}

// askLogins turns one host's run into its line of the report. Every error is in meterm's
// words, as in askAgents.
func askLogins(ctx context.Context, job agentJob, query agentQuery, events []activity.Event, timeout time.Duration) logins.Host {
	out := logins.Host{Host: termsafe.Text(job.label)}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, refused := prepareLook(ctx, job, timeout)
	if refused != "" {
		out.Error = refused
		return out
	}
	stdout, stderr, overflow, err := query(ctx, r)
	var exit *exec.ExitError
	switch {
	case overflow:
		out.Reachable = true
		out.Error = "unexpected output from last: more than meterm reads"
		return out
	case ctx.Err() == context.DeadlineExceeded:
		out.Error = "timed out after " + timeout.String()
		return out
	case ctx.Err() != nil:
		out.Error = "stopped before it answered"
		return out
	case errors.As(err, &exit):
		switch exit.ExitCode() {
		case -1:
			out.Error = "ssh was killed before it answered"
		case sshConnectionError:
			out.Error = sshFailureText(stderr)
		case logins.ExitNoLast:
			out.Reachable = true
			out.Error = "last not found on this host"
		default:
			out.Reachable = true
			out.Error = "last failed there: it may not take -F -i -w (as on macOS and BSD), or may not be able to read wtmp"
		}
		return out
	case err != nil && !errors.Is(err, exec.ErrWaitDelay):
		out.Error = "couldn't run ssh: " + termsafe.Text(err.Error())
		return out
	}
	out.Reachable = true
	parsed := logins.Parse(stdout)
	out.Since, out.Unparsed = parsed.Since, parsed.Unparsed
	out.Logins = parsed.Logins
	if out.Logins == nil {
		out.Logins = []logins.Login{}
	}
	logins.Match(out.Logins, events, job.host.Name)
	out.Truncated = parsed.Records >= logins.MaxLogins
	return out
}

func writeLogins(w io.Writer, rep logins.Report, format string) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	var buf bytes.Buffer
	t := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(t, "HOST\tTIME (UTC)\tUSER\tTTY\tSOURCE\tMETERM")
	for _, h := range rep.Hosts {
		switch {
		case h.Error != "":
			fmt.Fprintf(t, "%s\t%s\t\t\t\t\n", h.Host, h.Error)
		case len(h.Logins) == 0:
			fmt.Fprintf(t, "%s\tno logins in wtmp\t\t\t\t\n", h.Host)
		}
		for _, l := range h.Logins {
			match := "no event"
			if l.Event != nil {
				match = fmt.Sprintf("%s, seq %d", l.Event.Action, l.Event.Seq)
			}
			src := l.Source
			if src == "" {
				src = "local"
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Host, l.Time.Format("2006-01-02 15:04:05"), l.User, l.TTY, src, match)
		}
	}
	if err := t.Flush(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
