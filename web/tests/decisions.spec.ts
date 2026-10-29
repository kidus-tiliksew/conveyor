import { expect, type Page, type Request, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

// The Decisions surface and each decision's page
// (req-document-operating-surfaces REQ-7, REQ-8, AC-4.2, REQ-3;
// component-web-decision-surfaces). Every /v1 read is mocked, so these specs
// prove presentation and request composition, not server behaviour.

type DecisionFixture = Record<string, unknown> & { id: string }

const entry = (document_id: string, document_tier: string, status: string) => ({
  decision_id: 'DEC-7',
  superseded_decision_id: 'DEC-4',
  document_id,
  document_tier,
  status,
  detected_by: 'decision.confirmed',
  detected_at: '2026-10-20T10:00:00Z',
})

function demoDecisions(sweepStatus = { alpha: 'open', beta: 'open' }): DecisionFixture[] {
  return [
    {
      id: 'DEC-1',
      statement: 'Run every stage in one process.',
      context: 'The first factory ran on one host.',
      alternatives_rejected: 'Separate services: too much to operate.',
      status: 'superseded',
      origin: 'operator',
      superseded_by: 'DEC-4',
      confirmed_by: 'user:founder',
      confirmed_at: '2026-08-01T09:00:00Z',
      dismissed_at: '0001-01-01T00:00:00Z',
      sweep: { clean: true, entries: [] },
      workspace: 'demo',
      created_at: '2026-08-01T08:00:00Z',
    },
    {
      id: 'DEC-4',
      statement: 'Split the control plane from the workers.',
      context: 'Workers moved to operator machines.',
      alternatives_rejected: 'Keep one process: it cannot reach operator machines.',
      status: 'superseded',
      origin: 'operator',
      supersedes: 'DEC-1',
      superseded_by: 'DEC-7',
      confirmed_by: 'user:founder',
      confirmed_at: '2026-09-01T09:00:00Z',
      sweep: { clean: true, entries: [{ ...entry('req-old', 'requirement', 'auto_cleared'), decision_id: 'DEC-4' }] },
      workspace: 'demo',
      created_at: '2026-09-01T08:00:00Z',
    },
    {
      id: 'DEC-7',
      statement:
        '(1) The control plane owns every durable transition. (2) Workers claim orders through leases. (3) A lost lease returns the order to the queue.',
      context: 'Restates DEC-4 with explicit lease ownership.',
      alternatives_rejected:
        'Workers writing state directly: two writers race. Keeping DEC-4 as written: it named no lease owner. It also left recovery implicit.',
      status: 'confirmed',
      origin: 'implementation_deliberation',
      origin_task_id: '261020-abc123',
      supersedes: 'DEC-4',
      confirmed_by: 'user:operator',
      confirmed_at: '2026-10-20T09:00:00Z',
      dismissed_at: '0001-01-01T00:00:00Z',
      sweep: {
        clean: false,
        entries: [
          entry('req-alpha', 'requirement', sweepStatus.alpha),
          entry('design-beta', 'system_design', sweepStatus.beta),
          entry('ref-overview', 'reference_document', 'auto_cleared'),
        ],
      },
      workspace: 'demo',
      created_at: '2026-10-20T08:00:00Z',
    },
    {
      id: 'DEC-9',
      statement: 'Keep the queue oldest first.',
      context: 'Lease renewal must never reorder work.',
      alternatives_rejected: 'A priority field.',
      status: 'confirmed',
      origin: 'operator',
      confirmed_by: 'user:operator',
      confirmed_at: '2026-10-21T09:00:00Z',
      sweep: { clean: true, entries: [] },
      workspace: 'demo',
      created_at: '2026-10-21T08:00:00Z',
    },
    {
      id: 'DEC-12',
      statement: 'Add a per-task autonomy ladder.',
      context: 'Proposed during planning.',
      alternatives_rejected: '',
      status: 'dismissed',
      origin: 'planning_session',
      dismissed_by: 'user:operator',
      dismissed_at: '2026-10-22T09:00:00Z',
      sweep: { clean: true, entries: [] },
      workspace: 'demo',
      created_at: '2026-10-22T08:00:00Z',
    },
    {
      id: 'DEC-40',
      // Starts with (1) but skips (2): one paragraph, not clauses (AC-8.2).
      statement: '(1) Publish the bundle with the web change. (3) Never split it.',
      context: 'The bundle is build output.',
      alternatives_rejected: 'Untracking the bundle.\nBuilding it only in CI.',
      status: 'confirmed',
      origin: 'operator',
      confirmed_by: 'user:operator',
      confirmed_at: '2026-10-23T09:00:00Z',
      sweep: { clean: true, entries: [] },
      workspace: 'demo',
      created_at: '2026-10-23T08:00:00Z',
    },
    {
      id: 'DEC-50',
      statement: 'Add a Decisions page.',
      context: 'Operators cannot find decision records.',
      alternatives_rejected: 'Keep the System Design card.',
      status: 'proposed',
      origin: 'operator',
      sweep: { clean: true, entries: [] },
      workspace: 'demo',
      created_at: '2026-10-29T08:00:00Z',
    },
  ]
}

const demoCitations = {
  items: [
    { decision_id: 'DEC-7', tier: 'requirement', document_id: 'req-alpha', title: 'Alpha requirement', version: 3 },
    { decision_id: 'DEC-7', tier: 'system_design', document_id: 'design-beta', title: 'Beta design', version: 5 },
    { decision_id: 'DEC-40', tier: 'system_design', document_id: 'design-beta', title: 'Beta design', version: 5 },
  ],
}

const otherDecisions: DecisionFixture[] = [
  {
    id: 'DEC-100',
    statement: 'Other workspace choice.',
    context: 'Belongs to Other.',
    alternatives_rejected: '',
    status: 'confirmed',
    origin: 'operator',
    confirmed_at: '2026-10-24T09:00:00Z',
    sweep: { clean: true, entries: [] },
    workspace: 'other',
    created_at: '2026-10-24T08:00:00Z',
  },
]

interface Served {
  requests: Array<{ path: string; workspace: string | null }>
  dismissals: number
  releaseOther?: () => void
}

async function serve(
  page: Page,
  options: { role?: string; holdOther?: boolean; dismissFailsFirst?: boolean } = {},
): Promise<Served> {
  const served: Served = { requests: [], dismissals: 0 }
  let sweep = { alpha: 'open', beta: 'open' }
  const otherGate = options.holdOther
    ? new Promise<void>((resolve) => {
        served.releaseOther = resolve
      })
    : Promise.resolve()
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const request: Request = route.request()
    const url = new URL(request.url())
    const workspace = url.searchParams.get('workspace_id') ?? request.headers()['x-workspace-id'] ?? null
    served.requests.push({ path: url.pathname, workspace })
    if (url.pathname === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (url.pathname === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: options.role ?? 'operator' }, request) })
    if (url.pathname === '/v1/workspace') return route.fulfill({ json: { workspace, repos: ['conveyor'] } })
    if (url.pathname === '/v1/decisions') {
      if (workspace === 'other') {
        await otherGate
        return route.fulfill({ json: otherDecisions })
      }
      return route.fulfill({ json: demoDecisions(sweep) })
    }
    if (url.pathname === '/v1/decision-citations')
      return route.fulfill({ json: workspace === 'other' ? { items: [] } : demoCitations })
    if (url.pathname === '/v1/decisions/DEC-7/sweep/requirement/req-alpha/dismiss') {
      expect(request.method()).toBe('POST')
      expect(workspace).toBe('demo')
      served.dismissals++
      if (options.dismissFailsFirst && served.dismissals === 1)
        return route.fulfill({ status: 409, json: { message: 'This flag changed; refresh and try again.' } })
      sweep = { ...sweep, alpha: 'dismissed' }
      return route.fulfill({ json: { ...entry('req-alpha', 'requirement', 'dismissed') } })
    }
    if (url.pathname === '/v1/system-designs') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })
  return served
}

