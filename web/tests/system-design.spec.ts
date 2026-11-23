import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

const first = {
  document_id: 'design-dispatch',
  version: 1,
  content:
    '# Dispatch\n\nThe dispatcher owns durable stage transitions.\n\n```mermaid\nsequenceDiagram\n  participant UI\n  participant API\n  UI->>API: dispatch\n```\n\n```mermaid\nthis is deliberately malformed design mermaid\n```',
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
function versionSummary(version: typeof first | typeof pending) {
  const { content: _content, governs: _governs, ...summary } = version
  return summary
}

// System Design activity comes from the events route, never the detail
// (component-web-document-surfaces invariant 8).
const designEvents = [
  {
    id: 73,
    task_id: '',
    kind: 'system_design.consulted',
    actor_id: 'system',
    actor_role: 'system',
    payload: {
      workspace_id: 'demo',
      document_id: 'design-dispatch',
      version: 1,
      delivery_task_id: '260813-delivery',
      merge_event_id: 72,
      merge_head_sha: 'reviewed-head',
      matching_paths: ['internal/dispatch/dispatch.go'],
      consultation: 'delivery_no_revision',
    },
    at: '2026-08-05T09:30:00Z',
  },
]

function documentEventPage(
  events: Array<{ id: number }>,
  options: { total?: number; offset?: number; snapshot?: number } = {},
) {
  return {
    events,
    total: options.total ?? events.length,
    limit: 50,
    offset: options.offset ?? 0,
    snapshot_id: options.snapshot ?? Math.max(0, ...events.map((event) => event.id)),
  }
}

function summarizeDesign(view: typeof design) {
  return {
    document: view.document,
    current_version: view.current_version ? versionSummary(view.current_version) : undefined,
    pending_versions: view.pending_versions.map(versionSummary),
    pending_version_count: view.pending_versions.length,
    drift_count: view.drift.length,
  }
}

async function initialize(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
}

function shell(route: Route) {
  const path = new URL(route.request().url()).pathname
  if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
  if (path === '/v1/me')
    return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
  if (path === '/v1/activity') return route.fulfill({ json: [] })
}

const proposedDecisions = [
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

test('System Design renders a category tree, one attention surface, and authenticated confirmation', async ({
  page,
}) => {
  await initialize(page)
  let confirmed = false
  let dismissAttempts = 0
  let revisionInput: Record<string, unknown> = {}
  const protectedReads: string[] = []
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs' || url.pathname.startsWith('/v1/decision')) {
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      expect(request.headers().authorization).toBeUndefined()
      protectedReads.push(url.pathname)
    }
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(design)] })
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: design })
    if (url.pathname === '/v1/system-designs/design-dispatch/events')
      return route.fulfill({ json: documentEventPage(designEvents) })
    if (url.pathname === '/v1/decisions') return route.fulfill({ json: proposedDecisions })
    if (url.pathname === '/v1/system-designs/design-dispatch/versions/2/confirm') {
      expect(request.headers().authorization).toBeUndefined()
      expect(request.headers()['if-match']).toBe('"1"')
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      confirmed = true
      return route.fulfill({ json: { document: design.document, version: pending } })
    }
    if (url.pathname === '/v1/system-designs/design-dispatch/versions/2/dismiss') {
      dismissAttempts++
      return route.fulfill({ json: {} })
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

  await page.goto('/w/demo/system-design')
  await expect(page.getByRole('heading', { name: 'System Design' })).toBeVisible()
  // AC-2.1: the tree groups documents by their operator-named category and the
  // selected document is the canvas beside it.
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  await expect(tree.getByRole('heading', { name: 'Architecture' })).toBeVisible()
  await expect(tree.getByRole('button', { name: /Dispatch ownership/ })).toHaveAttribute('aria-current', 'true')
  await expect(page.getByRole('heading', { name: 'Dispatch ownership' })).toBeVisible()
  await expect(page.getByText('The dispatcher owns durable stage transitions.').first()).toBeVisible()
  const documentCanvas = page.getByRole('heading', { name: 'Dispatch ownership' }).locator('xpath=ancestor::article')
  await expect(documentCanvas.locator('[data-mermaid] svg').first()).toBeVisible()
  await expect(documentCanvas.locator('code.language-mermaid').first()).toContainText(
    'this is deliberately malformed design mermaid',
  )

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
  await attention.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('dialog', { name: 'Dismiss version 2 of Dispatch ownership' })).toBeVisible()
  expect(dismissAttempts).toBe(0)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(attention).toContainText('Version 2 is waiting for you')
  expect(dismissAttempts).toBe(0)

  // AC-4.2/AC-4.4: the judged merge remains visible as neutral history and
  // does not enter the document's attention surface or count.
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  const deliveryHistory = page.getByRole('region', { name: 'Delivery history' })
  await expect(deliveryHistory).toContainText('Consulted at delivery — no revision warranted')
  await expect(deliveryHistory).toContainText('Pinned version 1 · task 260813-delivery · merge reviewed-head')
  await expect(attention).not.toContainText('Consulted at delivery')
  await expect(tree.getByRole('button', { name: /Dispatch ownership/ })).toContainText('2')

  // AC-1.2: the retired duplicate renderings of those same signals.
  await expect(page.getByText('Design drift · 1')).toHaveCount(0)
  await expect(page.getByText('Pending confirmation')).toHaveCount(0)
  await expect(page.getByText('Pending revisions')).toHaveCount(0)
  await expect(tree.getByText('pending')).toHaveCount(0)
  // AC-2.2: the assistant column is withdrawn from this surface.
  await expect(page.getByRole('complementary', { name: 'Design assistant' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Draft' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Review changes · v2', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Revise', exact: true })).toBeVisible()
  await expect(page.locator('textarea')).toHaveCount(0)

  // Review uses the selected versions; history returns to an exact document read.
  const diff = page.getByRole('region', { name: 'Version comparison', exact: true })
  await expect(page.getByLabel('Base version', { exact: true })).toHaveValue('1')
  await expect(page.getByLabel('Target version', { exact: true })).toHaveValue('2')
  await expect(diff.locator('ins').filter({ hasText: 'and work-order leases' })).toBeVisible()
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await page.getByRole('button', { name: 'Read version 1', exact: true }).click()
  await expect(page.getByText('The dispatcher owns durable stage transitions.', { exact: true })).toBeVisible()
  await attention.getByRole('button', { name: 'Review changes · v2', exact: true }).click()

  await attention.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect.poll(() => confirmed).toBe(true)
  expect(revisionInput).toEqual({})
  // DEC-121: the page reads its own documents and never the decision list.
  await expect.poll(() => [...new Set(protectedReads)].sort()).toEqual(['/v1/system-designs'])
})

test('System Design search and deterministic sorting preserve categories and the selected canvas', async ({ page }) => {
  await initialize(page)
  const items = [
    {
      ...design,
      document: {
        ...design.document,
        id: 'design-alpha-a',
        slug: 'alpha-a',
        title: 'Alpha',
        created_at: '2026-07-01T10:00:00Z',
        updated_at: '2026-07-04T10:00:00Z',
      },
    },
    {
      ...design,
      document: {
        ...design.document,
        id: 'design-alpha-b',
        slug: 'alpha-b',
        title: 'alpha',
        created_at: '2026-07-01T10:00:00Z',
        updated_at: 'not-a-date',
      },
      pending_versions: [],
      drift: [],
    },
    {
      ...design,
      document: {
        ...design.document,
        id: 'design-beta',
        slug: 'beta',
        title: 'Beta',
        created_at: '2026-07-03T10:00:00Z',
        updated_at: '2026-07-02T10:00:00Z',
      },
      pending_versions: [],
      drift: [],
    },
    {
      ...design,
      document: {
        ...design.document,
        id: 'design-gamma',
        slug: 'gamma',
        title: 'Gamma',
        category: 'Operations',
        created_at: 'not-a-date',
        updated_at: '2026-07-03T10:00:00Z',
      },
      pending_versions: [],
      drift: [],
    },
  ]
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/system-designs') return route.fulfill({ json: items.map(summarizeDesign) })
    const detail = items.find((item) => path === `/v1/system-designs/${item.document.id}`)
    if (detail) return route.fulfill({ json: detail })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/system-design?document=design-alpha-a')
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  const architecture = tree.getByRole('heading', { name: 'Architecture' }).locator('..')
  const rowOrder = () =>
    architecture
      .getByRole('button')
      .filter({ hasText: /^(Alpha|alpha|Beta)/ })
      .allTextContents()

  await expect.poll(rowOrder).toEqual(['Alpha2', 'Beta', 'alpha'])
  const sortTrigger = tree.getByRole('button', { name: 'Sort System Design by' })
  const chooseSort = async (label: string) => {
    await sortTrigger.click()
    await expect(page.getByRole('menu', { name: 'Sort System Design by' })).toBeVisible()
    await page.getByRole('menuitemradio', { name: label }).click()
  }

  await chooseSort('Updated')
  await expect.poll(rowOrder).toEqual(['alpha', 'Beta', 'Alpha2'])
  await chooseSort('Name')
  await expect.poll(rowOrder).toEqual(['Alpha2', 'alpha', 'Beta'])
  await chooseSort('Name')
  await expect.poll(rowOrder).toEqual(['Beta', 'Alpha2', 'alpha'])
  await chooseSort('Created')
  await expect.poll(rowOrder).toEqual(['Beta', 'Alpha2', 'alpha'])

  const search = tree.getByRole('searchbox', { name: 'Search System Design' })
  await search.fill('gAm')
  await expect(tree.getByRole('heading', { name: 'Architecture' })).toHaveCount(0)
  await expect(tree.getByRole('heading', { name: 'Operations' })).toBeVisible()
  await expect(tree.getByRole('button', { name: 'Gamma' })).toBeVisible()
  await search.fill('missing')
  await expect(tree.getByText('No documents match your search.')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Alpha' })).toBeVisible()
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
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(settled)] })
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: settled })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/system-design')
  // AC-1.3: no alarm card for a document with nothing to act on.
  const quiet = page.getByRole('region', { name: 'Needs your attention' })
  await expect(quiet).toContainText('Nothing needs your attention on this document.')
  await expect(quiet.getByRole('button')).toHaveCount(0)
  const metadata = page.getByRole('heading', { name: 'Dispatch ownership' }).locator('..')
  await expect(metadata.getByText('v1', { exact: true })).toBeVisible()
  await expect(metadata.getByText('Confirmed', { exact: true })).toBeVisible()
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Document activity' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Settled decisions' })).toHaveCount(0)
})

