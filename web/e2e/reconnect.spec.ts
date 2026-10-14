import { restartGo } from './cluster'
import { expect, row, signIn, test, uniqueKey } from './fixtures'

test('after the server restarts, the console reconnects and keeps receiving changes', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[2]
  if (tenant === undefined) throw new Error('no seeded tenant')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  const status = page.getByTestId('feed-status')
  await expect(status).toHaveText('Live')

  const before = uniqueKey('before')
  await api.put(tenant.token, before, 1)
  await expect(row(page, before)).toBeVisible()

  // Drop the stream by restarting the server (same port, same database).
  const restarted = restartGo()
  try {
    await expect(status).toHaveText('Reconnecting…')
  } finally {
    await restarted // never leave the server down for the next test
  }
  await expect(status).toHaveText('Live', { timeout: 20_000 })

  const after = uniqueKey('after')
  await api.put(tenant.token, after, 2)
  await expect(row(page, after)).toBeVisible()
  await expect(row(page, before)).toBeVisible() // nothing lost
})

test('when streaming is impossible, the console falls back to polling and stays up to date', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[2]
  if (tenant === undefined) throw new Error('no seeded tenant')
  // Every attempt to open the event stream fails (as behind a proxy that blocks it).
  await page.route('**/v1/changes/stream', (route) => route.abort())
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  await expect(page.getByTestId('feed-status')).toHaveText('Live (polling)', { timeout: 20_000 })

  const key = uniqueKey('polled')
  await api.put(tenant.token, key, 'via long-poll')
  await expect(row(page, key)).toContainText('"via long-poll"')
})
