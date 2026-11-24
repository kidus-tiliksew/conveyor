import { expect, type Locator, type Page, type Route, type TestInfo, test } from '@playwright/test'
import { compareDocuments, reviewText } from '../src/components/documents/document-review-model'
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

// Comparison fixtures (component-web-document-surfaces "Block comparison"
// rules 13 to 15). Every text is deterministic, so a spec can run in any order.
const fillerWords =
  'each recorded transition keeps its owner and the store replays it in order so a later reader can trace why the lease moved between sessions without another source'.split(
    ' ',
  )

function fillerParagraph(seed: number, words: number) {
  const text = Array.from({ length: words }, (_, index) => fillerWords[(seed * 7 + index * 5) % fillerWords.length])
  return `Paragraph ${seed} says that ${text.join(' ')}.`
}

const largeSectionCount = 25
const largeChangedSections = [3, 12, 22]
const largeSectionTitle = (index: number) => `Section ${String(index).padStart(2, '0')}`

function largeSection(index: number, target: boolean) {
  const body = Array.from({ length: 8 }, (_, paragraph) => fillerParagraph(index * 10 + paragraph, 60))
  if (largeChangedSections.includes(index)) {
    // Padding stays in the paragraphs that do not change.
    body[3] = `The store holds every ${target ? 'archived' : 'retained'} record of section ${index}.`
    if (target) body.push(`Section ${index} also states one new rule about recovery.`)
  }
  return [`## ${largeSectionTitle(index)}`, ...body].join('\n\n')
}

// 26 headings and about 74,000 characters on each side.
function largeDocument(target: boolean) {
  const intro = `# Large design\n\n${fillerParagraph(1, 40)}`
  const sections = Array.from({ length: largeSectionCount }, (_, index) => largeSection(index + 1, target))
  return [intro, ...sections].join('\n\n')
}

function comparisonVersions(base: string, target: string) {
  return {
    confirmed: { ...first, content: base },
    proposed: { ...pending, content: target, governs: first.governs },
  }
}

function comparisonDesign(base: string, target: string): typeof design {
  const { confirmed, proposed } = comparisonVersions(base, target)
  return {
    ...design,
    current_version: confirmed,
    pending_versions: [proposed],
    versions: [confirmed, proposed],
    drift: [],
  }
}

async function openComparison(page: Page, base: string, target: string) {
  await initialize(page)
  const view = comparisonDesign(base, target)
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/system-designs') return route.fulfill({ json: [summarizeDesign(view)] })
    if (path === '/v1/system-designs/design-dispatch') return route.fulfill({ json: view })
    if (path === '/v1/system-designs/design-dispatch/events') return route.fulfill({ json: documentEventPage([]) })
    return route.fulfill({ json: [] })
  })
  await page.goto('/w/demo/system-design?document=design-dispatch&tab=changes&base=1&target=2')
  const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
  await expect(comparison).toBeVisible()
  return comparison
}

// Attaches the view at both review widths to the Playwright report, which CI
// uploads, centred on `at`. No page scrolls sideways at either width.
async function attachScreens(page: Page, testInfo: TestInfo, name: string, at: Locator) {
  const desktop = page.viewportSize()
  for (const [width, height] of [
    [1280, 900],
    [375, 812],
  ]) {
    await page.setViewportSize({ width, height })
    // At 375 px the Inbox shows its list until a row is chosen.
    if (!(await at.isVisible())) await page.locator('[data-inbox-row]').first().click()
    await at.evaluate((element) => element.scrollIntoView({ block: 'center' }))
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBe(0)
    await testInfo.attach(`${name}-${width}.png`, { body: await page.screenshot(), contentType: 'image/png' })
  }
  if (desktop) await page.setViewportSize(desktop)
}

// A paragraph of `count` words and the spaces between them: 2 * count - 1
// tokens, so a fully rewritten pair costs (2 * count) ** 2 cells.
const wordParagraph = (word: string, count: number) => Array.from({ length: count }, () => word).join(' ')

