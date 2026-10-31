package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/sshrun"
)

func newCpCmd() *cobra.Command {
	var recursive, sync bool
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
			return runCopy(args[0], args[1], recursive, sync, extra)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy directories recursively")
	cmd.Flags().BoolVar(&sync, "sync", false, "copy with rsync: incremental and resumable")
	return cmd
}

// runCopy is `cp` (and the deprecated `sync`): one end local, the other a
// saved host.
func runCopy(src, dst string, recursive, sync bool, rsyncFlags []string) error {
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
	srcHost, srcPath, srcRemote := splitRemoteArg(cfg, src)
	dstHost, dstPath, dstRemote := splitRemoteArg(cfg, dst)
	var host *config.Host
	var remotePath, localPath string
	upload := false
	var run *activityRun
	switch {
	case srcRemote && dstRemote:
		return fmt.Errorf("one end must be local; copy host to host as a download then an upload")
	case srcRemote:
		host, remotePath, localPath = srcHost, srcPath, dstPath
		run = beginActivity(cfg, *host, verb, "download "+srcPath+" -> "+dstPath)
	case dstRemote:
		host, remotePath, localPath, upload = dstHost, dstPath, srcPath, true
		run = beginActivity(cfg, *host, verb, "upload "+srcPath+" -> "+dstPath)
	default:
		return fmt.Errorf("neither argument references a saved host — write the remote side as <name>:<path>")
	}
	kickAutosync(false, true)
	r := cfg.Resolve(*host)
	if sync {
		return run.runInherited(sshrun.RsyncCommand(r, remotePath, localPath, upload, rsyncFlags))
	}
	return run.runInherited(sshrun.ScpCommand(r, remotePath, localPath, upload, recursive))
}

// splitRemoteArg recognizes a "<host>:<path>" argument where <host> is a
// name already saved in cfg. Anything else (a plain local path, or a
// host:path referring to an unknown name) is left untouched.
func splitRemoteArg(cfg *config.Config, arg string) (h *config.Host, path string, isRemote bool) {
	idx := strings.Index(arg, ":")
	if idx <= 0 {
		return nil, arg, false
	}
	name, rest := arg[:idx], arg[idx+1:]
	if host, i, err := cfg.Target(name); err == nil && i >= 0 {
		return host, rest, true
	}
	return nil, arg, false
}
