# storecraft v2.0.0-rc.1

Release snapshot created 2026-10-13 18:29:13 UTC by `scripts/snapshot.ps1`. This project does not use Git; a snapshot in
`releases/` takes the place of a release tag.

## Built with
- Go: go1.27.0 windows/amd64
- Node.js: v24.15.0 (npm 11.12.1)
- OS: Microsoft Windows 11 Business

## CI
- skipped (-SkipCI); run scripts\ci.ps1 to check this tree

## Contents
| File | What it is |
|---|---|
| `storecraft.exe` | The server (windows/amd64, built with `-trimpath`). Primary or replica: `serve -role=primary` / `-role=replica`. |
| `web-dist/` | The built console. Serve it as static files and forward `/v1` and `/metrics` to the primary (see `web/nginx.conf` in the source). |
| `source.zip` | The source tree (155 files), without node_modules, build or test output, databases, binaries, releases, demo state or tokens. |
| `SHA256SUMS.txt` | SHA-256 of every other file in this folder. |

## Run
```powershell
$env:STORECRAFT_ADMIN_TOKEN = "<synthetic admin token>"
.\storecraft.exe seed -db demo.db                      # prints 3 synthetic tenant tokens
.\storecraft.exe serve -role=primary -addr :8090 -db demo.db
```

## Verify the checksums
```powershell
Get-Content SHA256SUMS.txt | ForEach-Object {
  $hash, $file = $_ -split '  ', 2
  if ((Get-FileHash -Algorithm SHA256 $file).Hash.ToLower() -ne $hash) { "MISMATCH $file" } else { "ok $file" }
}
```
