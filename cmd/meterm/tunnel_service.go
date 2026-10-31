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

	"meterm/internal/termsafe"
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

// serviceGOOS picks the service manager; tests set it to try either on any machine.
var serviceGOOS = runtime.GOOS

// tunnelService is what differs between the systems that can keep a tunnel running.
type tunnelService interface {
	// binary is the path the job should run, and warnBinary speaks up if it's a poor one.
	binary() (string, error)
	warnBinary(exe string)
	// enable writes the job and starts it, and returns what it's called.
	enable(spec, exe, config string) (string, error)
	disable(spec string) error
}

func serviceForOS() (tunnelService, error) {
	switch serviceGOOS {
	case "linux":
		return systemdService{}, nil
	case "darwin":
		return launchdService{}, nil
	}
	return nil, checkServiceSupport()
}

func newTunnelEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <host>/<tunnel>",
		Short: "Keep a saved tunnel open in the background, from login on (Linux, macOS)",
		Long: "Writes a job that runs `meterm tunnel up --keep`, and starts it: a systemd user\n" +
			"unit on Linux, a LaunchAgent on macOS. It carries on after this terminal closes\n" +
			"and comes back at the next login; a locked key or a refused login is retried\n" +
			"rather than given up on. See how it's doing with `meterm status`. Its output is\n" +
			"in `journalctl --user -u <the unit this prints>` on Linux, and in\n" +
			"~/Library/Logs/meterm/<the label this prints>.log on macOS.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkServiceSupport(); err != nil {
				return err
			}
			spec, err := serviceSpec(args[0])
			if err != nil {
				return err
			}
			svc, err := serviceForOS()
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
				fmt.Fprintf(os.Stderr, "⚠ %s reaches PRODUCTION, and will stay open: %s\n", spec, termsafe.Text(f.Note))
			}

			exe, err := svc.binary()
			if err != nil {
				return err
			}
			svc.warnBinary(exe)
			config := ""
			if cfgPath != "" || os.Getenv("METERM_CONFIG") != "" {
				if config, err = resolveCfgPath(); err != nil {
					return err
				}
				if config, err = filepath.Abs(config); err != nil {
					return err
				}
			}
			name, err := svc.enable(spec, exe, config)
			if err != nil {
				return err
			}
			fmt.Printf("Enabled %s as %s. Undo with `meterm tunnel disable %s`.\n", spec, name, spec)
			return nil
		},
	}
}

func newTunnelDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <host>/<tunnel>",
		Short: "Stop a tunnel enabled with `meterm tunnel enable`, and remove its job",
		Long: "Works from the name alone, so a tunnel or host deleted since can still be\n" +
			"turned off.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkServiceSupport(); err != nil {
				return err
			}
			spec, err := serviceSpecShape(args[0])
			if err != nil {
				return err
			}
			svc, err := serviceForOS()
			if err != nil {
				return err
			}
			if err := svc.disable(spec); err != nil {
				return err
			}
			fmt.Printf("Disabled %s.\n", termsafe.Text(spec))
			return nil
		},
	}
}

func checkServiceSupport() error {
	if serviceGOOS != "linux" && serviceGOOS != "darwin" {
		return fmt.Errorf("tunnel enable and disable need systemd or launchd, so they work on Linux and macOS only; use `meterm tunnel up --keep` here")
	}
	return nil
}

// systemdService keeps a tunnel in a systemd user unit.
type systemdService struct{}

func (systemdService) binary() (string, error) { return runningBinary() }

func (systemdService) warnBinary(exe string) {
	if exe != packagedMeterm {
		fmt.Fprintf(os.Stderr, "meterm: warning: the unit runs %s, not %s; it stops working if that copy moves\n", exe, packagedMeterm)
	}
}

func (systemdService) enable(spec, exe, config string) (string, error) {
	dir, err := userUnitDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	unit := serviceUnitName(spec)
	if err := writeUnit(filepath.Join(dir, unit), renderTunnelUnit(exe, config, spec)); err != nil {
		return "", err
	}
	if err := systemctlUser("daemon-reload"); err != nil {
		return "", fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := systemctlUser("enable", "--now", unit); err != nil {
		return "", fmt.Errorf("systemctl enable %s: %w", unit, err)
	}
	return unit, nil
}

func (systemdService) disable(spec string) error {
	dir, err := userUnitDir()
	if err != nil {
		return err
	}
	unit := serviceUnitName(spec)
	path := filepath.Join(dir, unit)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s isn't enabled (no %s)", termsafe.Text(spec), path)
	}
	if err := systemctlUser("disable", "--now", unit); err != nil {
		return fmt.Errorf("systemctl disable %s: %w", unit, err)
	}
	// The SIGTERM exit code of --service counts as a failure, which would stay listed.
	_ = systemctlUser("reset-failed", unit)
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := systemctlUser("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

// serviceSpec checks for host/tunnel: a unit holds one tunnel, by name, so that
// adding a tunnel to a host later doesn't quietly open it too. The name ends up in a
// job's command line and on the terminal, and no saved host can need a leading - or
// an escape sequence to be turned on or off.
func serviceSpec(arg string) (string, error) {
	if _, err := serviceSpecShape(arg); err != nil {
		return "", err
	}
	if strings.HasPrefix(arg, "-") || termsafe.Unsafe(arg) {
		return "", fmt.Errorf("%q: not a name a tunnel can be enabled under", arg)
	}
	return arg, nil
}

// serviceSpecShape is all that disable asks of a name: one that an older meterm let
// through, or one that a team has since changed, can still be turned off.
func serviceSpecShape(arg string) (string, error) {
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
