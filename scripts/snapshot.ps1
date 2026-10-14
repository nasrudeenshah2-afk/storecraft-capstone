<#
.SYNOPSIS
  Creates a release snapshot in releases\<name>\ (this project uses snapshots instead of Git tags).

.DESCRIPTION
  1. Runs scripts\ci.ps1 and stops if it fails (skip with -SkipCI, for example
     right after a green CI run; the README then says so).
  2. Builds storecraft.exe (windows/amd64, -trimpath) and the console (web\dist).
  3. Zips the source tree, without node_modules, build output, test output,
     databases, binaries, releases, demo state or tokens.
  4. Writes README.md (Go, Node and npm versions, CI result, how to run and
     verify) and SHA256SUMS.txt (checksums of every file in the release).
  Everything is assembled in releases\.<name>.partial and renamed at the end,
  so a failed run never leaves a half-made release. An existing release is
  never overwritten unless -Force is given.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts/snapshot.ps1 v1.0.0
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true, Position = 0)]
  [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
  [string]$Name,
  # Do not run scripts\ci.ps1 first.
  [switch]$SkipCI,
  # Replace releases\<name> if it already exists.
  [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$releases = Join-Path $root 'releases'
$final = Join-Path $releases $Name
$stage = Join-Path $releases ".$Name.partial"

function Fail([string]$message) {
  Write-Host "snapshot: $message" -ForegroundColor Red
  if (Test-Path $stage) { Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue }
  exit 1
}

function Step([string]$text) { Write-Host "---- $text" -ForegroundColor Cyan }

# Any unexpected error: say what it was, remove the partial release, exit 1.
trap { Fail "unexpected error: $($_.Exception.Message)" }

if ((Test-Path $final) -and -not $Force) {
  Fail "releases\$Name already exists; choose another name or pass -Force to replace it."
}
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Force $stage | Out-Null

# --- 1. CI -------------------------------------------------------------------
$ciResult = 'skipped (-SkipCI); run scripts\ci.ps1 to check this tree'
if (-not $SkipCI) {
  Step 'running scripts\ci.ps1'
  & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'scripts\ci.ps1') | Out-Host
  if ($LASTEXITCODE -ne 0) { Fail 'CI failed; no snapshot was made.' }
  $ciResult = "PASS (all stages of scripts\ci.ps1) at $((Get-Date).ToUniversalTime().ToString('yyyy-MM-dd HH:mm:ss')) UTC"
}

# --- 2. builds -----------------------------------------------------------------
Step 'building storecraft.exe'
Push-Location $root
try {
  & go build -trimpath -o (Join-Path $stage 'storecraft.exe') ./cmd/storecraft | Out-Host
  if ($LASTEXITCODE -ne 0) { Fail 'go build failed' }
} finally {
  Pop-Location
}

Step 'building the console (web\dist)'
Push-Location (Join-Path $root 'web')
try {
  & npm run build | Out-Host
  if ($LASTEXITCODE -ne 0) { Fail 'npm run build failed' }
} finally {
  Pop-Location
}
Copy-Item (Join-Path $root 'web\dist') (Join-Path $stage 'web-dist') -Recurse

# --- 3. source zip -------------------------------------------------------------
Step 'zipping the source tree'
$excludeDirs = @('node_modules', 'dist', 'test-results', 'playwright-report', 'releases', 'demo-logs', '.claude', '.git')
# *.exe~ is the copy Go leaves behind when it rebuilds an .exe that is still running.
$excludeFiles = @('*.db', '*.db-wal', '*.db-shm', '*.exe', '*.exe~', 'demo-tokens.txt', 'demo-pids.json')

function Test-Excluded([IO.FileInfo]$file) {
  $rel = $file.FullName.Substring($root.Length + 1)
  foreach ($part in ($rel -split '\\')[0..(($rel -split '\\').Count - 2)]) {
    if ($excludeDirs -contains $part) { return $true }
  }
  foreach ($pattern in $excludeFiles) {
    if ($file.Name -like $pattern) { return $true }
  }
  return $false
}

Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
$zipPath = Join-Path $stage 'source.zip'
$sources = @(Get-ChildItem $root -Recurse -File -Force | Where-Object { -not (Test-Excluded $_) })
$zip = [System.IO.Compression.ZipFile]::Open($zipPath, [System.IO.Compression.ZipArchiveMode]::Create)
try {
  foreach ($f in $sources) {
    $entry = $f.FullName.Substring($root.Length + 1) -replace '\\', '/'
    [void][System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $f.FullName, $entry, [System.IO.Compression.CompressionLevel]::Optimal)
  }
} finally {
  $zip.Dispose()
}
Write-Host "source.zip: $($sources.Count) files"

# --- 4. README and checksums -------------------------------------------------
$goVersion = (& go version) -replace '^go version ', ''
$nodeVersion = (& node --version)
$npmVersion = (& npm --version)
$os = (Get-CimInstance Win32_OperatingSystem).Caption
$created = (Get-Date).ToUniversalTime().ToString('yyyy-MM-dd HH:mm:ss')
$readme = @"
# storecraft $Name

Release snapshot created $created UTC by ``scripts/snapshot.ps1``. This project does not use Git; a snapshot in
``releases/`` takes the place of a release tag.

## Built with
- Go: $goVersion
- Node.js: $nodeVersion (npm $npmVersion)
- OS: $os

## CI
- $ciResult

## Contents
| File | What it is |
|---|---|
| ``storecraft.exe`` | The server (windows/amd64, built with ``-trimpath``). Primary or replica: ``serve -role=primary`` / ``-role=replica``. |
| ``web-dist/`` | The built console. Serve it as static files and forward ``/v1`` and ``/metrics`` to the primary (see ``web/nginx.conf`` in the source). |
| ``source.zip`` | The source tree ($($sources.Count) files), without node_modules, build or test output, databases, binaries, releases, demo state or tokens. |
| ``SHA256SUMS.txt`` | SHA-256 of every other file in this folder. |

## Run
``````powershell
`$env:STORECRAFT_ADMIN_TOKEN = "<synthetic admin token>"
.\storecraft.exe seed -db demo.db                      # prints 3 synthetic tenant tokens
.\storecraft.exe serve -role=primary -addr :8090 -db demo.db
``````

## Verify the checksums
``````powershell
Get-Content SHA256SUMS.txt | ForEach-Object {
  `$hash, `$file = `$_ -split '  ', 2
  if ((Get-FileHash -Algorithm SHA256 `$file).Hash.ToLower() -ne `$hash) { "MISMATCH `$file" } else { "ok `$file" }
}
``````
"@
Set-Content -Path (Join-Path $stage 'README.md') -Value $readme -Encoding UTF8

$sums = Get-ChildItem $stage -Recurse -File | Sort-Object FullName | ForEach-Object {
  $rel = $_.FullName.Substring($stage.Length + 1) -replace '\\', '/'
  '{0}  {1}' -f (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLower(), $rel
}
Set-Content -Path (Join-Path $stage 'SHA256SUMS.txt') -Value $sums -Encoding ASCII

# --- 5. publish ------------------------------------------------------------------
if (Test-Path $final) { Remove-Item $final -Recurse -Force }
# Freshly written files (especially the .exe) can be held open for a moment,
# for example by an antivirus scan, which makes the folder rename fail; retry.
$renamed = $false
for ($attempt = 1; $attempt -le 30 -and -not $renamed; $attempt++) {
  try {
    Rename-Item $stage $Name -ErrorAction Stop
    $renamed = $true
  } catch {
    Start-Sleep -Seconds 1
  }
}
if (-not $renamed) { Fail "could not rename $stage to $Name (a file in it is still in use); nothing was published" }
Write-Host ''
Write-Host "snapshot releases\$Name created" -ForegroundColor Green
Get-ChildItem $final | ForEach-Object { Write-Host ('  {0,-16} {1,12:N0} bytes' -f $_.Name, $(if ($_.PSIsContainer) { (Get-ChildItem $_.FullName -Recurse -File | Measure-Object Length -Sum).Sum } else { $_.Length })) }
exit 0
