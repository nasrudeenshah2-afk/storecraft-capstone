import AxeBuilder from '@axe-core/playwright'
import type { Page, Route } from '@playwright/test'
import { expect, row, signIn, test, uniqueKey } from './fixtures'

// Acceptance tests for the STORE-210 console criteria (docs/capstone/spec.md
// section 7, AC-UI-1 .. AC-UI-8). Written in step B5 BEFORE the console
// feature exists, against the real Go server (stubs only where a state cannot
// be produced otherwise: AC-UI-1 loading/error and fixed numbers).
//
// Seeded tenants are demo-1..demo-3 with ids 1..3 (cluster.tenants[i] has id
// i + 1). Every test leaves the flag off and the default quotas in place.

const DEFAULT_KEYS = 1000
const DEFAULT_BYTES = 1048576

test.afterEach(async ({ api, cluster }) => {
  await api.setFlag(cluster.adminToken, false)
  for (const id of [1, 2, 3]) {
    await api.setQuota(cluster.adminToken, id, DEFAULT_KEYS, DEFAULT_BYTES)
  }
})

function tenant(cluster: { tenants: { token: string }[] }, index: number): { token: string; id: number } {
  const t = cluster.tenants[index]
  if (t === undefined) throw new Error('no seeded tenant')
  return { token: t.token, id: index + 1 }
}

function usageRegion(page: Page) {
  return page.getByRole('region', { name: 'Storage usage' })
}

/** Stubs GET /v1/flags (enforcement on) and GET /v1/usage with fixed numbers. */
async function stubUsage(page: Page, keys: [number, number | null], bytes: [number, number | null]): Promise<void> {
  await page.route('**/v1/flags', (route) => route.fulfill({ json: { quota_enforcement: { enabled: true, version: 1 } } }))
  await page.route('**/v1/usage', (route) =>
    route.fulfill({ json: { tenant_id: 1, keys: { used: keys[0], limit: keys[1] }, bytes: { used: bytes[0], limit: bytes[1] }, seq: 1 } }),
  )
}

async function axeClean(page: Page): Promise<void> {
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
}

test.describe('AC-UI-1 usage meter states', () => {
  test('normal: text and an accessible meter for keys and bytes', async ({ page, cluster }) => {
    await stubUsage(page, [12, 1000], [3482, 1048576])
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    const region = usageRegion(page)
    await expect(region).toContainText('12 of 1,000 keys')
    await expect(region).toContainText('3.4 KB of 1 MB')
    const keys = region.getByRole('meter', { name: 'Keys used' })
    await expect(keys).toHaveAttribute('aria-valuenow', '12')
    await expect(keys).toHaveAttribute('aria-valuemax', '1000')
    await expect(region.getByRole('meter', { name: 'Bytes used' })).toHaveAttribute('aria-valuemax', '1048576')
    await expect(region).not.toContainText('Almost full')
    await expect(region).not.toContainText('Over quota')
  })

  test('loading, then error with Try again, then the real numbers', async ({ page, cluster }) => {
    await page.route('**/v1/flags', (route) => route.fulfill({ json: { quota_enforcement: { enabled: true, version: 1 } } }))
    let release: () => void = () => undefined
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    await page.route('**/v1/usage', async (route: Route) => {
      await held
      await route.fulfill({ status: 500, json: { error: { code: 'internal', message: 'internal error' } } })
    })
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    await expect(usageRegion(page)).toContainText('Loading usage…')
    release()
    const retry = usageRegion(page).getByRole('button', { name: 'Try again' })
    await expect(retry).toBeVisible()
    await expect(usageRegion(page)).toContainText('Could not load usage')

    await page.unroute('**/v1/usage')
    await page.route('**/v1/usage', (route) =>
      route.fulfill({ json: { tenant_id: 1, keys: { used: 5, limit: 1000 }, bytes: { used: 40, limit: 1048576 }, seq: 1 } }),
    )
    await retry.click()
    await expect(usageRegion(page)).toContainText('5 of 1,000 keys')
  })

  test('unlimited: the word "unlimited" and no meter value', async ({ page, cluster }) => {
    await stubUsage(page, [3, null], [10, null])
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    const region = usageRegion(page)
    await expect(region).toContainText('3 keys (unlimited)')
    await expect(region).toContainText('10 bytes (unlimited)')
    await expect(region.getByRole('meter')).toHaveCount(0)
  })

  test('near limit at 80% or more: "Almost full" in text', async ({ page, cluster }) => {
    await stubUsage(page, [850, 1000], [10, 1048576])
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    await expect(usageRegion(page)).toContainText('850 of 1,000 keys')
    await expect(usageRegion(page)).toContainText('Almost full')
    await axeClean(page)
  })

  test('over quota: "Over quota" in text', async ({ page, cluster }) => {
    await stubUsage(page, [1200, 1000], [2097152, 1048576])
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    await expect(usageRegion(page)).toContainText('Over quota')
    await expect(usageRegion(page)).toContainText('2 MB of 1 MB')
    await axeClean(page)
  })
})

