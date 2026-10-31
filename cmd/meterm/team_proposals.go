package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"meterm/internal/team"
)

func teamProposalsCmd(teamFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "proposals",
		Short: "Hosts members proposed, waiting for an owner or admin to approve",
		Long: "A member's `team share` or `team push` doesn't change the team: it proposes the\n" +
			"change on the repository's " + team.ProposalsBranch + " branch, and nobody sees or reaches the\n" +
			"host until an owner or admin runs `meterm team approve <host>` (or reject).\n\n" +
			"For that to be enforced rather than a convention, protect the team's branch on\n" +
			"the Git host so only owners and admins can push to it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, t, err := teamFor(cmd, *teamFlag)
			if err != nil {
				return err
			}
			ps, err := tc.be.Proposals(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Printf("No proposals waiting in %q.\n", t.Name)
				return nil
			}
			hosts, err := tc.be.ListHosts(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tCHANGE\tBY\tWHEN\tWARNINGS")
			for _, p := range ps {
				rv := team.ReviewProposal(p, hosts)
				what := "update"
				switch {
				case p.Remove:
					what = "remove"
				case rv.Current == nil:
					what = "add"
				}
				warn := fmt.Sprint(len(rv.Warnings))
				if rv.Invalid != nil {
					warn = "invalid"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Host.Name, what, oneLine(p.By, 40), p.At.Local().Format("2006-01-02 15:04"), warn)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Println("\nmeterm team approve <host> shows everything first; meterm team reject <host> drops it.")
			return nil
		},
	}
}

func teamFor(cmd *cobra.Command, flag string) (*teamCtx, team.Team, error) {
	tc, err := openTeamAny()
	if err != nil {
		return nil, team.Team{}, err
	}
	t, err := tc.current(cmd.Context(), flag)
	return tc, t, err
}

func printReview(rv team.Review) {
	p := rv.Proposal
	fmt.Printf("Proposed by %s on %s:\n", p.By, p.At.Local().Format("2006-01-02 15:04"))
	switch {
	case p.Remove:
		fmt.Printf("  - remove %s (%s) from the team\n", p.Host.Name, p.Host.Describe())
	case rv.Current == nil:
		fmt.Printf("  + add %s\n", p.Host.Name)
		for _, d := range team.Diff(team.SharedHost{}, p.Host) {
			fmt.Printf("      %s\n", d)
		}
	default:
		fmt.Printf("  ~ update %s\n", p.Host.Name)
		for _, d := range rv.Diff {
			fmt.Printf("      %s\n", d)
		}
	}
	for _, w := range rv.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	if rv.Invalid != nil {
		fmt.Printf("  ✗ not allowed in a team: %v\n", rv.Invalid)
	}
}

func teamApproveCmd(teamFlag *string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "approve <host>",
		Short: "Apply a member's proposed host to the team (owners and admins), after showing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			tc, t, err := teamFor(cmd, *teamFlag)
			if err != nil {
				return err
			}
			if !t.Role.CanEditHosts() {
				return fmt.Errorf("approving needs admin or owner; you are %s in %q", t.Role, t.Name)
			}
			ps, err := tc.be.Proposals(ctx, t.ID)
			if err != nil {
				return err
			}
			hosts, err := tc.be.ListHosts(ctx, t.ID)
			if err != nil {
				return err
			}
			var rv *team.Review
			for _, p := range ps {
				if p.Host.Name == args[0] {
					r := team.ReviewProposal(p, hosts)
					rv = &r
				}
			}
			if rv == nil {
				return fmt.Errorf("no proposal for %s in %q (see `meterm team proposals`)", args[0], t.Name)
			}
			printReview(*rv)
			if rv.Invalid != nil {
				return fmt.Errorf("can't approve %s: %w (reject it: meterm team reject %s)", args[0], rv.Invalid, args[0])
			}
			if !yes {
				if !isatty.IsTerminal(os.Stdin.Fd()) {
					return errors.New("not a terminal: re-run with --yes to confirm")
				}
				ans, _ := promptLine(fmt.Sprintf("Apply this to %q for everyone? [y/N] ", t.Name))
				if strings.ToLower(ans) != "y" {
					fmt.Println("Nothing changed; the proposal is still waiting.")
					return nil
				}
			}
			if _, err := team.Approve(ctx, tc.be, tc.be, t.ID, t.Role, args[0]); err != nil {
				return err
			}
			fmt.Printf("Approved: %s is in %q now; members get it on their next pull.\n", args[0], t.Name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

func teamRejectCmd(teamFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <host>",
		Short: "Drop a member's proposed host (owners and admins)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tc, t, err := teamFor(cmd, *teamFlag)
			if err != nil {
				return err
			}
			if !t.Role.CanEditHosts() {
				return fmt.Errorf("rejecting needs admin or owner; you are %s in %q", t.Role, t.Name)
			}
			ps, err := tc.be.Proposals(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			for _, p := range ps {
				if p.Host.Name == args[0] {
					if err := tc.be.DropProposal(cmd.Context(), t.ID, args[0]); err != nil {
						return err
					}
					fmt.Printf("Rejected the proposal for %s (by %s). The team is unchanged.\n", args[0], p.By)
					return nil
				}
			}
			return fmt.Errorf("no proposal for %s in %q", args[0], t.Name)
		},
	}
}
