<#
.SYNOPSIS
  Rollback drill for STORE-210: current build -> v2.0.0-rc.1 -> current build on
  the SAME data, with storecraft verify and a smoke check after each step.

.DESCRIPTION
  Uses scripts/demo-up.ps1 and scripts/demo-down.ps1 for every start and stop,
  so processes are always stopped by their recorded PID (never by name). All
  data is synthetic and lives in a fresh temp folder; your demo.db is never
  touched. Writes docs/capstone/drill-log.md with the real outputs.

  Required legs (the drill passes only if every verify says zero divergence
  and every smoke check passes):
    1. current build, fresh data: write data, verify, smoke
    2. roll back to releases/v2.0.0-rc.1 on the same data: verify, smoke
    3. roll forward to the current build on the same data: verify, smoke

  Extra leg (-IncludeV1, on by default): roll back to releases/v1.0.0 (the
  previous release) and forward again. Its EXPECTED results differ and are
  checked as such: v1.0.0 has no usage API, its replica re-escapes < > &
  (intake finding F1), and after rolling forward `backfill-usage` must be run
  on both nodes before verify is clean again.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/rollback-drill.ps1
#>
[CmdletBinding()]
param(
  [int]$PrimaryPort = 18290,
  [int]$ReplicaPort = 18291,
  [string]$RollbackTo = 'releases/v2.0.0-rc.1',
  [bool]$IncludeV1 = $true,
  [string]$Workdir = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
if ($Workdir -eq '') { $Workdir = Join-Path $env:TEMP ('storecraft-drill-' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }
New-Item -ItemType Directory -Force $Workdir | Out-Null
$Workdir = (Resolve-Path $Workdir).Path
$logPath = Join-Path $root 'docs/capstone/drill-log.md'
$primary = "http://127.0.0.1:$PrimaryPort"
$replica = "http://127.0.0.1:$ReplicaPort"
$demoUp = Join-Path $root 'scripts/demo-up.ps1'
$demoDown = Join-Path $root 'scripts/demo-down.ps1'

$md = New-Object System.Collections.Generic.List[string]
$failures = New-Object System.Collections.Generic.List[string]
function Say([string]$line) { $md.Add($line); Write-Host $line }
function Stamp { (Get-Date).ToString('yyyy-MM-dd HH:mm:ss') }
function Fence([string[]]$lines) { $md.Add('```text'); foreach ($l in $lines) { $md.Add($l) }; $md.Add('```'); foreach ($l in $lines) { Write-Host "    $l" } }

# ---- helpers ---------------------------------------------------------------

function Get-Sha([string]$path) { (Get-FileHash -Algorithm SHA256 $path).Hash.Substring(0, 16).ToLower() }

# One HTTP call; returns status and body, never throws for an HTTP error.
function Call([string]$method, [string]$url, [string]$token, [string]$body = '', [hashtable]$headers = @{}) {
  $h = @{ Authorization = "Bearer $token" }
  foreach ($k in $headers.Keys) { $h[$k] = $headers[$k] }
  try {
    if ($body -ne '') {
      $r = Invoke-WebRequest -Method $method -Uri $url -Headers $h -Body ([Text.Encoding]::UTF8.GetBytes($body)) -ContentType 'application/json' -UseBasicParsing -TimeoutSec 15
    } else {
      $r = Invoke-WebRequest -Method $method -Uri $url -Headers $h -UseBasicParsing -TimeoutSec 15
    }
    return [pscustomobject]@{ status = [int]$r.StatusCode; body = [string]$r.Content }
  } catch {
    $resp = $_.Exception.Response
    if ($null -eq $resp) { return [pscustomobject]@{ status = 0; body = $_.Exception.Message } }
    # Windows PowerShell 5.1 puts the error response body in ErrorDetails.
    $text = if ($null -ne $_.ErrorDetails -and $null -ne $_.ErrorDetails.Message) { $_.ErrorDetails.Message } else { '' }
    return [pscustomobject]@{ status = [int]$resp.StatusCode; body = [string]$text }
  }
}

# Runs a native program and returns its exit code and output lines (stderr included).
function Run-Native([string]$exe, [string[]]$arguments, [hashtable]$envVars = @{}) {
  $saved = @{}
  foreach ($k in $envVars.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process'); [Environment]::SetEnvironmentVariable($k, $envVars[$k], 'Process') }
  $old = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $out = @(& $exe @arguments 2>&1 | ForEach-Object { "$_" })
    $code = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $old
    foreach ($k in $envVars.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
  }
  return [pscustomobject]@{ code = $code; lines = $out }
}

function Up([string]$binary, [bool]$keepData) {
  $args = @('-ExecutionPolicy', 'Bypass', '-File', $demoUp, '-Workdir', $Workdir, '-NoConsole',
    '-PrimaryPort', "$PrimaryPort", '-ReplicaPort', "$ReplicaPort", '-Binary', $binary)
  if ($keepData) { $args += '-KeepData' }
  $r = Run-Native 'powershell' $args
  if ($r.code -ne 0) { Fence $r.lines; throw "demo-up failed (exit $($r.code))" }
  $pids = (Get-Content (Join-Path $Workdir 'demo-pids.json') -Raw | ConvertFrom-Json).processes | ForEach-Object { "$($_.name) PID $($_.pid)" }
  Say "- $(Stamp) started with ``demo-up.ps1 -Binary $binary$(if ($keepData) { ' -KeepData' })``: $($pids -join ', ')"
}

function Down {
  $r = Run-Native 'powershell' @('-ExecutionPolicy', 'Bypass', '-File', $demoDown, '-Workdir', $Workdir)
  $stopped = @($r.lines | Where-Object { $_ -match 'stopped|free' } | ForEach-Object { $_.Trim() })
  Say "- $(Stamp) stopped with ``demo-down.ps1`` (exit $($r.code)): $($stopped -join '; ')"
  if ($r.code -ne 0) { throw 'demo-down failed' }
}

function Read-Tokens {
  $t = @{}
  foreach ($line in Get-Content (Join-Path $Workdir 'demo-tokens.txt')) {
    if ($line -match '^(admin|demo-\d): (\S+)$') { $t[$Matches[1]] = $Matches[2] }
  }
  return $t
}

# storecraft verify (always the CURRENT build's verify command).
function Verify([string]$label, [int]$expectExit = 0) {
  $tok = Read-Tokens
  $r = Run-Native $script:verifyExe @('verify', "-primary=$primary", "-replica=$replica", '-timeout', '60s') @{ STORECRAFT_ADMIN_TOKEN = $tok['admin']; STORECRAFT_REPLICA_ADMIN_TOKEN = '' }
  $ok = $r.code -eq $expectExit
  Say "- $(Stamp) **verify ($label)**: exit $($r.code) (expected $expectExit) -> $(if ($ok) { 'as expected' } else { 'NOT AS EXPECTED' })"
  Fence $r.lines
  if (-not $ok) { $failures.Add("verify $label exited $($r.code), expected $expectExit") }
  return $r
}

# Smoke: sign in (list keys), read a key, write a key, read usage.
function Smoke([string]$label, [bool]$hasUsage = $true) {
  $tok = (Read-Tokens)['demo-1']
  $rows = New-Object System.Collections.Generic.List[string]
  $pass = $true
  $list = Call 'GET' "$primary/v1/keys" $tok
  $count = if ($list.status -eq 200) { @(($list.body | ConvertFrom-Json).keys).Count } else { -1 }
  $rows.Add("| sign in (GET /v1/keys) | $($list.status) | $count keys |"); $pass = $pass -and $list.status -eq 200
  $read = Call 'GET' "$primary/v1/keys/drill/k001" $tok
  # Compare the JSON VALUE: v1.0.0 sends the same value with < > & escaped.
  $html = if ($read.status -eq 200) { ($read.body | ConvertFrom-Json).value.html } else { '' }
  $readOk = $html -eq '<b>Tom & Jerry</b>'
  $escaped = $read.body.Contains('u003c')
  $rows.Add("| read drill/k001 | $($read.status) | $(if ($readOk) { 'same value as written in leg 1' } else { $read.body.Trim() })$(if ($escaped) { ' (sent with HTML-escaped JSON: old encoder)' }) |"); $pass = $pass -and $readOk
  $key = 'drill/smoke-' + ($label -replace '[^a-z0-9]', '')
  $write = Call 'PUT' "$primary/v1/keys/$key" $tok "{`"value`":`"written during $label`"}"
  $rows.Add("| write $key | $($write.status) | $($write.body.Trim()) |"); $pass = $pass -and ($write.status -eq 200 -or $write.status -eq 201)
  $usage = Call 'GET' "$primary/v1/usage" $tok
  if ($hasUsage) {
    $u = if ($usage.status -eq 200) { $usage.body | ConvertFrom-Json } else { $null }
    $usageOk = $null -ne $u -and $u.keys.used -eq ($count + $(if ($write.status -eq 201) { 1 } else { 0 }))
    $rows.Add("| read usage | $($usage.status) | $($usage.body.Trim()) |"); $pass = $pass -and $usageOk
  } else {
    $rows.Add("| read usage | $($usage.status) | expected 404: v1.0.0 has no usage API |"); $pass = $pass -and $usage.status -eq 404
  }
  Say "- $(Stamp) **smoke ($label)**: $(if ($pass) { 'PASS' } else { 'FAIL' })"
  $md.Add(''); $md.Add('  | check | status | result |'); $md.Add('  |---|---|---|')
  foreach ($r in $rows) { $md.Add("  $r") }
  $md.Add('')
  if (-not $pass) { $failures.Add("smoke $label failed") }
}

function Write-Leg1Data {
  $tok = Read-Tokens
  $n = 0
  foreach ($t in @('demo-1', 'demo-2', 'demo-3')) {
    for ($i = 1; $i -le 20; $i++) {
      $r = Call 'PUT' ("$primary/v1/keys/drill/k{0:d3}" -f $i) $tok[$t] ('{"value": { "n" : ' + $i + ' ,  "html" : "<b>Tom & Jerry</b>" }}')
      if ($r.status -ne 201) { throw "write failed: $($r.status) $($r.body)" }
      $n++
    }
    foreach ($d in 5, 10) { $null = Call 'DELETE' ("$primary/v1/keys/drill/k{0:d3}" -f $d) $tok[$t] }
  }
  $f = Call 'PUT' "$primary/v1/admin/flags/quota_enforcement" $tok['admin'] '{"enabled":true}' @{ 'If-Match' = '"0"' }
  $q = Call 'PUT' "$primary/v1/admin/tenants/2/quota" $tok['admin'] '{"max_keys":18,"max_bytes":null}' @{ 'If-Match' = '"0"' }
  $rej = Call 'PUT' "$primary/v1/keys/drill/over" $tok['demo-2'] '{"value":1}'
  Say "- $(Stamp) wrote $n keys (values with extra whitespace and < > &) across 3 tenants, deleted 6"
  Say "- $(Stamp) flag on: $($f.status) $($f.body.Trim())"
  Say "- $(Stamp) demo-2 quota 18 keys: $($q.status) $($q.body.Trim())"
  Say "- $(Stamp) a 19th key for demo-2: $($rej.status) $($rej.body.Trim())"
  if ($rej.status -ne 403) { $failures.Add('leg 1: the quota was not enforced') }
}

# ---- the drill -------------------------------------------------------------

$verifyExe = Join-Path $Workdir 'storecraft-current.exe'
Push-Location $root
try {
  & go build -o $verifyExe ./cmd/storecraft
  if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally { Pop-Location }
$rcExe = (Resolve-Path (Join-Path $root "$RollbackTo/storecraft.exe")).Path
$v1Exe = (Resolve-Path (Join-Path $root 'releases/v1.0.0/storecraft.exe')).Path

Say '# Rollback drill log'
Say ''
Say "Written by ``scripts/rollback-drill.ps1`` on $(Stamp). Every line below is real output from this run; data is synthetic, in ``$Workdir``, on ports $PrimaryPort (primary) and $ReplicaPort (replica)."
Say ''
Say '| build | path | SHA-256 (first 16) |'
Say '|---|---|---|'
Say "| current (built from the source tree now) | ``$verifyExe`` | $(Get-Sha $verifyExe) |"
Say "| rollback target | ``$rcExe`` | $(Get-Sha $rcExe) |"
if ($IncludeV1) { Say "| previous release (extra leg) | ``$v1Exe`` | $(Get-Sha $v1Exe) |" }
Say ''

try {
  Say '## Leg 1: current build, fresh data'
  Up $verifyExe $false
  Write-Leg1Data
  Verify 'current, after writes' 0 | Out-Null
  Smoke 'current'
  Down

  Say ''
  Say "## Leg 2: roll back to $RollbackTo on the same data"
  Up $rcExe $true
  Verify 'after rollback' 0 | Out-Null
  Smoke 'rollback rc1'
  Down

  Say ''
  Say '## Leg 3: roll forward to the current build on the same data'
  Up $verifyExe $true
  Verify 'after roll-forward' 0 | Out-Null
  Smoke 'roll forward'
  Down

  if ($IncludeV1) {
    Say ''
    Say '## Extra leg: roll back to releases/v1.0.0 (the previous release), then forward again'
    Say 'Expected (documented in release-readiness.md): v1.0.0 runs on the migrated data but has no usage API; its replica re-escapes < > & (finding F1), so verify reports those keys; after rolling forward, usage counters of values written by v1.0.0 differ between the nodes until backfill-usage has run on both.'
    Up $v1Exe $true
    $tok = Read-Tokens
    $w = Call 'PUT' "$primary/v1/keys/drill/v1-write" $tok['demo-1'] '{"value": { "written" : "by v1.0.0 <b>" }}'
    Say "- $(Stamp) v1.0.0 accepted a write with whitespace and < >: $($w.status) $($w.body.Trim())"
    Verify 'on v1.0.0 (F1 expected)' 1 | Out-Null
    Smoke 'v1.0.0' $false
    Down
    Up $verifyExe $true
    Verify 'current again, before backfill (usage_mismatch expected)' 1 | Out-Null
    Down
    foreach ($db in @('demo.db', 'demo-replica.db')) {
      $r = Run-Native $verifyExe @('backfill-usage', '-db', (Join-Path $Workdir $db))
      Say "- $(Stamp) ``backfill-usage -db $db`` (both nodes stopped): exit $($r.code)"
      Fence $r.lines
      if ($r.code -ne 0) { $failures.Add("backfill $db failed") }
    }
    Up $verifyExe $true
    Verify 'current, after backfill on both nodes' 0 | Out-Null
    Smoke 'final'
    Down
  }
} catch {
  $failures.Add("drill aborted: $($_.Exception.Message)")
  Say "- $(Stamp) ABORTED: $($_.Exception.Message)"
  try { Down } catch { Say "- demo-down after the abort failed: $($_.Exception.Message)" }
}

Say ''
Say '## Result'
if ($failures.Count -eq 0) {
  Say '**PASS**: every verify gave the expected verdict (zero divergence in all required legs) and every smoke check passed.'
} else {
  Say "**FAIL** ($($failures.Count)):"
  foreach ($f in $failures) { Say "- $f" }
}
Set-Content -Path $logPath -Value $md -Encoding UTF8
Write-Host ''
Write-Host "drill log written to $logPath"
if ($failures.Count -eq 0) { exit 0 } else { exit 1 }
