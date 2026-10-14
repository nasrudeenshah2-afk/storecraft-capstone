import AxeBuilder from '@axe-core/playwright'
import { expect, signIn, test } from './fixtures'

test.describe('sign in', () => {
  test('a tenant signs in and lands on the keys page, which stays after a refresh', async ({ page, cluster }) => {
    const tenant = cluster.tenants[0]
    if (tenant === undefined) throw new Error('no seeded tenant')
    await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
    await expect(page.getByTestId('feed-status')).toHaveText('Live')

    await page.reload()
    await expect(page.getByRole('heading', { name: 'Keys' })).toBeVisible()
    // The token is never part of the address.
    expect(page.url()).not.toContain(tenant.token)
  })

  test('choosing the wrong role explains the mix-up and does not sign in', async ({ page, cluster }) => {
    await page.goto(cluster.webUrl)
    await page.getByLabel('Token').fill(cluster.adminToken)
    await page.getByRole('radio', { name: 'Tenant' }).check()
    await page.getByRole('button', { name: 'Sign in' }).click()
    await expect(page.locator('form').getByRole('alert')).toContainText('This is an admin token')
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  })

  test('an admin sees the replication status badge', async ({ page, cluster }) => {
    await signIn(page, cluster.webUrl, cluster.adminToken, 'Admin')
    const badge = page.getByLabel('Replication status')
    await expect(badge).toContainText('Primary')
    await expect(badge).toContainText('lag 0 (in sync)')
  })
})

test.describe('accessibility (axe)', () => {
  test('the login page has no violations', async ({ page, cluster }) => {
    await page.goto(cluster.webUrl)
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    const results = await new AxeBuilder({ page }).analyze()
    expect(results.violations).toEqual([])
  })

  test('the keys page, with data and with the editor open, has no violations', async ({ page, cluster, api }) => {
    const tenant = cluster.tenants[0]
    if (tenant === undefined) throw new Error('no seeded tenant')
    await api.put(tenant.token, 'a11y-sample', { theme: 'dark' })
    await signIn(page, cluster.webUrl, tenant.token, 'Tenant')
    await expect(page.getByRole('rowheader', { name: 'a11y-sample', exact: true })).toBeVisible()
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])

    await page.getByRole('button', { name: 'Edit a11y-sample' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  })

  test('the admin page has no violations', async ({ page, cluster }) => {
    await signIn(page, cluster.webUrl, cluster.adminToken, 'Admin')
    await expect(page.getByLabel('Replication status')).toContainText('Primary')
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  })
})
