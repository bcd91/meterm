package sshrun

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"meterm/internal/config"
)

// Scripts for Windows OpenSSH servers. Each runs as
// `powershell -EncodedCommand`, and its arguments travel base64-encoded
// inside it as $A, so no path or name a user types is ever parsed as
// PowerShell or cmd.exe syntax.

// psLine is the remote command line that runs script with args.
func psLine(script string, args ...string) string {
	enc := make([]string, len(args))
	for i, a := range args {
		enc[i] = base64.StdEncoding.EncodeToString([]byte(a))
	}
	// Progress bars would otherwise reach a redirected stdout as CLIXML
	// noise ("#< CLIXML <Objs ...").
	prelude := "$ErrorActionPreference = 'Stop'\n" +
		"$ProgressPreference = 'SilentlyContinue'\n" +
		"[Console]::OutputEncoding = [Text.Encoding]::UTF8\n" +
		"$A = @(if ('" + strings.Join(enc, ",") + "' -ne '') { '" + strings.Join(enc, ",") + "'.Split(',') | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_)) } })\n"
	// Errors as one plain line on stderr: left alone, PowerShell serialises
	// them as CLIXML when stderr isn't a console.
	body := prelude + "try {\n" + script + "\n} catch {\n  [Console]::Error.WriteLine('meterm: ' + $_.Exception.Message)\n  exit 1\n}\n"
	return "powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + encodePS(body)
}

func psCommand(r config.Resolved, script string, args ...string) *exec.Cmd {
	c := SSH(r, psLine(script, args...))
	c.Stdin = nil
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c
}

const winStatsScript = `$os = Get-CimInstance Win32_OperatingSystem
$cpu = @(Get-CimInstance Win32_Processor)
"== host =="
'{0}  {1} (build {2})' -f $env:COMPUTERNAME, $os.Caption, $os.BuildNumber
$up = (Get-Date) - $os.LastBootUpTime
'up {0}d {1}h {2}m' -f $up.Days, $up.Hours, $up.Minutes
""
"== cpu =="
'{0}' -f $cpu[0].Name.Trim()
'cores: {0}  threads: {1}  load: {2}%' -f ($cpu | Measure-Object NumberOfCores -Sum).Sum, ($cpu | Measure-Object NumberOfLogicalProcessors -Sum).Sum, [math]::Round(($cpu | Measure-Object LoadPercentage -Average).Average)
""
"== memory =="
$t = [double]$os.TotalVisibleMemorySize * 1KB
$f = [double]$os.FreePhysicalMemory * 1KB
'total {0:N1} GB  used {1:N1} GB  free {2:N1} GB  ({3:N0}% used)' -f ($t / 1GB), (($t - $f) / 1GB), ($f / 1GB), (100 * ($t - $f) / $t)
""
"== disk =="
Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | ForEach-Object {
  $s = [double]$_.Size; $fr = [double]$_.FreeSpace
  '{0}  size {1,8:N1} GB  used {2,8:N1} GB  free {3,8:N1} GB  ({4:N0}% used)' -f $_.DeviceID, ($s / 1GB), (($s - $fr) / 1GB), ($fr / 1GB), (100 * ($s - $fr) / [math]::Max($s, 1))
}
""
"== network =="
Get-CimInstance Win32_NetworkAdapterConfiguration -Filter 'IPEnabled=True' | ForEach-Object {
  '{0}  {1}' -f $_.Description, (@($_.IPAddress) -join ', ')
}
`

// WinStatsCommand is StatsCommand for a Windows server.
func WinStatsCommand(r config.Resolved) *exec.Cmd { return psCommand(r, winStatsScript) }

const winTailFileScript = `$n = [int]$A[0]
$follow = $A[1] -eq '1'
$paths = @($A | Select-Object -Skip 2)
# UTF8: Windows PowerShell would read a BOM-less UTF-8 log (the common
# kind) as the ANSI code page; a file with a BOM is still read by its BOM.
if ($follow) { Get-Content -Path $paths[0] -Tail $n -Wait -Encoding UTF8 } else { Get-Content -Path $paths -Tail $n -Encoding UTF8 }
`

