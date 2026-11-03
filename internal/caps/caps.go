// Package caps reads what a host can run: its OS and hardware, how busy it is, and which
// tools are on it. It's the far side's own word on itself, read by one fixed command, so
// that choosing a host for a job can rest on facts rather than on tags someone typed.
package caps

import (
	"bufio"
	"bytes"
	"math"
	"strconv"
	"strings"

	"meterm/internal/termsafe"
)

// Schema is the version of the --format json report. It goes up when a field changes
// meaning; new fields don't need it to.
const Schema = 1

// Tools are the programs looked for, in the order they're reported. A name here must be a
// plain word: it goes into RemoteCommand as it is.
var Tools = []string{
	"claude", "codex", "git", "go", "node", "python3", "uv", "docker", "podman", "kubectl",
	"make", "gcc", "rustc", "cargo", "java", "dotnet", "psql", "mysql", "sqlcmd", "tmux",
}

// SearchPath is added to the PATH a non-interactive login gets, which often leaves out
// where people install things. A tool that isn't found was looked for here and on that
// PATH, and nowhere else.
const SearchPath = "$HOME/.local/bin:$HOME/bin:$HOME/.claude/local:$HOME/.npm-global/bin:$HOME/go/bin:/usr/local/go/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin"

// RemoteCommand is all `meterm caps` ever runs on a host. As with the other looks it's
// fixed, made of constants and nothing from a host or a flag, on one line inside one pair of single quotes, for whatever the login shell is:
// csh can't take a newline there, and fish reads a doubled backslash, so neither appears.
//
// What it runs, from /: uname, id, sysctl, df, sed, cat, tr and xcode-select, with no limit
// of their own; /etc/os-release, which is shell code, in a subshell; `nvidia-smi --query-gpu`;
// `docker context inspect`, and `docker info` only when that names a local socket; and each
// tool it finds with `--version` or its like, with toolchain downloads, first-run setup and
// the JVM's perf file turned off. No sudo. nvidia-smi, docker and the tools are each under a
// five-second limit, with what they started in their process group, by perl or else
// `timeout` where the host has either. On a
// Mac without the developer tools, or without a JDK, /usr/bin's stand-ins aren't run, as
// they'd ask whoever is at its screen to install something.
//
// Every value goes through kv, which strips newlines, so nothing a command prints can
// start a line of its own. It starts with a newline, as a shell's startup file can print
// without one, such as a title for the terminal. A line is key=value, and `end=1` says it
// got to the end.
var RemoteCommand = `sh -c 'LC_ALL=C; export LC_ALL; PATH="$PATH:` + SearchPath + `"; export PATH; cd / || exit 1; echo; ` +
	`kv() { printf "%s=%s\n" "$1" "$(printf "%s" "$2" | tr -d "\r\n")"; }; ` +
	`TO=; PL=; if command -v perl >/dev/null 2>&1; then PL=1; elif timeout 5 true >/dev/null 2>&1; then TO=1; fi; ` +
	`t5() { if [ -n "$PL" ]; then perl -e "` + perlLimit + `" 5 "$@"; elif [ -n "$TO" ]; then timeout 5 "$@"; else "$@"; fi; }; ` +
	`D=$(uname -s); echo caps=1; kv user "$(id -un 2>/dev/null)"; kv os "$D"; kv arch "$(uname -m)"; kv kernel "$(uname -r)"; ` +
	`if [ -r /etc/os-release ]; then kv distro "$(. /etc/os-release; echo "$ID")"; kv distro_version "$(. /etc/os-release; echo "$VERSION_ID")"; fi; ` +
	`if command -v sw_vers >/dev/null 2>&1; then echo distro=macos; kv distro_version "$(sw_vers -productVersion)"; fi; ` +
	`kv cpus "$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null)"; ` +
	`if [ -r /proc/cpuinfo ]; then kv cpu_model "$(sed -n "s/^model name[[:space:]]*: //p" /proc/cpuinfo | head -n 1)"; ` +
	`else kv cpu_model "$(sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model 2>/dev/null)"; fi; ` +
	`if [ -r /proc/meminfo ]; then sed -n "s/^MemTotal: *\([0-9]*\) kB$/mem_total_kb=\1/p; s/^MemAvailable: *\([0-9]*\) kB$/mem_available_kb=\1/p" /proc/meminfo; ` +
	`else kv mem_total_bytes "$(sysctl -n hw.memsize 2>/dev/null || sysctl -n hw.physmem 2>/dev/null)"; fi; ` +
	`if [ -r /sys/fs/cgroup/memory.max ]; then kv cgroup_memory_max "$(cat /sys/fs/cgroup/memory.max)"; fi; ` +
	`if [ -r /sys/fs/cgroup/cpu.max ]; then kv cgroup_cpu_max "$(cat /sys/fs/cgroup/cpu.max)"; fi; ` +
	`if [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then kv cgroup1_memory_limit "$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)"; fi; ` +
	`kv home_df "$(df -Pk "$HOME" 2>/dev/null | tail -n 1)"; ` +
	`if [ -r /proc/loadavg ]; then kv load "$(cat /proc/loadavg)"; else kv load "$(sysctl -n vm.loadavg 2>/dev/null)"; fi; ` +
	`if command -v nvidia-smi >/dev/null 2>&1; then if g=$(t5 nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits </dev/null 2>/dev/null); ` +
	`then echo gpu_check=nvidia-smi; printf "%s\n" "$g" | sed -n "s/^./gpu=&/p"; else echo gpu_check=failed; fi; fi; ` +
	`NOCLT=; NOJDK=; if [ "$D" = Darwin ]; then [ -d "$(xcode-select -p 2>/dev/null)" ] || NOCLT=1; NOJDK=1; ` +
	`for j in /Library/Java/JavaVirtualMachines/*/Contents/Home/bin/java "$HOME"/Library/Java/JavaVirtualMachines/*/Contents/Home/bin/java; do [ -x "$j" ] && NOJDK=; done; fi; ` +
	`for t in ` + strings.Join(Tools, " ") + `; do p=$(command -v $t 2>/dev/null) || { echo "tool=$t"; continue; }; ` +
	`s=; if [ "$D" = Darwin ]; then case $p in /usr/bin/java) s=$NOJDK;; /usr/bin/*) s=$NOCLT;; esac; fi; ` +
	`if [ -n "$s" ]; then printf "tool=%s\t%s\t-\t\n" "$t" "$p"; continue; fi; ` +
	`case $t in go) a=version;; java) a="-XX:-UsePerfData -version";; kubectl) a="version --client";; tmux) a=-V;; *) a=--version;; esac; ` +
	`v=$(export GOTOOLCHAIN=local RUSTUP_AUTO_INSTALL=0 DOTNET_NOLOGO=1 DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 DOTNET_CLI_TELEMETRY_OPTOUT=1 ` +
	`DOTNET_GENERATE_ASPNET_CERTIFICATE=false DOTNET_ADD_GLOBAL_TOOLS_TO_PATH=false; t5 "$p" $a </dev/null 2>&1); e=$?; ` +
	`printf "tool=%s\t%s\t%s\t%s\n" "$t" "$p" "$e" "$(printf "%s\n" "$v" | sed -n "/[0-9]\.[0-9]/p" | head -n 1 | tr -d "\r")"; done; ` +
	`if command -v docker >/dev/null 2>&1; then h=$DOCKER_HOST; [ -n "$h" ] || h=$(t5 docker context inspect --format "{{.Endpoints.docker.Host}}" </dev/null 2>/dev/null); ` +
	`case $h in unix://*|npipe://*) if v=$(t5 docker info --format "{{.ServerVersion}}" </dev/null 2>/dev/null) && [ -n "$v" ]; ` +
	`then kv docker_daemon "$v"; else echo docker_daemon=-; fi;; "") echo docker_daemon=not-checked;; *) echo docker_daemon=not-local;; esac; fi; ` +
	`echo end=1'`

