import { expect, test, type Page, type Route } from '@playwright/test'

const proposedAt = '2026-08-10T10:00:00Z'

async function initialize(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
}

test('pending proposal queue covers every tier, resolves rows, updates the badge, and clears the task warning', async ({
  page,
}) => {
  await initialize(page)
  let requirementPending = true
  let decisionPending = true

  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity')
      return route.fulfill({
        json: [
          { task: { id: 'one', state: 'awaiting_human' }, needs_attention: true },
          { task: { id: 'two', state: 'parked' }, needs_attention: true },
        ],
      })
    if (path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      expect(request.headers().authorization).toBe('Bearer test-token')
      const items = [
        ...(requirementPending
          ? [
              {
                id: 'req-attention',
                title: 'Operator attention',
                tier: 'requirement',
                version: 2,
                origin_type: 'task',
                origin_id: 'review-task',
                proposed_at: proposedAt,
                age_seconds: 3600,
              },
              {
                id: 'req-attention',
                title: 'Operator attention',
                tier: 'requirement',
                version: 3,
                origin_type: 'task',
                origin_id: 'review-task',
                proposed_at: proposedAt,
                age_seconds: 1800,
              },
            ]
          : []),
        {
          id: 'design-dashboard',
          title: 'Web dashboard architecture',
          tier: 'system_design',
          version: 4,
          origin_type: 'session',
          origin_id: 'planning-1',
          proposed_at: proposedAt,
          age_seconds: 900,
        },
        ...(decisionPending
          ? [
              {
                id: 'DEC-16',
                title: 'Generate dashboard output from web sources.',
                tier: 'decision',
                origin_type: 'operator',
                proposed_at: proposedAt,
                age_seconds: 120,
              },
            ]
          : []),
      ]
      return route.fulfill({
        json: { items, attention: { task_count: 2, pending_proposal_count: items.length, total: 2 + items.length } },
      })
    }
    if (path === '/v1/requirements/req-attention' && request.method() === 'GET')
      return route.fulfill({
        json: { requirement: { id: 'req-attention', title: 'Operator attention', current_version: 1 } },
      })
    if (path === '/v1/requirements/req-attention/versions/3/confirm') {
      expect(request.headers()['if-match']).toBe('"1"')
      requirementPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/decisions/DEC-16/dismiss') {
      decisionPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/tasks/review-task/activity')
      return route.fulfill({
        json: {
          task: {
            id: 'review-task',
            workspace: 'demo',
            source: 'mcp',
            title: 'Review task',
            body: 'A task with a proposed document update.',
            repo: 'conveyor',
            base_branch: 'main',
            branch: 'conveyor/task-review-task',
            state: 'running',
            created_at: proposedAt,
          },
          jobs: [],
          events: [],
          interventions: [],
          checkout_available: false,
          checkout_guidance: '',
          needs_attention: requirementPending,
          pending_authority: requirementPending,
          work_orders: [],
          attachments: [],
          verification_evidence: [],
        },
      })
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  await expect(page.getByRole('heading', { name: 'Pending proposals' })).toBeVisible()
  await expect(page.getByText('Requirement', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('System Design', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('Decision', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('6')
  await expect(
    page.getByText('Confirming a later version also dismisses earlier pending versions.').first(),
  ).toBeVisible()

  await page.goto('/tasks/review-task/full')
  const warning = page.getByRole('region', { name: 'Review is waiting on a document decision' })
  await expect(warning).toContainText('Review will not include this proposed update yet')
  await warning.getByRole('link', { name: 'Open the proposal' }).click()
  await expect(page.getByText('Showing proposals from task review-task.')).toBeVisible()
  const later = page.getByRole('listitem').filter({ hasText: 'v3' })
  await later.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.getByText('No document decisions are waiting for you.')).toBeVisible()
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('4')

  await page.goBack()
  await expect(warning).toHaveCount(0)
  await page.goto('/pending-proposals')
  const decision = page.getByRole('listitem').filter({ hasText: 'Generate dashboard output from web sources.' })
  await decision.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByText('Generate dashboard output from web sources.')).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('3')
})
