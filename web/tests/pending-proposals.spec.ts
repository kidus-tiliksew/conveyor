import { expect, test, type Page, type Route } from '@playwright/test'

const proposedAt = '2026-08-10T10:00:00Z'

async function initialize(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
}

test('pending proposal label and attention badge stay on one line at the narrow reference viewport', async ({
  page,
}) => {
  await page.setViewportSize({ width: 550, height: 1982 })
  await initialize(page)
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({
        json: {
          items: [
            {
              id: 'DEC-42',
              title: 'Operator attention',
              tier: 'decision',
              origin_type: 'operator',
              proposed_at: proposedAt,
              age_seconds: 60,
            },
          ],
          attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
        },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  const primary = page.getByRole('navigation', { name: 'Primary' })
  const pending = primary.getByRole('link', { name: /Pending proposals/ })
  const label = pending.getByText('Pending proposals', { exact: true })
  const badge = pending.getByText('1', { exact: true })

  await expect(primary).toHaveCSS('width', '256px')
  await expect(label).toBeVisible()
  await expect(badge).toBeVisible()
  expect(
    await label.evaluate((element) => {
      const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight)
      return element.scrollHeight <= lineHeight + 1
    }),
  ).toBe(true)
  expect(
    await page.evaluate(() => {
      const main = document.querySelector('main')
      const nav = document.querySelector('nav[aria-label="Primary"]')
      if (!(main instanceof HTMLElement) || !(nav instanceof HTMLElement)) return false
      return (
        document.documentElement.scrollWidth <= window.innerWidth &&
        nav.scrollWidth <= nav.clientWidth &&
        main.clientWidth > 0 &&
        main.getBoundingClientRect().right <= window.innerWidth
      )
    }),
  ).toBe(true)
})

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
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
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
  await expect(page.getByRole('link', { name: /Requirements/ })).toContainText('1')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('4')
  await expect(
    page.getByText('Confirming a later version also dismisses earlier pending versions.').first(),
  ).toBeVisible()

  await page.goto('/tasks/review-task/full')
  const warning = page.getByRole('region', { name: 'Review is waiting on a document decision' })
  await expect(warning).toContainText('This review cannot be claimed until you confirm or dismiss')
  await warning.getByRole('link', { name: 'Confirm or dismiss the proposal' }).click()
  await expect(page.getByText('Showing proposals from task review-task.')).toBeVisible()
  const later = page.getByRole('listitem').filter({ hasText: 'v3' })
  await later.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.getByText('No document or context decisions are waiting for you.')).toBeVisible()
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('4')
  await expect(page.getByRole('link', { name: /Requirements/ })).not.toContainText('1')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('2')

  await page.goBack()
  await expect(warning).toHaveCount(0)
  await page.goto('/pending-proposals')
  const decision = page.getByRole('listitem').filter({ hasText: 'Generate dashboard output from web sources.' })
  await decision.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByText('Generate dashboard output from web sources.')).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('3')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('1')
})

test('task context suggestions reach attention with task links, justifications, actions, and count updates', async ({
  page,
}) => {
  await initialize(page)
  let requirementPending = true
  let designPending = true
  const calls: string[] = []

  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      const items = [
        ...(requirementPending
          ? [
              {
                id: 'req-intake',
                title: 'Task intake and triage',
                tier: 'task_context',
                origin_type: 'task',
                origin_id: 'context-task',
                target_kind: 'requirement',
                justification: 'The task changes how intake suggestions reach an operator.',
                proposed_at: proposedAt,
                age_seconds: 90,
              },
            ]
          : []),
        ...(designPending
          ? [
              {
                id: 'design-web-dashboard',
                title: 'Web dashboard',
                tier: 'task_context',
                origin_type: 'task',
                origin_id: 'context-task',
                target_kind: 'system_design',
                justification: 'The dashboard interaction inventory governs the change.',
                proposed_at: proposedAt,
                age_seconds: 60,
              },
            ]
          : []),
      ]
      return route.fulfill({
        json: { items, attention: { task_count: 0, pending_proposal_count: items.length, total: items.length } },
      })
    }
    if (path === '/v1/tasks/context-task/context/proposals/requirement/req-intake/confirm') {
      calls.push('confirm')
      expect(request.headers().authorization).toBe('Bearer test-token')
      requirementPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/tasks/context-task/context/proposals/system_design/design-web-dashboard/dismiss') {
      calls.push('dismiss')
      expect(request.headers().authorization).toBe('Bearer test-token')
      designPending = false
      return route.fulfill({ json: {} })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  const requirement = page.getByRole('listitem').filter({ hasText: 'Task intake and triage' })
  await expect(requirement.getByRole('link', { name: 'task context-task' })).toHaveAttribute(
    'href',
    /tasks\/context-task\/full/,
  )
  await expect(requirement).toContainText('The task changes how intake suggestions reach an operator.')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('2')
  await requirement.getByRole('button', { name: 'Confirm' }).click()
  await expect(requirement).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('1')

  const design = page.getByRole('listitem').filter({ hasText: 'Web dashboard' })
  await expect(design).toContainText('The dashboard interaction inventory governs the change.')
  await design.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByText('No document or context decisions are waiting for you.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Pending proposals', exact: true })).toHaveText('Pending proposals')
  expect(calls).toEqual(['confirm', 'dismiss'])
})

