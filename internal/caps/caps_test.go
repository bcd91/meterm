package caps

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
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
	if strings.ContainsAny(c.CPUModel, "\x1b\x07\u202e") || len([]rune(c.CPUModel)) > maxText {
		t.Errorf("%q", c.CPUModel)
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
	for _, guard := range []string{"cd / &&", "GOTOOLCHAIN=local", "RUSTUP_AUTO_INSTALL=0", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1",
		"DOTNET_GENERATE_ASPNET_CERTIFICATE=false", "xcode-select -p", "JavaVirtualMachines", "tmux) a=-V", "not-local", "kill 9, -"} {
		if !strings.Contains(RemoteCommand, guard) {
			t.Errorf("RemoteCommand lost %q", guard)
		}
	}
}

// RemoteCommand run for real by each login shell this machine has, as ssh would run it.
// HOME is a temporary one, and the tools it finds are this machine's.
func TestRemoteCommandRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a POSIX sh is what this needs")
	}
	t.Setenv("HOME", t.TempDir())
	ran := 0
	for _, sh := range []string{"sh", "bash", "zsh", "dash", "ksh", "fish", "tcsh", "csh"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s isn't here, so it wasn't tried", sh)
			continue
		}
		ran++
		t.Run(sh, func(t *testing.T) {
			cmd := exec.Command(path, "-c", RemoteCommand)
			cmd.Dir = "/"
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v", err)
			}
			c := Parse(out)
			if !c.Answered || !c.Complete || c.Unparsed != 0 {
				t.Fatalf("complete %v, unparsed %d:\n%s", c.Complete, c.Unparsed, out)
			}
			uname, _ := exec.Command("uname", "-s").Output()
			if c.OS != strings.TrimSpace(string(uname)) || c.Arch == "" || c.CPUs == nil || c.MemTotalMB == nil || c.HomeFreeMB == nil || c.Load1 == nil {
				t.Errorf("%+v\n%s", c, out)
			}
			if len(c.Tools) != len(Tools) {
				t.Errorf("%d tools of %d:\n%s", len(c.Tools), len(Tools), out)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no shell found to run it with")
	}
}
