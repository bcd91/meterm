//go:build !darwin

package status

func darwinProcStart(int) string { return "" }
