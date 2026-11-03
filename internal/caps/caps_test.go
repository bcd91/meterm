package caps

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// From RemoteCommand run by zsh -c on macOS 26 on an M2 with the developer tools and no
// JDK, as ssh would run it, with the account renamed. go is installed in ~/sdk, which a
// non-interactive login doesn't have on its PATH, and /usr/bin/java isn't run.
const macShape = `caps=1
user=alice
os=Darwin
arch=arm64
kernel=25.6.0
distro=macos
distro_version=26.6.2
cpus=8
cpu_model=Apple M2
mem_total_bytes=8589934592
home_df=/dev/disk3s1   239362496  96376220 113087844    47%    /System/Volumes/Data
load={ 4.33 3.87 4.04 }
tool=claude	/Users/alice/.local/bin/claude	0	2.1.317 (Claude Code)
tool=codex
tool=git	/usr/bin/git	0	git version 2.50.1 (Apple Git-155)
tool=go
tool=node	/opt/homebrew/bin/node	0	v26.11.1
tool=python3	/usr/bin/python3	0	Python 3.9.6
tool=uv	/opt/homebrew/bin/uv	0	uv 0.13.0 (Homebrew 2026-10-09 aarch64-apple-darwin)
tool=docker
tool=podman
tool=kubectl
tool=make	/usr/bin/make	0	GNU Make 3.81
tool=gcc	/usr/bin/gcc	0	Apple clang version 21.0.0 (clang-2100.1.1.101)
tool=rustc
tool=cargo
tool=java	/usr/bin/java	-
tool=dotnet
tool=psql
tool=mysql
tool=sqlcmd
tool=tmux
end=1
`

func TestParseMacShape(t *testing.T) {
	c := Parse([]byte(macShape))
	if !c.Answered || !c.Complete || c.Unparsed != 0 {
		t.Fatalf("answered %v complete %v, unparsed %d", c.Answered, c.Complete, c.Unparsed)
	}
	if c.OS != "Darwin" || c.Arch != "arm64" || c.Distro != "macos" || c.DistroVersion != "26.6.2" || c.CPUModel != "Apple M2" || c.User != "alice" {
		t.Errorf("%+v", c)
	}
	if c.CPUs == nil || *c.CPUs != 8 || c.MemTotalMB == nil || *c.MemTotalMB != 8192 {
		t.Errorf("cpus %v mem %v", c.CPUs, c.MemTotalMB)
	}
	if c.MemAvailableMB != nil || c.MemLimitMB != nil || c.CPULimit != nil {
		t.Errorf("macOS gives none of these, so they're unknown: %+v", c)
	}
	if c.HomeTotalMB == nil || *c.HomeTotalMB != 233752 || c.HomeFreeMB == nil || *c.HomeFreeMB != 110437 {
		t.Errorf("home %v %v", c.HomeTotalMB, c.HomeFreeMB)
	}
	if c.Load1 == nil || *c.Load1 != 4.33 {
		t.Errorf("load %v", c.Load1)
	}
	if c.GPUs != nil || c.GPUCheck != "" || c.DockerDaemon != "" {
		t.Errorf("no nvidia-smi says nothing of GPUs: %+v %q %q", c.GPUs, c.GPUCheck, c.DockerDaemon)
	}
	if len(c.Tools) != len(Tools) {
		t.Fatalf("%d tools", len(c.Tools))
	}
	byName := map[string]Tool{}
	for _, tool := range c.Tools {
		byName[tool.Name] = tool
	}
	if cl := byName["claude"]; !cl.Found || cl.Runs == nil || !*cl.Runs || cl.Version != "2.1.317 (Claude Code)" {
		t.Errorf("%+v", cl)
	}
	if j := byName["java"]; !j.Found || j.Runs != nil || j.Note != "not run: a macOS stand-in" {
		t.Errorf("a stand-in that wasn't run is unknown, not broken: %+v", j)
	}
	if d := byName["docker"]; d.Found || d.Runs != nil || d.Path != "" {
		t.Errorf("%+v", d)
	}
}