// perlLimit runs its arguments in a process group of their own and kills the whole group
// after the given seconds, exiting 142, and again when the tool ends, for what it left
// running. Killing only the one process isn't enough: a child it started keeps the output
// open, and the shell waits for that. A tool killed by a signal exits 128 and the signal, as
// a shell would say, never 0. It goes inside double quotes in sh, so each $ is escaped, and
// it has no ! for csh. A child that leaves the group, with setsid, isn't reached.
const perlLimit = `\$t=shift; \$p=fork; unless (\$p) { setpgrp(0,0); exec @ARGV; exit 127 } ` +
	`\$SIG{ALRM}=sub { kill 9, -\$p; exit 142 }; alarm \$t; waitpid(\$p,0); \$s=\$?; kill 9, -\$p; ` +
	`exit(\$s & 127 ? 128+(\$s & 127) : \$s>>8)`

// ExitNoShell is what a login shell gives when it can't find sh.
const ExitNoShell = 127

// timedOut are the statuses of a version check that was stopped: GNU timeout's, a kill,
// perl's alarm, and busybox timeout's TERM, as a shell reports them.
var timedOut = map[int]bool{124: true, 137: true, 142: true, 143: true}

// Limits on what's believed.
const (
	MaxOutput = 256 << 10
	maxText   = 256
	maxCPUs   = 1 << 16
	maxLoad   = 1e5
)

