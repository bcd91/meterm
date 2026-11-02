// Package caps reads what a host can run: its OS and hardware, how busy it is, and which
// tools are on it. It's the far side's own word on itself, read by one fixed command, so
// that choosing a host for a job can rest on facts rather than on tags someone typed.
package caps

import (
	"bufio"
	"bytes"
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

// RemoteCommand is all `meterm caps` ever runs on a host. As with the other looks it's a
// constant, on one line inside one pair of single quotes, for whatever the login shell is:
// csh can't take a newline there, and fish reads a doubled backslash, so neither appears.
// It reads, and runs nothing but `--version` (or its like) of tools it finds, under a
// five-second timeout where the host has one. No sudo. Those run from / with what would
// let them fetch or set themselves up turned off: a go.mod or rust-toolchain file in the
// home could otherwise make go or rustup download a toolchain, and dotnet writes its
// first-run files. Each fact is a line of key=value,
// and `end=1` says it got to the end.
var RemoteCommand = `sh -c 'LC_ALL=C; export LC_ALL; PATH="$PATH:` + SearchPath + `"; export PATH; ` +
	`T=; command -v timeout >/dev/null 2>&1 && T="timeout 5"; ` +
	`echo caps=1; echo "user=$(id -un 2>/dev/null)"; ` +
	`echo "os=$(uname -s)"; echo "arch=$(uname -m)"; echo "kernel=$(uname -r)"; ` +
	`if [ -r /etc/os-release ]; then (. /etc/os-release; echo "distro=$ID"; echo "distro_version=$VERSION_ID"); fi; ` +
	`if command -v sw_vers >/dev/null 2>&1; then echo distro=macos; echo "distro_version=$(sw_vers -productVersion)"; fi; ` +
	`echo "cpus=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null)"; ` +
	`if [ -r /proc/cpuinfo ]; then echo "cpu_model=$(sed -n "s/^model name[[:space:]]*: //p" /proc/cpuinfo | head -n 1)"; ` +
	`else echo "cpu_model=$(sysctl -n machdep.cpu.brand_string 2>/dev/null)"; fi; ` +
	`if [ -r /proc/meminfo ]; then sed -n "s/^MemTotal: *\([0-9]*\) kB$/mem_total_kb=\1/p; s/^MemAvailable: *\([0-9]*\) kB$/mem_available_kb=\1/p" /proc/meminfo; ` +
	`else echo "mem_total_bytes=$(sysctl -n hw.memsize 2>/dev/null)"; fi; ` +
	`echo "home_df=$(df -Pk "$HOME" 2>/dev/null | tail -n 1)"; ` +
	`if [ -r /proc/loadavg ]; then echo "load=$(cat /proc/loadavg)"; else echo "load=$(sysctl -n vm.loadavg 2>/dev/null)"; fi; ` +
	`if command -v nvidia-smi >/dev/null 2>&1; then echo gpu_check=nvidia-smi; ` +
	`$T nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits 2>/dev/null | sed "s/^/gpu=/"; fi; ` +
	`for t in ` + strings.Join(Tools, " ") + `; do p=$(command -v $t 2>/dev/null) || { echo "tool=$t"; continue; }; ` +
	`case $t in go) a=version;; java) a=-version;; kubectl) a="version --client";; *) a=--version;; esac; ` +
	`v=$(cd / && GOTOOLCHAIN=local RUSTUP_AUTO_INSTALL=0 DOTNET_NOLOGO=1 DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 DOTNET_CLI_TELEMETRY_OPTOUT=1 $T "$p" $a </dev/null 2>&1); e=$?; printf "tool=%s\t%s\t%s\t%s\n" "$t" "$p" "$e" "$(printf "%s" "$v" | head -n 1)"; done; ` +
	`if command -v docker >/dev/null 2>&1; then if v=$($T docker info --format "{{.ServerVersion}}" </dev/null 2>/dev/null) && [ -n "$v" ]; ` +
	`then echo "docker_daemon=$v"; else echo docker_daemon=-; fi; fi; ` +
	`echo end=1'`

// ExitNoShell is what a login shell gives when it can't find sh.
const ExitNoShell = 127

// Limits on what's believed.
const (
	MaxOutput = 256 << 10
	maxText   = 256
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
	MemTotalMB    *int64 `json:"mem_total_mb"`
	// MemAvailableMB is Linux's estimate of what can be had without swapping. Null
	// elsewhere.
	MemAvailableMB *int64 `json:"mem_available_mb"`
	// Of the filesystem holding the account's home.
	HomeTotalMB *int64   `json:"home_total_mb"`
	HomeFreeMB  *int64   `json:"home_free_mb"`
	Load1       *float64 `json:"load1"`
	// GPUCheck names how GPUs were looked for, or is empty when they weren't, and then
	// GPUs is null: no nvidia-smi says nothing about other GPUs.
	GPUCheck string `json:"gpu_check"`
	GPUs     []GPU  `json:"gpus"`
	Tools    []Tool `json:"tools"`
	// DockerDaemon is empty when docker isn't found, "unreachable" when the client
	// couldn't reach a daemon (stopped, or no access), else the daemon's version.
	DockerDaemon string `json:"docker_daemon"`
	// Unparsed counts lines that weren't the known shape.
	Unparsed int `json:"unparsed"`
}

// GPU is one line of nvidia-smi.
type GPU struct {
	Name     string `json:"name"`
	MemoryMB *int64 `json:"memory_mb"`
}

// Tool is one program looked for. Found false means not on the PATH that was searched,
// which isn't proof it isn't installed. Runs says asking its version exited 0: macOS has
// stand-ins, such as /usr/bin/java with no Java behind it, that are found and don't run.
// It's null when it wasn't found or the exit status didn't come back. Version is the first
// line the program printed when asked, as it printed it, which on failure is the error.
type Tool struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Runs    *bool  `json:"runs"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

func clean(s string) string { return termsafe.Clip(termsafe.Text(strings.TrimSpace(s)), maxText) }

// Parse reads RemoteCommand's output.
func Parse(raw []byte) Caps {
	var c Caps
	known := map[string]bool{}
	for _, t := range Tools {
		known[t] = true
	}
	seenTool := map[string]bool{}
	started := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), MaxOutput)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			c.Unparsed++
			continue
		}
		if !started {
			// A shell's startup file can print before the script does.
			if key == "caps" && val == "1" {
				started = true
			} else {
				c.Unparsed++
			}
			continue
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
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				c.CPUs = &n
			}
		case "mem_total_kb":
			c.MemTotalMB = positive(val, 1024)
		case "mem_available_kb":
			c.MemAvailableMB = positive(val, 1024)
		case "mem_total_bytes":
			c.MemTotalMB = positive(val, 1024*1024)
		case "home_df":
			c.HomeTotalMB, c.HomeFreeMB = parseDF(val)
		case "load":
			c.Load1 = parseLoad(val)
		case "gpu_check":
			c.GPUCheck = clean(val)
			if c.GPUs == nil {
				c.GPUs = []GPU{}
			}
		case "gpu":
			name, mem, _ := strings.Cut(val, ",")
			g := GPU{Name: clean(name)}
			if n, err := strconv.ParseInt(strings.TrimSpace(mem), 10, 64); err == nil && n > 0 {
				g.MemoryMB = &n
			}
			if c.GPUs == nil {
				c.GPUs = []GPU{}
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
					if e, err := strconv.Atoi(f[2]); err == nil {
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
			if v := clean(val); v == "-" {
				c.DockerDaemon = "unreachable"
			} else if v != "" {
				c.DockerDaemon = v
			}
		case "end":
			c.Complete = val == "1"
		default:
			c.Unparsed++
		}
	}
	if c.Tools == nil {
		c.Tools = []Tool{}
	}
	return c
}

// positive reads a whole number, in units of div after dividing, or nil.
func positive(s string, div int64) *int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
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

// parseLoad reads the one-minute load from /proc/loadavg ("0.12 0.10 0.05 1/234 5678") or
// sysctl's vm.loadavg ("{ 1.23 1.10 1.00 }").
func parseLoad(s string) *float64 {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(s), "{"))
	if len(f) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}
