//go:build windows

package status

import (
	"io/fs"
	"syscall"
)

// stillActive is what GetExitCodeProcess says of a process that's running.
const stillActive = 259

func processExists(pid int) bool {
	const queryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(queryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	return syscall.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

// ownedByMe is always true: the temp directory is per-user on Windows.
func ownedByMe(fs.FileInfo) bool { return true }