test('System Design resolves drift inline, restricts outcomes, and shows the confirmed-version precondition', async ({
  page,
}) => {
  await initialize(page)
  let resolved = false
  let resolution: Record<string, string> | undefined
  const currentDesign = () => ({
    ...design,
    pending_versions: [],
    versions: [first],
    drift: resolved ? [] : design.drift,
  })
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(currentDesign())] })
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: currentDesign() })
    if (url.pathname === '/v1/monitor/drift/design-drift-1/resolve') {
      resolution = request.postDataJSON() as Record<string, string>
      if (resolution.outcome === 'design_document_updated') {
        return route.fulfill({
          status: 422,
          body: 'A newer confirmed System Design version is required before resolving this change.',
        })
      }
      resolved = true
      return route.fulfill({ json: design.drift[0] })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/system-design?document=design-dispatch')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  const form = attention.getByRole('form', { name: 'Resolve drift design-drift-1' })
  const outcomes = form.getByLabel('Resolution outcome for design-drift-1')
  await expect(outcomes.getByRole('option', { name: 'Requirements amended' })).toHaveCount(0)
  await expect(outcomes.getByRole('option', { name: 'Design document updated' })).toBeAttached()

  await outcomes.selectOption('design_document_updated')
  await form.getByRole('button', { name: 'Resolve' }).click()
  await expect(form).toContainText('A newer confirmed System Design version is required')

  await outcomes.selectOption('conflict_resolved')
  await form.getByRole('button', { name: 'Resolve' }).click()
  await expect.poll(() => resolution).toEqual({ outcome: 'conflict_resolved' })
  await expect(attention).toContainText('Nothing needs your attention on this document.')
  await expect(attention.getByRole('form')).toHaveCount(0)
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
      return route.fulfill({ json: [summarizeDesign(design)] })
    }
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: design })
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

  await page.goto('/w/demo/system-design')
  await page.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect(page.getByText(/latest versions are loading; review them and try again/i)).toBeVisible()
  await expect.poll(() => designReads).toBeGreaterThanOrEqual(2)
})