const rows = (page: Page) => page.getByRole('list', { name: 'Decisions' }).getByRole('listitem')
// One row, matched by its exact DEC-n badge, so DEC-4 never matches DEC-40.
const row = (page: Page, id: string) => rows(page).filter({ has: page.getByText(id, { exact: true }) })
const rowIds = (page: Page) =>
  rows(page).evaluateAll((items) => items.map((item) => item.querySelector('.font-mono')?.textContent?.trim()))

test('primary navigation places Decisions directly after System Design and opens the surface (AC-4.2)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/')
  const nav = page.getByRole('navigation', { name: 'Primary' })
  const labels = await nav.locator('a').evaluateAll((links) => links.map((link) => link.textContent?.trim()))
  const position = labels.indexOf('System Design')
  expect(labels[position + 1]).toBe('Decisions')
  const entryLink = nav.getByRole('link', { name: 'Decisions', exact: true })
  await expect(entryLink).toHaveAttribute('href', '/w/demo/decisions')
  await entryLink.click()
  await expect(page).toHaveURL(/\/w\/demo\/decisions$/)
  await expect(page.getByRole('heading', { name: 'Decisions', level: 1 })).toBeVisible()
  await expect(entryLink).toHaveAttribute('aria-current', 'page')
})

test('the list orders by DEC-n, filters by state with counts, and shows each row meta line (AC-7.1, AC-7.2)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/decisions')
  const filters = page.getByRole('group', { name: 'Filter decisions' })
  await expect(filters.getByRole('button', { name: /In force/ })).toHaveAttribute('aria-pressed', 'true')
  await expect(filters.getByRole('button', { name: /In force/ })).toHaveText('In force3')
  await expect(filters.getByRole('button', { name: /Superseded/ })).toHaveText('Superseded2')
  await expect(filters.getByRole('button', { name: /Dismissed/ })).toHaveText('Dismissed1')
  await expect(filters.getByRole('button', { name: /All/ })).toHaveText('All7')

  // In force is the default: confirmed decisions, highest DEC-n first.
  await expect.poll(() => rowIds(page)).toEqual(['DEC-40', 'DEC-9', 'DEC-7'])
  const dec7 = row(page, 'DEC-7')
  await expect(dec7.getByRole('link')).toHaveAttribute('href', '/w/demo/decisions/DEC-7')
  // A clause-numbered statement shows clause (1) and states the clause count.
  await expect(dec7).toContainText('(1) The control plane owns every durable transition.')
  await expect(dec7).not.toContainText('Workers claim orders through leases.')
  await expect(dec7).toContainText('3 clauses')
  await expect(dec7).toContainText('Cited by 2')
  await expect(dec7).toContainText('Replaces DEC-4')
  await expect(dec7).toContainText('2 still cite DEC-4')
  await expect(dec7.locator('time')).toHaveAttribute('datetime', '2026-10-20T09:00:00Z')
  await expect(row(page, 'DEC-9')).toContainText('Cited by 0')

  await filters.getByRole('button', { name: /Superseded/ }).click()
  await expect(page).toHaveURL(/filter=superseded/)
  await expect.poll(() => rowIds(page)).toEqual(['DEC-4', 'DEC-1'])
  const dec4 = row(page, 'DEC-4')
  await expect(dec4).toContainText('Replaces DEC-1')
  await expect(dec4).toContainText('Replaced by DEC-7')
  await expect(dec4).toContainText('Superseded')

  await filters.getByRole('button', { name: /Dismissed/ }).click()
  await expect.poll(() => rowIds(page)).toEqual(['DEC-12'])
  await expect(rows(page).first().locator('time')).toHaveAttribute('datetime', '2026-10-22T09:00:00Z')

  await filters.getByRole('button', { name: /All/ }).click()
  await expect.poll(() => rowIds(page)).toEqual(['DEC-50', 'DEC-40', 'DEC-12', 'DEC-9', 'DEC-7', 'DEC-4', 'DEC-1'])
  await expect(row(page, 'DEC-50')).toContainText('Proposed')

  // The filter lives in the address, so a reload restores it.
  await page.reload()
  await expect(filters.getByRole('button', { name: /All/ })).toHaveAttribute('aria-pressed', 'true')
})

