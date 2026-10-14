import { expect, row, signIn, test, uniqueKey } from './fixtures'

test('changes from a second browser window appear live, in both directions', async ({ browser, cluster }) => {
  const tenant = cluster.tenants[1]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('live')

  const ctxA = await browser.newContext()
  const ctxB = await browser.newContext()
  try {
    const a = await ctxA.newPage()
    const b = await ctxB.newPage()
    await signIn(a, cluster.webUrl, tenant.token, 'Tenant')
    await signIn(b, cluster.webUrl, tenant.token, 'Tenant')
    await expect(a.getByTestId('feed-status')).toHaveText('Live')
    await expect(b.getByTestId('feed-status')).toHaveText('Live')

    // Create in A -> appears in B without a reload.
    await a.getByRole('button', { name: 'New key' }).click()
    await a.getByRole('dialog').getByLabel('Key').fill(key)
    await a.getByRole('dialog').getByLabel('Value (JSON)').fill('"from A"')
    await a.getByRole('dialog').getByRole('button', { name: 'Create' }).click()
    await expect(row(b, key)).toContainText('"from A"')

    // Edit in B -> A shows the new value and version.
    await b.getByRole('button', { name: `Edit ${key}` }).click()
    await b.getByRole('dialog').getByLabel('Value (JSON)').fill('"from B"')
    await b.getByRole('dialog').getByRole('button', { name: 'Save' }).click()
    await expect(row(a, key)).toContainText('"from B"')
    await expect(row(a, key)).toContainText('2')

    // Delete in A -> disappears from B.
    await a.getByRole('button', { name: `Delete ${key}` }).click()
    await a.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
    await expect(row(b, key)).toHaveCount(0)
  } finally {
    await ctxA.close()
    await ctxB.close()
  }
})

test('a write made directly through the API appears live', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[1]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('api')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  await expect(page.getByTestId('feed-status')).toHaveText('Live')

  expect((await api.put(tenant.token, key, { n: 1 })).status).toBe(201)
  await expect(row(page, key)).toContainText('{"n":1}')
  expect((await api.put(tenant.token, key, { n: 2 })).status).toBe(200)
  await expect(row(page, key)).toContainText('{"n":2}')
  expect((await api.del(tenant.token, key)).status).toBe(204)
  await expect(row(page, key)).toHaveCount(0)
})

test("another tenant's changes never appear", async ({ page, cluster, api }) => {
  const mine = cluster.tenants[1]
  const theirs = cluster.tenants[2]
  if (mine === undefined || theirs === undefined) throw new Error('need two seeded tenants')
  const theirKey = uniqueKey('theirs')
  const myKey = uniqueKey('mine')
  await signIn(page, cluster.webUrl, mine.token, 'Tenant')
  await expect(page.getByTestId('feed-status')).toHaveText('Live')

  await api.put(theirs.token, theirKey, 'not for you')
  await api.put(mine.token, myKey, 'for you') // written after theirs
  await expect(row(page, myKey)).toBeVisible()
  await expect(row(page, theirKey)).toHaveCount(0)
})
