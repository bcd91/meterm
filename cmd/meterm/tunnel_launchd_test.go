package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"meterm/internal/status"
)

// Nothing in this package's tests may reach the real launchd or read this user's
// real jobs. A test that forgets launchdEnv fails loudly here instead.
func init() {
	launchctl = func(args ...string) (string, error) {
		panic("a test reached launchctl: " + strings.Join(args, " "))
	}
	enabledServices = func() []enabledService { return nil }
	launchctlRetryDelay = time.Millisecond
}

type fakeLaunchd struct {
	calls  []string
	loaded map[string]bool
	fail   map[string]error
	print  string // what print says of a loaded job
}

// exitCode is an error with an exit code, like *exec.ExitError.
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

func (f *fakeLaunchd) run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if err := f.fail[args[0]]; err != nil {
		return "launchd says no", err
	}
	switch args[0] {
	case "print":
		if args[1] == launchdDomain() {
			return "", nil
		}
		if !f.loaded[args[1]] {
			return "", exitCode(launchctlNoService)
		}
		return f.print, nil
	case "bootstrap":
		f.loaded[launchdDomain()+"/"+strings.TrimSuffix(filepath.Base(args[2]), ".plist")] = true
	case "bootout":
		delete(f.loaded, args[1])
	case "getenv":
		return "/var/run/launchd-agent\n", nil
	}
	return "", nil
}

// launchdEnv is a temp home and config, with launchctl faked and macOS's code path
// chosen, on any Unix.
func launchdEnv(t *testing.T) (*fakeLaunchd, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METERM_CONFIG", filepath.Join(home, "hosts.yaml"))
	t.Setenv("METERM_ACTIVITY_DIR", filepath.Join(home, "activity"))
	t.Setenv("METERM_NO_AUTOSYNC", "1")
	t.Setenv("SSH_AUTH_SOCK", "/var/run/launchd-agent")
	host := "version: 1\nhosts:\n- name: box\n  address: 192.0.2.1\n  forwards:\n" +
		"  - {name: web, type: L, local_port: 8080, remote_host: localhost, remote_port: 80}\n"
	if err := os.WriteFile(filepath.Join(home, "hosts.yaml"), []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeLaunchd{loaded: map[string]bool{}, fail: map[string]error{}}
	oldRun, oldOS := launchctl, serviceGOOS
	launchctl, serviceGOOS = f.run, "darwin"
	t.Cleanup(func() { launchctl, serviceGOOS = oldRun, oldOS })
	return f, home
}

func runEnable(arg string) error {
	c := newTunnelEnableCmd()
	c.SetArgs([]string{arg})
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

func runDisable(arg string) error {
	c := newTunnelDisableCmd()
	c.SetArgs([]string{arg})
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

func TestLaunchdEnableAndDisable(t *testing.T) {
	f, home := launchdEnv(t)
	label := launchdLabel("box/web")
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	target := launchdDomain() + "/" + label
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	argv := plistStrings(b, "ProgramArguments")
	if want := []string{"tunnel", "up", "--keep", "--service", "--gave-up-exit", "0", "--", "box/web"}; !slices.Equal(argv[len(argv)-len(want):], want) {
		t.Errorf("argv = %v", argv)
	}
	if !filepath.IsAbs(argv[0]) {
		t.Errorf("not a full path: %s", argv[0])
	}
	for _, want := range []string{"<key>RunAtLoad</key>\n\t<true/>", "<key>SuccessfulExit</key>\n\t\t<false/>", "<key>ThrottleInterval</key>\n\t<integer>30</integer>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in:\n%s", want, b)
		}
	}
	logPath := filepath.Join(home, "Library", "Logs", "meterm", label+".log")
	if got := plistStrings(b, "StandardErrorPath"); !slices.Equal(got, []string{logPath}) {
		t.Errorf("log: %v", got)
	}
	if st, err := os.Stat(logPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("log file: %v %v", st, err)
	}
	if st, err := os.Stat(plist); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("job file (launchd refuses a writable one): %v %v", st, err)
	}
	dom := "print " + launchdDomain()
	want := []string{dom, "print " + target, "bootstrap " + launchdDomain() + " " + plist, "kickstart " + target, "getenv SSH_AUTH_SOCK"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("launchctl: %q", f.calls)
	}

	f.calls = nil
	if err := runEnable("box/web"); err != nil { // the same again: the tunnel isn't dropped
		t.Fatal(err)
	}
	want = []string{dom, "print " + target, "kickstart " + target, "getenv SSH_AUTH_SOCK"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("launchctl: %q", f.calls)
	}

	f.calls = nil
	t.Setenv("METERM_TEAM_DIR", "/changed") // now the job differs, so it starts over
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	want = []string{dom, "print " + target, "bootout " + target, "print " + target, "bootstrap " + launchdDomain() + " " + plist, "kickstart " + target, "getenv SSH_AUTH_SOCK"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("launchctl: %q", f.calls)
	}

	f.calls = nil
	if err := runDisable("box/web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Errorf("job still there: %v", err)
	}
	if !slices.Equal(f.calls, []string{"print " + target, "bootout " + target, "print " + target}) {
		t.Errorf("launchctl: %q", f.calls)
	}
}