test('search matches DEC-n, statement, or context inside the chosen filter, ignoring case (AC-7.3)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/decisions')
  const search = page.getByRole('textbox', { name: 'Search decisions' })
  await search.fill('LEASE RENEWAL')
  await expect.poll(() => rowIds(page)).toEqual(['DEC-9'])
  await search.fill('dec-40')
  await expect.poll(() => rowIds(page)).toEqual(['DEC-40'])
  await search.fill('durable TRANSITION')
  await expect.poll(() => rowIds(page)).toEqual(['DEC-7'])
  // The search applies inside the filter: DEC-7 is not superseded.
  await page
    .getByRole('group', { name: 'Filter decisions' })
    .getByRole('button', { name: /Superseded/ })
    .click()
  await expect(page.getByText('No decisions match.')).toBeVisible()
  await search.fill('workers')
  await expect.poll(() => rowIds(page)).toEqual(['DEC-4'])
})

test('the attention notice counts open flags and narrows to their decisions; proposed decisions link to the Inbox (AC-7.4, AC-7.5)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/decisions')
  const notice = page.getByRole('region', { name: 'Decisions needing attention' })
  await expect(notice).toContainText('2 open flags')
  await notice.getByRole('button', { name: 'Review' }).click()
  await expect(page).toHaveURL(/flagged=true/)
  await expect.poll(() => rowIds(page)).toEqual(['DEC-7'])
  await expect(
    page.getByRole('group', { name: 'Filter decisions' }).getByRole('button', { name: /All/ }),
  ).toHaveAttribute('aria-pressed', 'true')
  await notice.getByRole('button', { name: 'Show all decisions' }).click()
  await expect.poll(() => rowIds(page)).toHaveLength(7)

  await expect(page.getByText('1 proposed decision waits for an operator.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Open the Inbox' })).toHaveAttribute(
    'href',
    '/w/demo/inbox?band=decisions',
  )
  // The surface offers no decision control of its own.
  await expect(page.getByRole('button', { name: /^(Confirm|Dismiss)$/ })).toHaveCount(0)
})

