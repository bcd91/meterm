package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"meterm/internal/status"
	"meterm/internal/termsafe"
)

// launchdLabelPrefix is the reverse-DNS start of every tunnel's label and file name.
const launchdLabelPrefix = "com.github.bcd91.meterm.tunnel."

// launchdPath is what a tunnel's ssh and meterm run with. launchd's own default has
// the system's ssh; the rest is for a ProxyCommand from Homebrew in the ssh config.
const launchdPath = "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin"

// launchdThrottle is systemd's RestartSec: how soon launchd brings back a meterm
// that died.
const launchdThrottle = 30

// launchctl runs launchctl and returns what it printed. Tests replace it so they
// never touch the real launchd: see the init in tunnel_launchd_test.go. By its full
// path, since this has to work from a PATH that may hold anything.
var launchctl = func(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}

// launchctlRetryDelay is the wait between tries: launchd can take a moment to finish
// taking a job out after bootout has returned.
var launchctlRetryDelay = time.Second

type launchdService struct{}

func (launchdService) binary() (string, error) {
	// Not through symlinks, unlike systemd's: Homebrew's /opt/homebrew/bin/meterm
	// survives an upgrade, and what it points at in the Cellar doesn't.
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func (launchdService) warnBinary(exe string) {
	if tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator); strings.HasPrefix(exe, tmp) || strings.HasPrefix(exe, "/tmp/") || strings.HasPrefix(exe, "/private/tmp/") {
		fmt.Fprintf(os.Stderr, "meterm: warning: the job runs %s, in a temp directory; it stops working when that's cleaned\n", termsafe.Text(exe))
	}
}

func launchdAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func launchdLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "meterm"), nil
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// launchdLabel is readable as far as it can be, and the hash keeps a/b-c and a-b/c
// apart, and names that only differ past the cut. Only letters, digits, - and _
// come from the name, so it can't leave the directory or look like an option.
func launchdLabel(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	safe := unitNameDropRE.ReplaceAllString(strings.ReplaceAll(spec, "/", "-"), "_")
	if len(safe) > 64 {
		safe = safe[:64]
	}
	return launchdLabelPrefix + safe + "-" + hex.EncodeToString(sum[:8])
}

// launchdCarriedEnv is what's handed on from the enable to the job, as --config is:
// without it the job would write to another activity log than this command does.
var launchdCarriedEnv = []string{"METERM_ACTIVITY_DIR", "METERM_HISTORY", "METERM_NO_AUTOSYNC", "METERM_RUNTIME_DIR", "METERM_TEAM_DIR"}

func (launchdService) enable(spec, exe, config string) (string, error) {
	dir, err := launchdAgentsDir()
	if err != nil {
		return "", err
	}
	logDir, err := launchdLogDir()
	if err != nil {
		return "", err
	}
	label := launchdLabel(spec)
	logPath := filepath.Join(logDir, label+".log")
	env := map[string]string{"PATH": launchdPath}
	for _, k := range launchdCarriedEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	plist, err := renderLaunchAgent(label, exe, config, spec, logPath, env)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	// Made here so that it's private: launchd would make it readable by everyone.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	path := filepath.Join(dir, label+".plist")
	if err := writeUnit(path, plist); err != nil {
		return "", err
	}
	target := launchdDomain() + "/" + label
	if launchdLoaded(target) {
		// Enabled before: start over, from the file as it is now.
		_ = launchdBootout(target)
	}
	if err := launchctlTries("bootstrap", launchdDomain(), path); err != nil {
		return "", err
	}
	if out, err := launchctl("kickstart", target); err != nil {
		return "", launchctlError("kickstart", out, err)
	}
	warnLaunchdAgent()
	return label, nil
}

