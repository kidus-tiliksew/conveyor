// Visual-capture harness for the policy-only workspace settings page.
import { test, type Page, type Route } from '@playwright/test'

const document = {
  workspace: 'conveyor',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '2h', review: '30m' },
  review: { seats: [{}, {}] },
  execution: {
    spec_approval: true,
    merge_approval: true,
    require_verification_evidence: true,
    implement_concurrency: 5,
    review_concurrency: 2,
    first_activity_timeout: '2m',
  },
  repos: [
    {
      name: 'conveyor',
      url: 'https://github.com/kidus-tiliksew/conveyor',
      github: 'kidus-tiliksew/conveyor',
      base: 'main',
    },
  ],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

async function mockAPIs(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'conveyor')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces')
      return route.fulfill({ json: [{ id: 'conveyor', name: 'Conveyor', config_version: 1 }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document, version: 1 } })
    if (path === '/v1/workers')
      return route.fulfill({
        json: {
          workers: [
            {
              id: 'wrk-1',
              workspace: 'conveyor',
              name: 'kidus-mbp',
              last_seen_at: new Date().toISOString(),
              lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
              created_at: '2026-07-17T00:00:00Z',
              probes: [{ harness: 'codex', healthy: true, checked_at: new Date().toISOString() }],
            },
          ],
          worker_expected: true,
          worker_available: true,
        },
      })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'conveyor', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
}

test.use({ colorScheme: 'dark', viewport: { width: 1440, height: 1000 } })

test('capture workspace policy screenshots', async ({ page }) => {
  await mockAPIs(page)
  await page.goto('/workspace')
  await page.getByText('Pipeline policy').waitFor()
  await page.screenshot({ path: 'test-results/shots/1-policy.png' })

  await page.getByLabel('Review stage timeout').fill('45m')
  await page.screenshot({ path: 'test-results/shots/2-policy-dirty.png' })

  await page.getByRole('tab', { name: 'General' }).click()
  await page.screenshot({ path: 'test-results/shots/3-general.png' })

  await page.getByRole('tab', { name: 'Workers' }).click()
  await page.screenshot({ path: 'test-results/shots/4-workers.png' })
})