test('a decision page shows its record, clauses, chain, alternatives, and citing documents (AC-8.1 to AC-8.3, AC-8.5, REQ-3)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-7')
  await expect(page.getByRole('heading', { name: 'DEC-7', level: 1 })).toBeVisible()
  await expect(page.getByText('In force', { exact: true })).toBeVisible()
  await expect(page.getByText('Confirmed by user:operator on')).toBeVisible()
  await expect(page.getByText('Proposed while implementing a task')).toBeVisible()
  await expect(page.getByRole('link', { name: '261020-abc123' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/261020-abc123/full',
  )
  await expect(
    page.getByRole('navigation', { name: 'Breadcrumb' }).getByRole('link', { name: 'Decisions' }),
  ).toHaveAttribute('href', '/w/demo/decisions')

  const clauses = page.getByRole('region', { name: 'Statement' }).getByRole('listitem')
  await expect(clauses).toHaveCount(3)
  await expect(clauses.nth(0)).toHaveText('(1)The control plane owns every durable transition.')
  await expect(clauses.nth(2)).toHaveText('(3)A lost lease returns the order to the queue.')

  const chain = page.getByRole('region', { name: 'Version chain' })
  await expect(chain.getByRole('listitem')).toHaveText(['DEC-1', 'DEC-4', 'DEC-7'])
  await expect(chain.getByText('DEC-7')).toHaveAttribute('aria-current', 'page')
  await expect(chain.getByRole('link', { name: 'DEC-7' })).toHaveCount(0)
  await expect(chain.getByRole('link', { name: 'DEC-1' })).toHaveAttribute('href', '/w/demo/decisions/DEC-1')

  await expect(page.getByRole('region', { name: 'Why' })).toContainText('Restates DEC-4 with explicit lease ownership.')
  const alternatives = page.getByRole('region', { name: 'Alternatives rejected' }).getByRole('listitem')
  await expect(alternatives).toHaveText([
    'Workers writing state directly: two writers race.',
    'Keeping DEC-4 as written: it named no lease owner. It also left recovery implicit.',
  ])

  const cited = page.getByRole('region', { name: 'Cited by' })
  await expect(cited.getByRole('list', { name: 'Citing Requirements' })).toContainText('REQ')
  await expect(cited.getByRole('link', { name: 'Alpha requirement' })).toHaveAttribute(
    'href',
    '/w/demo/requirements?requirement=req-alpha',
  )
  await expect(cited.getByRole('list', { name: 'Citing System Design' })).toContainText('SD')
  await expect(cited.getByRole('link', { name: 'Beta design' })).toHaveAttribute(
    'href',
    '/w/demo/system-design?document=design-beta',
  )

  // The decision detail offers the lineage explorer (REQ-3).
  await expect(page.getByRole('button', { name: 'Knowledge explorer' })).toBeVisible()
})