test('AC-UI-2 the meter follows other clients within 2 seconds, without a reload', async ({ page, cluster, api }) => {
  const t = tenant(cluster, 0)
  await api.setFlag(cluster.adminToken, true)
  await signIn(page, cluster.webUrl, t.token, 'Tenant')
  await expect(page.getByTestId('feed-status')).toHaveText('Live')
  const before = await api.usage(t.token)
  const keysText = (n: number) => `${n.toLocaleString('en-US')} of 1,000 keys`
  await expect(usageRegion(page)).toContainText(keysText(before.keys.used))

  const key = uniqueKey('meter')
  expect((await api.put(t.token, key, 'x')).status).toBe(201)
  await expect(usageRegion(page)).toContainText(keysText(before.keys.used + 1), { timeout: 2000 })
  const grown = await api.usage(t.token)
  expect((await api.put(t.token, key, 'a much longer value than before')).status).toBe(200)
  const after = await api.usage(t.token)
  expect(after.bytes.used).toBeGreaterThan(grown.bytes.used)
  await expect(usageRegion(page).getByRole('meter', { name: 'Bytes used' })).toHaveAttribute('aria-valuenow', String(after.bytes.used), { timeout: 2000 })
  expect((await api.del(t.token, key)).status).toBe(204)
  await expect(usageRegion(page)).toContainText(keysText(before.keys.used), { timeout: 2000 })
})

