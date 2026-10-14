// Bundle budget: fails when the main JavaScript file of the production build
// is larger than BUDGET_BYTES (uncompressed). Run after `npm run build`.
// The budget and the reason for it are documented in web/CLAUDE.md.

import { readFileSync, statSync } from 'node:fs'
import { gzipSync } from 'node:zlib'

const BUDGET_BYTES = 120 * 1024

const dist = new URL('../dist/', import.meta.url)
let html: string
try {
  html = readFileSync(new URL('index.html', dist), 'utf8')
} catch {
  process.stderr.write('check:bundle: dist/index.html not found; run `npm run build` first\n')
  process.exit(1)
}

// The entry script referenced by index.html is the main bundle.
const match = /<script[^>]+src="\/(assets\/[^"]+\.js)"/.exec(html)
const entry = match?.[1]
if (entry === undefined) {
  process.stderr.write('check:bundle: no entry script found in dist/index.html\n')
  process.exit(1)
}

const file = new URL(entry, dist)
const size = statSync(file).size
const gz = gzipSync(readFileSync(file)).length
const kb = (n: number) => `${(n / 1024).toFixed(1)} kB`
const verdict = size <= BUDGET_BYTES ? 'PASS' : 'FAIL'
process.stdout.write(`check:bundle ${verdict}: ${entry} is ${kb(size)} (${kb(gz)} gzip); budget ${kb(BUDGET_BYTES)}\n`)
process.exit(size <= BUDGET_BYTES ? 0 : 1)
