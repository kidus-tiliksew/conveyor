import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

// A document written before the setting existed carries no issue_publication
// member; it reads as off (req-delivery-and-forge AC-8.1).
const document = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}] },
  execution: {
    spec_approval: true,
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

async function mockAPIs(page: Page, role: string, served: Record<string, unknown> = document) {
  const submitted: Array<{ body: Record<string, unknown>; ifMatch: string | undefined }> = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: `usr_${role}`, role }, request) })
    if (path === '/v1/workspace/config') {
      if (request.method() === 'PUT') {
        const body = request.postDataJSON() as Record<string, unknown>
        submitted.push({ body, ifMatch: request.headers()['if-match'] })
        const sent = body.document as Record<string, unknown>
        const stamped = { ...sent, issue_publication: { enabled: true, enabled_at: '2026-10-29T09:00:00Z' } }
        return route.fulfill({ json: { document: stamped, version: 2, event_id: 77 } })
      }
      return route.fulfill({ json: { document: served, version: 1 } })
    }
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return submitted
}

test('the General switch reads off for an absent setting and saves it with the read version', async ({ page }) => {
  const submitted = await mockAPIs(page, 'operator')
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'General' }).click()

  const publish = page.getByRole('switch', { name: 'Publish GitHub issues for approved plans' })
  await expect(publish).not.toBeChecked()
  await expect(
    page.getByText('Only plans approved after this is turned on get an issue.', { exact: false }),
  ).toBeVisible()
  await publish.click()
  await expect(publish).toBeChecked()
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 77/)).toBeVisible()
  expect(submitted).toHaveLength(1)
  expect(submitted[0].ifMatch).toBe('1')
  expect(submitted[0].body.document).toMatchObject({ issue_publication: { enabled: true } })
  await expect(publish).toBeChecked()
})

test('a member without manage_workspace sees the setting read-only', async ({ page }) => {
  const submitted = await mockAPIs(page, 'viewer', {
    ...document,
    issue_publication: { enabled: true, enabled_at: '2026-10-29T09:00:00Z' },
  })
  await page.goto('/workspace')

  await expect(page.getByText('GitHub issue publication for approved plans:')).toBeVisible()
  await expect(page.getByText('GitHub issue publication for approved plans: on')).toBeVisible()
  await expect(page.getByRole('switch', { name: 'Publish GitHub issues for approved plans' })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: 'General' })).toHaveCount(0)
  expect(submitted).toHaveLength(0)
})