// Written by hand from the formats of /proc, cgroup v2 and nvidia-smi, not copied from a
// host: replace it with real output from a Linux host with a GPU and docker when there is
// one.
const linuxShape = `Welcome to web1, from a .bashrc
caps=1
limiter=timeout-term
user=deploy
os=Linux
arch=x86_64
kernel=6.12.113+deb13-amd64
distro=debian
distro_version=13
cpus=16
cpu_model=AMD Ryzen 9 7950X 16-Core Processor
mem_total_kb=65536000
mem_available_kb=48000000
cgroup_memory_max=2147483648
cgroup_cpu_max=200000 100000
home_df=/dev/mapper/vg-root 960000000 400000000 512000000      44% /
load=0.52 0.40 0.33 2/812 99812
gpu_check=nvidia-smi
gpu=NVIDIA GeForce RTX 4090, 24564
tool=claude
tool=git	/usr/bin/git	0	git version 2.47.3
tool=docker	/usr/bin/docker	0	Docker version 28.5.1, build e180ab8
tool=python3	/usr/bin/python3	124
tool=tmux	/usr/bin/tmux	1
tool=psql	/usr/bin/psql	143	
docker_daemon=28.5.1
end=1
`

func TestParseLinuxShape(t *testing.T) {
	c := Parse([]byte(linuxShape))
	if !c.Complete || c.Unparsed != 1 {
		t.Fatalf("complete %v, unparsed %d (the banner)", c.Complete, c.Unparsed)
	}
	if c.MemTotalMB == nil || *c.MemTotalMB != 64000 || c.MemAvailableMB == nil || *c.MemAvailableMB != 46875 {
		t.Errorf("mem %v %v", c.MemTotalMB, c.MemAvailableMB)
	}
	if c.MemLimitMB == nil || *c.MemLimitMB != 2048 || c.CPULimit == nil || *c.CPULimit != 2 {
		t.Errorf("cgroup limits %v %v", c.MemLimitMB, c.CPULimit)
	}
	if c.HomeFreeMB == nil || *c.HomeFreeMB != 500000 || c.Load1 == nil || *c.Load1 != 0.52 {
		t.Errorf("home %v load %v", c.HomeFreeMB, c.Load1)
	}
	if len(c.GPUs) != 1 || c.GPUs[0].Name != "NVIDIA GeForce RTX 4090" || c.GPUs[0].MemoryMB == nil || *c.GPUs[0].MemoryMB != 24564 {
		t.Errorf("%+v", c.GPUs)
	}
	if c.DockerDaemon != "28.5.1" {
		t.Errorf("docker %q", c.DockerDaemon)
	}
	for _, tool := range c.Tools {
		switch tool.Name {
		case "python3":
			if tool.Runs != nil || tool.Note != "timed out" {
				t.Errorf("timed out is unknown, not broken: %+v", tool)
			}
		case "psql":
			if tool.Runs != nil || tool.Note != "timed out" {
				t.Errorf("busybox timeout's TERM is a timeout: %+v", tool)
			}
		case "tmux":
			if tool.Runs == nil || *tool.Runs {
				t.Errorf("%+v", tool)
			}
		}
	}
}

func TestParseGPUs(t *testing.T) {
	// nvidia-smi there and failing says nothing about GPUs, and its error isn't one.
	c := Parse([]byte("caps=1\ngpu_check=failed\ngpu=NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\nend=1\n"))
	if c.GPUCheck != "failed" || c.GPUs != nil || c.Unparsed != 1 {
		t.Errorf("%+v", c)
	}
	// Answered with nothing is none.
	c = Parse([]byte("caps=1\ngpu_check=nvidia-smi\nend=1\n"))
	if c.GPUs == nil || len(c.GPUs) != 0 {
		t.Errorf("%+v", c.GPUs)
	}
	// A gpu line with no check before it isn't believed.
	c = Parse([]byte("caps=1\ngpu=H100, 81920\nend=1\n"))
	if c.GPUs != nil || c.Unparsed != 1 {
		t.Errorf("%+v", c)
	}
}

