package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// packagedMeterm is where a medistro install keeps the binary: a unit pointing
// anywhere else breaks when that copy moves, so it gets a warning.
const packagedMeterm = "/usr/bin/meterm"

// systemctlUser runs `systemctl --user`; tests replace it so they never touch
// the real user manager.
var systemctlUser = func(args ...string) error {
	c := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func newTunnelEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <host>/<tunnel>",
		Short: "Keep a saved tunnel open in the background, from login on (Linux)",
		Long: "Writes a systemd user unit that runs `meterm tunnel up --keep`, and starts it.\n" +
			"It carries on after this terminal closes and comes back at the next login; a\n" +
			"locked key or a refused login is retried rather than given up on. See how it's\n" +
			"doing with `meterm status`, and its output with\n" +
			"`journalctl --user -u <the unit this prints>`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkServiceSupport(); err != nil {
				return err
			}
			spec, err := serviceSpec(args[0])
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			sets, err := pickTunnels(cfg, []string{spec})
			if err != nil {
				return err
			}
			if err := cfg.Resolve(sets[0].host).Err; err != nil {
				return err
			}
			f := sets[0].forward[0]
			if cfg.IsProduction(sets[0].host) || f.MentionsProduction() {
				fmt.Fprintf(os.Stderr, "⚠ %s reaches PRODUCTION, and will stay open: %s\n", spec, f.Note)
			}

			exe, err := runningBinary()
			if err != nil {
				return err
			}
			if exe != packagedMeterm {
				fmt.Fprintf(os.Stderr, "meterm: warning: the unit runs %s, not %s; it stops working if that copy moves\n", exe, packagedMeterm)
			}
			config := ""
			if cfgPath != "" || os.Getenv("METERM_CONFIG") != "" {
				if config, err = resolveCfgPath(); err != nil {
					return err
				}
				if config, err = filepath.Abs(config); err != nil {
					return err
				}
			}
			dir, err := userUnitDir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			unit := serviceUnitName(spec)
			if err := writeUnit(filepath.Join(dir, unit), renderTunnelUnit(exe, config, spec)); err != nil {
				return err
			}
			if err := systemctlUser("daemon-reload"); err != nil {
				return fmt.Errorf("systemctl daemon-reload: %w", err)
			}
			if err := systemctlUser("enable", "--now", unit); err != nil {
				return fmt.Errorf("systemctl enable %s: %w", unit, err)
			}
			fmt.Printf("Enabled %s as %s. Undo with `meterm tunnel disable %s`.\n", spec, unit, spec)
			return nil
		},
	}
}

func newTunnelDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <host>/<tunnel>",
		Short: "Stop a tunnel enabled with `meterm tunnel enable`, and remove its unit",
		Long: "Works from the name alone, so a tunnel or host deleted since can still be\n" +
			"turned off.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkServiceSupport(); err != nil {
				return err
			}
			spec, err := serviceSpec(args[0])
			if err != nil {
				return err
			}
			dir, err := userUnitDir()
			if err != nil {
				return err
			}
			unit := serviceUnitName(spec)
			path := filepath.Join(dir, unit)
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s isn't enabled (no %s)", spec, path)
			}
			if err := systemctlUser("disable", "--now", unit); err != nil {
				return fmt.Errorf("systemctl disable %s: %w", unit, err)
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			if err := systemctlUser("daemon-reload"); err != nil {
				return fmt.Errorf("systemctl daemon-reload: %w", err)
			}
			fmt.Printf("Disabled %s.\n", spec)
			return nil
		},
	}
}

func checkServiceSupport() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("tunnel enable and disable need systemd, so they work on Linux only; use `meterm tunnel up --keep` here")
	}
	return nil
}

// serviceSpec checks for host/tunnel: a unit holds one tunnel, by name, so that
// adding a tunnel to a host later doesn't quietly open it too.
func serviceSpec(arg string) (string, error) {
	host, name, ok := strings.Cut(arg, "/")
	if !ok || host == "" || name == "" {
		return "", fmt.Errorf("%q: give one tunnel as <host>/<tunnel>", arg)
	}
	return arg, nil
}

var unitNameDropRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// serviceUnitName is readable, and the hash keeps a/b-c and a-b/c apart.
func serviceUnitName(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return "meterm-tunnel-" + unitNameDropRE.ReplaceAllString(strings.ReplaceAll(spec, "/", "-"), "_") +
		"-" + hex.EncodeToString(sum[:4]) + ".service"
}

// runningBinary is this program's real path, since a unit outlives the PATH it
// was written under.
func runningBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

func userUnitDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user"), nil
}

// renderTunnelUnit is the unit for spec. Restart covers meterm itself dying;
// --service has it ride out a locked key on its own, rather than restarting and
// writing to the activity log on every try, and exit with tunnelGaveUp when there's
// nothing left to hold. The limit is off so that a machine offline for a long time
// doesn't lose the tunnel for good.
func renderTunnelUnit(exe, config, spec string) string {
	argv := []string{exe}
	if config != "" {
		argv = append(argv, "--config", config)
	}
	argv = append(argv, "tunnel", "up", "--keep", "--service", spec)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = systemdQuote(a)
	}
	return "[Unit]\n" +
		"Description=" + unitText("meterm tunnel "+spec) + "\n" +
		"StartLimitIntervalSec=0\n\n" +
		"[Service]\n" +
		"ExecStart=" + strings.Join(quoted, " ") + "\n" +
		"Restart=on-failure\n" +
		"RestartPreventExitStatus=" + strconv.Itoa(tunnelGaveUp) + "\n" +
		"RestartSec=30\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

// unitText makes a value safe in a unit file: specifiers are doubled, and neither
// a newline, which would start another setting, nor a backslash, which at the end
// of a line would swallow the next one, can get through.
func unitText(s string) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", `\`, "/").Replace(s)
	return strings.ReplaceAll(s, "%", "%%")
}

// systemdQuote quotes one word of ExecStart, where % starts a specifier and $ a
// variable even inside double quotes.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$", "%", "%%", "\n", `\n`, "\r", `\r`).Replace(s)
	return `"` + s + `"`
}

// writeUnit puts the file in place whole, so systemd never reads half of one.
func writeUnit(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meterm-unit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(content)
	if err := errors.Join(werr, tmp.Chmod(0o644), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