test('the comparison source bound admits exactly 500,000 combined UTF-16 units of compared text', () => {
  // "\u{1F600}" is two UTF-16 units and one character, so a count by character
  // admits more than the bound.
  const emoji = '\u{1F600}'
  const side = (units: number, word: string) => {
    const head = `# Title\n\n${word}\n\n## Padding\n\n`
    const rest = units - head.length
    return `${head}${emoji.repeat(Math.floor(rest / 2))}${rest % 2 ? 'x' : ''}`
  }
  const base = { content: side(250_000, 'retained') }
  expect(base.content).toHaveLength(250_000)
  const admitted = { content: side(250_000, 'archived') }
  const atBound = compareDocuments(base, admitted)
  expect(atBound.limited).toBe(false)
  expect(atBound.rows.map((row) => [row.title, row.changed, row.limited])).toEqual([
    ['Title', true, false],
    ['Padding', false, false],
  ])
  const rejected = { content: side(250_001, 'archived') }
  expect(base.content.length + rejected.content.length).toBe(500_001)
  const overBound = compareDocuments(base, rejected)
  expect(overBound.limited).toBe(true)
  expect(overBound.rows).toEqual([])
  expect(overBound.leftText === base.content && overBound.rightText === rejected.content).toBe(true)

  // The bound is combined, not per side.
  const lopsided = { content: `# Title\n\n${'x'.repeat(499_991)}` }
  expect(lopsided.content).toHaveLength(500_000)
  expect(compareDocuments({ content: '' }, lopsided).limited).toBe(false)
  expect(compareDocuments({ content: ' ' }, lopsided).limited).toBe(true)

  // A requirement counts its projected prose and statements, not its raw fence.
  const requirementSource = (statement: string) => ({
    content: `Intro.\n\n\`\`\`conveyor:requirements\n${'x'.repeat(400_000)}\n\`\`\``,
    statements: [{ id: 'REQ-1', statement }],
  })
  const before = requirementSource('Retries stop.')
  const after = requirementSource('Retries stop after a limit.')
  expect(reviewText(after)).toBe('Intro.\n\nREQ-1: Retries stop after a limit.')
  const projected = compareDocuments(before, after)
  expect(projected.limited).toBe(false)
  expect(projected.rows.map((row) => [row.id, row.changed])).toEqual([
    ['heading:Overview:1', false],
    ['REQ-1', true],
  ])
  const longStatement = requirementSource('x'.repeat(500_000))
  const limitedRequirement = compareDocuments(before, longStatement)
  expect(limitedRequirement.limited).toBe(true)
  expect(limitedRequirement.leftText === reviewText(before)).toBe(true)
  expect(limitedRequirement.rightText === reviewText(longStatement)).toBe(true)
})

test('the comparison block and cell bounds still fall back below the source bound', () => {
  const sections = (count: number, body: (index: number) => string) => ({
    content: Array.from({ length: count }, (_, index) => `## Part ${index}\n\n${body(index)}`).join('\n\n'),
  })

  const blocks = (count: number) => sections(count, () => 'Same text.')
  expect(compareDocuments(blocks(200), blocks(200)).limited).toBe(false)
  const overBlocks = compareDocuments(blocks(201), blocks(200))
  expect(reviewText(blocks(201)).length + reviewText(blocks(200)).length).toBeLessThan(120_000)
  expect(overBlocks.limited).toBe(true)
  expect(overBlocks.rows).toEqual([])
  expect(overBlocks.leftText).toBe(blocks(201).content)
  expect(overBlocks.rightText).toBe(blocks(200).content)

  // Each row rewrites 345 words for 476,104 cells, below the section limit alone.
  const rewritten = (count: number, word: string) => sections(count, () => wordParagraph(word, 345))
  const fourRows = compareDocuments(rewritten(4, 'alpha'), rewritten(4, 'beta'))
  expect(fourRows.limited).toBe(false)
  expect(fourRows.rows.map((row) => row.limited)).toEqual([false, false, false, false])
  expect(compareDocuments(rewritten(5, 'alpha'), rewritten(5, 'beta')).limited).toBe(true)

  // A section over its own limit spends none of the document budget.
  const oversized = (word: string) =>
    sections(6, (index) => (index < 5 ? wordParagraph(word, 500) : `A short ${word} text.`))
  const limitedRows = compareDocuments(oversized('alpha'), oversized('beta'))
  expect(limitedRows.limited).toBe(false)
  expect(limitedRows.rows.map((row) => row.limited)).toEqual([true, true, true, true, true, false])
})

