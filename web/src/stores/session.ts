import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

export type Role = 'tenant' | 'admin'

/** sessionStorage key. sessionStorage survives a page refresh but not closing the tab. */
export const SESSION_STORAGE_KEY = 'storecraft.session'

interface SavedSession {
  token: string
  role: Role
}

function isSavedSession(v: unknown): v is SavedSession {
  if (typeof v !== 'object' || v === null) {
    return false
  }
  const s = v as Record<string, unknown>
  return typeof s.token === 'string' && s.token !== '' && (s.role === 'tenant' || s.role === 'admin')
}

function readSaved(): SavedSession | null {
  try {
    const raw = sessionStorage.getItem(SESSION_STORAGE_KEY)
    if (raw === null) {
      return null
    }
    const parsed: unknown = JSON.parse(raw)
    return isSavedSession(parsed) ? parsed : null
  } catch {
    return null // storage blocked, or damaged contents: start logged out
  }
}

function writeSaved(s: SavedSession | null): void {
  try {
    if (s === null) {
      sessionStorage.removeItem(SESSION_STORAGE_KEY)
    } else {
      sessionStorage.setItem(SESSION_STORAGE_KEY, JSON.stringify(s))
    }
  } catch {
    // Storage unavailable: the session still works until the page is reloaded.
  }
}

/** The signed-in token and role. The token is never put in a URL. */
export const useSessionStore = defineStore('session', () => {
  const saved = readSaved()
  const token = ref<string | null>(saved?.token ?? null)
  const role = ref<Role | null>(saved?.role ?? null)

  const isLoggedIn = computed(() => token.value !== null && role.value !== null)

  function login(newToken: string, newRole: Role): void {
    token.value = newToken
    role.value = newRole
    writeSaved({ token: newToken, role: newRole })
  }

  function logout(): void {
    token.value = null
    role.value = null
    writeSaved(null)
  }

  return { token, role, isLoggedIn, login, logout }
})