test('a decision page copies its own scoped address', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-7')
  await page.getByRole('button', { name: 'Copy link' }).click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(`${new URL(page.url()).origin}/w/demo/decisions/DEC-7`)
})

test('a statement without ordered clause markers renders as one paragraph (AC-8.2)', async ({ page }) => {
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-40')
  const statement = page.getByRole('region', { name: 'Statement' })
  await expect(statement.getByRole('listitem')).toHaveCount(0)
  await expect(statement).toHaveText('(1) Publish the bundle with the web change. (3) Never split it.')
  await expect(page.getByRole('region', { name: 'Version chain' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Alternatives rejected' }).getByRole('listitem')).toHaveText([
    'Untracking the bundle.',
    'Building it only in CI.',
  ])
})

test('a superseded decision says it no longer governs and links its successor and the newest decision (AC-8.4)', async ({
  page,
}) => {
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-1')
  const banner = page.getByRole('region', { name: 'No longer governs' })
  await expect(banner).toContainText('DEC-1 no longer governs. DEC-4 replaced it.')
  await expect(banner.getByRole('link', { name: 'DEC-4' })).toHaveAttribute('href', '/w/demo/decisions/DEC-4')
  await expect(banner).toContainText('The newest decision in its chain is DEC-7.')
  await expect(banner.getByRole('link', { name: 'DEC-7' })).toHaveAttribute('href', '/w/demo/decisions/DEC-7')
  await expect(page.getByRole('region', { name: 'Version chain' }).getByText('DEC-1')).toHaveAttribute(
    'aria-current',
    'page',
  )
  await banner.getByRole('link', { name: 'DEC-4' }).click()
  await expect(page).toHaveURL(/\/w\/demo\/decisions\/DEC-4$/)
  await expect(page.getByRole('heading', { name: 'DEC-4', level: 1 })).toBeVisible()
})

test('a proposed decision waits for an operator and links its Inbox item (AC-8.7)', async ({ page }) => {
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-50')
  const waiting = page.getByRole('region', { name: 'Waiting for an operator' })
  await expect(waiting).toContainText('DEC-50 waits for an operator decision.')
  const href = await waiting.getByRole('link', { name: 'Open it in the Inbox' }).getAttribute('href')
  const target = new URL(href ?? '', 'http://dashboard.test')
  expect(target.pathname).toBe('/w/demo/inbox')
  expect(target.searchParams.get('band')).toBe('decisions')
  expect(target.searchParams.get('selected')).toBe('decision:DEC-50')
  await expect(page.getByRole('button', { name: /^(Confirm|Dismiss)$/ })).toHaveCount(0)
})

test('a caller who may confirm documents dismisses an open flag, and a refusal stays beside it (AC-8.6)', async ({
  page,
}) => {
  const served = await serve(page, { dismissFailsFirst: true })
  await page.goto('/w/demo/decisions/DEC-7')
  const cleanup = page.getByRole('region', { name: 'Cleanup after replacing DEC-4' })
  const open = cleanup.getByRole('list', { name: 'Documents still citing DEC-4' })
  await expect(open.getByRole('listitem')).toHaveCount(2)
  await expect(open.getByRole('link', { name: 'req-alpha' })).toHaveAttribute(
    'href',
    '/w/demo/requirements?requirement=req-alpha',
  )
  await expect(open.getByRole('link', { name: 'design-beta' })).toHaveAttribute(
    'href',
    '/w/demo/system-design?document=design-beta',
  )
  const alpha = open.getByRole('listitem').filter({ hasText: 'req-alpha' })
  await expect(alpha).toContainText('Requirement')
  await expect(alpha).toContainText('Open')
  await alpha.getByRole('button', { name: 'Dismiss' }).click()
  await expect(alpha).toContainText('This flag changed; refresh and try again.')
  await alpha.getByRole('button', { name: 'Dismiss' }).click()
  await expect.poll(() => served.dismissals).toBe(2)
  await expect(open.getByRole('listitem')).toHaveCount(1)
  await expect(cleanup.getByText('2 cleared')).toBeVisible()
  await cleanup.getByText('2 cleared').click()
  const cleared = cleanup.getByRole('list', { name: 'Cleared flags for DEC-4' })
  await expect(cleared.getByRole('link', { name: 'ref-overview' })).toHaveAttribute(
    'href',
    '/w/demo/requirements#reference-ref-overview-v0',
  )
  await expect(cleared.getByRole('listitem').filter({ hasText: 'req-alpha' })).toContainText('Dismissed')
  await expect(cleared.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('a caller without confirm_documents sees open flags without Dismiss (AC-8.6)', async ({ page }) => {
  await serve(page, { role: 'maintainer' })
  await page.goto('/w/demo/decisions/DEC-7')
  const open = page.getByRole('list', { name: 'Documents still citing DEC-4' })
  await expect(open.getByRole('listitem')).toHaveCount(2)
  await expect(open.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('an unknown DEC-n says the workspace has no such decision and links the surface (AC-8.8)', async ({ page }) => {
  await serve(page)
  await page.goto('/w/demo/decisions/DEC-999')
  const missing = page.getByRole('region', { name: 'Decision not found' })
  await expect(missing).toContainText('This workspace has no decision DEC-999.')
  await expect(missing.getByRole('link', { name: 'Open Decisions' })).toHaveAttribute('href', '/w/demo/decisions')
})

test('the earlier System Design decision address opens the decision page (AC-8.9)', async ({ page }) => {
  await serve(page)
  await page.goto('/w/demo/system-design#decision-dec-7')
  await expect(page).toHaveURL(/\/w\/demo\/decisions\/DEC-7$/)
  await expect(page.getByRole('heading', { name: 'DEC-7', level: 1 })).toBeVisible()
  // An unscoped legacy address resolves under the remembered workspace first.
  await page.goto('/system-design#decision-dec-4')
  await expect(page).toHaveURL(/\/w\/demo\/decisions\/DEC-4$/)
  await expect(page.getByRole('heading', { name: 'DEC-4', level: 1 })).toBeVisible()
})

test('both reads name the address workspace and show no data from another workspace', async ({ page }) => {
  const served = await serve(page, { holdOther: true })
  await page.goto('/w/demo/decisions')
  await expect.poll(() => rowIds(page)).toEqual(['DEC-40', 'DEC-9', 'DEC-7'])
  for (const path of ['/v1/decisions', '/v1/decision-citations'])
    expect(served.requests.filter((request) => request.path === path).map((request) => request.workspace)).toEqual(
      expect.arrayContaining(['demo']),
    )

  await page.goto('/w/other/decisions')
  // While Other's list is still loading, nothing from Demo renders.
  await expect(page.getByRole('status', { name: 'Loading decisions' })).toBeVisible()
  await expect(page.getByText('DEC-7')).toHaveCount(0)
  served.releaseOther?.()
  await expect.poll(() => rowIds(page)).toEqual(['DEC-100'])
  await expect(page.getByText('DEC-40')).toHaveCount(0)
  const otherReads = served.requests.filter(
    (request) =>
      (request.path === '/v1/decisions' || request.path === '/v1/decision-citations') && request.workspace === 'other',
  )
  expect(new Set(otherReads.map((request) => request.path))).toEqual(
    new Set(['/v1/decisions', '/v1/decision-citations']),
  )

  // A decision page reads the workspace its address names.
  await page.goto('/w/other/decisions/DEC-7')
  await expect(page.getByRole('region', { name: 'Decision not found' })).toBeVisible()
})
