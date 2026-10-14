import { rmSync } from 'node:fs'
import { readState, stopPid, waitPortFree } from './cluster'

/** After all tests: stop exactly the processes global setup started, then delete the temp folder. */
export default async function globalTeardown(): Promise<void> {
  const state = readState()
  for (const pid of [state.vitePid, state.goPid]) {
    if (pid > 0) {
      stopPid(pid)
    }
  }
  await waitPortFree(state.goPort)
  await waitPortFree(state.webPort)
  // The database file can stay locked for a moment after the process exits on Windows.
  for (let attempt = 0; attempt < 20; attempt++) {
    try {
      rmSync(state.tmpDir, { recursive: true, force: true })
      return
    } catch {
      await new Promise((r) => setTimeout(r, 250))
    }
  }
  process.stdout.write(`note: could not delete ${state.tmpDir}; remove it by hand\n`)
}
