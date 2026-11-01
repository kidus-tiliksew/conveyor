import { readFileSync } from 'node:fs'
import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

const document = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}, {}] },
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

async function mockAPIs(page: Page, reject = false) {
  let submitted: Record<string, unknown> | undefined
  const workerRequests: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
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
    // The server serves no worker routes (DEC-97(1)); a read here is a regression.
    if (path.startsWith('/v1/workers')) {
      workerRequests.push(path)
      return route.fulfill({ status: 404, json: { error: 'not_found' } })
    }
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return Object.assign(() => submitted, { workerRequests: () => workerRequests })
}

test('workspace edits policy only and sends no retired execution detail', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')

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

// case: FF-001
test('fail-fast review is off by default and survives a seat change when enabled', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')

  const failFast = page.getByRole('switch', { name: 'Fail-fast review' })
  await expect(failFast).not.toBeChecked()
  await failFast.click()
  await page.getByLabel('Review seats').fill('3')
  await expect(failFast).toBeChecked()
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(submitted()?.document).toMatchObject({ review: { seats: [{}, {}, {}], fail_fast: true } })
})

test('workspace surfaces named policy rejections verbatim', async ({ page }) => {
  await mockAPIs(page, true)
  await page.goto('/w/demo/workspace')
  await page.getByLabel('Review stage timeout').fill('0s')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText('stage_timeouts.review: stage_timeouts.review must be positive')).toBeVisible()
})

test('workspace shows no worker surface', async ({ page }) => {
  const mocks = await mockAPIs(page)
  // A link saved while the retired Workers tab was open names no tab: tab
  // selection is page state. A stray tab parameter changes nothing.
  for (const address of ['/w/demo/workspace', '/w/demo/workspace?tab=workers']) {
    await page.goto(address)
    await expect(page.getByText('Pipeline policy')).toBeVisible()
    await expect(page.getByRole('tab')).toHaveText(['General', 'Policy', 'Verification', 'Members'])
    await expect(page.getByRole('tab', { name: 'Workers' })).toHaveCount(0)
    for (const tab of ['General', 'Policy', 'Verification', 'Members']) {
      await page.getByRole('tab', { name: tab }).click()
      await expect(page.getByRole('tab', { name: tab })).toHaveAttribute('aria-selected', 'true')
      await expect(page.getByText(/worker|pairing|pull claiming|rate limit/i)).toHaveCount(0)
      await expect(page.getByRole('button', { name: 'Issue pairing token' })).toHaveCount(0)
    }
  }
  expect(mocks.workerRequests()).toEqual([])
})

test('scoped configuration sources contain no retired vocabulary or API paths', () => {
  const workspace = readFileSync('src/pages/workspace.tsx', 'utf8')
  const taskCreate = readFileSync('src/components/task/task-create-sheet.tsx', 'utf8')
  const taskHeader = readFileSync('src/components/task/task-header.tsx', 'utf8')
  const api = readFileSync('src/lib/api.ts', 'utf8')

  expect(`${workspace}\n${taskCreate}\n${taskHeader}`).not.toMatch(/setup|harness|model/i)
  expect(api).not.toMatch(/getHarnessTemplates|changeTaskSetup|setup\?: string/)
})