func TestParseUnknownsAndLimits(t *testing.T) {
	// Cut short, with every number unreadable: nothing turns into 0, and nothing passes for
	// complete.
	c := Parse([]byte("caps=1\ncpus=0\nmem_total_kb=abc\ncgroup_memory_max=max\ncgroup_cpu_max=max 100000\nhome_df=df: /home/x: No such file\nload=NaN\ndocker_daemon=-\n"))
	if c.Complete || c.CPUs != nil || c.MemTotalMB != nil || c.MemLimitMB != nil || c.CPULimit != nil || c.HomeFreeMB != nil || c.Load1 != nil {
		t.Errorf("%+v", c)
	}
	if c.DockerDaemon != "unreachable" || c.Tools == nil {
		t.Errorf("%+v", c)
	}
	for _, load := range []string{"Inf", "1e400", "-1", "1e9"} {
		if c := Parse([]byte("caps=1\nload=" + load + "\n")); c.Load1 != nil {
			t.Errorf("load %s read as %v", load, *c.Load1)
		}
	}
	// Whatever a host says, the report still encodes: NaN or Inf would fail it for every host.
	if _, err := json.Marshal(Parse([]byte("caps=1\nload=NaN\ncgroup_cpu_max=1e400 1\n"))); err != nil {
		t.Fatal(err)
	}
	if c := Parse([]byte("caps=1\ndocker_daemon=not-local\n")); c.DockerDaemon != "not-local" {
		t.Errorf("%q", c.DockerDaemon)
	}

	// Nothing counts before caps=1, so a banner can't plant facts, and a second caps=1
	// starts again. A tool line for a name not asked about, or a second line for one that
	// was, isn't believed.
	c = Parse([]byte("cpus=999\nend=1\ncaps=1\ntool=docker\t/x\t0\tfake\ncaps=1\ntool=evil\t/x\t0\tv\ntool=git\t/usr/bin/git\t0\tgit 2\ntool=git\t/tmp/git\t0\tfake\n"))
	if c.CPUs != nil || c.Complete || len(c.Tools) != 1 || c.Tools[0].Path != "/usr/bin/git" || c.Unparsed != 4 {
		t.Errorf("%+v", c)
	}
	// end said twice isn't complete; a status means timed out only for the limiter that gives it.
	if c := Parse([]byte("caps=1\nend=1\nend=1\n")); c.Complete {
		t.Error("end twice")
	}
	for _, x := range []struct {
		limiter, code string
		timedOut      bool
	}{{"perl", "142", true}, {"perl", "137", false}, {"timeout", "137", true}, {"timeout", "143", false}, {"timeout-term", "143", true}, {"none", "137", false}, {"none", "142", false}} {
		c := Parse([]byte("caps=1\nlimiter=" + x.limiter + "\ntool=git\t/usr/bin/git\t" + x.code + "\t\n"))
		if got := c.Tools[0].Note == "timed out"; got != x.timedOut || (!got && (c.Tools[0].Runs == nil || *c.Tools[0].Runs)) {
			t.Errorf("%s %s: %+v", x.limiter, x.code, c.Tools[0])
		}
	}
	// Said twice is unknown, whichever came first.
	c = Parse([]byte("caps=1\ncpus=8\ncpus=512\ndistro=debian\ndistro=fake\nend=1\n"))
	if c.CPUs != nil || c.Distro != "" || !c.Complete || c.Unparsed != 2 {
		t.Errorf("%+v", c)
	}
	// Nothing from the script at all isn't an answer.
	if c := Parse([]byte("This account is for git only.\n")); c.Answered || c.Unparsed != 1 {
		t.Errorf("%+v", c)
	}

	// What a host says is text, cleaned and cut.
	c = Parse([]byte("caps=1\ncpu_model=x\x1b]0;pwned\x07\u202e" + strings.Repeat("a", 1000) + "\n"))
	if strings.ContainsAny(c.CPUModel, "\x1b\x07\u202e") || len([]rune(c.CPUModel)) > 256 {
		t.Errorf("%q", c.CPUModel)
	}
}