test('System Design archives with successors, shows inline rejection, and restores from an archived deep link', async ({
  page,
}) => {
  await initialize(page)
  let archived = false
  let archiveCalls = 0
  let restoreCalls = 0
  let submittedSuccessors: string[] = []
  const archivedAt = '2026-09-02T18:15:00Z'
  const view = () => ({
    ...design,
    document: {
      ...design.document,
      archived,
      archived_by: archived ? 'Operator One' : undefined,
      archived_at: archived ? archivedAt : undefined,
      superseded_by: archived ? submittedSuccessors : undefined,
    },
  })
  const events = () =>
    archived
      ? [
          {
            id: 74,
            task_id: '',
            kind: 'system_design.archived',
            actor_id: 'Operator One',
            actor_role: 'human',
            payload: {},
            at: archivedAt,
          },
          ...designEvents,
        ]
      : designEvents

  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/system-designs') {
      if (url.searchParams.has('include_archived')) expect(url.searchParams.get('include_archived')).toBe('true')
      return route.fulfill({ json: [summarizeDesign(view())] })
    }
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/system-designs/design-dispatch/events')
      return route.fulfill({ json: documentEventPage(events()) })
    if (url.pathname === '/v1/requirements') {
      return route.fulfill({
        json: [
          {
            requirement: {
              id: 'req-replacement',
              slug: 'replacement',
              title: 'Replacement requirement',
              current_version: 1,
              statement_high_water_mark: 1,
              archived: false,
              workspace: 'demo',
              created_at: '2026-09-02T17:00:00Z',
              updated_at: '2026-09-02T17:00:00Z',
            },
            pending_version_count: 0,
            serving_tasks: [],
            staleness: {},
            confirmation_eligible: true,
          },
        ],
      })
    }
    if (url.pathname === '/v1/system-designs/design-dispatch/archive' && request.method() === 'POST') {
      archiveCalls++
      submittedSuccessors = (JSON.parse(request.postData() ?? '{}') as { superseded_by?: string[] }).superseded_by ?? []
      expect(submittedSuccessors).toEqual(['req-replacement'])
      if (archiveCalls === 1) {
        return route.fulfill({
          status: 409,
          json: {
            error: 'superseded_by_invalid',
            detail: 'superseded_by identifier "req-replacement" is archived',
          },
        })
      }
      archived = true
      return route.fulfill({ json: view().document })
    }
    if (url.pathname === '/v1/system-designs/design-dispatch/restore' && request.method() === 'POST') {
      archived = false
      restoreCalls++
      return route.fulfill({ json: view().document })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/system-design?document=design-dispatch')
  await page.getByRole('button', { name: 'Archive' }).click()
  const archiveDialog = page.getByRole('dialog', { name: 'Archive Dispatch ownership' })
  await archiveDialog.getByLabel(/Replacement requirement/).check()
  await archiveDialog.getByRole('button', { name: 'Archive' }).click()
  await expect.poll(() => archiveCalls).toBe(1)
  await expect(archiveDialog.getByText('superseded_by identifier "req-replacement" is archived')).toBeVisible()
  await archiveDialog.getByRole('button', { name: 'Archive' }).click()
  await expect.poll(() => archiveCalls).toBe(2)
  const archivedNotice = page.getByText('This System Design document is archived.').locator('..')
  await expect(archivedNotice).toBeVisible()
  await expect(archivedNotice.getByText('Superseded by')).toBeVisible()
  await expect(archivedNotice.getByRole('link', { name: 'req-replacement' })).toHaveAttribute('href', /requirements/)
  const archivedGroup = page.getByRole('navigation', { name: 'Document tree' }).locator('details')
  await expect(archivedGroup).not.toHaveAttribute('open', '')
  await archivedGroup.locator('summary').click()
  const archivedTreeItem = archivedGroup.getByRole('button', { name: 'Dispatch ownership' })
  await expect(archivedTreeItem).toBeVisible()
  await expect(archivedTreeItem).toHaveAttribute('title', 'Superseded by req-replacement')
  await archivedTreeItem.hover()
  await expect(archivedGroup.getByRole('link', { name: 'req-replacement' })).toBeVisible()
  await expect(page.getByText('Archived', { exact: true }).last()).toHaveAttribute(
    'title',
    /Archived by Operator One on/,
  )
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await expect(page.getByText('System Design document archived')).toBeVisible()
  await expect(page).toHaveURL(/document=design-dispatch/)
  await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
  await page.getByRole('button', { name: 'Restore' }).click()
  await expect.poll(() => restoreCalls).toBe(1)
  await expect(page.getByRole('button', { name: 'Archive' })).toBeVisible()
  await expect(page.getByText('This System Design document is archived.')).toHaveCount(0)
  await expect(page.getByRole('navigation', { name: 'Document tree' }).locator('details')).toHaveCount(0)
})