// Report is what `meterm caps --format json` prints.
type Report struct {
	Schema int    `json:"schema"`
	Hosts  []Host `json:"hosts"`
}

// Host is one host's line of the report. Caps is null when Error is set: a host that
// couldn't be asked isn't one with nothing on it.
type Host struct {
	Host      string `json:"host"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error"`
	Caps      *Caps  `json:"caps"`
}

// Caps is what a host said of itself. Every number is null when it couldn't be read, which
// isn't zero, and an empty string says nothing either.
type Caps struct {
	// Complete says the output got to its end. Without it, what's missing is unknown.
	Complete      bool   `json:"complete"`
	User          string `json:"user"`
	OS            string `json:"os"` // uname -s: Linux, Darwin, FreeBSD ...
	Arch          string `json:"arch"`
	Kernel        string `json:"kernel"`
	Distro        string `json:"distro"` // os-release's ID, or macos
	DistroVersion string `json:"distro_version"`
	CPUModel      string `json:"cpu_model"`
	CPUs          *int   `json:"cpus"`
	// CPULimit is a cgroup's CPU quota in CPUs, as inside a container, when there is one.
	CPULimit   *float64 `json:"cpu_limit"`
	MemTotalMB *int64   `json:"mem_total_mb"`
	// MemAvailableMB is Linux's estimate of what can be had without swapping. Null
	// elsewhere.
	MemAvailableMB *int64 `json:"mem_available_mb"`
	// MemLimitMB is a cgroup's memory limit, as inside a container, when there is one.
	// Inside one, the totals above are usually the machine's, not what's allowed.
	MemLimitMB *int64 `json:"mem_limit_mb"`
	// CgroupRead says a cgroup's memory limit file was read, so a null limit means none.
	// False says nothing either way: no such file, or one this doesn't read.
	CgroupRead bool `json:"cgroup_read"`
	// Of the filesystem holding the account's home.
	HomeTotalMB *int64   `json:"home_total_mb"`
	HomeFreeMB  *int64   `json:"home_free_mb"`
	Load1       *float64 `json:"load1"`
	// GPUCheck is "nvidia-smi" when it answered, and then GPUs is a list, maybe empty;
	// "failed" when it was there and didn't answer; or empty when it wasn't there. GPUs is
	// null in both of the last two: they say nothing about what GPUs there are.
	GPUCheck string `json:"gpu_check"`
	GPUs     []GPU  `json:"gpus"`
	Tools    []Tool `json:"tools"`
	// DockerDaemon is empty when docker isn't found, "unreachable" when the client couldn't
	// reach a local daemon (stopped, or no access), "not-local" when its context or
	// DOCKER_HOST isn't a local socket, "not-checked" when where it points couldn't be read,
	// neither of which was asked, else the daemon's version.
	DockerDaemon string `json:"docker_daemon"`
	// Unparsed counts lines that weren't the known shape, or said a second time.
	Unparsed int `json:"unparsed"`

	// Answered says the script's output was seen at all, which a forced command or a
	// shell that exits early wouldn't give.
	Answered bool `json:"-"`
}

// GPU is one line of nvidia-smi.
type GPU struct {
	Name     string `json:"name"`
	MemoryMB *int64 `json:"memory_mb"`
}

