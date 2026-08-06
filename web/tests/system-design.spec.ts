import { expect, test, type Page, type Route } from '@playwright/test'

const first = {
  document_id: 'design-dispatch',
  version: 1,
  content: '# Dispatch\n\nThe dispatcher owns durable stage transitions.',
  governs: [{ repository: 'conveyor', paths: ['internal/dispatch/**'] }],
  origin: 'operator',
  confirmed: true,
  workspace: 'demo',
  created_at: '2026-08-05T08:00:00Z',
}
const pending = {
  ...first,
  version: 2,
  content: '# Dispatch\n\nThe dispatcher owns durable stage transitions and work-order leases.',
  governs: [{ repository: 'conveyor', paths: ['internal/dispatch/**', 'internal/workorder/**'] }],
  origin: 'planning_session',
  origin_session_id: 'session-design',
  confirmed: false,
  created_at: '2026-08-05T09:00:00Z',
}
const design = {
  document: {
    id: 'design-dispatch',
    slug: 'dispatch',
    title: 'Dispatch ownership',
    category: 'Architecture',
    current_version: 1,
    workspace: 'demo',
    created_at: '2026-08-05T08:00:00Z',
    updated_at: '2026-08-05T09:00:00Z',
  },
  current_version: first,
  pending_versions: [pending],
  versions: [first, pending],
  lineage: [],
  drift: [
    {
      id: 'design-drift-1',
      workspace_id: 'demo',
      repository: 'conveyor',
      kind: 'external_pr_merge',
      source_url: 'https://example.test/pr/42',
      commit_sha: 'abc123',
      task_id: '260805-example',
      system_design_id: 'design-dispatch',
      system_design_version: 1,
      matching_paths: ['internal/dispatch/dispatch.go'],
      detected_at: '2026-08-05T10:00:00Z',
    },
  ],
}

async function initialize(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
}

function shell(route: Route) {
  const path = new URL(route.request().url()).pathname
  if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
  if (path === '/v1/activity') return route.fulfill({ json: [] })
}

test('System Design renders category, diff, readback, drift, decisions, and authenticated confirmation', async ({
  page,
}) => {
  await initialize(page)
  let confirmed = false
  let revisionInput: Record<string, unknown> = {}
  const protectedReads: string[] = []
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs' || url.pathname === '/v1/decisions') {
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      expect(request.headers().authorization).toBe('Bearer test-token')
      protectedReads.push(url.pathname)
    }
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [design] })
    if (url.pathname === '/v1/decisions')
      return route.fulfill({
        json: [
          {
            id: 'DEC-1',
            statement: 'Use event-derived lineage.',
            context: 'Rebuilds need provenance.',
            alternatives_rejected: 'Mutable edges.',
            status: 'confirmed',
            origin: 'operator',
            workspace: 'demo',
            created_at: '2026-08-05T08:00:00Z',
          },
        ],
      })
    if (url.pathname === '/v1/system-designs/design-dispatch/versions/2/confirm') {
      expect(request.headers().authorization).toBe('Bearer test-token')
      expect(request.headers()['if-match']).toBe('"1"')
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      confirmed = true
      return route.fulfill({ json: { document: design.document, version: pending } })
    }
    if (url.pathname === '/v1/planning-sessions' && request.method() === 'POST') {
      revisionInput = JSON.parse(request.postData() ?? '{}') as Record<string, unknown>
      return route.fulfill({
        json: {
          id: 'session-design',
          title: 'Revising System Design…',
          goal: 'system_design',
          system_design_context_id: 'design-dispatch',
          status: 'active',
          workspace: 'demo',
          created_at: '2026-08-05T10:00:00Z',
          updated_at: '2026-08-05T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions/session-design')
      return route.fulfill({
        json: {
          id: 'session-design',
          title: 'Revising System Design…',
          goal: 'system_design',
          system_design_context_id: 'design-dispatch',
          status: 'active',
          workspace: 'demo',
          created_at: '2026-08-05T10:00:00Z',
          updated_at: '2026-08-05T10:00:00Z',
        },
      })
    if (url.pathname === '/v1/planning-sessions/session-design/messages') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design')
  await expect(page.getByRole('heading', { name: 'System Design' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Architecture' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Dispatch ownership' })).toBeVisible()
  await expect(page.getByText('Design drift · 1')).toBeVisible()
  await expect(page.getByText('internal/dispatch/dispatch.go')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Pending version diff' })).toContainText('From version 1')
  await expect(page.getByRole('region', { name: 'Pending version diff' })).toContainText('To version 2')
  await expect(page.getByText('Use event-derived lineage.')).toBeVisible()
  await expect(page.locator('textarea')).toHaveCount(0)

  await page.getByText('Prior versions').click()
  await page.getByText('Read version').first().click()
  await expect(
    page.getByRole('list').getByText('The dispatcher owns durable stage transitions.', { exact: true }),
  ).toBeVisible()

  await page.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect.poll(() => confirmed).toBe(true)
  await page.getByRole('button', { name: 'Revise' }).click()
  await expect
    .poll(() => revisionInput)
    .toMatchObject({
      goal: 'system_design',
      system_design_context_id: 'design-dispatch',
    })
  await expect.poll(() => [...new Set(protectedReads)].sort()).toEqual(['/v1/decisions', '/v1/system-designs'])
})

test('System Design Draft starts a second document without revising the selected corpus', async ({ page }) => {
  await initialize(page)
  let creationInput: Record<string, unknown> = {}
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [design] })
    if (url.pathname === '/v1/decisions') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions' && request.method() === 'POST') {
      creationInput = JSON.parse(request.postData() ?? '{}') as Record<string, unknown>
      return route.fulfill({
        json: {
          id: 'session-new-design',
          title: 'Drafting System Design…',
          goal: 'system_design',
          status: 'active',
          workspace: 'demo',
          created_at: '2026-08-05T10:00:00Z',
          updated_at: '2026-08-05T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions/session-new-design')
      return route.fulfill({
        json: {
          id: 'session-new-design',
          title: 'Drafting System Design…',
          goal: 'system_design',
          status: 'active',
          workspace: 'demo',
          created_at: '2026-08-05T10:00:00Z',
          updated_at: '2026-08-05T10:00:00Z',
        },
      })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design')
  await expect(page.locator('textarea')).toHaveCount(0)
  await page.getByRole('button', { name: 'Draft' }).click()
  await expect.poll(() => creationInput).toEqual({ goal: 'system_design' })
  await expect(page.locator('textarea')).toHaveCount(1)
})

test('stale System Design confirmation refreshes the list and explains the retry', async ({ page }) => {
  await initialize(page)
  let designReads = 0
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs') {
      designReads++
      return route.fulfill({ json: [design] })
    }
    if (url.pathname === '/v1/decisions') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/system-designs/design-dispatch/versions/2/confirm')
      return route.fulfill({
        status: 409,
        json: {
          error: 'system_design_version_conflict',
          message: 'system design design-dispatch current version changed from 1 to 2',
          current_version: 2,
        },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design')
  await page.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect(page.getByText(/latest versions are loading; review them and try again/i)).toBeVisible()
  await expect.poll(() => designReads).toBeGreaterThanOrEqual(2)
})

test('operators confirm and dismiss proposed decisions with conflict-safe refresh and attribution', async ({
  page,
}) => {
  await initialize(page)
  let decisionReads = 0
  let decisions = [
    {
      id: 'DEC-1',
      statement: 'Keep events append-only.',
      context: 'Audit history must remain durable.',
      alternatives_rejected: 'Mutable lifecycle rows alone.',
      status: 'proposed',
      origin: 'operator',
      workspace: 'demo',
      created_at: '2026-08-05T08:00:00Z',
    },
    {
      id: 'DEC-2',
      statement: 'Use stable decision anchors.',
      context: 'Operators need direct blocker links.',
      alternatives_rejected: 'Search the page manually.',
      status: 'proposed',
      origin: 'operator',
      workspace: 'demo',
      created_at: '2026-08-05T09:00:00Z',
    },
  ]
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [design] })
    if (url.pathname === '/v1/decisions') {
      decisionReads++
      return route.fulfill({ json: decisions })
    }
    if (url.pathname === '/v1/decisions/DEC-1/confirm') {
      expect(request.method()).toBe('POST')
      expect(request.headers().authorization).toBe('Bearer test-token')
      expect(request.headers()['x-conveyor-actor']).toBe('dashboard-operator')
      decisions = decisions.map((item) =>
        item.id === 'DEC-1'
          ? {
              ...item,
              status: 'confirmed',
              confirmed_by: 'dashboard-operator',
              confirmed_at: '2026-08-06T08:00:00Z',
            }
          : item,
      )
      return route.fulfill({ json: decisions[0] })
    }
    if (url.pathname === '/v1/decisions/DEC-2/dismiss') {
      expect(request.headers().authorization).toBe('Bearer test-token')
      decisions = decisions.map((item) =>
        item.id === 'DEC-2'
          ? {
              ...item,
              status: 'dismissed',
              dismissed_by: 'second-operator',
              dismissed_at: '2026-08-06T08:01:00Z',
            }
          : item,
      )
      return route.fulfill({ status: 409, body: 'decision was already dismissed' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design#decision-dec-2')
  await expect(page.locator('#decision-dec-2')).toBeVisible()
  await page.locator('#decision-dec-1').getByRole('button', { name: 'Confirm' }).click()
  await expect(page.locator('#decision-dec-1')).toContainText('Confirmed by dashboard-operator')
  await page.locator('#decision-dec-2').getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.locator('#decision-dec-2')).toContainText('Dismissed by second-operator')
  await expect.poll(() => decisionReads).toBeGreaterThanOrEqual(3)
})
