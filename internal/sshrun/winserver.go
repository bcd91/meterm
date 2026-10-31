package sshrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"meterm/internal/config"
)

// Windows' OpenSSH server runs commands with cmd.exe (or PowerShell), so
// the POSIX install script can't work there: `sh` doesn't exist, and keys
// for an administrator go in a different file. copy-id checks what the
// server is first, from the version string it sends before anyone logs in.

var softwareRE = regexp.MustCompile(`remote software version (\S+)`)

// RemoteSoftware is the SSH server's software version ("OpenSSH_9.6p1",
// "OpenSSH_for_Windows_10.0"), read without logging in: BatchMode refuses
// password prompts and unknown host keys (nothing is added to known_hosts),
// and the version is exchanged before either. Through a jump host it's the
// last one ssh reports. "" when it can't be read.
func RemoteSoftware(r config.Resolved) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := append(commonArgs(r, "-p"), "-v", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"--", target(r), "exit")
	c := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	_ = c.Run() // it's expected to fail: no key yet
	m := softwareRE.FindAllStringSubmatch(stderr.String(), -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// IsWindowsServer reports whether software (from RemoteSoftware) is
// Windows' OpenSSH.
func IsWindowsServer(software string) bool {
	return strings.Contains(strings.ToLower(software), "windows")
}

func target(r config.Resolved) string {
	if r.User != "" {
		return r.User + "@" + r.Address
	}
	return r.Address
}

var pubKeyLineRE = regexp.MustCompile(`^(ssh-[a-z0-9-]+|ecdsa-sha2-nistp(256|384|521)|sk-[a-z0-9@.-]+) [A-Za-z0-9+/]+={0,3}( [^\x00-\x1f\x7f]*)?$`)

// windowsInstallScript is PowerShell. The key arrives base64-encoded inside
// the script, so nothing in it is ever parsed as code. It goes where sshd
// will look:
//   - an administrator, with sshd_config's stock "Match Group
//     administrators" block: %ProgramData%\ssh\administrators_authorized_keys,
//     which sshd ignores unless only Administrators and SYSTEM can touch it,
//     so its permissions are reset to exactly that;
//   - anyone else: %USERPROFILE%\.ssh\authorized_keys.
//
// It's idempotent, and writes UTF-8 without a BOM (sshd can't read a BOM).
const windowsInstallScript = `$ErrorActionPreference = 'Stop'
$k = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))
$admin = [bool]((whoami.exe /groups) -match 'S-1-5-32-544')
$cfg = Join-Path $env:ProgramData 'ssh\sshd_config'
$useAdmin = $false
if ($admin -and (Test-Path -LiteralPath $cfg)) {
  $useAdmin = [bool](Select-String -LiteralPath $cfg -Pattern '^\s*AuthorizedKeysFile\s+__PROGRAMDATA__/ssh/administrators_authorized_keys' -Quiet)
}
if ($useAdmin) {
  $f = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'
} else {
  $d = Join-Path $env:USERPROFILE '.ssh'
  if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Path $d | Out-Null }
  $f = Join-Path $d 'authorized_keys'
}
$raw = ''
if (Test-Path -LiteralPath $f) { $raw = [IO.File]::ReadAllText($f) }
$have = @($raw -split "\r?\n" | ForEach-Object { $_.Trim() })
if ($have -contains $k) {
  Write-Output "meterm: the key was already in $f"
} else {
  $text = $k + "` + "`r`n" + `"
  if ($raw.Length -gt 0 -and -not $raw.EndsWith("` + "`n" + `")) { $text = "` + "`r`n" + `" + $text }
  [IO.File]::AppendAllText($f, $text, (New-Object Text.UTF8Encoding $false))
  Write-Output "meterm: added the key to $f"
}
if ($useAdmin) {
  icacls.exe $f /inheritance:r /grant '*S-1-5-32-544:F' /grant '*S-1-5-18:F' | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "could not set the permissions sshd needs on $f" }
}
`

// CopyIDCommandWindows is CopyIDCommand for a Windows OpenSSH server.
func CopyIDCommandWindows(r config.Resolved, pubKey string) (*exec.Cmd, error) {
	key := strings.TrimSpace(pubKey)
	if strings.ContainsAny(key, "\r\n") || !pubKeyLineRE.MatchString(key) {
		return nil, fmt.Errorf("that doesn't look like a single public key line")
	}
	script := fmt.Sprintf(windowsInstallScript, base64.StdEncoding.EncodeToString([]byte(key)))
	args := append(commonArgs(r, "-p"), "--", target(r),
		"powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "+encodePS(script))
	cmd := guarded(r, "ssh", args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// encodePS is a script as -EncodedCommand wants it: base64 of UTF-16LE. It
// passes through cmd.exe or PowerShell, whichever the server runs, without
// any quoting to get wrong.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
