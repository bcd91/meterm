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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/caps"
	"meterm/internal/termsafe"
)

// `meterm caps`: what each host can run, in its own words. It's for choosing where a job
// goes: OS and architecture, CPUs, memory, disk, how busy it is, GPUs, and which tools are
// there and actually start.
//
// It's a look, like `agents` and `logins`: one constant that changes nothing, no activity
// entry, and nothing written here. Each look shows in the host's own sshd log instead.
// Keeping what it found over time is for whoever reads the JSON.

// capsDefaultTimeout is longer than the other looks', as it asks up to twenty programs
// their version, and a cold disk or a JVM takes a while.
const capsDefaultTimeout = 30 * time.Second

func newCapsCmd() *cobra.Command {
	var flags lookFlags
	cmd := &cobra.Command{
		Use:   "caps [host...] [--tag t] [--group g]",
		Short: "Show what hosts can run: OS, CPUs, memory, disk, load, GPUs, tools (read-only, not logged)",
		Long: "Runs one fixed, read-only script on each selected host, with no sudo, and reports what\n" +
			"the host says of itself: OS and architecture, CPUs and model, memory, free disk in the\n" +
			"account's home, the one-minute load, NVIDIA GPUs, whether a docker daemon answers, and\n" +
			"for each of " + strconv.Itoa(len(caps.Tools)) + " tools whether it's found, whether asking its version works, and\n" +
			"what that says. It runs nothing else. Hosts are chosen, and production refused unless\n" +
			"named, as with `agents`, and so are the extra_args a host can have.\n\n" +
			"What it can't say:\n" +
			"- A tool that isn't found wasn't on the PATH a non-interactive login gets, plus the usual\n" +
			"  install places. One installed elsewhere, or behind nvm or a shell's rc file, won't show.\n" +
			"- GPUs other than NVIDIA's, which need nvidia-smi. gpus is null when it wasn't there.\n" +
			"- Whether claude is logged in, what repos are checked out, or what a login can reach.\n" +
			"- Load is a moment. Run it again before a choice that depends on it.\n\n" +
			"--format json prints {\"schema\":1,\"hosts\":[{\"host\",\"reachable\",\"error\",\"caps\"}]}:\n" +
			"see docs/export.md. caps is null when error is set, and a number that couldn't be read\n" +
			"is null, never 0.\n\n" +
			"Exit codes:\n" +
			"  0  every host answered\n" +
			"  1  nothing was asked: bad flags, no selection, or an unknown host\n" +
			"  2  the report is on stdout, and at least one host has an error in it",
		RunE: func(cmd *cobra.Command, args []string) error {
			jobs, _, err := flags.jobs("caps", args)
			if err != nil {
				return err
			}
			sshConfig, removeSSHConfig, err := writeAgentSSHConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			rep := runCaps(ctx, jobs, sshLookQuery(sshConfig, flags.timeout, caps.RemoteCommand, caps.MaxOutput), flags.timeout, flags.parallel)
			removeSSHConfig()
			if err := writeCaps(os.Stdout, rep, flags.format); err != nil {
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
	flags.timeout = capsDefaultTimeout
	cmd.Flags().Lookup("timeout").DefValue = capsDefaultTimeout.String()
	return cmd
}

func runCaps(ctx context.Context, jobs []agentJob, query agentQuery, timeout time.Duration, parallel int) caps.Report {
	rep := caps.Report{Schema: caps.Schema, Hosts: make([]caps.Host, len(jobs))}
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rep.Hosts[i] = askCaps(ctx, job, query, timeout)
		}()
	}
	wg.Wait()
	return rep
}

// askCaps turns one host's run into its line of the report. Every error is in meterm's
// words, as in askAgents.
func askCaps(ctx context.Context, job agentJob, query agentQuery, timeout time.Duration) caps.Host {
	out := caps.Host{Host: termsafe.Text(job.label)}
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
		out.Error = "unexpected output: more than meterm reads"
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
		case caps.ExitNoShell:
			out.Reachable = true
			out.Error = "sh not found on this host"
		default:
			out.Reachable = true
			out.Error = "the script failed there: it needs a POSIX sh, which a Windows server doesn't have"
		}
		return out
	case err != nil && !errors.Is(err, exec.ErrWaitDelay):
		out.Error = "couldn't run ssh: " + termsafe.Text(err.Error())
		return out
	}
	out.Reachable = true
	c := caps.Parse(stdout)
	out.Caps = &c
	return out
}

func writeCaps(w io.Writer, rep caps.Report, format string) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	var buf bytes.Buffer
	t := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(t, "HOST\tOS\tARCH\tCPUS\tMEM GB\tAVAIL GB\tHOME FREE GB\tLOAD\tGPU\tDOCKER\tTOOLS THAT RUN")
	for _, h := range rep.Hosts {
		if h.Error != "" {
			fmt.Fprintf(t, "%s\t%s\t\t\t\t\t\t\t\t\t\n", h.Host, h.Error)
			continue
		}
		c := h.Caps
		osName := c.Distro
		if c.DistroVersion != "" {
			osName += " " + c.DistroVersion
		}
		if osName == "" {
			osName = c.OS
		}
		var runs, broken []string
		for _, tool := range c.Tools {
			switch {
			case tool.Runs != nil && *tool.Runs:
				runs = append(runs, tool.Name)
			case tool.Found:
				broken = append(broken, tool.Name)
			}
		}
		tools := strings.Join(runs, " ")
		if len(broken) > 0 {
			tools += " (found, won't run: " + strings.Join(broken, " ") + ")"
		}
		if !c.Complete {
			tools += " [incomplete: the output stopped early]"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Host, dash(osName), dash(c.Arch),
			intOrDash(c.CPUs), gbOrDash(c.MemTotalMB), gbOrDash(c.MemAvailableMB), gbOrDash(c.HomeFreeMB),
			loadOrDash(c.Load1), gpuText(c), dash(c.DockerDaemon), dash(tools))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// The text table shows "-" for what couldn't be said, never 0.

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func intOrDash(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}

func gbOrDash(mb *int64) string {
	if mb == nil {
		return "-"
	}
	return strconv.FormatFloat(float64(*mb)/1024, 'f', 1, 64)
}

func loadOrDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func gpuText(c *caps.Caps) string {
	if c.GPUs == nil {
		return "-" // not looked for, which isn't none
	}
	if len(c.GPUs) == 0 {
		return "none (nvidia-smi)"
	}
	var names []string
	for _, g := range c.GPUs {
		n := g.Name
		if g.MemoryMB != nil {
			n += " " + gbOrDash(g.MemoryMB) + "GB"
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}