// Tool is one program looked for. Found false means not on the PATH that was searched,
// which isn't proof it isn't installed. Runs says asking its version exited 0. False means
// that check failed: it crashed, it's a stand-in with nothing behind it, or it doesn't take
// the flag. Null means not found, or not known, and Note says why. Version is the first line
// it printed with a dotted number in it.
type Tool struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Runs    *bool  `json:"runs"`
	Note    string `json:"note"` // "timed out", or "not run: a macOS stand-in"
	Path    string `json:"path"`
	Version string `json:"version"`
}

func clean(s string) string { return termsafe.Clip(termsafe.Text(strings.TrimSpace(s)), maxText) }

// scalars are the keys said once. A second one, from anything that got a line in, makes
// the value unknown rather than either.
var scalars = map[string]bool{
	"user": true, "os": true, "arch": true, "kernel": true, "distro": true, "distro_version": true, "cpu_model": true,
	"cpus": true, "mem_total_kb": true, "mem_available_kb": true, "mem_total_bytes": true, "cgroup_memory_max": true,
	"cgroup_cpu_max": true, "cgroup1_memory_limit": true, "home_df": true, "load": true, "gpu_check": true, "docker_daemon": true, "end": true,
}

// Parse reads RemoteCommand's output.
func Parse(raw []byte) Caps {
	var c Caps
	known := map[string]bool{}
	for _, t := range Tools {
		known[t] = true
	}
	var seenTool, seen, twice map[string]bool
	before := 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), MaxOutput)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if key == "caps" && val == "1" {
			// The script's start. Anything before it, a shell's startup file included, is
			// thrown away, and so is everything if it comes again.
			if c.Answered {
				before = c.Unparsed // which already holds what came before
			}
			c = Caps{Answered: true, Unparsed: before}
			seenTool, seen, twice = map[string]bool{}, map[string]bool{}, map[string]bool{}
			continue
		}
		if !c.Answered {
			before++
			continue
		}
		if !ok {
			c.Unparsed++
			continue
		}
		if scalars[key] {
			if seen[key] {
				twice[key] = true
				c.Unparsed++
				continue
			}
			seen[key] = true
		}
		switch key {
		case "user":
			c.User = clean(val)
		case "os":
			c.OS = clean(val)
		case "arch":
			c.Arch = clean(val)
		case "kernel":
			c.Kernel = clean(val)
		case "distro":
			c.Distro = clean(val)
		case "distro_version":
			c.DistroVersion = clean(val)
		case "cpu_model":
			c.CPUModel = clean(val)
		case "cpus":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 && n <= maxCPUs {
				c.CPUs = &n
			}
		case "mem_total_kb":
			c.MemTotalMB = positive(val, 1024)
		case "mem_available_kb":
			c.MemAvailableMB = positive(val, 1024)
		case "mem_total_bytes":
			c.MemTotalMB = positive(val, 1024*1024)
		case "cgroup_memory_max":
			c.MemLimitMB = positive(val, 1024*1024) // "max" is no limit, and stays null
			c.CgroupRead = c.MemLimitMB != nil || strings.TrimSpace(val) == "max"
		case "cgroup1_memory_limit":
			// cgroup v1 says no limit with a number near 2^63.
			if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && n > 0 {
				c.CgroupRead = true
				if n < 1<<60 {
					c.MemLimitMB = positive(val, 1024*1024)
				}
			}
		case "cgroup_cpu_max":
			c.CPULimit = parseCPUMax(val)
		case "home_df":
			c.HomeTotalMB, c.HomeFreeMB = parseDF(val)
		case "load":
			c.Load1 = parseLoad(val)
		case "gpu_check":
			switch v := clean(val); v {
			case "nvidia-smi":
				c.GPUCheck, c.GPUs = v, []GPU{}
			case "failed":
				c.GPUCheck = v
			default:
				c.Unparsed++
			}
		case "gpu":
			name, mem, _ := strings.Cut(val, ",")
			if c.GPUCheck != "nvidia-smi" || clean(name) == "" {
				c.Unparsed++
				continue
			}
			g := GPU{Name: clean(name)}
			if n, err := strconv.ParseInt(strings.TrimSpace(mem), 10, 64); err == nil && n > 0 {
				g.MemoryMB = &n
			}
			c.GPUs = append(c.GPUs, g)
		case "tool":
			f := strings.SplitN(val, "\t", 4)
			if !known[f[0]] || seenTool[f[0]] {
				c.Unparsed++
				continue
			}
			seenTool[f[0]] = true
			t := Tool{Name: f[0]}
			if len(f) >= 2 && strings.TrimSpace(f[1]) != "" {
				t.Found, t.Path = true, clean(f[1])
				if len(f) >= 3 {
					e, err := strconv.Atoi(f[2])
					switch {
					case f[2] == "-":
						t.Note = "not run: a macOS stand-in"
					case err != nil:
					case timedOut[e]:
						t.Note = "timed out"
					default:
						runs := e == 0
						t.Runs = &runs
					}
				}
				if len(f) == 4 {
					t.Version = clean(f[3])
				}
			}
			c.Tools = append(c.Tools, t)
		case "docker_daemon":
			switch v := clean(val); v {
			case "-":
				c.DockerDaemon = "unreachable"
			case "":
			default:
				c.DockerDaemon = v
			}
		case "end":
			c.Complete = val == "1"
		default:
			c.Unparsed++
		}
	}
	// Said twice is unknown: which the host meant can't be told.
	for key := range twice {
		switch key {
		case "user":
			c.User = ""
		case "os":
			c.OS = ""
		case "arch":
			c.Arch = ""
		case "kernel":
			c.Kernel = ""
		case "distro":
			c.Distro = ""
		case "distro_version":
			c.DistroVersion = ""
		case "cpu_model":
			c.CPUModel = ""
		case "cpus":
			c.CPUs = nil
		case "mem_total_kb", "mem_total_bytes":
			c.MemTotalMB = nil
		case "mem_available_kb":
			c.MemAvailableMB = nil
		case "cgroup_memory_max", "cgroup1_memory_limit":
			c.MemLimitMB, c.CgroupRead = nil, false
		case "cgroup_cpu_max":
			c.CPULimit = nil
		case "home_df":
			c.HomeTotalMB, c.HomeFreeMB = nil, nil
		case "load":
			c.Load1 = nil
		case "gpu_check":
			c.GPUCheck, c.GPUs = "", nil
		case "docker_daemon":
			c.DockerDaemon = ""
		case "end":
			c.Complete = false
		}
	}
	if !c.Answered {
		c.Unparsed = before
	}
	if c.Tools == nil {
		c.Tools = []Tool{}
	}
	return c
}