test('System Design archive controls are absent without document confirmation capability', async ({ page }) => {
  await initialize(page)
  let archived = false
  const view = () => ({
    ...design,
    document: {
      ...design.document,
      archived,
      archived_by: archived ? 'Operator One' : undefined,
      archived_at: archived ? '2026-09-02T18:15:00Z' : undefined,
    },
  })
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_contributor', role: 'contributor' }, route.request()) })
    const handled = shell(route)
    if (handled) return await handled
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(view())] })
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: view() })
    return route.fulfill({ json: [] })
  })
  await page.goto('/w/demo/system-design?document=design-dispatch')
  await expect(page.getByRole('button', { name: 'Archive' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Restore' })).toHaveCount(0)
  archived = true
  await page.reload()
  await expect(page.getByText('This System Design document is archived.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Archive' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Restore' })).toHaveCount(0)
})

test('System Design activity loads only in History, pages from the first snapshot, and loads its explorer on open', async ({
  page,
}) => {
  await initialize(page)
  const archivedEvent = {
    ...designEvents[0],
    id: 100,
    kind: 'system_design.archived',
    payload: { ...designEvents[0].payload, consultation: undefined },
  }
  const events = [
    archivedEvent,
    ...Array.from({ length: 50 }, (_, index) => ({
      ...designEvents[0],
      id: 99 - index,
      payload: {
        ...designEvents[0].payload,
        delivery_task_id: index === 49 ? 'oldest-delivery-task' : `delivery-${index}`,
      },
    })),
  ]
  const eventReads: Array<{ offset: string | null; snapshot: string | null; limit: string | null }> = []
  let graphReads = 0
  // Walks that delivered a response. The explorer's read consumes its abort
  // signal, so StrictMode's discarded first mount aborts a read the page never
  // receives.
  // Event reads that delivered a response. A read consumes its abort signal,
  // so StrictMode's discarded first mount aborts a read the page never
  // receives.
  page.on('requestfinished', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/v1/lineage/system_design/design-dispatch') graphReads++
    if (url.pathname === '/v1/system-designs/design-dispatch/events')
      eventReads.push({
        offset: url.searchParams.get('offset'),
        snapshot: url.searchParams.get('snapshot_id'),
        limit: url.searchParams.get('limit'),
      })
  })
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(design)] })
    if (url.pathname === '/v1/system-designs/design-dispatch') return route.fulfill({ json: design })
    if (url.pathname === '/v1/system-designs/design-dispatch/events') {
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      const offset = url.searchParams.get('offset')
      return route.fulfill({
        json:
          offset === '0'
            ? documentEventPage(events.slice(0, 50), { total: 51, snapshot: 100 })
            : documentEventPage(events.slice(50), { total: 51, offset: 50, snapshot: 100 }),
      })
    }
    if (url.pathname === '/v1/lineage/system_design/design-dispatch') {
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      return route.fulfill({
        json: {
          roots: [],
          nodes: [{ type: 'system_design', id: 'design-dispatch', label: 'Dispatch ownership' }],
          links: [],
          truncated: false,
        },
      })
    }
    return route.fulfill({ json: [] })
  })
  await page.goto('/w/demo/system-design?document=design-dispatch')
  await expect(page.getByText('The dispatcher owns durable stage transitions.').first()).toBeVisible()
  await page.getByRole('tab', { name: 'changes', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Version comparison', exact: true })).toBeVisible()
  // Document and Changes send no events request.
  expect(eventReads).toEqual([])
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await expect(page.getByText('51 activity events')).toBeVisible()
  // The first page names no snapshot.
  await expect.poll(() => eventReads).toEqual([{ offset: '0', snapshot: null, limit: '50' }])
  const deliveryHistory = page.getByRole('region', { name: 'Delivery history' })
  await expect(deliveryHistory.getByText('Consulted at delivery — no revision warranted')).toHaveCount(49)
  await expect(page.getByRole('region', { name: 'Archive activity' })).toContainText('System Design document archived')
  expect(graphReads).toBe(0)
  await page.getByRole('button', { name: 'Load older activity' }).click()
  await expect(page.getByText('oldest-delivery-task', { exact: true })).toBeVisible()
  await expect(deliveryHistory.getByText('Consulted at delivery — no revision warranted')).toHaveCount(50)
  await expect(page.getByRole('button', { name: 'Load older activity' })).toHaveCount(0)
  // The older page passes the first page's snapshot and the next offset.
  await expect
    .poll(() => eventReads)
    .toEqual([
      { offset: '0', snapshot: null, limit: '50' },
      { offset: '50', snapshot: '100', limit: '50' },
    ])
  expect(graphReads).toBe(0)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  await expect(page.getByRole('dialog', { name: 'Knowledge explorer' })).toBeVisible()
  await expect.poll(() => graphReads).toBe(1)
})