func TestParseNumbers(t *testing.T) {
	for in, want := range map[string]bool{"mem_total_kb=0": false, "mem_total_bytes=1000": false, "mem_total_kb=-5": false, "mem_total_kb=2048": true} {
		if got := Parse([]byte("caps=1\n"+in+"\n")).MemTotalMB != nil; got != want {
			t.Errorf("%s: %v", in, got)
		}
	}
	for in, want := range map[string]float64{"150000 100000": 1.5, "33333 100000": 0.33, "0 100000": -1, "max 100000": -1, "5 0": -1} {
		got := Parse([]byte("caps=1\ncgroup_cpu_max=" + in + "\n")).CPULimit
		if (want < 0) != (got == nil) || (got != nil && *got != want) {
			t.Errorf("%s: %v", in, got)
		}
	}
	// cgroup: max is read and means none; v1's huge number means none; v1's limit is a limit.
	if c := Parse([]byte("caps=1\ncgroup_memory_max=max\n")); !c.CgroupRead || c.MemLimitMB != nil {
		t.Errorf("%+v", c)
	}
	if c := Parse([]byte("caps=1\ncgroup1_memory_limit=9223372036854771712\n")); !c.CgroupRead || c.MemLimitMB != nil {
		t.Errorf("%+v", c)
	}
	if c := Parse([]byte("caps=1\ncgroup1_memory_limit=2147483648\n")); !c.CgroupRead || c.MemLimitMB == nil || *c.MemLimitMB != 2048 {
		t.Errorf("%+v", c)
	}
	if c := Parse([]byte("caps=1\n")); c.CgroupRead {
		t.Error("no file read isn't no limit")
	}
	// Said twice, these too are unknown.
	c := Parse([]byte("caps=1\nmem_total_kb=2048\nmem_total_kb=4096\nhome_df=/dev/a 1024 0 1024 0% /\nhome_df=/dev/a 2048 0 2048 0% /\n"))
	if c.MemTotalMB != nil || c.HomeFreeMB != nil || c.HomeTotalMB != nil {
		t.Errorf("%+v", c)
	}
}

// RemoteCommand goes inside one pair of single quotes for whatever the login shell is.
func TestRemoteCommandQuoting(t *testing.T) {
	inner := strings.TrimSuffix(strings.TrimPrefix(RemoteCommand, "sh -c '"), "'")
	if inner == RemoteCommand || strings.Contains(inner, "'") {
		t.Fatal("RemoteCommand must be sh -c '...' with no single quote inside")
	}
	if strings.ContainsAny(RemoteCommand, "\n!") || strings.Contains(RemoteCommand, `\\`) {
		t.Error("a newline, ! or a doubled backslash breaks csh or fish")
	}
	for _, tool := range Tools {
		if tool == "" || strings.Trim(tool, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Errorf("tool %q isn't a plain word", tool)
		}
	}
	// Asking a version mustn't fetch or set anything up, or ask anyone at the screen.
	for _, guard := range []string{"cd / ||", "-XX:-UsePerfData", "GOTOOLCHAIN=local", "RUSTUP_AUTO_INSTALL=0", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1",
		"DOTNET_GENERATE_ASPNET_CERTIFICATE=false", "xcode-select -p", "JavaVirtualMachines", "unset OMP_NUM_THREADS", "timeout -k 1 5", "defined(", "tmux) a=-V", "not-local", "kill 9, -"} {
		if !strings.Contains(RemoteCommand, guard) {
			t.Errorf("RemoteCommand lost %q", guard)
		}
	}
}