func TestLaunchdRefusals(t *testing.T) {
	f, home := launchdEnv(t)
	for _, arg := range []string{"box", "box/", "/web", "box/nope", "other/web", "-oProxyCommand=x/web",
		"box/web\x1b]0;x\x07", "box/we\nb", "bo‮x/web", "../../x/web"} {
		if err := runEnable(arg); err == nil {
			t.Errorf("%q was accepted", arg)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("launchctl was called: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !os.IsNotExist(err) {
		t.Errorf("something was written: %v", err)
	}
	err := runDisable("gone/web")
	if err == nil || !strings.Contains(err.Error(), "isn't enabled") {
		t.Errorf("err = %v", err)
	}
	if err := runDisable("box/web\x1b[2J"); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("err = %q", err)
	}
	// disable may ask, as a job can be loaded with its file gone, and nothing more.
	if len(f.calls) != countPrefix(f.calls, "print ") {
		t.Errorf("launchctl was called: %v", f.calls)
	}
}

func jobFile(home, spec string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel(spec)+".plist")
}

// A failed enable leaves no job behind to start at the next login, and says so.
func TestLaunchdEnableFailureRemovesTheNewFile(t *testing.T) {
	f, home := launchdEnv(t)
	f.fail["bootstrap"] = exitCode(125)
	err := runEnable("box/web")
	if err == nil || !strings.Contains(err.Error(), "launchd says no") || !strings.Contains(err.Error(), "nothing is enabled") {
		t.Errorf("err = %v", err)
	}
	if n := countPrefix(f.calls, "bootstrap "); n != 3 {
		t.Errorf("%d tries: %q", n, f.calls)
	}
	if _, err := os.Stat(jobFile(home, "box/web")); !os.IsNotExist(err) {
		t.Errorf("job file: %v", err)
	}
}

// A failed re-enable of a working job brings that job back, from its old file.
func TestLaunchdReEnableFailureRestoresTheOldJob(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(jobFile(home, "box/web"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_TEAM_DIR", "/changed")
	inner := launchctl
	tries := 0
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootstrap" {
			if tries++; tries <= 3 {
				f.calls = append(f.calls, "failed bootstrap")
				return "launchd says no", exitCode(125)
			}
		}
		return inner(args...)
	}
	err = runEnable("box/web")
	if err == nil || !strings.Contains(err.Error(), "the previous job is back") {
		t.Errorf("err = %v", err)
	}
	if now, _ := os.ReadFile(jobFile(home, "box/web")); string(now) != string(old) {
		t.Errorf("file not restored:\n%s", now)
	}
	if !f.loaded[launchdDomain()+"/"+launchdLabel("box/web")] {
		t.Error("the old job isn't loaded again")
	}
}

// With no gui domain, or a launchctl that can't say, a working job isn't touched.
func TestLaunchdEnablePreflightTouchesNothing(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(jobFile(home, "box/web"))
	t.Setenv("METERM_TEAM_DIR", "/changed")
	f.calls = nil
	f.fail["print"] = exitCode(112)
	err := runEnable("box/web")
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v", err)
	}
	if countPrefix(f.calls, "bootout") != 0 || countPrefix(f.calls, "bootstrap") != 0 {
		t.Errorf("calls: %q", f.calls)
	}
	if now, _ := os.ReadFile(jobFile(home, "box/web")); string(now) != string(old) {
		t.Error("file changed")
	}
}

