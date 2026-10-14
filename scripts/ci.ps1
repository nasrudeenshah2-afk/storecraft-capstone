<#
.SYNOPSIS
  Local CI for storecraft: runs every check in order and stops at the first failure.

.DESCRIPTION
  Stages (in this order):
    1. gofmt -l          (must print nothing; checks only this module's Go packages)
    2. go vet ./...
    3. go test -race -count=1 ./...
    4. web: npm ci       (only if web/node_modules is missing)
    5. web: npm run lint
    6. web: npm run typecheck
    7. web: npm run test
    8. web: npm run build
    9. web: npm run check:bundle
   10. web: npm run e2e  (Playwright against a temporary real Go server)

  Prints PASS or FAIL for each stage and a summary with the total time.
  Exit code: 0 when every stage passed, 1 at the first failure.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/ci.ps1
#>
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$web = Join-Path $root 'web'
$results = New-Object System.Collections.Generic.List[object]
$total = [System.Diagnostics.Stopwatch]::StartNew()

function Write-Line([string]$text, [ConsoleColor]$color = [ConsoleColor]::Gray) {
  Write-Host $text -ForegroundColor $color
}

function Show-Summary([bool]$ok) {
  Write-Line ''
  Write-Line '================ CI summary ================'
  foreach ($r in $results) {
    $color = if ($r.Status -eq 'PASS') { [ConsoleColor]::Green } else { [ConsoleColor]::Red }
    Write-Line ('{0,-4}  {1,-34} {2,8:N1}s' -f $r.Status, $r.Name, $r.Seconds) $color
  }
  $verdict = if ($ok) { 'PASS' } else { 'FAIL' }
  $color = if ($ok) { [ConsoleColor]::Green } else { [ConsoleColor]::Red }
  Write-Line ('{0}: {1} of {2} stages passed, total {3:N1}s' -f $verdict, @($results | Where-Object Status -eq 'PASS').Count, $results.Count, $total.Elapsed.TotalSeconds) $color
}

# Runs one stage. $action must return exactly one value, $true, to pass;
# anything else (including stray output mixed into the result) is a FAIL.
# Stops CI on failure.
function Invoke-Stage([string]$name, [string]$dir, [scriptblock]$action) {
  Write-Line ''
  Write-Line "---- $name" Cyan
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  $ok = $false
  Push-Location $dir
  try {
    $result = @(& $action)
    $ok = ($result.Count -eq 1) -and ($result[0] -is [bool]) -and $result[0]
    if (-not $ok -and $result.Count -ne 1) {
      Write-Line "stage returned $($result.Count) values instead of one boolean; treating it as a failure" Red
    }
  } catch {
    Write-Line "error: $($_.Exception.Message)" Red
    $ok = $false
  } finally {
    Pop-Location
  }
  $sw.Stop()
  $status = if ($ok) { 'PASS' } else { 'FAIL' }
  $results.Add([pscustomobject]@{ Name = $name; Status = $status; Seconds = $sw.Elapsed.TotalSeconds })
  $color = if ($ok) { [ConsoleColor]::Green } else { [ConsoleColor]::Red }
  Write-Line ('[{0}] {1} ({2:N1}s)' -f $status, $name, $sw.Elapsed.TotalSeconds) $color
  if (-not $ok) {
    Show-Summary $false
    exit 1
  }
}

# Runs a native command; success means exit code 0. Its output goes straight
# to the console (Out-Host) so it can never leak into the stage result: in
# PowerShell everything a function prints to the output stream is returned.
function Invoke-Native([string]$exe, [string[]]$arguments) {
  & $exe @arguments | Out-Host
  return ($LASTEXITCODE -eq 0)
}

Write-Line "storecraft CI  root=$root" White
Write-Line ("go: {0}   node: {1}   npm: {2}" -f ((& go version) -replace '^go version ', ''), (& node --version), (& npm --version)) White

Invoke-Stage 'gofmt -l (must print nothing)' $root {
  # Only this module's package folders (go list honours the go.mod ignore of
  # web/node_modules), so third-party .go files can never fail this stage.
  $dirs = @(& go list -f '{{.Dir}}' ./...)
  if ($LASTEXITCODE -ne 0) { return $false }
  $unformatted = @(& gofmt -l @dirs)
  if ($LASTEXITCODE -ne 0) { return $false }
  if ($unformatted.Count -gt 0) {
    Write-Line 'gofmt would reformat:' Red
    $unformatted | ForEach-Object { Write-Line "  $_" Red }
    Write-Line 'fix with: gofmt -w <file>' Red
    return $false
  }
  Write-Line "checked $($dirs.Count) package folders; all formatted"
  return $true
}

Invoke-Stage 'go vet ./...' $root { Invoke-Native 'go' @('vet', './...') }

Invoke-Stage 'go test -race -count=1 ./...' $root { Invoke-Native 'go' @('test', '-race', '-count=1', './...') }

if (-not (Test-Path (Join-Path $web 'node_modules'))) {
  Invoke-Stage 'web: npm ci' $web { Invoke-Native 'npm' @('ci', '--no-fund', '--no-audit') }
}

foreach ($script in 'lint', 'typecheck', 'test', 'build', 'check:bundle', 'e2e') {
  Invoke-Stage "web: npm run $script" $web { Invoke-Native 'npm' @('run', $script) }
}

$total.Stop()
Show-Summary $true
exit 0