const winTailEventsScript = `$n = [int]$A[0]
$follow = $A[1] -eq '1'
$log = $A[2]
Get-WinEvent -ListLog $log | Out-Null
function Show($e) {
  $msg = ''
  if ($e.Message) { $msg = ($e.Message -replace '\s+', ' ').Trim() }
  '{0:yyyy-MM-dd HH:mm:ss}  {1,-11} {2} [{3}] {4}' -f $e.TimeCreated, $e.LevelDisplayName, $e.ProviderName, $e.Id, $msg
}
function ReadEvents($max) {
  try { @(Get-WinEvent -LogName $log -MaxEvents $max) }
  catch { if ($_.FullyQualifiedErrorId -notlike 'NoMatchingEventsFound*') { throw } ; @() }
}
$ev = ReadEvents $n
[array]::Reverse($ev)
$last = [long]0
foreach ($e in $ev) { Show $e; if ($e.RecordId -gt $last) { $last = $e.RecordId } }
while ($follow) {
  Start-Sleep -Seconds 2
  $new = @(ReadEvents 500 | Where-Object { $_.RecordId -gt $last })
  [array]::Reverse($new)
  foreach ($e in $new) { Show $e; if ($e.RecordId -gt $last) { $last = $e.RecordId } }
}
`

var winFileRE = regexp.MustCompile(`^[A-Za-z]:|^\\\\|\\|\.[A-Za-z0-9]{1,6}$|[*?]`)

// WinIsEventLog reports whether a `logs` argument names an event log
// ("System", "Microsoft-Windows-PowerShell/Operational", "event:Setup")
// rather than a file ("C:\app\log.txt", "D:/logs/*.log", "app.log").
func WinIsEventLog(arg string) bool {
	if strings.HasPrefix(arg, "event:") {
		return true
	}
	return !winFileRE.MatchString(arg)
}

// WinLogsCommand is the `logs` tail for a Windows server: files with
// Get-Content -Tail, an event log as one line per event. Following (-f)
// takes a single file or log.
func WinLogsCommand(r config.Resolved, paths []string, lines int, follow bool) (*exec.Cmd, error) {
	f := "0"
	if follow {
		f = "1"
		if len(paths) != 1 {
			return nil, fmt.Errorf("on Windows, -f follows one file or event log at a time")
		}
	}
	if WinIsEventLog(paths[0]) {
		if len(paths) != 1 {
			return nil, fmt.Errorf("one event log at a time")
		}
		return psCommand(r, winTailEventsScript, fmt.Sprint(lines), f, strings.TrimPrefix(paths[0], "event:")), nil
	}
	for _, p := range paths {
		if WinIsEventLog(p) {
			return nil, fmt.Errorf("%q looks like an event log: show it on its own", p)
		}
	}
	return psCommand(r, winTailFileScript, append([]string{fmt.Sprint(lines), f}, paths...)...), nil
}

const winListEventLogsScript = `Get-WinEvent -ListLog * -ErrorAction SilentlyContinue |
  Where-Object { $_.RecordCount -gt 0 } |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 40 |
  ForEach-Object { '{0:yyyy-MM-dd HH:mm}  {1,9} events  {2}' -f $_.LastWriteTime, $_.RecordCount, $_.LogName }
`

const winListDirScript = `Get-ChildItem -LiteralPath $A[0] -File |
  Sort-Object LastWriteTime -Descending |
  ForEach-Object { '{0:yyyy-MM-dd HH:mm}  {1,14:N0}  {2}' -f $_.LastWriteTime, $_.Length, $_.Name }
`

// WinListLogsCommand lists what `logs` can show on a Windows server: the
// event logs with entries, newest first, or the files in dir.
func WinListLogsCommand(r config.Resolved, dir string) *exec.Cmd {
	if dir == "" {
		return psCommand(r, winListEventLogsScript)
	}
	return psCommand(r, winListDirScript, dir)
}
