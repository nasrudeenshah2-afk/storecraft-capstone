<#
.SYNOPSIS
  Stops exactly the demo processes that scripts\demo-up.ps1 started, nothing else.

.DESCRIPTION
  Reads demo-pids.json. For each recorded process it checks that the process
  with that ID is still the same program (same path and start time) before
  stopping it, so a process ID that Windows has since reused for another
  program is never touched. Never stops anything by name. Then confirms the
  recorded ports are free and removes demo-pids.json.
  The demo databases and demo-tokens.txt are kept.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/demo-down.ps1
#>
[CmdletBinding()]
param(
  # Same folder as given to demo-up.ps1 (default: project root).
  [string]$Workdir = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
if ($Workdir -eq '') { $Workdir = $root }
$pidFile = Join-Path $Workdir 'demo-pids.json'

if (-not (Test-Path $pidFile)) {
  Write-Host "demo-down: no $pidFile, so there is nothing to stop."
  exit 0
}

$state = Get-Content $pidFile -Raw | ConvertFrom-Json
$problems = 0

# Stop in reverse start order: console, replica, primary.
$entries = @($state.processes)
[array]::Reverse($entries)
foreach ($e in $entries) {
  $p = Get-Process -Id $e.pid -ErrorAction SilentlyContinue
  if ($null -eq $p) {
    Write-Host ("  {0,-8} PID {1}: already stopped" -f $e.name, $e.pid)
    continue
  }
  $sameStart = $p.StartTime.ToUniversalTime().ToString('o') -eq $e.startTime
  $samePath = $p.Path -eq $e.path
  if (-not ($sameStart -and $samePath)) {
    Write-Host ("  {0,-8} PID {1}: now belongs to another program ({2}); NOT stopping it" -f $e.name, $e.pid, $p.ProcessName) -ForegroundColor Yellow
    continue
  }
  Stop-Process -Id $e.pid -Force
  $null = $p.WaitForExit(10000)
  Write-Host ("  {0,-8} PID {1}: stopped" -f $e.name, $e.pid)
}

# Confirm the recorded ports are free.
foreach ($e in @($state.processes)) {
  $deadline = (Get-Date).AddSeconds(10)
  $busy = $true
  while ((Get-Date) -lt $deadline) {
    $busy = $null -ne (Get-NetTCPConnection -LocalPort $e.port -State Listen -ErrorAction SilentlyContinue)
    if (-not $busy) { break }
    Start-Sleep -Milliseconds 200
  }
  if ($busy) {
    Write-Host ("  port {0} ({1}) is still in use" -f $e.port, $e.name) -ForegroundColor Red
    $problems++
  } else {
    Write-Host ("  port {0} ({1}) is free" -f $e.port, $e.name)
  }
}

if ($problems -gt 0) {
  Write-Host "demo-down: finished with $problems problem(s); $pidFile kept." -ForegroundColor Red
  exit 1
}
Remove-Item $pidFile -Force
Write-Host 'demo-down: demo stopped; all its ports are free.' -ForegroundColor Green
exit 0
