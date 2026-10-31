package sshrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestWinIsEventLog(t *testing.T) {
	for arg, want := range map[string]bool{
		"System": true, "Application": true, "Microsoft-Windows-PowerShell/Operational": true,
		"event:Setup": true, "Windows PowerShell": true,
		`C:\app\log.txt`: false, "D:/logs/app.log": false, "app.log": false, `\\srv\share\x`: false,
		`logs\*.log`: false, "C:": false,
	} {
		if got := WinIsEventLog(arg); got != want {
			t.Errorf("WinIsEventLog(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestPSLineCarriesArgsEncoded(t *testing.T) {
	evil := `C:\x'; Write-Output PWNED; '`
	line := psLine("Write-Output $A[0]", evil, "")
	enc := strings.TrimPrefix(line, "powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ")
	if enc == line || strings.ContainsAny(enc, " \"'&|<>^%") {
		t.Fatalf("remote line = %q", line)
	}
	script := decodePS(t, enc)
	if strings.Contains(script, "PWNED") {
		t.Error("an argument reached the script as text")
	}
}

func TestWinLogsCommandChecks(t *testing.T) {
	r := config.Resolved{Address: "h"}
	if _, err := WinLogsCommand(r, []string{"a.log", "b.log"}, 10, true); err == nil {
		t.Error("following two files accepted")
	}
	if _, err := WinLogsCommand(r, []string{"a.log", "System"}, 10, false); err == nil {
		t.Error("a file and an event log together accepted")
	}
	if _, err := WinLogsCommand(r, []string{"System"}, 10, true); err != nil {
		t.Error(err)
	}
}

// runPS runs a remote line here, on a Windows machine, the way the server
// would run it: the scripts themselves get exercised for real in CI.
func runPS(t *testing.T, line string) (string, error) {
	t.Helper()
	f := strings.Fields(line)
	out, err := exec.Command(f[0], f[1:]...).CombinedOutput()
	return string(out), err
}

func TestWindowsScriptsRunForReal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("needs Windows PowerShell")
	}
	if out, _ := runPS(t, psLine(winTailEventsScript, "1", "0", "System")); strings.Contains(out, "CLIXML") || strings.Contains(out, "<Objs") {
		t.Errorf("progress records leaked into the output:\n%s", out)
	}
	out, err := runPS(t, psLine(winStatsScript))
	if err != nil {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	for _, want := range []string{"== host ==", "== cpu ==", "cores:", "== memory ==", "GB", "== disk ==", "C:", "== network =="} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output lacks %q:\n%s", want, out)
		}
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "app.log")
	os.WriteFile(log, []byte("one\r\ntwo\r\nthree\r\nสี่\r\n"), 0o644)
	out, err = runPS(t, psLine(winTailFileScript, "2", "0", log))
	if err != nil || !strings.Contains(out, "three") || !strings.Contains(out, "สี่") || strings.Contains(out, "two") {
		t.Errorf("tail file: %v\n%q", err, out)
	}

	out, err = runPS(t, psLine(winTailEventsScript, "3", "0", "System"))
	if err != nil {
		t.Errorf("tail System: %v\n%s", err, out)
	} else if n := len(strings.Split(strings.TrimSpace(out), "\n")); n > 3 {
		t.Errorf("tail System printed %d lines:\n%s", n, out)
	}
	if out, err = runPS(t, psLine(winTailEventsScript, "3", "0", "No-Such-Log-meterm")); err == nil {
		t.Errorf("a missing event log didn't fail:\n%s", out)
	}

	out, err = runPS(t, psLine(winListEventLogsScript))
	if err != nil || !strings.Contains(out, "System") {
		t.Errorf("list event logs: %v\n%s", err, out)
	}
	out, err = runPS(t, psLine(winListDirScript, dir))
	if err != nil || !strings.Contains(out, "app.log") {
		t.Errorf("list dir: %v\n%s", err, out)
	}

	// A hostile path is a path, never code. (The marker is only spelled out
	// if the code runs; an error message quoting the path doesn't count.)
	out, err = runPS(t, psLine(winTailFileScript, "1", "0", `x'; Write-Output ('PW'+'NED'); '`))
	if strings.Contains(out, "PWNED") {
		t.Errorf("path ran as code:\n%s", out)
	}
	if err == nil || strings.Contains(out, "CLIXML") || !strings.Contains(out, "meterm: ") {
		t.Errorf("a missing file should fail with a plain error line: %v\n%s", err, out)
	}

	// The copy-id script, against a temporary profile.
	t.Setenv("USERPROFILE", dir)
	c, err := CopyIDCommandWindows(config.Resolved{Address: "h"}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample ci@test")
	if err != nil {
		t.Fatal(err)
	}
	remote := c.Args[len(c.Args)-1]
	for i := 0; i < 2; i++ { // twice: idempotent
		if out, err := runPS(t, remote); err != nil {
			t.Fatalf("copy-id script: %v\n%s", err, out)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".ssh", "authorized_keys"))
	if strings.Count(string(got), "ssh-ed25519") > 1 {
		t.Errorf("key installed twice:\n%s", got)
	}
}