test('attention navigation omits requirement and pending proposal badges when projection fails or is empty', async ({
  page,
}) => {
  await initialize(page)
  let failProjection = true
  let projectionRequests = 0
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      projectionRequests++
      if (failProjection) return route.fulfill({ status: 500, json: { error: 'projection unavailable' } })
      return route.fulfill({
        json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/')
  await expect.poll(() => projectionRequests).toBeGreaterThan(0)
  await expect(page.getByRole('link', { name: 'Requirements', exact: true })).toHaveText('Requirements')
  await expect(page.getByRole('link', { name: 'Pending proposals', exact: true })).toHaveText('Pending proposals')

  failProjection = false
  await page.reload()
  await expect.poll(() => projectionRequests).toBeGreaterThan(1)
  await expect(page.getByRole('link', { name: 'Requirements', exact: true })).toHaveText('Requirements')
  await expect(page.getByRole('link', { name: 'Pending proposals', exact: true })).toHaveText('Pending proposals')
})

test('maintainer can read pending decisions without corpus-authority controls', async ({ page }) => {
  await initialize(page)
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_maintainer', role: 'maintainer' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({
        json: {
          items: [
            {
              id: 'DEC-42',
              title: 'Operator-only corpus decision',
              tier: 'decision',
              origin_type: 'operator',
              proposed_at: proposedAt,
              age_seconds: 60,
            },
          ],
          attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
        },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  const decision = page.getByRole('listitem').filter({ hasText: 'Operator-only corpus decision' })
  await expect(decision).toBeVisible()
  await expect(decision.getByRole('button', { name: 'Confirm' })).toHaveCount(0)
  await expect(decision.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('pending proposal consumers share one active query and hidden documents stop interval polling', async ({
  page,
}) => {
  await initialize(page)
  await page.addInitScript(() => {
    let visibility: DocumentVisibilityState = 'visible'
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    })
    Object.defineProperty(window, '__setTestVisibility', {
      value: (next: DocumentVisibilityState) => {
        visibility = next
        window.dispatchEvent(new Event('visibilitychange'))
      },
    })
  })
  let projectionRequests = 0
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      projectionRequests++
      return route.fulfill({
        json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } },
      })
    }
    return route.fulfill({ json: [] })
  })

  // AppShell and the queue page both subscribe to the same workspace key.
  await page.goto('/pending-proposals')
  await expect.poll(() => projectionRequests).toBe(1)

  await page.evaluate(() => {
    ;(window as Window & { __setTestVisibility(next: DocumentVisibilityState): void }).__setTestVisibility('hidden')
  })
  await page.waitForTimeout(16_000)
  expect(projectionRequests).toBe(1)

  await page.evaluate(() => {
    ;(window as Window & { __setTestVisibility(next: DocumentVisibilityState): void }).__setTestVisibility('visible')
  })
  await expect.poll(() => projectionRequests).toBe(2)

  await page.waitForTimeout(5_100)
  await page.context().setOffline(true)
  await page.context().setOffline(false)
  await expect.poll(() => projectionRequests).toBe(3)
})
