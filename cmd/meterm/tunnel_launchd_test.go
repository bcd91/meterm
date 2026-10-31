package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
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
}

func (f *fakeLaunchd) run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if err := f.fail[args[0]]; err != nil {
		return "launchd says no", err
	}
	switch args[0] {
	case "print":
		if !f.loaded[args[1]] {
			return "", errors.New("exit status 113")
		}
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
	want := []string{"print " + target, "bootstrap " + launchdDomain() + " " + plist, "kickstart " + target, "getenv SSH_AUTH_SOCK"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("launchctl: %q", f.calls)
	}

	f.calls = nil
	if err := runEnable("box/web"); err != nil { // again: from the file as it is now
		t.Fatal(err)
	}
	want = []string{"print " + target, "bootout " + target, "bootstrap " + launchdDomain() + " " + plist, "kickstart " + target, "getenv SSH_AUTH_SOCK"}
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
	if !slices.Equal(f.calls, []string{"print " + target, "bootout " + target}) {
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
	if len(f.calls) != 0 {
		t.Errorf("launchctl was called: %v", f.calls)
	}
}

// A failure is reported, with the job file left for the next login, as on systemd.
func TestLaunchdEnableReportsBootstrap(t *testing.T) {
	f, home := launchdEnv(t)
	f.fail["bootstrap"] = errors.New("exit status 125")
	err := runEnable("box/web")
	if err == nil || !strings.Contains(err.Error(), "launchd says no") {
		t.Errorf("err = %v", err)
	}
	if n := countPrefix(f.calls, "bootstrap "); n != 3 {
		t.Errorf("%d tries: %q", n, f.calls)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", launchdLabel("box/web")+".plist")); err != nil {
		t.Errorf("job file: %v", err)
	}
}

func TestLaunchdDisableKeepsTheFileIfItCantStop(t *testing.T) {
	f, home := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	f.fail["bootout"] = errors.New("exit status 1")
	if err := runDisable("box/web"); err == nil {
		t.Error("no error")
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", launchdLabel("box/web")+".plist")); err != nil {
		t.Errorf("job file: %v", err)
	}
}

// launchctl can fail on the way and still have stopped the job.
func TestLaunchdDisableGoesByWhetherItStopped(t *testing.T) {
	f, _ := launchdEnv(t)
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	f.fail["bootout"] = errors.New("exit status 36")
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
	_, home := launchdEnv(t)
	old := enabledServices
	enabledServices = launchdEnabled
	defer func() { enabledServices = old }()
	t.Setenv("METERM_RUNTIME_DIR", filepath.Join(home, "run"))
	if err := runEnable("box/web"); err != nil {
		t.Fatal(err)
	}
	rep, err := readStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].State != stoppedStatus || rep.Entries[0].Host != "box" || rep.Entries[0].Name != "web" || rep.Entries[0].PID != 0 || !strings.Contains(rep.Entries[0].Detail, ".log") {
		t.Fatalf("entries: %+v", rep.Entries)
	}

	reg := status.Open()
	defer reg.Close()
	reg.Add(status.KindTunnel, "box", "web").Set(status.Connected, "")
	rep, err = readStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].State != status.Connected || rep.Entries[0].PID != os.Getpid() {
		t.Errorf("a running tunnel: %+v", rep.Entries)
	}

	reg.Close()
	if err := runDisable("box/web"); err != nil {
		t.Fatal(err)
	}
	rep, err = readStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 0 {
		t.Errorf("after disable: %+v", rep.Entries)
	}
}

func TestLaunchdEnabledSkipsWhatIsntOurs(t *testing.T) {
	_, home := launchdEnv(t)
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"com.example.other.plist":          "<plist/>",
		launchdLabelPrefix + "a-1.plist":   "not xml <<<",
		launchdLabelPrefix + "b-1.plist":   "<plist><dict><key>ProgramArguments</key><array><string>/x</string></array></dict></plist>",
		launchdLabelPrefix + "c-1.txt":     "x",
		launchdLabelPrefix + "e-1.plist":   strings.Repeat(" ", 1<<17),
		launchdLabelPrefix + "good.plist":  "<plist><dict><key>ProgramArguments</key><array><string>/x</string><string>--</string><string>h/t</string></array></dict></plist>",
		launchdLabelPrefix + "esc-1.plist": "<plist><dict><key>ProgramArguments</key><array><string>--</string><string>h&#x1b;[2J/t&#x202e;</string></array></dict></plist>",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := launchdEnabled()
	specs := []string{}
	for _, s := range got {
		specs = append(specs, s.spec)
	}
	if !slices.Contains(specs, "h/t") {
		t.Errorf("specs: %q", specs)
	}
	old := enabledServices
	enabledServices = func() []enabledService { return got }
	defer func() { enabledServices = old }()
	for _, r := range serviceRows(nil) {
		for _, s := range []string{r.Host, r.Name, r.Detail} {
			if strings.ContainsAny(s, "\x1b‮") {
				t.Errorf("got through: %s", strconv.Quote(s))
			}
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
