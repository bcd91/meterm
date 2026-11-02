package caps

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// From RemoteCommand run by zsh -c on macOS 26 on an M2, as ssh would, with the account renamed.
// go is installed in ~/sdk, which a non-interactive login doesn't have on its PATH.
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
home_df=/dev/disk3s1   239362496  95134624 115379720    46%    /System/Volumes/Data
load={ 2.11 2.64 3.74 }
tool=claude	/Users/alice/.local/bin/claude	0	2.1.316 (Claude Code)
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
tool=java	/usr/bin/java	1	The operation couldn’t be completed. Unable to locate a Java Runtime.
tool=dotnet
tool=psql
tool=mysql
tool=sqlcmd
tool=tmux
end=1
`

func TestParseMacShape(t *testing.T) {
	c := Parse([]byte(macShape))
	if !c.Complete || c.Unparsed != 0 {
		t.Fatalf("complete %v, unparsed %d", c.Complete, c.Unparsed)
	}
	if c.OS != "Darwin" || c.Arch != "arm64" || c.Distro != "macos" || c.DistroVersion != "26.6.2" || c.CPUModel != "Apple M2" || c.User != "alice" {
		t.Errorf("%+v", c)
	}
	if c.CPUs == nil || *c.CPUs != 8 || c.MemTotalMB == nil || *c.MemTotalMB != 8192 {
		t.Errorf("cpus %v mem %v", c.CPUs, c.MemTotalMB)
	}
	if c.MemAvailableMB != nil {
		t.Errorf("macOS gives no available memory, so it's unknown: %v", *c.MemAvailableMB)
	}
	if c.HomeTotalMB == nil || *c.HomeTotalMB != 233752 || c.HomeFreeMB == nil || *c.HomeFreeMB != 112675 {
		t.Errorf("home %v %v", c.HomeTotalMB, c.HomeFreeMB)
	}
	if c.Load1 == nil || *c.Load1 != 2.11 {
		t.Errorf("load %v", c.Load1)
	}
	if c.GPUs != nil || c.GPUCheck != "" {
		t.Errorf("no nvidia-smi says nothing of GPUs: %+v %q", c.GPUs, c.GPUCheck)
	}
	if c.DockerDaemon != "" {
		t.Errorf("docker %q", c.DockerDaemon)
	}
	if len(c.Tools) != len(Tools) {
		t.Fatalf("%d tools", len(c.Tools))
	}
	byName := map[string]Tool{}
	for _, tool := range c.Tools {
		byName[tool.Name] = tool
	}
	if cl := byName["claude"]; !cl.Found || cl.Runs == nil || !*cl.Runs || cl.Version != "2.1.316 (Claude Code)" {
		t.Errorf("%+v", cl)
	}
	if j := byName["java"]; !j.Found || j.Runs == nil || *j.Runs {
		t.Errorf("a stand-in java is found and doesn't run: %+v", j)
	}
	if d := byName["docker"]; d.Found || d.Runs != nil || d.Path != "" {
		t.Errorf("%+v", d)
	}
}

// Written by hand from the formats of /proc and util-linux, not copied from a host: replace
// it with real output from a Linux host with a GPU and docker when there is one.
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
home_df=/dev/mapper/vg-root 960000000 400000000 512000000      44% /
load=0.52 0.40 0.33 2/812 99812
gpu_check=nvidia-smi
gpu=NVIDIA GeForce RTX 4090, 24564
tool=claude
tool=git	/usr/bin/git	0	git version 2.47.3
tool=docker	/usr/bin/docker	0	Docker version 28.5.1, build e180ab8
tool=python3	/usr/bin/python3	124
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
		if tool.Name == "python3" && (tool.Runs == nil || *tool.Runs) {
			t.Errorf("timed out isn't running: %+v", tool)
		}
	}
}

func TestParseUnknownsAndLimits(t *testing.T) {
	// Cut short, with every number unreadable: nothing turns into 0, and nothing passes for
	// complete.
	c := Parse([]byte("caps=1\ncpus=\nmem_total_kb=abc\nhome_df=df: /home/x: No such file\nload=\ngpu_check=nvidia-smi\ndocker_daemon=-\n"))
	if c.Complete || c.CPUs != nil || c.MemTotalMB != nil || c.HomeFreeMB != nil || c.HomeTotalMB != nil || c.Load1 != nil {
		t.Errorf("%+v", c)
	}
	if c.GPUs == nil || len(c.GPUs) != 0 {
		t.Errorf("nvidia-smi ran and found none: %+v", c.GPUs)
	}
	if c.DockerDaemon != "unreachable" || c.Tools == nil {
		t.Errorf("%+v", c)
	}

	// Nothing counts before caps=1, so a banner can't plant facts, and neither can a tool
	// line for a name that wasn't asked about, or a second line for one that was.
	c = Parse([]byte("cpus=999\nend=1\ncaps=1\ntool=evil\t/x\t0\tv\ntool=git\t/usr/bin/git\t0\tgit 2\ntool=git\t/tmp/git\t0\tfake\n"))
	if c.CPUs != nil || c.Complete || len(c.Tools) != 1 || c.Tools[0].Path != "/usr/bin/git" || c.Unparsed != 4 {
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
}

// RemoteCommand run for real by each login shell this machine has, as ssh would run it.
// HOME is a temporary one, and the tools it finds are this machine's.
func TestRemoteCommandRunsHere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a POSIX sh is what this needs")
	}
	t.Setenv("HOME", t.TempDir())
	ran := 0
	for _, sh := range []string{"sh", "bash", "zsh", "dash", "ksh", "fish", "tcsh"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		ran++
		t.Run(sh, func(t *testing.T) {
			out, err := exec.Command(path, "-c", RemoteCommand).Output()
			if err != nil {
				t.Fatalf("%v", err)
			}
			c := Parse(out)
			if !c.Complete || c.Unparsed != 0 {
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
	// Asking a version mustn't fetch or set anything up because of what's in the home.
	for _, guard := range []string{"cd / &&", "GOTOOLCHAIN=local", "RUSTUP_AUTO_INSTALL=0", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1"} {
		if !strings.Contains(RemoteCommand, guard) {
			t.Errorf("RemoteCommand lost %q", guard)
		}
	}
	if ran == 0 {
		t.Fatal("no shell found to run it with")
	}
}