// positive reads a whole number, in units of div after dividing, or nil.
func positive(s string, div int64) *int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 || n/div == 0 {
		return nil
	}
	n /= div
	return &n
}

// parseDF reads `df -Pk`'s line: filesystem, 1024-blocks, used, available, capacity,
// mount. A filesystem name can have spaces, so it looks for the numbers.
func parseDF(s string) (total, free *int64) {
	f := strings.Fields(s)
	for i := 0; i+3 < len(f); i++ {
		if !strings.HasSuffix(f[i+3], "%") {
			continue
		}
		t, err1 := strconv.ParseInt(f[i], 10, 64)
		_, err2 := strconv.ParseInt(f[i+1], 10, 64)
		a, err3 := strconv.ParseInt(f[i+2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || t <= 0 || a < 0 {
			continue
		}
		t, a = t/1024, a/1024
		return &t, &a
	}
	return nil, nil
}

// finite is a float JSON can carry, in range.
func finite(v, hi float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= hi }

// parseLoad reads the one-minute load from /proc/loadavg ("0.12 0.10 0.05 1/234 5678") or
// sysctl's vm.loadavg ("{ 1.23 1.10 1.00 }"). NaN or Inf would make the whole report fail
// to encode, for every host, so they're unknown.
func parseLoad(s string) *float64 {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(s), "{"))
	if len(f) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || !finite(v, maxLoad) {
		return nil
	}
	return &v
}

// parseCPUMax reads cgroup v2's cpu.max, "quota period" or "max period", as CPUs.
func parseCPUMax(s string) *float64 {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "max" {
		return nil
	}
	q, err1 := strconv.ParseFloat(f[0], 64)
	p, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || p <= 0 || q <= 0 {
		return nil
	}
	v := math.Round(q/p*100) / 100
	if !finite(v, maxCPUs) || v == 0 {
		return nil
	}
	return &v
}
