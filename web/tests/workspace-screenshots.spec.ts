// Visual-capture harness for the policy-only workspace settings page.
import { type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

const document = {
  workspace: 'conveyor',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '2h', review: '30m' },
  review: { seats: [{}, {}] },
  execution: {
    default_plan_state: 'needed',
    merge_approval: true,
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
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document, version: 1 } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'conveyor', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
}

test.use({ colorScheme: 'dark', viewport: { width: 1440, height: 1000 } })

test('capture workspace policy screenshots', async ({ page }) => {
  await mockAPIs(page)
  await page.goto('/w/conveyor/workspace')
  await page.getByText('Pipeline policy').waitFor()
  await page.screenshot({ path: 'test-results/shots/1-policy.png' })

  await page.getByLabel('Review stage timeout').fill('45m')
  await page.screenshot({ path: 'test-results/shots/2-policy-dirty.png' })

  await page.getByRole('tab', { name: 'General' }).click()
  await page.screenshot({ path: 'test-results/shots/3-general.png' })
})