test('System Design pending attention revises through the shared dialog and refreshes the document', async ({
  page,
}) => {
  await initialize(page)
  let revised = false
  const calls: string[] = []
  const content =
    '# Corrected dispatch design\n\n```conveyor:governs\n- repo: conveyor\n  paths: [internal/dispatch/**]\n```'
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const path = new URL(route.request().url()).pathname
    if (path.endsWith('/events'))
      return route.fulfill({ json: { events: [], total: 0, limit: 50, offset: 0, snapshot_id: 0 } })
    const view = revised
      ? {
          ...design,
          document: { ...design.document, current_version: 3 },
          current_version: { ...first, version: 3, content },
          pending_versions: [],
          drift: [],
        }
      : { ...design, drift: [] }
    if (path === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(view)] })
    if (path === '/v1/system-designs/design-dispatch') return route.fulfill({ json: view })
    if (path === '/v1/system-designs/design-dispatch/versions') {
      calls.push('propose')
      expect(route.request().postDataJSON()).toEqual({ content, origin: 'operator' })
      return route.fulfill({ status: 201, json: { ...pending, version: 3, content } })
    }
    if (path === '/v1/system-designs/design-dispatch/versions/3/confirm') {
      calls.push('confirm')
      expect(route.request().headers()['if-match']).toBe('"1"')
      revised = true
      return route.fulfill({ json: {} })
    }
    return route.fulfill({ json: [] })
  })
  await page.goto('/w/demo/system-design?document=design-dispatch')
  await page.getByRole('button', { name: 'Revise', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('Proposal content')).toHaveValue(pending.content)
  await expect(dialog).toContainText('planning session session-design')
  await dialog.getByLabel('Proposal content').fill(content)
  await dialog.getByRole('button', { name: 'Propose and confirm' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Revise', exact: true })).toHaveCount(0)
  await expect(page.getByText('Corrected dispatch design', { exact: true })).toBeVisible()
  expect(calls).toEqual(['propose', 'confirm'])
})
