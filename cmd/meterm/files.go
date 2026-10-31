package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"

	"meterm/internal/fileman"
	"meterm/internal/sshrun"
)

func newFilesCmd() *cobra.Command {
	var localDir string
	var shell bool
	cmd := &cobra.Command{
		Use:     "files <host> [remote-dir]",
		Aliases: []string{"fm"},
		Short:   "Two-pane file manager: this machine on the left, the host on the right",
		Long: "Browse both sides and copy between them, like vifm or Midnight Commander.\n\n" +
			"  tab        switch side            h j k l / arrows   move, enter, go up\n" +
			"  space      select / unselect      a                  select all / none\n" +
			"  c (F5)     copy to the other side d (F8)             delete (asks first)\n" +
			"  r (F6)     rename                 m (F7)             new directory\n" +
			"  /          find by name (n: next) .                  show hidden files\n" +
			"  ~          home directory\n" +
			"  R          reload                 q                  quit\n" +
			"  esc        cancel a running copy (the unfinished file is removed)\n\n" +
			"Copying over existing files asks first: overwrite them, or skip them.\n" +
			"It runs SFTP through your own ssh, so jump hosts and keys work as for\n" +
			"`meterm connect`. Uploads, downloads, deletes and renames on the host are\n" +
			"recorded in the activity log.\n\n" +
			"--shell opens OpenSSH's plain sftp prompt (ls, cd, get, put, mget, mput)\n" +
			"instead. For one-off copies from scripts, use `meterm cp`.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if shell {
				return runSftpShell(args)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
			}
			host := *h

			c := sshrun.SubsystemCommand(cfg.Resolve(host), "sftp")
			stdin, err := c.StdinPipe()
			if err != nil {
				return err
			}
			stdout, err := c.StdoutPipe()
			if err != nil {
				return err
			}
			var stderr limitedBuffer
			c.Stderr = &stderr
			if err := c.Start(); err != nil {
				return fmt.Errorf("start ssh: %w", err)
			}
			client, err := sftp.NewClientPipe(stdout, stdin,
				sftp.UseConcurrentWrites(true), sftp.MaxConcurrentRequestsPerFile(64))
			if err != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = err.Error()
				}
				return fmt.Errorf("couldn't open SFTP on %s: %s", host.Name, msg)
			}
			defer func() {
				client.Close()
				_ = c.Wait()
			}()

			remote := fileman.Remote{Host: host.Name, Client: client}
			rdir := ""
			if len(args) == 2 {
				rdir = args[1]
			} else if rdir, err = remote.Home(); err != nil {
				rdir = "."
			}
			ldir := localDir
			if ldir == "" {
				if ldir, err = os.Getwd(); err != nil {
					ldir, _ = os.UserHomeDir()
				}
			}

			recordActivity(cfg, host, "sftp", "files "+rdir)
			kickAutosync(false, true)
			err = fileman.RunUI(fileman.Options{
				Left: fileman.Local{}, Right: remote, LeftDir: ldir, RightDir: rdir,
				OnEvent: func(kind, detail string, onHost bool) {
					if onHost {
						recordActivity(cfg, host, "sftp", kind+" "+detail)
					}
				},
			})
			kickAutosync(false, true)
			// A full-screen app needs no "press enter" pause when the picker launched it.
			os.Unsetenv(fromTUIEnv)
			return err
		},
	}
	cmd.Flags().StringVar(&localDir, "local", "", "local directory to start in (default: the current directory)")
	cmd.Flags().BoolVar(&shell, "shell", false, "open the plain sftp prompt instead of the two-pane manager")
	return cmd
}

// limitedBuffer keeps the first 8 KB of ssh's stderr: enough for the error
// that explains a failed connection, without growing during a long session.
type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := 8192 - len(l.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }
