import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

// A document written before risk review existed carries no risk_policy
// member; it reads as Off (req-risk-based-review REQ-1, AC-1.1).
const document = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}] },
  execution: {
    default_plan_state: 'needed',
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

const defaultTiers = {
  low: { agent_reviews: 1, human_approvals: 0 },
  medium: { agent_reviews: 1, human_approvals: 1 },
  high: { agent_reviews: 2, human_approvals: 2 },
}

const warning = {
  code: 'risk_policy_human_approvals_unavailable',
  field: 'risk_policy',
  message: 'The risk policy can require 3 human approvals. People in this workspace who hold operate_gates: 1.',
  required: 3,
  available: 1,
}

type Answer = { status?: number; json: Record<string, unknown> }

async function mockAPIs(
  page: Page,
  options: {
    role?: string
    served?: Record<string, unknown>
    warnings?: unknown[]
    answer?: (document: Record<string, unknown>) => Answer
  } = {},
) {
  const submitted: Array<Record<string, unknown>> = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: options.role ?? 'operator' }, request) })
    if (path === '/v1/workspace/config') {
      if (request.method() === 'PUT') {
        const sent = (request.postDataJSON() as { document: Record<string, unknown> }).document
        submitted.push(sent)
        const answer = options.answer?.(sent) ?? { json: { document: sent, version: 2, event_id: 88 } }
        return route.fulfill({ status: answer.status ?? 200, json: answer.json })
      }
      return route.fulfill({
        json: {
          document: options.served ?? document,
          version: 1,
          ...(options.warnings ? { warnings: options.warnings } : {}),
        },
      })
    }
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return submitted
}

test('an absent risk policy reads as Off and an unrelated save leaves it absent', async ({ page }) => {
  const submitted = await mockAPIs(page)
  await page.goto('/w/demo/workspace')
  const mode = page.getByLabel('Risk mode')
  await expect(mode).toHaveValue('off')
  await expect(page.getByText('No task gets a risk tier.')).toBeVisible()
  await expect(page.getByRole('table', { name: 'Risk tier requirements' })).toHaveCount(0)

  // Choosing Off again is no change.
  await mode.selectOption('off')
  await expect(page.getByRole('button', { name: 'Save changes' })).toHaveCount(0)

  await page.getByLabel('Review stage timeout').fill('45m')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 88/)).toBeVisible()
  expect(submitted).toHaveLength(1)
  expect(submitted[0]).not.toHaveProperty('risk_policy')
})

// AC-1.3, AC-1.4: turning the policy on shows the default table, every value
// is editable, and the save answer's warning names both numbers.
test('turning the policy on shows the default table and saves every edited value', async ({ page }) => {
  const submitted = await mockAPIs(page, {
    answer: (sent) => ({ json: { document: sent, version: 2, event_id: 89, warnings: [warning] } }),
  })
  await page.goto('/w/demo/workspace')
  await page.getByLabel('Risk mode').selectOption('enforce')
  await expect(page.getByText('its tier sets the reviews and approvals it needs')).toBeVisible()
  for (const [tier, values] of Object.entries({ Low: [1, 0], Medium: [1, 1], High: [2, 2] })) {
    await expect(page.getByLabel(`${tier} agent reviews`)).toHaveValue(String(values[0]))
    await expect(page.getByLabel(`${tier} human approvals`)).toHaveValue(String(values[1]))
  }
  await page.getByLabel('Low agent reviews').fill('1')
  await page.getByLabel('Low human approvals').fill('0')
  await page.getByLabel('Medium agent reviews').fill('2')
  await page.getByLabel('Medium human approvals').fill('1')
  await page.getByLabel('High agent reviews').fill('3')
  await page.getByLabel('High human approvals').fill('3')
  await page.getByLabel('Risk mode').selectOption('record')
  await expect(page.getByLabel('High agent reviews')).toHaveValue('3')
  await page.getByRole('button', { name: 'Save changes' }).click()

  await expect(page.getByText(/Recorded config.updated event 89/)).toBeVisible()
  expect(submitted).toHaveLength(1)
  expect(submitted[0].risk_policy).toEqual({
    mode: 'record',
    tiers: {
      low: { agent_reviews: 1, human_approvals: 0 },
      medium: { agent_reviews: 2, human_approvals: 1 },
      high: { agent_reviews: 3, human_approvals: 3 },
    },
  })
  await expect(page.getByRole('status').filter({ hasText: warning.message })).toBeVisible()

  // An unsaved edit hides the warning about the saved policy.
  await page.getByLabel('High human approvals').fill('2')
  await expect(page.getByText(warning.message)).toHaveCount(0)
})

test('a read warning shows on load beside the saved policy', async ({ page }) => {
  await mockAPIs(page, {
    served: { ...document, risk_policy: { mode: 'enforce', tiers: defaultTiers } },
    warnings: [warning],
  })
  await page.goto('/w/demo/workspace')
  await expect(page.getByLabel('Risk mode')).toHaveValue('enforce')
  await expect(page.getByLabel('High human approvals')).toHaveValue('2')
  await expect(page.getByRole('status').filter({ hasText: warning.message })).toBeVisible()
})

// AC-1.2: the server refuses an invalid table; the refusal shows on the
// Policy tab and saves nothing.
test('a refused table shows its field error on the Policy tab', async ({ page }) => {
  const message = 'risk_policy.tiers.high.agent_reviews must not be lower than medium'
  const submitted = await mockAPIs(page, {
    served: { ...document, risk_policy: { mode: 'record', tiers: defaultTiers } },
    answer: () => ({
      status: 422,
      json: { error: 'validation_failed', fields: [{ field: 'risk_policy', message }] },
    }),
  })
  await page.goto('/w/demo/workspace')
  await page.getByLabel('Medium agent reviews').fill('3')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByRole('alert').filter({ hasText: message })).toBeVisible()
  await expect(page.getByRole('tab', { name: /Policy/ }).locator('[title="Validation errors"]')).toBeVisible()
  expect(submitted).toHaveLength(1)
  expect((submitted[0].risk_policy as { tiers: typeof defaultTiers }).tiers.medium.agent_reviews).toBe(3)
})

test('a member without manage_workspace sees the risk mode read-only', async ({ page }) => {
  const submitted = await mockAPIs(page, {
    role: 'viewer',
    served: { ...document, risk_policy: { mode: 'record', tiers: defaultTiers } },
  })
  await page.goto('/w/demo/workspace')
  await expect(page.getByText('Risk mode: record')).toBeVisible()
  await expect(page.getByLabel('Risk mode')).toHaveCount(0)
  expect(submitted).toHaveLength(0)
})

const shots = process.env.CONVEYOR_SCREENSHOT_DIR ?? 'test-results/shots'

for (const viewport of [
  { name: 'mobile', width: 375, height: 2200 },
  { name: 'desktop', width: 1280, height: 1100 },
]) {
  test(`capture the risk policy settings at ${viewport.width} px`, async ({ page }) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    await mockAPIs(page, {
      served: {
        ...document,
        risk_policy: {
          mode: 'enforce',
          tiers: { ...defaultTiers, high: { agent_reviews: 3, human_approvals: 3 } },
        },
      },
      warnings: [warning],
    })
    await page.goto('/w/demo/workspace')
    const card = page.getByText('Risk review', { exact: true })
    await card.scrollIntoViewIfNeeded()
    await expect(page.getByRole('status').filter({ hasText: warning.message })).toBeVisible()
    await page.screenshot({ path: `${shots}/risk-policy-${viewport.name}.png`, fullPage: true })
  })
}