test.describe('AC-UI-3 over quota at save', () => {
  test('the editor stays open with my text and explains the limit from the error details', async ({ page, cluster, api }) => {
    const t = tenant(cluster, 0)
    await api.setFlag(cluster.adminToken, true)
    const used = (await api.usage(t.token)).keys.used
    await api.setQuota(cluster.adminToken, t.id, used, null)
    await signIn(page, cluster.webUrl, t.token, 'Tenant')

    await page.getByRole('button', { name: 'New key' }).click()
    const editor = page.getByRole('dialog')
    const key = uniqueKey('over')
    await editor.getByLabel('Key').fill(key)
    await editor.getByLabel('Value (JSON)').fill('{"keep": "my text"}')
    await editor.getByRole('button', { name: 'Create' }).click()

    const alert = editor.getByRole('alert')
    await expect(alert).toContainText('key limit')
    await expect(alert).toContainText(`${used.toLocaleString('en-US')} of ${used.toLocaleString('en-US')} keys`)
    await expect(alert).toContainText('Delete keys you no longer need, or ask an admin to raise the quota')
    await expect(editor.getByLabel('Key')).toHaveValue(key)
    await expect(editor.getByLabel('Value (JSON)')).toHaveValue('{"keep": "my text"}')
    await axeClean(page)
    await editor.getByRole('button', { name: 'Cancel' }).click()
    await expect(row(page, key)).toHaveCount(0)
  })

  test('the message comes from the error code, not the server text; forbidden reads differently', async ({ page, cluster, api }) => {
    const t = tenant(cluster, 0)
    await api.setFlag(cluster.adminToken, true)
    await signIn(page, cluster.webUrl, t.token, 'Tenant')
    let reply = { code: 'quota_exceeded', message: 'SERVER TEXT THAT MUST NOT BE SHOWN', details: { dimension: 'bytes', used: 1047000, limit: 1048576, requested: 2048 } as object | undefined }
    await page.route('**/v1/keys/stubbed-*', (route) =>
      route.request().method() === 'PUT' ? route.fulfill({ status: 403, json: { error: reply } }) : route.fallback(),
    )
    await page.getByRole('button', { name: 'New key' }).click()
    const editor = page.getByRole('dialog')
    await editor.getByLabel('Key').fill('stubbed-1')
    await editor.getByLabel('Value (JSON)').fill('"v"')
    await editor.getByRole('button', { name: 'Create' }).click()
    const alert = editor.getByRole('alert')
    await expect(alert).toContainText('byte limit')
    await expect(alert).toContainText('1,047,000 of 1,048,576 bytes')
    await expect(alert).not.toContainText('SERVER TEXT')
    const quotaText = await alert.innerText()

    reply = { code: 'forbidden', message: 'SERVER TEXT THAT MUST NOT BE SHOWN', details: undefined }
    await editor.getByRole('button', { name: 'Create' }).click()
    await expect(editor.getByRole('alert')).not.toHaveText(quotaText)
    await expect(editor.getByRole('alert')).not.toContainText('Delete keys you no longer need')
  })
})

async function openQuotaEditor(page: Page, adminToken: string, webUrl: string, tenantName: string) {
  await signIn(page, webUrl, adminToken, 'Admin')
  const section = page.getByRole('region', { name: 'Tenant quotas' })
  await section.getByLabel('Tenant').selectOption({ label: tenantName })
  return section
}

test.describe('AC-UI-4 admin quota editor', () => {
  test('shows quota and usage, rejects invalid input without sending, saves with If-Match', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, true)
    const before = await api.quota(cluster.adminToken, 2)
    const section = await openQuotaEditor(page, cluster.adminToken, cluster.webUrl, 'demo-2')
    await expect(section.getByLabel('Max keys')).toHaveValue(String(before.max_keys))
    await expect(section.getByLabel('Max bytes')).toHaveValue(String(before.max_bytes))
    await expect(section).toContainText(`Version ${before.version}`)
    await expect(section).toContainText('Usage:')

    const puts: string[] = []
    page.on('request', (req) => {
      if (req.method() === 'PUT' && req.url().includes('/quota')) puts.push(req.headers()['if-match'] ?? '')
    })
    for (const bad of ['-1', '1.5', 'ten']) {
      await section.getByLabel('Max keys').fill(bad)
      await section.getByRole('button', { name: 'Save quota' }).click()
      await expect(section.getByText('Enter a whole number of 0 or more, or leave empty for unlimited.')).toBeVisible()
    }
    expect(puts).toEqual([])

    await section.getByLabel('Max keys').fill('250')
    await section.getByLabel('Max bytes').fill('') // empty = unlimited
    await section.getByRole('button', { name: 'Save quota' }).click()
    await expect(section).toContainText(`Version ${before.version + 1}`)
    expect(puts).toEqual([`"${before.version}"`])
    expect(await api.quota(cluster.adminToken, 2)).toEqual({ max_keys: 250, max_bytes: null, version: before.version + 1 })
    await expect(section.getByLabel('Max bytes')).toHaveValue('')
  })
})