// kickstart failing means failure only if the job isn't running.
func TestLaunchdKickstartFailure(t *testing.T) {
	f, home := launchdEnv(t)
	f.fail["kickstart"] = exitCode(1)
	err := runEnable("box/web")
	if err == nil || !strings.Contains(err.Error(), "launchctl kickstart") || !strings.Contains(err.Error(), "nothing is enabled") {
		t.Errorf("err = %v", err)
	}
	if _, serr := os.Stat(jobFile(home, "box/web")); !os.IsNotExist(serr) {
		t.Errorf("job file: %v", serr)
	}
	if f.loaded[launchdDomain()+"/"+launchdLabel("box/web")] {
		t.Error("still loaded")
	}

	f.print = "x = {\n\tstate = running\n\tpid = 5\n}\n"
	if err := runEnable("box/web"); err != nil {
		t.Errorf("it's running: %v", err)
	}
	if _, serr := os.Stat(jobFile(home, "box/web")); serr != nil {
		t.Errorf("job file: %v", serr)
	}
}

func TestLaunchdDisableKeepsTheFileIfItCantStop(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	f.fail["bootout"] = exitCode(1)
	if err := runDisable("box/web"); err == nil {
		t.Error("no error")
	}
	if _, err := os.Stat(jobFile(home, "box/web")); err != nil {
		t.Errorf("job file: %v", err)
	}
}

// launchctl can fail on the way and still have stopped the job.
func TestLaunchdDisableGoesByWhetherItStopped(t *testing.T) {
	f, _ := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	f.fail["bootout"] = exitCode(36)
	target := launchdDomain() + "/" + launchdLabel("box/web")
	inner := launchctl
	launchctl = func(args ...string) (string, error) {
		out, err := inner(args...)
		if args[0] == "bootout" {
			delete(f.loaded, target)
		}
		return out, err
	}
	if err := runDisable("box/web"); err != nil {
		t.Errorf("err = %v", err)
	}
}

// bootout returning 0 isn't enough: the job can still be on its way out, and Disabled
// is only said once it's gone.
func TestLaunchdDisableWaitsForTheJobToGo(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	target := launchdDomain() + "/" + launchdLabel("box/web")
	inner := launchctl
	prints := 0
	stuck := false
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootout" {
			return "", nil
		}
		if args[0] == "print" && args[1] == target {
			if prints++; prints > 4 && !stuck {
				delete(f.loaded, target)
			}
		}
		return inner(args...)
	}
	if err := runDisable("box/web"); err != nil {
		t.Errorf("err = %v", err)
	}
	if prints <= 4 {
		t.Errorf("said Disabled after %d looks", prints)
	}

	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	stuck = true
	if err := runDisable("box/web"); err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(jobFile(home, "box/web")); err != nil {
		t.Errorf("job file: %v", err)
	}
}

// By label, with the file gone.
func TestLaunchdDisableWithTheFileDeleted(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jobFile(home, "box/web")); err != nil {
		t.Fatal(err)
	}
	if err := runDisable("box/web"); err != nil {
		t.Errorf("err = %v", err)
	}
	if f.loaded[launchdDomain()+"/"+launchdLabel("box/web")] {
		t.Error("still loaded")
	}
}

// launchctl failing isn't "not loaded", and nothing is called Disabled on that.
func TestLaunchdDisableWhenLaunchctlCantSay(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	f.fail["print"] = exitCode(112)
	err := runDisable("box/web")
	if err == nil || !strings.Contains(err.Error(), "can't tell") {
		t.Errorf("err = %v", err)
	}
	if _, serr := os.Stat(jobFile(home, "box/web")); serr != nil {
		t.Errorf("job file: %v", serr)
	}
	// A plain error, with no code to go by, is the same.
	f.fail["print"] = errors.New("launchctl isn't there")
	if err := runDisable("gone/web"); err == nil || !strings.Contains(err.Error(), "can't tell") {
		t.Errorf("err = %v", err)
	}
}

