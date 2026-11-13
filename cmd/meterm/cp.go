package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/sshrun"
)

func newCpCmd() *cobra.Command {
	var recursive, sync, yes bool
	cmd := &cobra.Command{
		Use:   "cp <src> <dst> [-- rsync-flags...]",
		Short: "Copy files to or from a saved host (scp, or rsync with --sync)",
		Long: "One side must be a local path; the other a saved host written as <name>:<path> —\n" +
			"meterm fills in the user, port, key, and jump host for you, so you never re-type\n" +
			"connection details just to move a file.\n\n" +
			"--sync copies with rsync instead: incremental and resumable, for large trees\n" +
			"or repeated deploys. Flags after -- go to rsync (default: -avz --progress).\n\n" +
			"Examples:\n" +
			"  meterm cp web1:/etc/nginx/nginx.conf ./nginx.conf   # download\n" +
			"  meterm cp ./deploy.tar.gz web1:/tmp/                # upload\n" +
			"  meterm cp -r web1:/var/log/myapp ./logs             # a directory\n" +
			"  meterm cp --sync ./dist/ web1:/var/www/app/ -- -avz --delete\n\n" +
			"--sync with --delete on a production host asks first (y only; --yes doesn't).\n" +
			"When stdin isn't a terminal it doesn't wait: it warns on stderr, and the\n" +
			"activity log says so. The log has a line when the copy starts and one with\n" +
			"how it ended.\n\n" +
			"To browse and pick files instead, use `meterm files`.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var extra []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				args, extra = args[:dash], args[dash:]
			}
			if len(args) != 2 {
				return fmt.Errorf("cp needs exactly a <src> and a <dst>")
			}
			if len(extra) > 0 && !sync {
				return fmt.Errorf("flags after -- are for rsync; add --sync")
			}
			return runCopy(args[0], args[1], recursive, sync, extra, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before --sync --delete on a production host")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy directories recursively")
	cmd.Flags().BoolVar(&sync, "sync", false, "copy with rsync: incremental and resumable")
	return cmd
}

// runCopy is `cp` (and the deprecated `sync`): one end local, the other a
// saved host.
func runCopy(src, dst string, recursive, sync bool, rsyncFlags []string, yes bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	verb := "cp"
	if sync {
		if _, err := exec.LookPath("rsync"); err != nil {
			return fmt.Errorf("rsync not found on this system; drop --sync to copy with scp")
		}
		verb = "sync"
		if len(rsyncFlags) == 0 {
			rsyncFlags = []string{"-avz", "--progress"}
		}
	}
	srcHost, srcPath, srcRemote, err := splitRemoteArg(cfg, src)
	if err != nil {
		return err
	}
	dstHost, dstPath, dstRemote, err := splitRemoteArg(cfg, dst)
	if err != nil {
		return err
	}
	var host *config.Host
	var remotePath, localPath, detail string
	upload := false
	switch {
	case srcRemote && dstRemote:
		return fmt.Errorf("one end must be local; copy host to host as a download then an upload")
	case srcRemote:
		host, remotePath, localPath = srcHost, srcPath, dstPath
		detail = "download " + srcPath + " -> " + dstPath
	case dstRemote:
		host, remotePath, localPath, upload = dstHost, dstPath, srcPath, true
		detail = "upload " + srcPath + " -> " + dstPath
	default:
		return fmt.Errorf("neither argument references a saved host — write the remote side as <name>:<path>")
	}
	// Naming the host is asking for it; a --delete on production is asked
	// about all the same.
	named := namedOnly(*host)
	what := verb
	if sync && deletes(rsyncFlags) {
		detail += " (" + strings.Join(deleteFlags(rsyncFlags), " ") + ")"
		named, what = nil, "rsync "+strings.Join(deleteFlags(rsyncFlags), " ")
	}
	noteFor, ok := productionNotes(cfg, []config.Host{*host}, named, what, yes)
	if !ok {
		return nil
	}
	note := noteFor(*host)
	// A start before it runs, so a crash or a kill still leaves evidence it
	// started, and an outcome with how it ended.
	run := beginActivity(cfg, *host, verb, noteDetail(note, detail))
	kickAutosync(false, true)
	r := cfg.Resolve(*host)
	c := sshrun.ScpCommand(r, remotePath, localPath, upload, recursive)
	if sync {
		c = sshrun.RsyncCommand(r, remotePath, localPath, upload, rsyncFlags)
	}
	runErr := c.Run()
	run.outcome(copyOutcome(runErr, time.Since(run.began)))
	kickAutosync(false, true)
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		finish(exitErr.ExitCode())
	}
	return runErr
}

// copyOutcome is how scp or rsync ended, for the log. Both exit 255 when ssh
// itself failed: the host was unreachable, refused the login, or its host key
// didn't match.
func copyOutcome(err error, took time.Duration) activity.Outcome {
	o := outcomeOf(err, took)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
		o.Note = "ssh: unreachable, auth, or host key"
	}
	return o
}

// deleteFlags is rsync's flags that remove files on the receiving side:
// --delete and its variants, --remove-source-files.
func deleteFlags(flags []string) []string {
	var out []string
	for _, f := range flags {
		if strings.HasPrefix(f, "--delete") || f == "--del" || f == "--remove-source-files" {
			out = append(out, f)
		}
	}
	return out
}

func deletes(flags []string) bool { return len(deleteFlags(flags)) > 0 }

// splitRemoteArg recognizes a "<host>:<path>" argument where <host> is a
// name already saved in cfg. Anything else (a plain local path, or a
// host:path referring to an unknown name) is left untouched. A name that
// two hosts show as is an error, not a local path.
func splitRemoteArg(cfg *config.Config, arg string) (h *config.Host, path string, isRemote bool, err error) {
	idx := strings.Index(arg, ":")
	if idx <= 0 {
		return nil, arg, false, nil
	}
	name, rest := arg[:idx], arg[idx+1:]
	host, i, err := cfg.Target(name)
	if err != nil {
		return nil, arg, false, err
	}
	if i >= 0 {
		return host, rest, true, nil
	}
	return nil, arg, false, nil
}
