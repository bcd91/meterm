package sshrun

import (
	"os"
	"os/exec"

	"meterm/internal/config"
)

// statsScript is a one-shot diagnostic dump: uptime/load, CPU core count,
// memory, disk usage, and network interfaces. It's written to work on both
// Linux and macOS/BSD remotes (where free/nproc/ip don't exist) by
// preferring the Linux tool and falling back to its BSD/macOS equivalent.
const statsScript = `sh -c '
echo "== uptime / load =="
uptime
echo
echo "== cpu =="
if command -v nproc >/dev/null 2>&1; then
  echo "cores: $(nproc)"
else
  echo "cores: $(sysctl -n hw.ncpu 2>/dev/null || echo unknown)"
fi
echo
echo "== memory =="
if command -v free >/dev/null 2>&1; then
  free -h
else
  vm_stat 2>/dev/null || echo "memory info unavailable"
fi
echo
echo "== disk =="
df -h 2>/dev/null | grep -Ev "^(tmpfs|devtmpfs|udev|overlay)"
echo
echo "== network =="
if command -v ip >/dev/null 2>&1; then
  ip -brief addr 2>/dev/null || ip addr
else
  ifconfig
fi
'`

// StatsCommand runs statsScript on r. Like exec, it is deliberately
// non-interactive (no stdin) so it fails fast instead of hanging on a
// password prompt — it's for hosts key-based auth already works on.
func StatsCommand(r config.Resolved) *exec.Cmd {
	cmd := SSH(r, statsScript)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
