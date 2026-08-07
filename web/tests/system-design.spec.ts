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

test('System Design renders a category tree, one attention surface, and authenticated confirmation', async ({
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
  // AC-2.1: the tree groups documents by their operator-named category and the
  // selected document is the canvas beside it.
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  await expect(tree.getByRole('heading', { name: 'Architecture' })).toBeVisible()
  await expect(tree.getByRole('button', { name: /Dispatch ownership/ })).toHaveAttribute('aria-current', 'true')
  await expect(page.getByRole('heading', { name: 'Dispatch ownership' })).toBeVisible()
  await expect(page.getByText('The dispatcher owns durable stage transitions.').first()).toBeVisible()

  // AC-1.1: drift and the pending version are listed in the one attention
  // surface, each beside the affordance that resolves it.
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText('Code changed in conveyor without a matching update here')
  await expect(attention).toContainText('internal/dispatch/dispatch.go')
  await expect(attention.getByRole('link', { name: 'Open the change' })).toHaveAttribute(
    'href',
    'https://example.test/pr/42',
  )
  await expect(attention).toContainText('Version 2 is waiting for you')
  await expect(attention.getByRole('button', { name: 'Confirm version 2' })).toBeVisible()

  // AC-1.2: the retired duplicate renderings of those same signals.
  await expect(page.getByText('Design drift · 1')).toHaveCount(0)
  await expect(page.getByText('Pending confirmation')).toHaveCount(0)
  await expect(page.getByText('Pending revisions')).toHaveCount(0)
  await expect(tree.getByText('pending')).toHaveCount(0)
  // AC-2.2: the assistant column is withdrawn from this surface.
  await expect(page.getByRole('complementary', { name: 'Design assistant' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Draft' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Revise' })).toHaveCount(0)
  await expect(page.locator('textarea')).toHaveCount(0)

  // The diff and the version history stay, subordinated under the document.
  await page.getByText('Compare version 1 with the proposed version 2').click()
  await expect(page.getByRole('region', { name: 'Pending version diff' })).toContainText('From version 1')
  await expect(page.getByRole('region', { name: 'Pending version diff' })).toContainText('To version 2')
  await page.getByText('Version history').click()
  await page.getByText('Read version').first().click()
  await expect(
    page.getByRole('list').getByText('The dispatcher owns durable stage transitions.', { exact: true }),
  ).toBeVisible()

  await attention.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect.poll(() => confirmed).toBe(true)
  expect(revisionInput).toEqual({})
  await expect.poll(() => [...new Set(protectedReads)].sort()).toEqual(['/v1/decisions', '/v1/system-designs'])
})

test('a System Design with nothing outstanding says so in one quiet line', async ({ page }) => {
  await initialize(page)
  const settled = {
    ...design,
    pending_versions: [],
    versions: [first],
    drift: [],
  }
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [settled] })
    if (url.pathname === '/v1/decisions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design')
  // AC-1.3: no alarm card for a document with nothing to act on.
  const quiet = page.getByRole('region', { name: 'Needs your attention' })
  await expect(quiet).toContainText('Nothing needs your attention on this document.')
  await expect(quiet.getByRole('button')).toHaveCount(0)
  await expect(page.getByText('Version 1 · confirmed')).toBeVisible()
})

test('the System Design surface never starts a planning session on its own', async ({ page }) => {
  await initialize(page)
  let planningRequests = 0
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [design] })
    if (url.pathname === '/v1/decisions') return route.fulfill({ json: [] })
    if (url.pathname.startsWith('/v1/planning-sessions')) {
      planningRequests++
      return route.fulfill({ json: [] })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/system-design')
  await expect(page.getByRole('heading', { name: 'Dispatch ownership' })).toBeVisible()
  // AC-2.2 and spec §21.61 change 3: the assistant presentation and its
  // guided-action entry points are withdrawn, so this surface neither renders
  // a chat column nor reaches for the planning routes. The routes themselves
  // are untouched.
  await expect(page.locator('textarea')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Draft' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Q&A' })).toHaveCount(0)
  expect(planningRequests).toBe(0)
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
