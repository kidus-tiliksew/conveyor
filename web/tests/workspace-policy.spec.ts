import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

const document = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}, {}] },
  execution: {
    default_plan_state: 'needed' as 'needed' | 'not_required',
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

async function mockAPIs(page: Page, reject = false, served: typeof document = document) {
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
      return route.fulfill({ json: { document: served, version: 1 } })
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

// req-review-gates-evidence AC-2.9: off by default and sent only once turned on.
test('distinct seat models is off by default and submits when enabled', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')

  const distinct = page.getByRole('switch', { name: 'Distinct seat models' })
  await expect(distinct).not.toBeChecked()
  await distinct.click()
  await page.getByLabel('Review seats').fill('3')
  await expect(distinct).toBeChecked()
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(submitted()?.document).toMatchObject({ review: { seats: [{}, {}, {}], distinct_models: true } })
})

// component-web-dashboard invariant 33; req-task-plan-state AC-2.1, AC-3.5;
// DEC-125(7): the Default plan state select shows the served value, save
// writes execution.default_plan_state and never execution.spec_approval, and
// no plan approval switch remains.
test('the default plan state select shows the served value and saves only default_plan_state', async ({ page }) => {
  const submitted = await mockAPIs(page, false, {
    ...document,
    execution: { ...document.execution, default_plan_state: 'not_required' },
  })
  await page.goto('/w/demo/workspace')

  await expect(page.getByRole('switch', { name: 'Plan approval' })).toHaveCount(0)
  await expect(page.getByText('Pause for plan approval')).toHaveCount(0)
  const select = page.getByRole('combobox', { name: 'Default plan state' })
  await expect(select).toHaveValue('not_required')
  await expect(select.locator('option')).toHaveText(['Planning needed', 'No plan required'])
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)

  await select.selectOption('needed')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  const execution = (submitted()?.document as { execution: Record<string, unknown> } | undefined)?.execution ?? {}
  expect(execution.default_plan_state).toBe('needed')
  expect(execution).not.toHaveProperty('spec_approval')
  expect(JSON.stringify(submitted())).not.toContain('spec_approval')
})

test('a save after an unrelated edit keeps the served default plan state and sends no spec_approval', async ({
  page,
}) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')
  await expect(page.getByRole('combobox', { name: 'Default plan state' })).toHaveValue('needed')
  await page.getByLabel('Review seats').fill('3')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  const execution = (submitted()?.document as { execution: Record<string, unknown> } | undefined)?.execution ?? {}
  expect(execution.default_plan_state).toBe('needed')
  expect(execution).not.toHaveProperty('spec_approval')
})

test('workspace surfaces named policy rejections verbatim', async ({ page }) => {
  await mockAPIs(page, true)
  await page.goto('/w/demo/workspace')
  await page.getByLabel('Review stage timeout').fill('0s')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText('stage_timeouts.review: stage_timeouts.review must be positive')).toBeVisible()
})

// A stateful mock for the review scope settings: a save replaces the served
// document, so a reload reads what was saved, and a listed refusal answers 422.
async function mockReviewScopeAPIs(
  page: Page,
  served: Record<string, unknown>,
  refusal?: { field: string; message: string },
) {
  const saved: Array<Record<string, unknown>> = []
  let current = served
  let version = 1
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
        const body = route.request().postDataJSON() as { document: Record<string, unknown> }
        saved.push(body.document)
        if (refusal) return route.fulfill({ status: 422, json: { error: 'validation_failed', fields: [refusal] } })
        current = body.document
        version++
        return route.fulfill({ json: { document: current, version, event_id: 42 } })
      }
      return route.fulfill({ json: { document: current, version } })
    }
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return () => saved
}

