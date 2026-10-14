import type { Page } from '@playwright/test'
import { expect, row, signIn, test, uniqueKey } from './fixtures'

/** Opens the editor for key, types text, lets another client change the key, then saves. */
async function editAndCollide(page: Page, key: string, myText: string, collide: () => Promise<unknown>) {
  await page.getByRole('button', { name: `Edit ${key}` }).click()
  const editor = page.getByRole('dialog')
  await editor.getByLabel('Value (JSON)').fill(myText)
  await collide()
  await editor.getByRole('button', { name: 'Save' }).click()
  const conflict = page.getByRole('dialog', { name: /Conflict/ })
  await expect(conflict).toBeVisible()
  return conflict
}

test('a conflicting save shows both values; Overwrite saves mine on top of the latest', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[1]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('conflict')
  await api.put(tenant.token, key, 'original')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  await expect(row(page, key)).toBeVisible()

  const conflict = await editAndCollide(page, key, '"mine"', () => api.put(tenant.token, key, 'theirs'))
  await expect(conflict).toContainText('Your value')
  await expect(conflict).toContainText('"mine"')
  await expect(conflict).toContainText('Latest value (version 2)')
  await expect(conflict).toContainText('"theirs"')

  await conflict.getByRole('button', { name: 'Overwrite with my value' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(await api.get(tenant.token, key)).toEqual({ status: 200, value: 'mine', version: 3 })
  await expect(row(page, key)).toContainText('"mine"')
})

test('Reload continues editing from the latest value; Cancel keeps my text', async ({ page, cluster, api }) => {
  const tenant = cluster.tenants[1]
  if (tenant === undefined) throw new Error('no seeded tenant')
  const key = uniqueKey('reload')
  await api.put(tenant.token, key, 'original')
  await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
  await expect(row(page, key)).toBeVisible()

  let conflict = await editAndCollide(page, key, '"mine"', () => api.put(tenant.token, key, 'theirs'))
  await conflict.getByRole('button', { name: 'Cancel' }).click()
  const editor = page.getByRole('dialog')
  await expect(editor.getByLabel('Value (JSON)')).toHaveValue('"mine"')

  await editor.getByRole('button', { name: 'Save' }).click()
  conflict = page.getByRole('dialog', { name: /Conflict/ })
  await conflict.getByRole('button', { name: 'Reload latest' }).click()
  await expect(page.getByRole('dialog')).toContainText('Editing version 2')
  await expect(page.getByRole('dialog').getByLabel('Value (JSON)')).toHaveValue('"theirs"')
  expect((await api.get(tenant.token, key)).value).toBe('theirs') // nothing was overwritten
})