test.describe('AC-UI-5 quota editor conflict', () => {
  async function collide(page: Page, cluster: { adminToken: string; webUrl: string }, api: { setQuota: (a: string, id: number, k: number | null, b: number | null) => Promise<void> }) {
    const section = await openQuotaEditor(page, cluster.adminToken, cluster.webUrl, 'demo-3')
    await section.getByLabel('Max keys').fill('111')
    await api.setQuota(cluster.adminToken, 3, 222, 2222) // another admin saves first
    await section.getByRole('button', { name: 'Save quota' }).click()
    const dialog = page.getByRole('dialog', { name: /Conflict/ })
    await expect(dialog).toBeVisible()
    return { section, dialog }
  }

  test('shows both sides; Cancel keeps my input and returns focus to Save; Esc cancels', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, true)
    const { section, dialog } = await collide(page, cluster, api)
    const latest = await api.quota(cluster.adminToken, 3)
    await expect(dialog).toContainText('Your values')
    await expect(dialog).toContainText(`Latest values (version ${latest.version})`)
    await expect(dialog).toContainText('111')
    await expect(dialog).toContainText('222')
    await axeClean(page)

    // Tab stays inside the dialog.
    for (let i = 0; i < 6; i++) {
      await page.keyboard.press('Tab')
      expect(await dialog.evaluate((el) => el.contains(document.activeElement))).toBe(true)
    }
    await page.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(section.getByLabel('Max keys')).toHaveValue('111')
    await expect(section.getByRole('button', { name: 'Save quota' })).toBeFocused()

    await section.getByRole('button', { name: 'Save quota' }).click()
    await page.getByRole('dialog', { name: /Conflict/ }).getByRole('button', { name: 'Cancel' }).click()
    await expect(section.getByLabel('Max keys')).toHaveValue('111')
    expect((await api.quota(cluster.adminToken, 3)).max_keys).toBe(222)
  })

  test('Reload loads the latest values; Overwrite saves mine on top of the latest version', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, true)
    const collided = await collide(page, cluster, api)
    const section = collided.section
    let dialog = collided.dialog
    await dialog.getByRole('button', { name: /Reload/ }).click()
    await expect(section.getByLabel('Max keys')).toHaveValue('222')
    await expect(section.getByLabel('Max bytes')).toHaveValue('2222')

    // A second collision on the same page: Overwrite this time.
    await section.getByLabel('Max keys').fill('111')
    await api.setQuota(cluster.adminToken, 3, 333, 3333)
    await section.getByRole('button', { name: 'Save quota' }).click()
    dialog = page.getByRole('dialog', { name: /Conflict/ })
    await expect(dialog).toContainText('333')
    const latest = await api.quota(cluster.adminToken, 3)
    await dialog.getByRole('button', { name: /Overwrite/ }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(await api.quota(cluster.adminToken, 3)).toMatchObject({ max_keys: 111, version: latest.version + 1 })
    await expect(section).toContainText(`Version ${latest.version + 1}`)
  })
})

test('AC-UI-6 the flag hides and shows the quota feature live in every open console', async ({ browser, cluster, api }) => {
  const t = tenant(cluster, 0)
  await api.setFlag(cluster.adminToken, false)
  const tenantCtx = await browser.newContext()
  const adminCtx = await browser.newContext()
  try {
    const tp = await tenantCtx.newPage()
    const ap = await adminCtx.newPage()
    await signIn(tp, cluster.webUrl, t.token, 'Tenant')
    await signIn(ap, cluster.webUrl, cluster.adminToken, 'Admin')
    await expect(tp.getByRole('heading', { name: 'Keys' })).toBeVisible()
    await expect(usageRegion(tp)).toHaveCount(0)
    await expect(ap.getByRole('region', { name: 'Tenant quotas' })).toHaveCount(0)
    await expect(ap.getByRole('region', { name: 'Quota enforcement' })).toBeVisible() // the toggle stays

    await api.setFlag(cluster.adminToken, true)
    await expect(usageRegion(tp)).toBeVisible({ timeout: 5000 })
    await expect(ap.getByRole('region', { name: 'Tenant quotas' })).toBeVisible({ timeout: 5000 })

    await api.setFlag(cluster.adminToken, false)
    await expect(usageRegion(tp)).toHaveCount(0, { timeout: 5000 })
    await expect(ap.getByRole('region', { name: 'Tenant quotas' })).toHaveCount(0, { timeout: 5000 })
  } finally {
    await tenantCtx.close()
    await adminCtx.close()
  }
})

