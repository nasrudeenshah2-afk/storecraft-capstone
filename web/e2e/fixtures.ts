import { test as base, expect, type Page } from '@playwright/test'
import { readState, type E2EState } from './cluster'

/** Direct API calls to the Go server (the "other client" in the tests). */
export class Api {
  constructor(private readonly baseUrl: string) {}

  async put(token: string, key: string, value: unknown, headers: Record<string, string> = {}): Promise<Response> {
    return fetch(`${this.baseUrl}/v1/keys/${key.split('/').map(encodeURIComponent).join('/')}`, {
      method: 'PUT',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', ...headers },
      body: JSON.stringify({ value }),
    })
  }

  async del(token: string, key: string): Promise<Response> {
    return fetch(`${this.baseUrl}/v1/keys/${encodeURIComponent(key)}`, {
      method: 'DELETE',
      headers: { Authorization: `Bearer ${token}` },
    })
  }

  async get(token: string, key: string): Promise<{ status: number; value: unknown; version: number }> {
    const res = await fetch(`${this.baseUrl}/v1/keys/${encodeURIComponent(key)}`, {
      headers: { Authorization: `Bearer ${token}` },
    })
    if (res.status !== 200) {
      return { status: res.status, value: null, version: 0 }
    }
    const body = (await res.json()) as { value: unknown; version: number }
    return { status: 200, value: body.value, version: body.version }
  }

  // ---- STORE-210 (quotas, usage, flag) ----

  /** A JSON request that must succeed; fails clearly while a route is missing. */
  private async json<T>(method: string, path: string, token: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', ...headers },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) {
      throw new Error(`STORE-210 API: ${method} ${path} answered ${res.status} ${await res.text()}`)
    }
    return (await res.json()) as T
  }

  async flag(token: string): Promise<{ enabled: boolean; version: number }> {
    return (await this.json<{ quota_enforcement: { enabled: boolean; version: number } }>('GET', '/v1/flags', token)).quota_enforcement
  }

  /** Turns quota_enforcement on or off (admin token; CAS on the current version). */
  async setFlag(adminToken: string, enabled: boolean): Promise<void> {
    const cur = await this.flag(adminToken)
    await this.json('PUT', '/v1/admin/flags/quota_enforcement', adminToken, { enabled }, { 'If-Match': `"${cur.version}"` })
  }

  async quota(adminToken: string, tenantId: number): Promise<{ max_keys: number | null; max_bytes: number | null; version: number }> {
    return this.json('GET', `/v1/admin/tenants/${tenantId}/quota`, adminToken)
  }

  /** Sets a tenant's quota (admin token; CAS on the current version). */
  async setQuota(adminToken: string, tenantId: number, maxKeys: number | null, maxBytes: number | null): Promise<void> {
    const cur = await this.quota(adminToken, tenantId)
    await this.json('PUT', `/v1/admin/tenants/${tenantId}/quota`, adminToken, { max_keys: maxKeys, max_bytes: maxBytes }, { 'If-Match': `"${cur.version}"` })
  }

  async usage(token: string): Promise<{ keys: { used: number; limit: number | null }; bytes: { used: number; limit: number | null } }> {
    return this.json('GET', '/v1/usage', token)
  }
}

/** A key name no other test uses. */
export function uniqueKey(label: string): string {
  return `${label}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e6).toString(36)}`
}

/** Signs in through the login form. */
export async function signIn(page: Page, webUrl: string, token: string, role: 'Tenant' | 'Admin'): Promise<void> {
  await page.goto(webUrl)
  await page.getByLabel('Token').fill(token)
  await page.getByRole('radio', { name: role }).check()
  await page.getByRole('button', { name: 'Sign in' }).click()
  if (role === 'Tenant') {
    await expect(page.getByRole('heading', { name: 'Keys' })).toBeVisible()
  } else {
    await expect(page.getByRole('heading', { name: 'Signed in as admin' })).toBeVisible()
  }
}

/** The row for an exact key name. */
export function row(page: Page, key: string) {
  return page.locator('tbody tr').filter({ has: page.getByRole('rowheader', { name: key, exact: true }) })
}

export const test = base.extend<{ cluster: E2EState; api: Api }>({
  // Playwright fixtures always take the fixtures object first; this one needs none.
  cluster: async ({}, use) => {
    await use(readState())
  },
  api: async ({ cluster }, use) => {
    await use(new Api(cluster.apiUrl))
  },
})

export { expect }