test('changed review scope settings save and read back after a reload', async ({ page }) => {
  const saved = await mockReviewScopeAPIs(page, document)
  await page.goto('/w/demo/workspace')

  await page.getByRole('combobox', { name: 'Later review rounds' }).selectOption('delta')
  await page.getByRole('combobox', { name: 'Refresh review' }).selectOption('full')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(saved()).toHaveLength(1)
  expect(saved()[0]).toMatchObject({ later_round_review: 'delta', refresh_review: 'full' })

  await page.reload()
  await expect(page.getByRole('combobox', { name: 'Later review rounds' })).toHaveValue('delta')
  await expect(page.getByRole('combobox', { name: 'Refresh review' })).toHaveValue('full')
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)
})

// req-delegated-execution AC-6.4; component-web-dashboard invariant 33: the
// Policy tab shows the served progress report interval, says that 0 turns it
// off, and saves the text under execution.progress_report_interval.
const servedInterval = { ...document, execution: { ...document.execution, progress_report_interval: '10m' } }

test('the progress report interval shows the served value and saves under its key', async ({ page }) => {
  const saved = await mockReviewScopeAPIs(page, servedInterval)
  await page.goto('/w/demo/workspace')

  const interval = page.getByRole('textbox', { name: 'Progress report interval' })
  await expect(interval).toHaveValue('10m')
  await expect(interval).toHaveAccessibleDescription(/Use 0 to turn the interval off\./)
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)

  await interval.fill('5m')
  await expect(page.getByText('Unsaved changes', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(saved().at(-1)).toMatchObject({
    execution: { ...document.execution, progress_report_interval: '5m' },
  })

  await page.reload()
  await expect(page.getByRole('textbox', { name: 'Progress report interval' })).toHaveValue('5m')

  await page.getByRole('textbox', { name: 'Progress report interval' }).fill('0')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(saved().at(-1)).toMatchObject({ execution: { progress_report_interval: '0' } })
})

test('a refused progress report interval shows as a field error on the Policy tab', async ({ page }) => {
  const message = 'execution.progress_report_interval must be 0 or a duration from 1m to 60m'
  const saved = await mockReviewScopeAPIs(page, servedInterval, {
    field: 'execution.progress_report_interval',
    message,
  })
  await page.goto('/w/demo/workspace')

  const interval = page.getByRole('textbox', { name: 'Progress report interval' })
  await interval.fill('2h')
  await page.getByRole('button', { name: 'Save changes' }).click()
  expect(saved().at(-1)).toMatchObject({ execution: { progress_report_interval: '2h' } })
  const control = interval.locator('xpath=ancestor::div[1]')
  await expect(control.getByRole('alert')).toHaveText(message)
  await expect(interval).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByRole('tab', { name: /Policy/ })).toHaveAttribute('aria-selected', 'true')
})

// req-risk-based-review AC-1.3, AC-1.4: turning the risk policy on shows the
// default tier table, and a save sends the chosen mode with every edited value.
test('turning the risk policy on saves the chosen mode and every edited tier value', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')
  const mode = page.getByLabel('Risk mode')
  await expect(mode).toHaveValue('off')
  await mode.selectOption('enforce')
  await expect(page.getByText('its tier sets the reviews and approvals it needs')).toBeVisible()
  await page.getByLabel('Medium agent reviews').fill('2')
  await page.getByLabel('High human approvals').fill('3')
  await mode.selectOption('record')
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(submitted()?.document).toMatchObject({
    risk_policy: {
      mode: 'record',
      tiers: {
        low: { agent_reviews: 1, human_approvals: 0 },
        medium: { agent_reviews: 2, human_approvals: 1 },
        high: { agent_reviews: 2, human_approvals: 3 },
      },
    },
  })
})

// component-web-dashboard v61 invariant 33; req-delivery-and-forge AC-8.1: the
// General tab's issue publication switch reads off when the key is absent, and
// a save writes issue_publication.enabled.
test('the General tab saves issue publication as issue_publication.enabled', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')
  await page.getByRole('tab', { name: 'General' }).click()
  const publish = page.getByRole('switch', { name: 'Publish GitHub issues for approved plans' })
  await expect(publish).not.toBeChecked()
  await publish.click()
  await expect(publish).toBeChecked()
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()
  expect(submitted()?.document).toMatchObject({ issue_publication: { enabled: true } })
})
