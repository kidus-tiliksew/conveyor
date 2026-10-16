import { expect, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

test('workspace summary shows the automatic recovery bound', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_viewer', role: 'viewer' }, route.request()) })
    if (path === '/v1/workspace') {
      return route.fulfill({
        json: {
          workspace: 'demo',
          max_bounces: 3,
          recovery: { automatic_recoveries: 2, window: '24h' },
          database: 'postgres',
          repos: [],
        },
      })
    }
    return route.fulfill({ json: [] })
  })
  await page.goto('/workspace')
  await expect(page.getByText('2 automatic recoveries / 24h')).toBeVisible()
})

test('workspace summary shows off when automatic recovery is disabled', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_viewer', role: 'viewer' }, route.request()) })
    if (path === '/v1/workspace') {
      return route.fulfill({
        json: {
          workspace: 'demo',
          max_bounces: 3,
          recovery: { automatic_recoveries: 0, window: '24h' },
          database: 'postgres',
          repos: [],
        },
      })
    }
    return route.fulfill({ json: [] })
  })
  await page.goto('/workspace')
  await expect(page.getByText('off / 24h')).toBeVisible()
  await expect(page.getByText('0 automatic recoveries')).toHaveCount(0)
})