// safePath is a directory of links to the few programs the script needs and nothing else,
// for PATH, with nothing added to it. Run with it, the script can't reach a real stand-in
// such as /usr/bin/java, which would put an install dialog on this machine's screen, or
// any other real tool: the tests see only the fakes they put first.
func safePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"sh", "uname", "id", "sed", "cat", "tr", "df", "head", "tail", "sysctl", "getconf", "nproc", "perl", "sleep", "sw_vers", "env", "timeout"} {
		for _, d := range []string{"/bin", "/usr/bin", "/usr/sbin", "/sbin"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				if err := os.Symlink(filepath.Join(d, name), filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}
	return dir
}

// safeCommand is RemoteCommand with nothing added to PATH and stand-ins looked for in
// standIns.
func safeCommand(standIns string) string { return remoteCommand(standIns, "") }

func runScript(t *testing.T, shell, script string) []byte {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

// The script run for real by each login shell this machine has, as ssh would run it, with
// a temporary HOME and only safePath's programs.
func TestRemoteCommandRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a POSIX sh is what this needs")
	}
	if !strings.Contains(RemoteCommand, SearchPath) || !strings.Contains(RemoteCommand, "/usr/bin/java)") {
		t.Fatal("RemoteCommand isn't remoteCommand with the real places")
	}
	shells := map[string]string{}
	for _, sh := range []string{"sh", "bash", "zsh", "dash", "ksh", "fish", "tcsh", "csh"} {
		if path, err := exec.LookPath(sh); err == nil {
			shells[sh] = path
		} else {
			t.Logf("%s isn't here, so it wasn't tried", sh)
		}
	}
	if len(shells) == 0 {
		t.Fatal("no shell found to run it with")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", safePath(t))
	script := safeCommand(t.TempDir())
	for sh, path := range shells {
		t.Run(sh, func(t *testing.T) {
			out := runScript(t, path, script)
			c := Parse(out)
			if !c.Answered || !c.Complete || c.Unparsed != 0 {
				t.Fatalf("complete %v, unparsed %d:\n%s", c.Complete, c.Unparsed, out)
			}
			uname, _ := exec.Command("/usr/bin/uname", "-s").Output()
			if c.OS != strings.TrimSpace(string(uname)) || c.Arch == "" || c.CPUs == nil || c.MemTotalMB == nil || c.HomeFreeMB == nil || c.Load1 == nil || c.Limiter == "" {
				t.Errorf("%+v\n%s", c, out)
			}
			if len(c.Tools) != len(Tools) {
				t.Errorf("%d tools of %d:\n%s", len(c.Tools), len(Tools), out)
			}
		})
	}
}

// fakeTool writes an executable shell script named name into dir.
func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The script against made-up tools: what a host's tools do mustn't turn into facts that
// aren't so, hang the look, or reach another host.
func TestRemoteCommandAgainstFakes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a POSIX sh is what this needs")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("the time limit under test is perl's")
	}
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+safePath(t))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("OMP_NUM_THREADS", "1")
	log := filepath.Join(home, "docker.log")
	fakeTool(t, bin, "git", `echo "git version 9.9.9"; kill -SEGV $$`)
	fakeTool(t, bin, "node", `sleep 30`)
	fakeTool(t, bin, "python3", `sleep 30 & echo "Python 3.1.1"`)
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTool(t, bin, "nvidia-smi", `echo "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."; exit 9`)
	fakeTool(t, bin, "docker", `echo "$1" >> "`+log+`"; case "$1" in --version) echo "Docker version 1.2.3";; context) exit 1;; info) echo 9.9;; esac`)
	fakeTool(t, bin, "uname", `case "$1" in -m) printf 'x86_64\ncpus=999\ngpu_check=nvidia-smi\n';; *) exec /usr/bin/uname "$@";; esac`)

	start := time.Now()
	out := runScript(t, "/bin/sh", "printf '\033]0;title\007'; "+safeCommand(t.TempDir()))
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("a hung tool, or a child left behind, held the look %v", took)
	}
	c := Parse(out)
	if !c.Answered || !c.Complete || c.Limiter != "perl" {
		t.Fatalf("a banner with no newline lost the answer:\n%q", out)
	}
	if c.CPUs != nil && *c.CPUs == 999 || c.GPUCheck != "failed" || c.GPUs != nil {
		t.Errorf("planted or failed facts were believed: cpus %v gpu %q %v\n%s", c.CPUs, c.GPUCheck, c.GPUs, out)
	}
	if real, err := exec.Command("/bin/sh", "-c", "getconf _NPROCESSORS_ONLN").Output(); err == nil && strings.TrimSpace(string(real)) != "1" {
		if c.CPUs == nil || *c.CPUs == 1 {
			t.Errorf("OMP_NUM_THREADS decided the CPU count: %v", c.CPUs)
		}
	}
	byName := map[string]Tool{}
	for _, tool := range c.Tools {
		byName[tool.Name] = tool
	}
	if g := byName["git"]; g.Runs == nil || *g.Runs {
		t.Errorf("a tool that crashed passed: %+v", g)
	}
	if u := byName["uv"]; u.Runs == nil || *u.Runs {
		t.Errorf("a tool that couldn't be run passed: %+v", u)
	}
	if n := byName["node"]; n.Runs != nil || n.Note != "timed out" {
		t.Errorf("%+v", n)
	}
	if p := byName["python3"]; p.Runs == nil || !*p.Runs || p.Version == "" {
		t.Errorf("%+v", p)
	}
	if c.DockerDaemon != "not-checked" {
		t.Errorf("docker %q", c.DockerDaemon)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "info") {
		t.Errorf("docker info was asked with nowhere known to point: %s", b)
	}

	// With DOCKER_HOST elsewhere, it isn't asked either.
	_ = os.Remove(log)
	t.Setenv("DOCKER_HOST", "ssh://me@elsewhere")
	if c := Parse(runScript(t, "/bin/sh", safeCommand(t.TempDir()))); c.DockerDaemon != "not-local" {
		t.Errorf("%q", c.DockerDaemon)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "info") {
		t.Errorf("docker info followed DOCKER_HOST elsewhere: %s", b)
	}
	// A local daemon that says nothing is unreachable, not no docker.
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	fakeTool(t, bin, "docker", `case "$1" in --version) echo "Docker version 1.2.3";; info) exit 0;; esac`)
	if c := Parse(runScript(t, "/bin/sh", safeCommand(t.TempDir()))); c.DockerDaemon != "unreachable" {
		t.Errorf("%q", c.DockerDaemon)
	}
}

