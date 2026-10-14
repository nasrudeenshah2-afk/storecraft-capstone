import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  STATE_ENV,
  buildBinary,
  freePort,
  randomToken,
  seed,
  startGo,
  startVite,
  writeState,
  type E2EState,
} from './cluster'

/**
 * Before any test: build a private copy of the Go program, seed a fresh
 * database with synthetic tenants, and start the Go primary and the Vite dev
 * server on free ports. Everything lives in one temp folder; the PIDs are
 * recorded so teardown stops exactly these processes.
 */
export default async function globalSetup(): Promise<void> {
  const tmpDir = mkdtempSync(join(tmpdir(), 'storecraft-e2e-'))
  process.env[STATE_ENV] = join(tmpDir, 'state.json')

  const binary = buildBinary(tmpDir)
  const db = join(tmpDir, 'e2e.db')
  const tenants = seed(binary, db)
  const goPort = await freePort()
  const webPort = await freePort()
  const state: E2EState = {
    tmpDir,
    binary,
    db,
    goPort,
    webPort,
    goPid: 0,
    vitePid: 0,
    adminToken: randomToken(),
    tenants,
    apiUrl: `http://127.0.0.1:${goPort}`,
    webUrl: `http://127.0.0.1:${webPort}`,
  }
  writeState(state) // record early so teardown can clean up even if a start fails
  state.goPid = await startGo(state)
  writeState(state)
  state.vitePid = await startVite(state)
  writeState(state)
}