// A name the stricter check of enable would refuse can still be turned off.
func TestLaunchdDisableAcceptsOldNames(t *testing.T) {
	f, home := launchdEnv(t)
	for _, spec := range []string{"-old/web", "bo\x1bx/web", "box/we\u202eb"} {
		if err := os.MkdirAll(filepath.Dir(jobFile(home, spec)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jobFile(home, spec), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.loaded[launchdDomain()+"/"+launchdLabel(spec)] = true
		c := newTunnelDisableCmd()
		c.SetArgs([]string{"--", spec})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			t.Errorf("%q: %v", spec, err)
		}
		if f.loaded[launchdDomain()+"/"+launchdLabel(spec)] {
			t.Errorf("%q: still loaded", spec)
		}
		if _, err := os.Stat(jobFile(home, spec)); !os.IsNotExist(err) {
			t.Errorf("%q: %v", spec, err)
		}
	}
}

// parseLaunchdJob on what launchctl print really looks like, nested states included.
func TestParseLaunchdJob(t *testing.T) {
	const out = "gui/501/x = {\n\tactive count = 1\n\tpath = /a\n\ttype = LaunchAgent\n\tstate = spawn scheduled\n\n\tprogram = /m\n\truns = 2\n\tlast exit code = 78: EX_CONFIG\n\n\tspawn type = daemon\n\tendpoints = {\n\t\tstate = active\n\t}\n\tpid = 9\n}\n"
	j := parseLaunchdJob(out)
	if j.state != "spawn scheduled" || j.running || j.lastExit != "78: EX_CONFIG" || j.pid != 9 {
		t.Errorf("got %+v", j)
	}
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func TestLaunchdCarriesOnlyWhatItShould(t *testing.T) {
	_, home := launchdEnv(t)
	t.Setenv("METERM_TEAM_DIR", "/a&b<c>")
	t.Setenv("METERM_DEBUG", "1")
	t.Setenv("SECRET_TOKEN", "hunter2")
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", launchdLabel("box/web")+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"<key>METERM_TEAM_DIR</key>", "/a&amp;b&lt;c&gt;", "<key>METERM_NO_AUTOSYNC</key>", "<key>PATH</key>", "/usr/bin:/bin"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
	for _, not := range []string{"hunter2", "METERM_DEBUG", "SSH_AUTH_SOCK"} {
		if strings.Contains(s, not) {
			t.Errorf("%q in:\n%s", not, s)
		}
	}
}

var hostileNames = []string{
	"box/web",
	"box/</string></array><key>Program</key><string>/bin/evil</string>",
	"box/]]><!-- x -->&amp;&lt;",
	`box/"quote'single`,
	"box/$(touch /tmp/pwned)`id`;rm -rf ~",
	"box/%h %n ${HOME}",
	"../../../../etc/passwd/x",
	"box/" + strings.Repeat("a", 5000),
	"üñí/日本語",
	"box/web.plist",
	"box/we\nb",
	"box/x\nExecStartPost=/bin/evil\n<key>Program</key>",
	"a/b/c/d",
	"--config=/tmp/evil/x",
	"-oProxyCommand=x/y",
}

var labelRE = regexp.MustCompile(`^com\.github\.bcd91\.meterm\.tunnel\.[A-Za-z0-9_-]+-[0-9a-f]{16}$`)

func TestLaunchdLabelIsSafe(t *testing.T) {
	seen := map[string]string{}
	for _, spec := range append(hostileNames, "a-b/c", "a/b-c", "box/"+strings.Repeat("a", 5001)) {
		l := launchdLabel(spec)
		if !labelRE.MatchString(l) {
			t.Errorf("%q: %q", spec, l)
		}
		if strings.Contains(l, "..") || len(l)+len(".plist") > 255 {
			t.Errorf("%q: %q", spec, l)
		}
		if other, dup := seen[l]; dup {
			t.Errorf("%q and %q share %s", spec, other, l)
		}
		seen[l] = spec
	}
}

// Whatever a name holds, it comes back from the XML as that one argument, and the
// document has only the keys meterm wrote.
func TestRenderLaunchAgentHostile(t *testing.T) {
	wantKeys := []string{"EnvironmentVariables", "KeepAlive", "Label", "ProgramArguments", "RunAtLoad", "StandardErrorPath", "StandardOutPath", "ThrottleInterval"}
	for _, spec := range hostileNames {
		exe, config, logPath := "/opt/my tools/<meterm>&", "/home/a b/'hosts'&.yaml", "/l/a&b.log"
		env := map[string]string{"PATH": launchdPath, "METERM_TEAM_DIR": spec}
		x, err := renderLaunchAgent("com.x", exe, config, spec, logPath, env)
		if err != nil {
			t.Errorf("%q: %v", spec, err)
			continue
		}
		var doc struct {
			Dict struct {
				Keys []string `xml:"key"`
			} `xml:"dict"`
		}
		if err := xml.Unmarshal([]byte(x), &doc); err != nil {
			t.Errorf("%q: not XML: %v\n%s", spec, err, x)
			continue
		}
		slices.Sort(doc.Dict.Keys)
		if !slices.Equal(doc.Dict.Keys, wantKeys) {
			t.Errorf("%q: keys %v", spec, doc.Dict.Keys)
		}
		argv := plistStrings([]byte(x), "ProgramArguments")
		want := []string{exe, "--config", config, "tunnel", "up", "--keep", "--service", "--gave-up-exit", "0", "--", spec}
		if !slices.Equal(argv, want) {
			t.Errorf("%q: argv %q", spec, argv)
		}
		lintPlist(t, x, want)
	}
}

// lintPlist has macOS's own parser read the file. It reads a temp file and nothing
// more; launchd isn't involved.
func lintPlist(t *testing.T, x string, argv []string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	p := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(p, []byte(x), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", p).CombinedOutput()
	if err != nil {
		t.Errorf("plutil: %v: %s\n%s", err, out, x)
		return
	}
	var got struct {
		ProgramArguments []string
		KeepAlive        map[string]bool
		RunAtLoad        bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ProgramArguments, argv) || got.KeepAlive["SuccessfulExit"] || !got.RunAtLoad {
		t.Errorf("plutil read: %+v", got)
	}
}

func TestRenderLaunchAgentRefusesWhatXMLCantHold(t *testing.T) {
	for _, bad := range []string{"a\x1bb", "a\x00b", "a\x07b", "a\xffb", "a￾b"} {
		if _, err := renderLaunchAgent("l", "/e", "", "box/"+bad, "/l", nil); err == nil {
			t.Errorf("%q: spec accepted", bad)
		}
		if _, err := renderLaunchAgent("l", "/e"+bad, "", "box/web", "/l", nil); err == nil {
			t.Errorf("%q: exe accepted", bad)
		}
		if _, err := renderLaunchAgent("l", "/e", "", "box/web", "/l", map[string]string{"K": bad}); err == nil {
			t.Errorf("%q: env accepted", bad)
		}
	}
}

func TestServiceSupport(t *testing.T) {
	old := serviceGOOS
	defer func() { serviceGOOS = old }()
	for goos, ok := range map[string]bool{"linux": true, "darwin": true, "windows": false, "freebsd": false} {
		serviceGOOS = goos
		if err := checkServiceSupport(); (err == nil) != ok {
			t.Errorf("%s: %v", goos, err)
		}
		if _, err := serviceForOS(); (err == nil) != ok {
			t.Errorf("%s: %v", goos, err)
		}
	}
}

// A tunnel that gave up has no process and no state file, so this is all that shows it.
func TestStatusShowsEnabledAndStopped(t *testing.T) {
	f, home := launchdEnv(t)
	old := enabledServices
	enabledServices = launchdEnabled
	defer func() { enabledServices = old }()
	t.Setenv("METERM_RUNTIME_DIR", filepath.Join(home, "run"))
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	one := func() status.Row {
		t.Helper()
		rep, err := readStatus()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Entries) != 1 {
			t.Fatalf("entries: %+v", rep.Entries)
		}
		return rep.Entries[0]
	}
	f.print = "x = {\n\tstate = not running\n\tlast exit code = 0\n}\n"
	r := one()
	if r.State != stoppedStatus || r.Host != "box" || r.Name != "web" || r.PID != 0 || !strings.Contains(r.Detail, ".log") {
		t.Errorf("gave up: %+v", r)
	}
	// No state file doesn't mean stopped: launchd is what says so.
	f.print = "x = {\n\tstate = running\n\tpid = 77\n\tlast exit code = (never exited)\n}\n"
	if r := one(); r.State != runningStatus || r.PID != 77 {
		t.Errorf("running with no file: %+v", r)
	}
	f.print = "x = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}\n"
	if r := one(); r.State != stoppedStatus || !strings.Contains(r.Detail, "tries again") {
		t.Errorf("failing: %+v", r)
	}
	f.loaded = map[string]bool{}
	if r := one(); r.State != stoppedStatus || !strings.Contains(r.Detail, "not loaded") {
		t.Errorf("not loaded: %+v", r)
	}
	f.fail["print"] = exitCode(112)
	if r := one(); r.State != unknownStatus {
		t.Errorf("launchctl can't say: %+v", r)
	}
	delete(f.fail, "print")

	reg := status.Open()
	defer reg.Close()
	reg.Add(status.KindTunnel, "box", "web").Set(status.Connected, "")
	if r := one(); r.State != status.Connected || r.PID != os.Getpid() {
		t.Errorf("a running tunnel: %+v", r)
	}
	reg.Close()
	f.loaded[launchdDomain()+"/"+launchdLabel("box/web")] = true
	if err := runDisable("box/web"); err != nil {
		t.Fatal(err)
	}
	rep, err := readStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 0 {
		t.Errorf("after disable: %+v", rep.Entries)
	}
}

// The binary moved: launchd can't start it, and the row says that rather than "see the
// log", which has nothing in it about this.
func TestStatusSaysWhenLaunchdCantSpawn(t *testing.T) {
	f, _ := launchdEnv(t)
	target := launchdDomain() + "/x"
	f.loaded[target] = true
	f.print = "x = {\n\tstate = spawn scheduled\n\tlast exit code = 78: EX_CONFIG\n}\n"
	exe := filepath.Join(t.TempDir(), "meterm")
	state, _, detail := launchdRowState(enabledService{exe: exe, target: target, log: "/l"})
	if state != stoppedStatus || !strings.Contains(detail, "can't start it") || !strings.Contains(detail, exe) {
		t.Errorf("missing binary: %s, %s", state, detail)
	}
	if err := os.WriteFile(exe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, detail := launchdRowState(enabledService{exe: exe, target: target, log: "/l"}); !strings.Contains(detail, "78: EX_CONFIG") {
		t.Errorf("not runnable: %s", detail)
	}
}

// A job file holding what XML allows but a terminal shouldn't see. &#x1b; wouldn't do:
// that isn't XML, so such a file never gets as far as the sanitizer.
func TestServiceRowsCleanNamesFromJobFiles(t *testing.T) {
	f, home := launchdEnv(t)
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const job = "<plist><dict><key>ProgramArguments</key><array><string>/x\u202e</string><string>--</string><string>h\u202eost\u0085/t\u2066un\u009b2J</string></array></dict></plist>"
	if err := os.WriteFile(filepath.Join(dir, launchdLabelPrefix+"esc-1.plist"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	f.loaded[launchdDomain()+"/"+launchdLabelPrefix+"esc-1"] = true
	f.print = "x = {\n\tstate = not running\n}\n"
	old := enabledServices
	enabledServices = launchdEnabled
	defer func() { enabledServices = old }()
	rows := serviceRows(nil)
	if len(rows) != 1 {
		t.Fatalf("the fixture didn't reach the sanitizer: %+v", rows)
	}
	r := rows[0]
	if !strings.HasPrefix(r.Host, "h") || !strings.HasPrefix(r.Name, "t") {
		t.Errorf("row: %+v", r)
	}
	for _, s := range []string{r.Host, r.Name, r.Detail} {
		if strings.ContainsAny(s, "\u202e\u0085\u2066\u009b") {
			t.Errorf("got through: %s", strconv.Quote(s))
		}
	}
}

func TestLaunchdEnabledSkipsWhatIsntOurs(t *testing.T) {
	_, home := launchdEnv(t)
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"com.example.other.plist":         "<plist/>",
		launchdLabelPrefix + "a-1.plist":  "not xml <<<",
		launchdLabelPrefix + "b-1.plist":  "<plist><dict><key>ProgramArguments</key><array><string>/x</string></array></dict></plist>",
		launchdLabelPrefix + "c-1.txt":    "x",
		launchdLabelPrefix + "e-1.plist":  strings.Repeat(" ", 1<<17),
		launchdLabelPrefix + "good.plist": "<plist><dict><key>ProgramArguments</key><array><string>/x</string><string>--</string><string>h/t</string></array></dict></plist>",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := launchdEnabled()
	if len(got) != 1 || got[0].spec != "h/t" || got[0].exe != "/x" {
		t.Errorf("got %+v", got)
	}
}

// How each way of ending comes out as an exit code, through the real command and
// finish. Only a verdict from a config that parsed may stop a job for good.
func TestTunnelUpServiceExitCodes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(cfg string, args ...string) int {
		t.Helper()
		p := filepath.Join(t.TempDir(), "hosts.yaml")
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		c := exec.Command(self)
		c.Env = append(os.Environ(), runTunnelUpEnv+"="+strings.Join(args, "\x1f"), "METERM_CONFIG="+p,
			"METERM_ACTIVITY_DIR="+filepath.Join(filepath.Dir(p), "a"), "METERM_NO_AUTOSYNC=1", "METERM_RUNTIME_DIR="+filepath.Join(filepath.Dir(p), "r"))
		_ = c.Run()
		return c.ProcessState.ExitCode()
	}
	launchd := []string{"--keep", "--service", "--gave-up-exit", "0", "--", "box/web"}
	systemd := []string{"--keep", "--service", "--", "box/web"}
	const good = "version: 1\nhosts:\n- {name: other, address: 192.0.2.1}\n"
	const unsafe = "version: 1\nhosts:\n- name: box\n  address: \"-oProxyCommand=x\"\n  forwards:\n  - {name: web, type: L, local_port: 1, remote_host: a, remote_port: 2}\n"
	for name, tc := range map[string]struct {
		cfg              string
		launchd, systemd int
	}{
		"tunnel removed":      {good, 0, tunnelGaveUp},
		"host turned unsafe":  {unsafe, 0, tunnelGaveUp},
		"config won't parse":  {"hosts: [ {{{ not yaml", 1, 1},
		"config has no hosts": {"version: 1\nhosts: []\n", 1, 1},
		"config is empty":     {"", 1, 1},
	} {
		if got := run(tc.cfg, launchd...); got != tc.launchd {
			t.Errorf("%s, launchd: exit %d, want %d", name, got, tc.launchd)
		}
		if got := run(tc.cfg, systemd...); got != tc.systemd {
			t.Errorf("%s, systemd: exit %d, want %d", name, got, tc.systemd)
		}
	}
}

func TestTunnelServiceExit(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{tunnelVerdict{boom}, 5},
		{fmt.Errorf("wrapped: %w", tunnelVerdict{boom}), 5},
		{errTunnelSignalled, tunnelSignalled},
		{boom, 1},
	} {
		if got := tunnelServiceExit(tc.err, 5); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.err, got, tc.want)
		}
	}
}

// What ssh printed never ends a service, whatever it says: that includes the server's
// banner, sent before authentication.
func TestHoldTunnelServiceOutlivesWhatSSHSays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	for _, said := range []string{"Unable to negotiate with x", "Bad configuration option: x", "Bad owner or permissions on x", "Permission denied (publickey).", "REMOTE HOST IDENTIFICATION HAS CHANGED"} {
		dir := t.TempDir()
		script := "#!/bin/sh\necho '" + said + "' >&2\nexit 255\n"
		if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err := holdTunnel(ctx, "h", func() ([]string, error) { return nil, nil }, tunnelHold{keep: true, service: true}, io.Discard)
		cancel()
		if err != nil {
			t.Errorf("%q ended it: %v", said, err)
		}
		// A person at a terminal still hears about it at once.
		err = holdTunnel(context.Background(), "h", func() ([]string, error) { return nil, nil }, tunnelHold{keep: true, stopOnFailure: true}, io.Discard)
		if strings.HasPrefix(said, "Unable") && err == nil {
			t.Errorf("%q: not stopped without service", said)
		}
	}
}