// On a Mac, a tool where the stand-ins live is run only when the developer directory has
// the real one, and java only with a JDK. The stand-ins here are fakes that leave a mark,
// in a directory of the test's own, so nothing real is ever run.
func TestMacStandInsArentRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	home, bin, standIns, dev := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+standIns+string(os.PathListSeparator)+safePath(t))
	mark := filepath.Join(home, "ran")
	for _, name := range []string{"git", "java"} {
		fakeTool(t, standIns, name, `echo `+name+` >> "`+mark+`"; echo "`+name+` version 1.2.3"`)
	}
	ran := func() string { b, _ := os.ReadFile(mark); _ = os.Remove(mark); return string(b) }
	tool := func(out []byte, name string) Tool {
		for _, tl := range Parse(out).Tools {
			if tl.Name == name {
				return tl
			}
		}
		return Tool{}
	}
	script := safeCommand(standIns)

	// No developer tools: neither runs.
	fakeTool(t, bin, "xcode-select", `echo /nonexistent/Developer`)
	out := runScript(t, "/bin/sh", script)
	if r := ran(); r != "" || tool(out, "git").Note != "not run: a macOS stand-in" {
		t.Fatalf("ran %q:\n%s", r, out)
	}
	if _, err := os.Stat("/Library/Java/JavaVirtualMachines"); os.IsNotExist(err) && tool(out, "java").Note != "not run: a macOS stand-in" {
		t.Errorf("java with no JDK: %+v", tool(out, "java"))
	}
	// A developer directory without git, as after an upgrade: still not run.
	if err := os.MkdirAll(filepath.Join(dev, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTool(t, bin, "xcode-select", `echo `+dev)
	if r := ran(); r != "" {
		t.Fatal(r)
	}
	if out = runScript(t, "/bin/sh", script); strings.Contains(ran(), "git") {
		t.Fatalf("git ran with no real git behind it:\n%s", out)
	}
	// With the real one there, and a JDK in the home, both run: the check isn't just off.
	fakeTool(t, filepath.Join(dev, "usr", "bin"), "git", `true`)
	jdk := filepath.Join(home, "Library", "Java", "JavaVirtualMachines", "x.jdk", "Contents", "Home", "bin")
	if err := os.MkdirAll(jdk, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTool(t, jdk, "java", `true`)
	out = runScript(t, "/bin/sh", script)
	if r := ran(); !strings.Contains(r, "git") || !strings.Contains(r, "java") {
		t.Fatalf("ran %q:\n%s", r, out)
	}
}
