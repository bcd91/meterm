// Package sshrun turns a resolved host into an actual `ssh` invocation.
package sshrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"meterm/internal/config"
	"meterm/internal/sshkey"
)

// commonArgs builds the connection options shared by every ssh-family tool
// (ssh, scp, sftp; rsync borrows them via -e). portFlag differs per tool:
// ssh uses "-p", while scp and sftp use "-P".
func commonArgs(r config.Resolved, portFlag string) []string {
	var args []string
	if r.Port != 0 && r.Port != 22 {
		args = append(args, portFlag, fmt.Sprint(r.Port))
	}
	if r.IdentityFile != "" {
		args = append(args, "-i", sshkey.LocalPath(r.IdentityFile))
	}
	if spec := proxyJumpSpec(r); spec != "" {
		args = append(args, "-J", spec)
	}
	return args
}

// remoteSpec renders the user@host:path argument used by scp/rsync.
func remoteSpec(r config.Resolved, path string) string {
	host := bracketed(r.Address)
	if r.User != "" {
		host = r.User + "@" + host
	}
	return host + ":" + path
}

// bracketed puts an IPv6 address in brackets, as scp/sftp/rsync specs and
// -J hops with a port need: user@[2001:db8::1]:path.
func bracketed(addr string) string {
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		return "[" + addr + "]"
	}
	return addr
}

// guarded is exec.Command, except that it won't start for a host
// config.Resolve flagged as unsafe: Start returns r.Err instead.
func guarded(r config.Resolved, name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	if r.Err != nil {
		c.Err = r.Err
	}
	return c
}

// SSHContext is SSH bound to ctx.
func SSHContext(ctx context.Context, r config.Resolved, extra ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "ssh", BuildArgs(r, extra...)...)
	if r.Err != nil {
		c.Err = r.Err
	}
	return c
}

// SSH is the ssh command for r (BuildArgs), not wired to any stdio. It
// refuses to start for a host flagged unsafe.
func SSH(r config.Resolved, extra ...string) *exec.Cmd {
	return guarded(r, "ssh", BuildArgs(r, extra...)...)
}

// BuildArgs turns a resolved host into the argv for the system `ssh` binary
// (everything after "ssh"). extra is appended verbatim (e.g. a remote command).
func BuildArgs(r config.Resolved, extra ...string) []string {
	args := commonArgs(r, "-p")
	for _, f := range r.Forwards {
		if f.Name != "" {
			continue // a tunnel: only `meterm tunnel up` opens it
		}
		flag, spec := forwardFlag(f)
		if flag != "" {
			args = append(args, flag, spec)
		}
	}
	args = append(args, r.ExtraArgs...)

	target := r.Address
	if r.User != "" {
		target = r.User + "@" + r.Address
	}
	// No "--" before the destination: ssh reads options after the host too
	// (`meterm connect web1 -- -L 8080:localhost:80`), and a "--" here would
	// turn those into the remote command. A destination that could pass for
	// an option never gets this far: config.Resolve flags it and SSH() won't
	// start (see config.CheckAddress). This guard is for anything else.
	if strings.HasPrefix(target, "-") {
		args = append(args, "--")
	}
	args = append(args, target)
	args = append(args, extra...)
	return args
}

// proxyJumpSpec renders a -J value for a resolved jump chain, preferring
// hosts resolved from the inventory and falling back to raw literal specs.
func proxyJumpSpec(r config.Resolved) string {
	var hops []string
	for _, jh := range r.ProxyJump {
		hop := jh.Address
		if jh.Port != 0 && jh.Port != 22 {
			hop = fmt.Sprintf("%s:%d", bracketed(hop), jh.Port)
		}
		if jh.User != "" {
			hop = jh.User + "@" + hop
		}
		hops = append(hops, hop)
	}
	spec := strings.Join(hops, ",")
	if r.ProxyJumpRaw != "" {
		if spec != "" {
			spec += ","
		}
		spec += r.ProxyJumpRaw
	}
	return spec
}

// ForwardSpec renders f as ssh's flag and value, e.g. "-L", "5432:db:5432".
func ForwardSpec(f config.Forward) (flag, spec string) { return forwardFlag(f) }

func forwardFlag(f config.Forward) (flag, spec string) {
	switch strings.ToUpper(f.Type) {
	case "L":
		return "-L", fmt.Sprintf("%d:%s:%d", f.LocalPort, f.RemoteHost, f.RemotePort)
	case "R":
		return "-R", fmt.Sprintf("%d:%s:%d", f.LocalPort, f.RemoteHost, f.RemotePort)
	case "D":
		return "-D", fmt.Sprint(f.LocalPort)
	default:
		return "", ""
	}
}

// Command builds an *exec.Cmd for r, wired to the current process's stdio so
// an interactive session behaves exactly like running ssh by hand.
func Command(r config.Resolved, extra ...string) *exec.Cmd {
	c := inherited(SSH(r, extra...))
	c.Env = withRemoteTerm(os.Environ())
	return c
}

// CommandContext is Command bound to ctx.
func CommandContext(ctx context.Context, r config.Resolved, extra ...string) *exec.Cmd {
	c := inherited(SSHContext(ctx, r, extra...))
	c.Env = withRemoteTerm(os.Environ())
	return c
}
