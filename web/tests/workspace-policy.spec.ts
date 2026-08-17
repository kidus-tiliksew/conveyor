import { expect, test, type Page, type Route } from '@playwright/test'
import { readFileSync } from 'node:fs'

const document = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}, {}] },
  execution: {
    spec_approval: true,
    merge_approval: true,
    require_verification_evidence: false,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

async function mockAPIs(page: Page, reject = false, workers: Record<string, unknown> = {}) {
  let submitted: Record<string, unknown> | undefined
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace/config') {
      if (route.request().method() === 'PUT') {
        submitted = route.request().postDataJSON()
        if (reject)
          return route.fulfill({
            status: 422,
            json: {
              error: 'validation_failed',
              message: 'policy rejected',
              fields: [{ field: 'stage_timeouts.review', message: 'stage_timeouts.review must be positive' }],
            },
          })
        return route.fulfill({ json: { document: submitted!.document, version: 2, event_id: 42 } })
      }
      return route.fulfill({ json: { document, version: 1 } })
    }
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false, ...workers } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return () => submitted
}

test('workspace edits policy only and sends no retired execution detail', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/workspace')

  await expect(page.getByRole('tab', { name: 'Policy' })).toBeVisible()
  await expect(page.getByRole('tab', { name: /Harness|Execution/ })).toHaveCount(0)
  await expect(page.getByText('Pipeline policy')).toBeVisible()
  await expect(page.getByLabel('Review seats')).toHaveValue('2')
  await page.getByLabel('Review seats').fill('3')
  await page.getByLabel('Review stage timeout').fill('90m')
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(submitted()?.document).toMatchObject({
    max_bounces: 3,
    stage_timeouts: { spec: '30m', implement: '4h', review: '90m' },
    review: { seats: [{}, {}, {}] },
  })
  const body = JSON.stringify(submitted())
  expect(body).not.toMatch(/setup|harness|model|argv/)
})

test('workspace surfaces named policy rejections verbatim', async ({ page }) => {
  await mockAPIs(page, true)
  await page.goto('/workspace')
  await page.getByLabel('Review stage timeout').fill('0s')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText('stage_timeouts.review: stage_timeouts.review must be positive')).toBeVisible()
})

test('workspace retains operational worker health and provider limits', async ({ page }) => {
  await mockAPIs(page, false, {
    worker_expected: true,
    worker_available: true,
    workers: [
      {
        id: 'worker-1',
        name: 'Build worker',
        last_seen_at: '2026-08-17T07:00:00Z',
        lease_expires_at: '2026-08-17T08:00:00Z',
        probes: [{ harness: 'codex', healthy: true, checked_at: '2026-08-17T07:00:00Z' }],
      },
    ],
    rate_limits: [
      {
        provider: 'openai',
        rate_limit: { status: 'available', remaining: 80, limit: 100 },
      },
    ],
  })
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Workers' }).click()

  await expect(page.getByText('Worker available')).toBeVisible()
  await expect(page.getByText('Provider rate limits')).toBeVisible()
  await expect(page.getByText('80 of 100 remaining')).toBeVisible()
  await expect(page.getByText('Build worker')).toBeVisible()
})

test('scoped configuration sources contain no retired vocabulary or API paths', () => {
  const workspace = readFileSync('src/pages/workspace.tsx', 'utf8').split('\nfunction WorkersTab(')[0]
  const taskCreate = readFileSync('src/components/task/task-create-sheet.tsx', 'utf8')
  const taskHeader = readFileSync('src/components/task/task-header.tsx', 'utf8')
  const api = readFileSync('src/lib/api.ts', 'utf8')

  expect(`${workspace}\n${taskCreate}\n${taskHeader}`).not.toMatch(/setup|harness|model/i)
  expect(api).not.toMatch(/getHarnessTemplates|changeTaskSetup|setup\?: string/)
})
