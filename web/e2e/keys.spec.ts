import { expect, row, signIn, test, uniqueKey } from './fixtures'

test('create, edit and delete a key through the console', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[0]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('crud')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')

  // Create.
  await page.getByRole('button', { name: 'New key' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Key').fill(key)
  await dialog.getByLabel('Value (JSON)').fill('{"theme": "dark"}')
  await dialog.getByRole('button', { name: 'Create' }).click()
  await expect(dialog).toBeHidden()
  await expect(row(page, key)).toContainText('{"theme":"dark"}')
  expect(await api.get(tenant.token, key)).toEqual({ status: 200, value: { theme: 'dark' }, version: 1 })

  // Edit.
  await page.getByRole('button', { name: `Edit ${key}` }).click()
  await expect(dialog).toContainText('Editing version 1')
  await dialog.getByLabel('Value (JSON)').fill('"light"')
  await dialog.getByRole('button', { name: 'Save' }).click()
  await expect(dialog).toBeHidden()
  await expect(row(page, key)).toContainText('"light"')
  await expect(row(page, key)).toContainText('2')
  expect((await api.get(tenant.token, key)).version).toBe(2)

  // Delete (with confirmation).
  await page.getByRole('button', { name: `Delete ${key}` }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
  await expect(row(page, key)).toHaveCount(0)
  expect((await api.get(tenant.token, key)).status).toBe(404)
})

test('invalid JSON is caught in the form and nothing is sent', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[0]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('badjson')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  await page.getByRole('button', { name: 'New key' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Key').fill(key)
  await dialog.getByLabel('Value (JSON)').fill('dark')
  await dialog.getByRole('button', { name: 'Create' }).click()
  await expect(dialog.getByLabel('Value (JSON)')).toHaveAttribute('aria-invalid', 'true')
  expect((await api.get(tenant.token, key)).status).toBe(404)
})