func (launchdService) disable(spec string) error {
	dir, err := launchdAgentsDir()
	if err != nil {
		return err
	}
	label := launchdLabel(spec)
	path := filepath.Join(dir, label+".plist")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s isn't enabled (no %s)", termsafe.Text(spec), path)
	}
	target := launchdDomain() + "/" + label
	if launchdLoaded(target) {
		if err := launchdBootout(target); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func launchdLoaded(target string) bool {
	_, err := launchctl("print", target)
	return err == nil
}

// launchdBootout stops the job. launchctl can say "Operation now in progress" on
// its way, so what counts is whether the job is gone afterwards.
func launchdBootout(target string) error {
	var last error
	for try := 0; try < 3; try++ {
		out, err := launchctl("bootout", target)
		if err == nil || !launchdLoaded(target) {
			return nil
		}
		last = launchctlError("bootout", out, err)
		time.Sleep(launchctlRetryDelay)
	}
	return last
}

func launchctlTries(args ...string) error {
	var last error
	for try := 0; try < 3; try++ {
		out, err := launchctl(args...)
		if err == nil {
			return nil
		}
		last = launchctlError(args[0], out, err)
		time.Sleep(launchctlRetryDelay)
	}
	return last
}

func launchctlError(verb, out string, err error) error {
	msg := termsafe.Text(strings.TrimSpace(out))
	if msg == "" {
		return fmt.Errorf("launchctl %s: %w", verb, err)
	}
	if verb == "bootstrap" {
		return fmt.Errorf("launchctl bootstrap: %s: %w (it needs someone logged in at this Mac's screen, and the entry switched on under Login Items in System Settings)", msg, err)
	}
	return fmt.Errorf("launchctl %s: %s: %w", verb, msg, err)
}

// warnLaunchdAgent says so when the job won't get the agent this shell has: a job in
// the gui domain inherits launchd's SSH_AUTH_SOCK, the system's own agent.
func warnLaunchdAgent() {
	out, err := launchctl("getenv", "SSH_AUTH_SOCK")
	sock := strings.TrimSpace(out)
	switch {
	case err != nil || sock == "":
		fmt.Fprintln(os.Stderr, "meterm: warning: launchd has no SSH_AUTH_SOCK, so the tunnel can only use a key with no passphrase")
	case os.Getenv("SSH_AUTH_SOCK") != "" && os.Getenv("SSH_AUTH_SOCK") != sock:
		fmt.Fprintln(os.Stderr, "meterm: warning: this shell uses another ssh agent than launchd's; the tunnel gets launchd's, so its key has to be in that one (ssh-add --apple-use-keychain) or named by IdentityAgent in your ssh config")
	}
}

// renderLaunchAgent is the job for spec. KeepAlive brings it back unless it exited
// with 0, and launchd has no list of exit codes to stay away from as systemd has, so
// the job is told to give up with 0 (see tunnelUp's --gave-up-exit). That's what
// keeps it from restarting forever into a tunnel that's gone. RunAtLoad brings it
// back at each login. Everything variable goes through plistString, and the option
// parser stops at --, so no name can become anything else.
func renderLaunchAgent(label, exe, config, spec, logPath string, env map[string]string) (string, error) {
	argv := []string{exe}
	if config != "" {
		argv = append(argv, "--config", config)
	}
	argv = append(argv, "tunnel", "up", "--keep", "--service", "--gave-up-exit", "0", "--", spec)

	var b bytes.Buffer
	var firstErr error
	str := func(s string) string {
		out, err := plistString(s)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return out
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t" + str(label) + "\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range argv {
		b.WriteString("\t\t" + str(a) + "\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	fmt.Fprintf(&b, "\t<key>ThrottleInterval</key>\n\t<integer>%d</integer>\n", launchdThrottle)
	b.WriteString("\t<key>StandardOutPath</key>\n\t" + str(logPath) + "\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t" + str(logPath) + "\n")
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kx, err := plistEscape(k)
		if err != nil {
			return "", err
		}
		b.WriteString("\t\t<key>" + kx + "</key>\n\t\t" + str(env[k]) + "\n")
	}
	b.WriteString("\t</dict>\n</dict>\n</plist>\n")
	if firstErr != nil {
		return "", firstErr
	}
	return b.String(), nil
}

func plistString(s string) (string, error) {
	x, err := plistEscape(s)
	return "<string>" + x + "</string>", err
}

// plistEscape is the real XML encoder's escaping. That encoder would quietly turn a
// character XML can't hold into U+FFFD, and a path with one in it would then be some
// other path, so those are refused.
func plistEscape(s string) (string, error) {
	if !validXMLText(s) {
		return "", fmt.Errorf("%q can't be written to a launchd job", s)
	}
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

func validXMLText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		ok := r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || r >= 0x10000
		if !ok {
			return false
		}
	}
	return true
}

// plistStrings reads a job file back for one key: what's in <array> after the key,
// or in a <string>. It's for meterm's own files, and returns nil for anything else.
func plistStrings(data []byte, key string) []string {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	d.Entity = map[string]string{}
	var out []string
	var text strings.Builder
	found, inArray := false, false
	depthKey := false
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			text.Reset()
			if found && t.Name.Local == "array" {
				inArray = true
			}
			depthKey = t.Name.Local == "key"
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch {
			case depthKey && t.Name.Local == "key":
				found = text.String() == key
				depthKey = false
			case found && t.Name.Local == "string":
				out = append(out, text.String())
				if !inArray {
					return out
				}
			case inArray && t.Name.Local == "array":
				return out
			}
		}
	}
}

// launchdEnabled is every tunnel enabled here: its spec, and the file to read about it.
type enabledService struct {
	spec, log string
	since     time.Time
}

func launchdEnabled() []enabledService {
	dir, err := launchdAgentsDir()
	if err != nil {
		return nil
	}
	logDir, err := launchdLogDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []enabledService
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, launchdLabelPrefix) || !strings.HasSuffix(name, ".plist") || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > 1<<16 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		argv := plistStrings(data, "ProgramArguments")
		if len(argv) < 2 || argv[len(argv)-2] != "--" {
			continue
		}
		s := enabledService{spec: argv[len(argv)-1], log: filepath.Join(logDir, strings.TrimSuffix(name, ".plist")+".log"), since: info.ModTime()}
		if st, err := os.Stat(s.log); err == nil {
			s.since = st.ModTime()
		}
		out = append(out, s)
	}
	return out
}

// enabledServices is what serviceRows looks through, replaced in tests.
var enabledServices = func() []enabledService {
	if serviceGOOS == "darwin" {
		return launchdEnabled()
	}
	return nil
}

// stoppedStatus is the state of a tunnel that's enabled with nothing holding it: it
// gave up, or launchd is about to bring it back. On Linux systemctl says so.
const stoppedStatus = "stopped"

// serviceRows adds the tunnels that are enabled and not running. It's what makes a
// tunnel that gave up show at all, as a process that has ended leaves no state file.
func serviceRows(rows []status.Row) []status.Row {
	for _, s := range enabledServices() {
		hostPart, name, ok := strings.Cut(s.spec, "/")
		if !ok {
			continue
		}
		host := hostPart
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if slices.ContainsFunc(rows, func(r status.Row) bool {
			return r.Kind == status.KindTunnel && r.Name == name && (r.Host == hostPart || r.Host == host)
		}) {
			continue
		}
		rows = append(rows, status.Row{Entry: status.Entry{
			Kind: status.KindTunnel, Host: termsafe.Text(host), Name: termsafe.Text(name), State: stoppedStatus,
			Detail: "enabled, not running; see " + termsafe.Text(s.log), Since: s.since.UTC(),
		}})
	}
	return rows
}