// Hostile names in a state file don't reach the terminal through `meterm status`.
func TestWriteStatusTextCleans(t *testing.T) {
	var b strings.Builder
	bad := "x\x1b]52;c;aGk=\x07‮\ny"
	rep := statusReport{Schema: status.SchemaVersion, Entries: []status.Row{{Entry: status.Entry{Kind: status.KindTunnel, Host: bad, Name: bad, State: bad, Route: bad, Detail: bad, Since: time.Now()}}}}
	if err := writeStatus(&b, rep, "text"); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(b.String(), "\x1b\x07‮") || strings.Count(b.String(), "\n") != 2 {
		t.Errorf("%q", b.String())
	}
}

// Both formats: a status bar shows JSON strings as they come.
func TestReadStatusCleansForJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	t.Setenv("METERM_RUNTIME_DIR", dir)
	reg := status.Open()
	defer reg.Close()
	bad := "x\x1b]52;c;aGk=\x07\u202e\u2028y"
	h := reg.Add(status.KindTunnel, bad, bad)
	h.SetRoute(bad)
	h.Set(status.Failing, bad)
	rep, err := readStatus()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := writeStatus(&b, rep, "json"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{`\u001b`, `\u0007`, "\u202e", `\u2028`, "\x1b"} {
		if strings.Contains(b.String(), c) {
			t.Errorf("%q in %s", c, b.String())
		}
	}
}
