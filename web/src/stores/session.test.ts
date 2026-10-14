import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SESSION_STORAGE_KEY, useSessionStore } from './session'

/** A fresh Pinia, as after a page refresh: the store re-reads sessionStorage. */
function freshStore() {
  setActivePinia(createPinia())
  return useSessionStore()
}

describe('session store', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  it('starts logged out', () => {
    const s = freshStore()
    expect(s.isLoggedIn).toBe(false)
    expect(s.token).toBeNull()
    expect(s.role).toBeNull()
  })

  it('keeps the session in sessionStorage so a refresh stays logged in', () => {
    freshStore().login('tok-123', 'admin')
    expect(JSON.parse(sessionStorage.getItem(SESSION_STORAGE_KEY) ?? '')).toEqual({ token: 'tok-123', role: 'admin' })

    const afterRefresh = freshStore()
    expect(afterRefresh.isLoggedIn).toBe(true)
    expect(afterRefresh.token).toBe('tok-123')
    expect(afterRefresh.role).toBe('admin')
  })

  it('logout clears memory and storage', () => {
    const s = freshStore()
    s.login('tok', 'tenant')
    s.logout()
    expect(s.isLoggedIn).toBe(false)
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
    expect(freshStore().isLoggedIn).toBe(false)
  })

  it('ignores damaged or invalid saved data', () => {
    for (const bad of ['not json', '{"token":"","role":"tenant"}', '{"token":"t","role":"root"}', '[]', 'null']) {
      sessionStorage.setItem(SESSION_STORAGE_KEY, bad)
      expect(freshStore().isLoggedIn, bad).toBe(false)
    }
  })

  it('still works when storage throws (for example blocked by the browser)', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    const s = freshStore()
    expect(s.isLoggedIn).toBe(false)
    s.login('tok', 'tenant')
    expect(s.isLoggedIn).toBe(true)
  })
})
