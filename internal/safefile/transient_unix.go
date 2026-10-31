//go:build !windows

package safefile

func transient(error) bool { return false }