test('a large System Design with localized edits renders section navigation and highlights', async ({
  page,
}, testInfo) => {
  const base = largeDocument(false)
  const target = largeDocument(true)
  for (const content of [base, target]) {
    expect(content.length).toBeGreaterThan(72_000)
    expect(content.length).toBeLessThan(76_000)
    expect(content.match(/^#{1,2} /gm)).toHaveLength(26)
  }
  expect(base.length + target.length).toBeGreaterThan(120_000)

  const comparison = await openComparison(page, base, target)
  await expect(comparison.getByText('3 changed sections', { exact: true })).toBeVisible()
  await expect(page.getByText('Diff too large')).toHaveCount(0)
  const titles = largeChangedSections.map(largeSectionTitle)
  const navigator = comparison.getByRole('navigation', { name: 'Changed sections' })
  const entries = navigator.getByRole('button', { name: /^Section \d\d$/ })
  await expect(entries).toHaveText(titles)
  const row = (title: string) => comparison.getByRole('region', { name: title, exact: true })

  const marksInEveryRow = async () => {
    for (const [index, title] of titles.entries()) {
      const marks = row(title)
      await expect(marks.locator('del').filter({ hasText: /^retained$/ })).toBeVisible()
      await expect(marks.locator('ins').filter({ hasText: /^archived$/ })).toBeVisible()
      await expect(
        marks.locator('ins').filter({ hasText: `Section ${largeChangedSections[index]} also states one new rule` }),
      ).toBeVisible()
      await expect(marks.getByText(/Detailed highlighting unavailable/)).toHaveCount(0)
    }
  }
  await marksInEveryRow()

  for (const title of [titles[2], titles[0]]) {
    await entries.filter({ hasText: title }).click()
    await expect(row(title)).toBeFocused()
    await expect(entries.filter({ hasText: title })).toHaveAttribute('aria-current', 'true')
  }
  await navigator.getByRole('button', { name: 'Next' }).click()
  await expect(row(titles[1])).toBeFocused()

  // A closed run mounts none of its Markdown.
  const runs = comparison.locator('details')
  await expect(runs.locator('summary')).toHaveText([
    'Show 3 unchanged sections',
    'Show 8 unchanged sections',
    'Show 9 unchanged sections',
    'Show 3 unchanged sections',
  ])
  await expect(comparison.locator('details[open]')).toHaveCount(0)
  await expect(runs.locator('.markdown')).toHaveCount(0)

  await attachScreens(page, testInfo, 'design-inline', row(titles[0]).locator('ins').first())

  // The keyboard opens a run, which then shows its complete sections.
  const run = runs.nth(1)
  const summary = run.locator('summary')
  await summary.focus()
  await page.keyboard.press('Enter')
  const bodies = run.locator('.markdown')
  await expect(bodies).toHaveCount(8)
  for (let index = 4; index <= 11; index++) {
    const body = bodies.nth(index - 4)
    await expect(body.getByRole('heading', { name: largeSectionTitle(index) })).toBeVisible()
    const expected = [largeSectionTitle(index), ...largeSection(index, true).split('\n\n').slice(1)]
    expect(await body.locator('h2, p').allTextContents()).toEqual(expected)
  }
  await expect(runs.nth(0).locator('.markdown')).toHaveCount(0)

  // Expansion and highlights survive a mode change.
  await comparison.getByRole('button', { name: 'Side by side', exact: true }).click()
  await expect(bodies).toHaveCount(8)
  await expect(run).toHaveAttribute('open', '')
  await marksInEveryRow()
  await expect(row(titles[0]).getByText('Target', { exact: true }).first()).toBeVisible()
  await attachScreens(page, testInfo, 'design-side-by-side', row(titles[0]).locator('ins').first())
  await attachScreens(page, testInfo, 'design-open-run', summary)
  await comparison.getByRole('button', { name: 'Inline', exact: true }).click()
  await expect(bodies).toHaveCount(8)

  await summary.focus()
  await page.keyboard.press('Space')
  await expect(run).not.toHaveAttribute('open', '')
  await expect(bodies).toHaveCount(0)

  // Another version pair starts with every run collapsed.
  await page.keyboard.press('Space')
  await expect(bodies).toHaveCount(8)
  await page.getByLabel('Base version', { exact: true }).selectOption('0')
  await expect(comparison.getByText('26 changed sections', { exact: true })).toBeVisible()
  await page.getByLabel('Base version', { exact: true }).selectOption('1')
  await expect(comparison.getByText('3 changed sections', { exact: true })).toBeVisible()
  await expect(comparison.locator('details[open]')).toHaveCount(0)
  await expect(runs.locator('.markdown')).toHaveCount(0)
})

test('a System Design over the source bound shows both complete versions as literal text', async ({
  page,
}, testInfo) => {
  const markup = '<script>window.injected = true</script>\n\n<b>bold</b> & **strong** [link](https://example.test)\n\n'
  const document = (word: string, length: number) => {
    const head = `# Oversized\n\n${markup}The ${word} text.\n\n`
    const filler = `${fillerParagraph(2, 60)}\n\n`
    const tail = `The last ${word} line.`
    return `${head}${filler.repeat(Math.ceil(length / filler.length))}`.slice(0, length - tail.length) + tail
  }
  const base = document('retained', 250_000)
  const target = document('archived', 250_001)
  expect(base.length + target.length).toBe(500_001)
  const comparison = await openComparison(page, base, target)
  await expect(comparison.getByRole('status')).toContainText(
    'Diff too large; showing both complete versions without highlighting.',
  )
  await expect(comparison.getByRole('heading', { name: 'Base version' })).toBeVisible()
  const sources = comparison.locator('pre')
  await expect(sources).toHaveCount(2)
  // Compare as booleans: a failed string match prints both 250,000-character texts.
  const texts = await sources.evaluateAll((elements) => elements.map((element) => element.textContent))
  expect(texts.map((text) => text?.length)).toEqual([base.length, target.length])
  expect([texts[0] === base, texts[1] === target]).toEqual([true, true])
  expect(texts[1]).toContain(markup)
  await expect(sources.locator('*')).toHaveCount(0)
  expect(await page.evaluate(() => 'injected' in window)).toBe(false)
  await expect(comparison.getByRole('navigation')).toHaveCount(0)
  await expect(comparison.getByText(/changed sections?/)).toHaveCount(0)
  await expect(comparison.locator('ins, del')).toHaveCount(0)
  await attachScreens(page, testInfo, 'design-document-fallback', comparison.getByRole('status'))
})

test('an oversized section and an unhighlighted block degrade alone', async ({ page }, testInfo) => {
  // 500 words cost 1,000,000 cells: over the section limit and below the
  // single-matrix limit. 800 words cost 2,560,000, over the matrix limit, so
  // that pair renders whole and its section stays eligible.
  const document = (word: string) =>
    [
      '# Bounds\n\nThe opening text stays the same.',
      `## Oversized section\n\n${wordParagraph(word, 500)}\n\nThe ${word} end of the oversized section.`,
      `## Cheap section\n\nThis line names the ${word} record.`,
      `## Unsafe block\n\nA safe ${word} neighbour.\n\nA <kbd>${word}</kbd> key in raw markup.`,
      `## Large block\n\nA second safe ${word} neighbour.\n\n${wordParagraph(word, 800)}`,
    ].join('\n\n')
  const base = document('retained')
  const target = document('archived')
  const model = compareDocuments({ content: base }, { content: target })
  expect(model.limited).toBe(false)
  expect(model.rows.map((row) => [row.title, row.changed, row.limited])).toEqual([
    ['Bounds', false, false],
    ['Oversized section', true, true],
    ['Cheap section', true, false],
    ['Unsafe block', true, false],
    ['Large block', true, false],
  ])

  const comparison = await openComparison(page, base, target)
  await expect(page.getByText('Diff too large')).toHaveCount(0)
  await expect(comparison.getByText('4 changed sections', { exact: true })).toBeVisible()
  const entries = comparison.getByRole('navigation', { name: 'Changed sections' })
  const row = (title: string) => comparison.getByRole('region', { name: title, exact: true })
  for (const mode of ['Inline', 'Side by side']) {
    await comparison.getByRole('button', { name: mode, exact: true }).click()

    const oversized = row('Oversized section')
    await entries.getByRole('button', { name: 'Oversized section', exact: true }).click()
    await expect(oversized).toBeFocused()
    await expect(oversized.getByText(/Detailed highlighting unavailable for this block/)).toHaveCount(1)
    await expect(oversized.locator('ins, del')).toHaveCount(0)
    await expect(oversized.locator('.markdown')).toHaveCount(2)
    for (const [index, word] of ['retained', 'archived'].entries())
      expect(await oversized.locator('.markdown').nth(index).locator('p').allTextContents()).toEqual([
        wordParagraph(word, 500),
        `The ${word} end of the oversized section.`,
      ])

    await expect(row('Cheap section').locator('del')).toHaveText(['retained'])
    await expect(row('Cheap section').locator('ins')).toHaveText(['archived'])
    for (const [title, whole] of [
      ['Unsafe block', (word: string) => `A <kbd>${word}</kbd> key in raw markup.`],
      ['Large block', (word: string) => wordParagraph(word, 800)],
    ] as const) {
      await expect(row(title).getByText(/Detailed highlighting unavailable for this block/)).toHaveCount(1)
      expect(await row(title).locator('.markdown').allTextContents()).toEqual([whole('retained'), whole('archived')])
      // The safe neighbour in the same section still carries its marks.
      await expect(row(title).locator('del')).toHaveText(['retained'])
      await expect(row(title).locator('ins')).toHaveText(['archived'])
    }
  }
  await comparison.getByRole('button', { name: 'Inline', exact: true }).click()
  await attachScreens(page, testInfo, 'design-section-fallback', row('Cheap section').locator('ins'))
  await attachScreens(page, testInfo, 'design-block-fallback', row('Unsafe block').locator('ins'))
})

// Requirements Changes and the Inbox preview draw the same comparison
// (req-document-operating-surfaces AC-1.1; req-260810-23b69f AC-1.2).
test('a large requirement compares on Requirements Changes and in the Inbox preview', async ({ page }, testInfo) => {
  await initialize(page)
  // 180 criteria on each side, two of them changed.
  const changedCriteria = ['AC-4.2', 'AC-9.5']
  const requirementVersion = (version: number, target: boolean) => {
    const statements = Array.from({ length: 10 }, (_, requirement) => ({
      id: `REQ-${requirement + 1}`,
      statement: `Requirement ${requirement + 1} keeps every record.`,
      acceptance_criteria: Array.from({ length: 18 }, (_, criterion) => {
        const id = `AC-${requirement + 1}.${criterion + 1}`
        const word = changedCriteria.includes(id) && target ? 'archived' : 'retained'
        return { id, statement: `The ${word} record. ${fillerParagraph(requirement * 20 + criterion, 64)}` }
      }),
    }))
    return {
      requirement_id: 'req-large',
      version,
      content: '# Large requirement\n\nThe intent stays the same.',
      statements,
      origin: 'operator',
      confirmed: !target,
      workspace: 'demo',
      created_at: '2026-08-05T09:00:00Z',
    }
  }
  const confirmed = requirementVersion(1, false)
  const proposed = requirementVersion(2, true)
  expect(reviewText(confirmed).length + reviewText(proposed).length).toBeGreaterThan(120_000)
  const requirement = {
    id: 'req-large',
    slug: 'large-requirement',
    title: 'Large requirement',
    statement_high_water_mark: 10,
    workspace: 'demo',
    created_at: '2026-08-05T08:00:00Z',
    updated_at: '2026-08-05T09:00:00Z',
  }
  const staleness = { delivery_after_intent: false, deliveries: [], active_drift: [] }
  const view = {
    requirement,
    current_version: confirmed,
    pending_versions: [proposed],
    serving_blueprints: [],
    serving_tasks: [],
    planning_sessions: [],
    artifacts: [],
    staleness,
    migrated_seed: false,
    confirmation_eligible: true,
  }
  const { content: _content, statements: _statements, ...summaryVersion } = confirmed
  await page.route('**/v1/**', async (route) => {
    const handled = shell(route)
    if (handled) return await handled
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements')
      return route.fulfill({
        json: [
          {
            requirement,
            current_version: summaryVersion,
            pending_version_count: 1,
            serving_tasks: [],
            staleness,
            confirmation_eligible: true,
          },
        ],
      })
    if (path === '/v1/requirements/req-large') return route.fulfill({ json: view })
    if (path === '/v1/requirements/req-large/versions') return route.fulfill({ json: [confirmed, proposed] })
    if (path === '/v1/requirements/req-large/events') return route.fulfill({ json: documentEventPage([]) })
    if (path === '/v1/next-actions')
      return route.fulfill({
        json: {
          items: [
            {
              band: 'decisions',
              kind: 'document_proposal',
              label: 'Requirement proposal',
              question: 'Confirm or dismiss this proposed version?',
              if_ignored: 'The proposal stays pending.',
              proposal: {
                id: 'req-large',
                title: 'Large requirement',
                tier: 'requirement',
                version: 2,
                origin_type: 'operator',
                proposed_at: '2026-08-05T09:00:00Z',
                age_seconds: 3600,
              },
            },
          ],
          acts: [],
        },
        headers: { 'X-Conveyor-Total': '1' },
      })
    if (path === '/v1/pending-proposals')
      return route.fulfill({
        json: {
          items: [],
          attention: { task_count: 0, pending_proposal_count: 1, operator_decisions: 1, total: 1 },
        },
      })
    return route.fulfill({ json: [] })
  })

  const showsComparison = async () => {
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    await expect(comparison.getByText('2 changed sections', { exact: true })).toBeVisible()
    await expect(page.getByText('Diff too large')).toHaveCount(0)
    await expect(
      comparison.getByRole('navigation', { name: 'Changed sections' }).getByRole('button', { name: /^AC-/ }),
    ).toHaveText(changedCriteria)
    for (const id of changedCriteria) {
      const row = comparison.getByRole('region', { name: id, exact: true })
      await expect(row.locator('del')).toHaveText(['retained'])
      await expect(row.locator('ins')).toHaveText(['archived'])
    }
    await expect(comparison.locator('details .markdown')).toHaveCount(0)
  }
  await page.goto('/w/demo/requirements?requirement=req-large&tab=changes&base=1&target=2')
  await expect(page.getByLabel('Target version', { exact: true })).toHaveValue('2')
  await showsComparison()
  const marks = page.getByRole('region', { name: changedCriteria[0], exact: true }).locator('ins')
  await attachScreens(page, testInfo, 'requirements-changes', marks)

  await page.goto('/w/demo/inbox')
  await expect(page.getByRole('region', { name: 'Proposed changes' })).toBeVisible()
  await expect(page.locator('[data-comparison-base]')).toHaveText('Confirmed v1 compared with proposed v2.')
  await showsComparison()
  await attachScreens(page, testInfo, 'inbox-preview', marks)
})