test.describe('AC-UI-7 admin flag toggle', () => {
  test('confirms with the effect, sends If-Match, and shows the new state', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, false)
    const before = await api.flag(cluster.adminToken)
    await signIn(page, cluster.webUrl, cluster.adminToken, 'Admin')
    const section = page.getByRole('region', { name: 'Quota enforcement' })
    await expect(section).toContainText('Quota enforcement is off')
    await section.getByRole('button', { name: 'Turn on' }).click()
    const confirm = page.getByRole('dialog', { name: 'Turn on quota enforcement?' })
    await expect(confirm).toContainText('Writes over quota will be rejected')
    const put = page.waitForRequest((r) => r.method() === 'PUT' && r.url().endsWith('/v1/admin/flags/quota_enforcement'))
    await confirm.getByRole('button', { name: 'Turn on' }).click()
    expect((await put).headers()['if-match']).toBe(`"${before.version}"`)
    await expect(section).toContainText('Quota enforcement is on')
    expect((await api.flag(cluster.adminToken)).enabled).toBe(true)
  })

  test('a stale toggle (412) asks to reload the current state', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, false)
    await signIn(page, cluster.webUrl, cluster.adminToken, 'Admin')
    const section = page.getByRole('region', { name: 'Quota enforcement' })
    await expect(section).toContainText('Quota enforcement is off')
    // The admin asks to turn it on; while the confirmation is open, someone
    // else turns it on and off again (same state, newer version). The console
    // sends the version the admin saw when asking, so the save must be a 412.
    await section.getByRole('button', { name: 'Turn on' }).click()
    const confirm = page.getByRole('dialog', { name: 'Turn on quota enforcement?' })
    await expect(confirm).toBeVisible()
    await api.setFlag(cluster.adminToken, true)
    await api.setFlag(cluster.adminToken, false)
    await confirm.getByRole('button', { name: 'Turn on' }).click()
    await expect(section.getByRole('alert')).toContainText('Someone else changed this setting. Reload to see the current state.')
    await section.getByRole('button', { name: 'Reload' }).click()
    await expect(section).toContainText('Quota enforcement is off')
    expect((await api.flag(cluster.adminToken)).enabled).toBe(false)
  })
})

test.describe('AC-UI-8 keyboard and accessibility', () => {
  test('the quota editor works with the keyboard alone; the admin page is axe-clean', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, true)
    const section = await openQuotaEditor(page, cluster.adminToken, cluster.webUrl, 'demo-2')
    await axeClean(page)
    await section.getByLabel('Max keys').focus()
    await page.keyboard.press('Control+A')
    await page.keyboard.type('321')
    await page.keyboard.press('Tab') // Max bytes
    await page.keyboard.press('Tab') // Save quota
    await expect(section.getByRole('button', { name: 'Save quota' })).toBeFocused()
    await page.keyboard.press('Enter')
    await expect(section.locator('[aria-live]')).toContainText('Quota saved')
    expect((await api.quota(cluster.adminToken, 2)).max_keys).toBe(321)
  })

  test('the keys page is axe-clean with the meter shown', async ({ page, cluster, api }) => {
    await api.setFlag(cluster.adminToken, true)
    await signIn(page, cluster.webUrl, tenant(cluster, 0).token, 'Tenant')
    await expect(usageRegion(page).getByRole('meter', { name: 'Keys used' })).toBeVisible()
    await axeClean(page)
  })
})
