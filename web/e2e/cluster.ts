// Node-side helpers for the browser tests: a temporary storecraft primary
// (fresh database, synthetic tenants) plus the Vite dev server proxying to it.
// Used by global-setup.ts, global-teardown.ts and the tests (restartGo).

import { execFileSync, spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { openSync, readFileSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

export const webDir = resolve(fileURLToPath(new URL('.', import.meta.url)), '..')
export const repoRoot = resolve(webDir, '..')

export interface Tenant {
  name: string
  token: string
}

export interface E2EState {
  tmpDir: string
  binary: string
  db: string
  goPort: number
  webPort: number
  goPid: number
  vitePid: number
  adminToken: string
  tenants: Tenant[]
  apiUrl: string
  webUrl: string
}

/** The state file location is passed from global setup to everything else. */
export const STATE_ENV = 'STORECRAFT_E2E_STATE'

export function readState(): E2EState {
  const path = process.env[STATE_ENV]
  if (path === undefined || path === '') {
    throw new Error(`${STATE_ENV} is not set: run the tests with "npm run e2e" so global setup runs`)
  }
  return JSON.parse(readFileSync(path, 'utf8')) as E2EState
}

export function writeState(state: E2EState): void {
  const path = process.env[STATE_ENV]
  if (path === undefined || path === '') {
    throw new Error(`${STATE_ENV} is not set`)
  }
  writeFileSync(path, JSON.stringify(state, null, 2))
}

/** A TCP port that is free right now on 127.0.0.1. */
export function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      srv.close(() => {
        if (addr !== null && typeof addr === 'object') {
          resolvePort(addr.port)
        } else {
          reject(new Error('could not get a free port'))
        }
      })
    })
  })
}

export function randomToken(): string {
  return randomBytes(16).toString('hex')
}

/** Builds the Go program into dir (never touching the project's storecraft.exe). */
export function buildBinary(dir: string): string {
  const binary = join(dir, process.platform === 'win32' ? 'storecraft-e2e.exe' : 'storecraft-e2e')
  execFileSync('go', ['build', '-o', binary, './cmd/storecraft'], { cwd: repoRoot, stdio: 'inherit' })
  return binary
}

/** Seeds the database and returns the printed tenant tokens ("demo-1: <token>"). */
export function seed(binary: string, db: string): Tenant[] {
  const out = execFileSync(binary, ['seed', '-db', db], { encoding: 'utf8' })
  const tenants: Tenant[] = []
  for (const line of out.split(/\r?\n/)) {
    const m = /^(\S+): ([0-9a-f]{64})$/.exec(line.trim())
    if (m?.[1] !== undefined && m[2] !== undefined) {
      tenants.push({ name: m[1], token: m[2] })
    }
  }
  if (tenants.length === 0) {
    throw new Error(`seed printed no tokens:\n${out}`)
  }
  return tenants
}

/** Polls url until it answers with the expected status. */
export async function waitForHttp(url: string, status: number, timeoutMs = 30_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let last = 'no answer'
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url)
      if (res.status === status) {
        return
      }
      last = `HTTP ${res.status}`
    } catch (e) {
      last = e instanceof Error ? e.message : String(e)
    }
    await new Promise((r) => setTimeout(r, 150))
  }
  throw new Error(`${url} did not answer ${status} within ${timeoutMs}ms (last: ${last})`)
}

/** Starts the Go primary on the state's port and database; returns its PID. */
export async function startGo(state: Pick<E2EState, 'binary' | 'db' | 'goPort' | 'adminToken' | 'tmpDir' | 'apiUrl'>): Promise<number> {
  const log = openSync(join(state.tmpDir, 'go-server.log'), 'a')
  const child = spawn(
    state.binary,
    ['serve', '-addr', `127.0.0.1:${state.goPort}`, '-db', state.db, '-poll-timeout', '2s', '-sse-heartbeat', '1s'],
    { env: { ...process.env, STORECRAFT_ADMIN_TOKEN: state.adminToken }, stdio: ['ignore', log, log] },
  )
  child.unref()
  if (child.pid === undefined) {
    throw new Error('the Go server did not start')
  }
  // /metrics without a token answers 401 once the server is up.
  await waitForHttp(`${state.apiUrl}/metrics`, 401)
  return child.pid
}

/** Starts the Vite dev server proxying to the Go server; returns its PID. */
export async function startVite(state: Pick<E2EState, 'webPort' | 'apiUrl' | 'tmpDir' | 'webUrl'>): Promise<number> {
  const log = openSync(join(state.tmpDir, 'vite.log'), 'a')
  const child = spawn(
    process.execPath,
    [join(webDir, 'node_modules', 'vite', 'bin', 'vite.js'), '--port', String(state.webPort), '--strictPort', '--host', '127.0.0.1'],
    { cwd: webDir, env: { ...process.env, STORECRAFT_API_TARGET: state.apiUrl }, stdio: ['ignore', log, log] },
  )
  child.unref()
  if (child.pid === undefined) {
    throw new Error('the Vite dev server did not start')
  }
  await waitForHttp(`${state.webUrl}/`, 200)
  return child.pid
}

/** Stops a process this test run started (by its recorded PID). Missing is fine. */
export function stopPid(pid: number): void {
  try {
    process.kill(pid)
  } catch {
    // already gone
  }
}

/** Waits until nothing accepts connections on the port any more. */
export async function waitPortFree(port: number, timeoutMs = 15_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      await fetch(`http://127.0.0.1:${port}/`)
    } catch {
      return // connection refused: free
    }
    await new Promise((r) => setTimeout(r, 100))
  }
  throw new Error(`port ${port} is still in use`)
}

/** Stops and starts the Go server on the same port and database (a server restart). */
export async function restartGo(): Promise<void> {
  const state = readState()
  stopPid(state.goPid)
  await waitPortFree(state.goPort)
  state.goPid = await startGo(state)
  writeState(state)
}
