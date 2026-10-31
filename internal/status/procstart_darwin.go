package status

import "golang.org/x/sys/unix"

// darwinProcStart is when the process started, from the kernel's process table.
func darwinProcStart(pid int) string {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info.Proc.P_pid != int32(pid) {
		return ""
	}
	t := info.Proc.P_starttime
	return fmtStart(int64(t.Sec), int64(t.Usec))
}
