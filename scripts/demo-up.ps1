<#
.SYNOPSIS
  Starts a fresh storecraft demo: primary, replica and console, with synthetic tenants.

.DESCRIPTION
  1. Refuses to run if a recorded demo is still running or a demo port is in use
     (it reports who holds the port and never stops anything itself).
  2. Builds storecraft.exe and deletes the old demo databases.
  3. Seeds 3 synthetic tenants into demo.db and saves their tokens (plus a random
     synthetic admin token) to demo-tokens.txt.
  4. Starts the primary, the replica and the console (Vite dev server) as background
     processes, logs in demo-logs\, and records each process ID, start time and
     program path in demo-pids.json (scripts\demo-down.ps1 stops exactly these).
  5. Prints the tokens and URLs.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1 -PrimaryPort 18090 -ReplicaPort 18091 -ConsolePort 15173 -Workdir $env:TEMP\demo
.EXAMPLE
  # Restart an existing demo's data with another build (used by scripts\rollback-drill.ps1):
  powershell -ExecutionPolicy Bypass -File scripts/demo-up.ps1 -Workdir $env:TEMP\drill -NoConsole -KeepData -Binary releases\v2.0.0-rc.1\storecraft.exe
#>
[CmdletBinding()]
param(
  [int]$PrimaryPort = 8090,
  [int]$ReplicaPort = 8091,
  [int]$ConsolePort = 5173,
  # Folder for demo.db, demo-replica.db, demo-tokens.txt, demo-pids.json and demo-logs\ (default: project root).
  [string]$Workdir = '',
  # Do not start the console (Vite dev server).
  [switch]$NoConsole,
  # Run this storecraft.exe instead of building one from the source tree.
  [string]$Binary = '',
  # Keep demo.db, demo-replica.db and demo-tokens.txt from an earlier run (no reset, no seed).
  [switch]$KeepData
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
if ($Workdir -eq '') { $Workdir = $root }
New-Item -ItemType Directory -Force $Workdir | Out-Null
$Workdir = (Resolve-Path $Workdir).Path
$exe = if ($Binary -ne '') { (Resolve-Path $Binary).Path } else { Join-Path $root 'storecraft.exe' }
$pidFile = Join-Path $Workdir 'demo-pids.json'
$tokensFile = Join-Path $Workdir 'demo-tokens.txt'
$logDir = Join-Path $Workdir 'demo-logs'
$primaryDb = Join-Path $Workdir 'demo.db'
$replicaDb = Join-Path $Workdir 'demo-replica.db'
$primaryUrl = "http://127.0.0.1:$PrimaryPort"
$replicaUrl = "http://127.0.0.1:$ReplicaPort"
$consoleUrl = "http://127.0.0.1:$ConsolePort"

function Fail([string]$message) {
  Write-Host "demo-up: $message" -ForegroundColor Red
  exit 1
}

function Get-Listener([int]$port) {
  Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
}

# Polls url until it answers with the expected HTTP status.
function Wait-Http([string]$url, [int]$status, [int]$seconds = 30, [hashtable]$headers = @{}) {
  $deadline = (Get-Date).AddSeconds($seconds)
  while ((Get-Date) -lt $deadline) {
    try {
      $r = Invoke-WebRequest -Uri $url -Headers $headers -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
      if ($r.StatusCode -eq $status) { return $true }
    } catch {
      $resp = $_.Exception.Response
      if ($null -ne $resp -and [int]$resp.StatusCode -eq $status) { return $true }
    }
    Start-Sleep -Milliseconds 250
  }
  return $false
}

# --- 1. refuse instead of disturbing anything ---------------------------------
if (Test-Path $pidFile) {
  $old = Get-Content $pidFile -Raw | ConvertFrom-Json
  foreach ($entry in @($old.processes)) {
    if (Get-Process -Id $entry.pid -ErrorAction SilentlyContinue) {
      Fail "a demo started earlier is still running (PID $($entry.pid), $($entry.name)). Run scripts\demo-down.ps1 first."
    }
  }
}
$ports = @($PrimaryPort, $ReplicaPort)
if (-not $NoConsole) { $ports += $ConsolePort }
foreach ($port in $ports) {
  $l = Get-Listener $port
  if ($null -ne $l) {
    $owner = Get-Process -Id $l.OwningProcess -ErrorAction SilentlyContinue
    $who = if ($owner) { "$($owner.ProcessName) (PID $($owner.Id))" } else { "PID $($l.OwningProcess)" }
    Fail "port $port is already in use by $who. Stop it yourself (or choose other ports with -PrimaryPort/-ReplicaPort/-ConsolePort); demo-up never stops other programs."
  }
}
if (-not $NoConsole -and -not (Test-Path (Join-Path $root 'web\node_modules\vite'))) {
  Fail 'web\node_modules is missing; run "npm ci" in web\ first (or use -NoConsole).'
}

# --- 2. build and reset the demo databases -----------------------------------
if ($Binary -eq '') {
  Write-Host 'building storecraft.exe ...'
  Push-Location $root
  try {
    & go build -o $exe ./cmd/storecraft
    if ($LASTEXITCODE -ne 0) { Fail 'go build failed' }
  } finally {
    Pop-Location
  }
} else {
  Write-Host "using $exe (no build)"
}
if ($KeepData) {
  if (-not (Test-Path $primaryDb) -or -not (Test-Path $tokensFile)) { Fail "-KeepData: $primaryDb or $tokensFile does not exist" }
}
foreach ($f in $(if ($KeepData) { @() } else { @($primaryDb, $replicaDb) })) {
  foreach ($suffix in '', '-wal', '-shm') {
    $path = "$f$suffix"
    if (Test-Path $path) {
      try {
        Remove-Item $path -Force -ErrorAction Stop
      } catch {
        Fail "cannot delete $path (is another program using it?): $($_.Exception.Message)"
      }
    }
  }
}
New-Item -ItemType Directory -Force $logDir | Out-Null

# --- 3. seed synthetic tenants (or reuse them with -KeepData) -----------------
$tenants = @()
if ($KeepData) {
  $adminToken = ''
  foreach ($line in Get-Content $tokensFile) {
    if ($line -match '^admin: (\S+)$') { $adminToken = $Matches[1] }
    elseif ($line -match '^(\S+): ([0-9a-f]{64})$') { $tenants += [pscustomobject]@{ name = $Matches[1]; token = $Matches[2] } }
  }
  if ($adminToken -eq '' -or $tenants.Count -eq 0) { Fail "-KeepData: no admin or tenant tokens in $tokensFile" }
} else {
  $seedOut = @(& $exe seed -db $primaryDb)
  if ($LASTEXITCODE -ne 0) { Fail 'seeding failed' }
  foreach ($line in $seedOut) {
    if ($line -match '^(\S+): ([0-9a-f]{64})$') { $tenants += [pscustomobject]@{ name = $Matches[1]; token = $Matches[2] } }
  }
  if ($tenants.Count -eq 0) { Fail "seed printed no tokens: $($seedOut -join ' ')" }
  $adminToken = 'demo-admin-' + [guid]::NewGuid().ToString('N')
}

$lines = @(
  '# storecraft demo tokens (synthetic, for local demos only)',
  "# created $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') by scripts\demo-up.ps1",
  "admin: $adminToken"
)
$lines += $tenants | ForEach-Object { "$($_.name): $($_.token)" }
$lines += @("primary: $primaryUrl", "replica: $replicaUrl")
if (-not $NoConsole) { $lines += "console: $consoleUrl" }
Set-Content -Path $tokensFile -Value $lines -Encoding UTF8

# --- 4. start the processes and record them ----------------------------------
$started = New-Object System.Collections.Generic.List[object]
function Save-Pids {
  [pscustomobject]@{ workdir = $Workdir; processes = $started } | ConvertTo-Json -Depth 4 | Set-Content -Path $pidFile -Encoding UTF8
}

# Starts a background program with extra environment variables (removed again
# afterwards so they do not leak into this shell) and records it.
#
# The program is started through a small cmd.exe wrapper that writes the log
# files, and the wrapper is launched without redirection (the shell-execute
# way). Start-Process -RedirectStandardOutput would make the program inherit
# THIS script's output handles, so anything capturing demo-up's output (another
# script, a CI job) would hang until the demo is stopped. The real program's
# PID (the wrapper's child) is what gets recorded and stopped.
function Start-DemoProcess([string]$name, [string]$file, [string[]]$arguments, [hashtable]$envVars, [int]$port, [string]$workingDir) {
  $out = Join-Path $logDir "$name.out.log"
  $err = Join-Path $logDir "$name.err.log"
  $argLine = ($arguments | ForEach-Object { if ($_ -match '[\s"]') { '"' + $_ + '"' } else { $_ } }) -join ' '
  $cmdLine = "/d /s /c `"`"$file`" $argLine 1>`"$out`" 2>`"$err`"`""
  $saved = @{}
  foreach ($k in $envVars.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process'); [Environment]::SetEnvironmentVariable($k, $envVars[$k], 'Process') }
  try {
    $wrapper = Start-Process -FilePath $env:ComSpec -ArgumentList $cmdLine -WorkingDirectory $workingDir -WindowStyle Hidden -PassThru
  } finally {
    foreach ($k in $envVars.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
  }
  $exeName = [IO.Path]::GetFileName($file)
  $child = $null
  $deadline = (Get-Date).AddSeconds(15)
  while ($null -eq $child -and (Get-Date) -lt $deadline) {
    $child = Get-CimInstance Win32_Process -Filter "ParentProcessId = $($wrapper.Id)" -ErrorAction SilentlyContinue |
      Where-Object { $_.Name -eq $exeName } | Select-Object -First 1
    if ($null -eq $child) { Start-Sleep -Milliseconds 100 }
  }
  if ($null -eq $child) { throw "$name did not start (see $err)" }
  $p = Get-Process -Id $child.ProcessId
  $started.Add([pscustomobject]@{
      name       = $name
      pid        = $p.Id
      port       = $port
      path       = $p.Path
      startTime  = $p.StartTime.ToUniversalTime().ToString('o')
      wrapperPid = $wrapper.Id
    })
  Save-Pids
  return $p
}

function Stop-Started {
  foreach ($s in $started) { Stop-Process -Id $s.pid -Force -ErrorAction SilentlyContinue }
}

try {
  $null = Start-DemoProcess 'primary' $exe @('serve', '-role=primary', '-addr', "127.0.0.1:$PrimaryPort", '-db', $primaryDb) `
    @{ STORECRAFT_ADMIN_TOKEN = $adminToken } $PrimaryPort $Workdir
  if (-not (Wait-Http "$primaryUrl/metrics" 401)) { throw "the primary did not start (see $logDir\primary.err.log)" }

  $null = Start-DemoProcess 'replica' $exe @('serve', '-role=replica', "-primary=$primaryUrl", '-addr', "127.0.0.1:$ReplicaPort", '-db', $replicaDb) `
    @{ STORECRAFT_ADMIN_TOKEN = $adminToken; STORECRAFT_REPLICATION_TOKEN = $adminToken } $ReplicaPort $Workdir
  if (-not (Wait-Http "$replicaUrl/metrics" 401)) { throw "the replica did not start (see $logDir\replica.err.log)" }

  if (-not $NoConsole) {
    $node = (Get-Command node).Source
    $vite = Join-Path $root 'web\node_modules\vite\bin\vite.js'
    $null = Start-DemoProcess 'console' $node @($vite, '--port', "$ConsolePort", '--strictPort', '--host', '127.0.0.1') `
      @{ STORECRAFT_API_TARGET = $primaryUrl } $ConsolePort (Join-Path $root 'web')
    if (-not (Wait-Http "$consoleUrl/" 200 60)) { throw "the console did not start (see $logDir\console.err.log)" }
  }
} catch {
  Write-Host "demo-up: $($_.Exception.Message); stopping what was started" -ForegroundColor Red
  Stop-Started
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
  exit 1
}

# --- 5. report ----------------------------------------------------------------
Write-Host ''
Write-Host 'storecraft demo is running' -ForegroundColor Green
Write-Host ("  primary  {0}   (PID {1})" -f $primaryUrl, $started[0].pid)
Write-Host ("  replica  {0}   (PID {1})" -f $replicaUrl, $started[1].pid)
if (-not $NoConsole) { Write-Host ("  console  {0}   (PID {1})  <- open this in a browser" -f $consoleUrl, $started[2].pid) }
Write-Host ''
Write-Host 'tokens (synthetic; also saved in demo-tokens.txt):'
Write-Host "  admin   : $adminToken"
foreach ($t in $tenants) { Write-Host ("  {0,-8}: {1}" -f $t.name, $t.token) }
Write-Host ''
Write-Host "process IDs: $pidFile   logs: $logDir"
Write-Host 'stop with:   powershell -ExecutionPolicy Bypass -File scripts/demo-down.ps1'
exit 0
