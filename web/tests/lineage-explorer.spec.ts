import { expect, type Page, type Route } from '@playwright/test'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { serveNextActionsFromActivity } from './helpers/next-action-stub'

// The Knowledge explorer and the parked navigation.
// Task, requirement, and System Design detail each open a focused right panel
// derived only from the canonical lineage read. REQ-4: primary navigation carries exactly the accepted
// operating surfaces while the parked routes stay reachable by deep link
// (AC-4.1).

const createdAt = '2026-08-06T09:00:00Z'
const taskId = 'task-explorer'

function link(srcType: string, srcId: string, dstType: string, dstId: string, kind: string, id: number) {
  return {
    workspace: 'demo',
    src_type: srcType,
    src_id: srcId,
    dst_type: dstType,
    dst_id: dstId,
    kind,
    created_by_event_id: id,
    created_at: createdAt,
  }
}

// One node of each of the eighteen lineage kinds (conveyor:internal/core/lineage.go),
// with the owner links the server projects for work orders, verdicts,
// bundles, and governed paths.
function populatedGraph(rootType: string, rootId: string) {
  const rootLabel =
    rootType === 'task'
      ? 'Bound the retry loop'
      : rootType === 'system_design'
        ? 'Dispatch ownership'
        : 'Retry behavior'
  return {
    roots: [{ type: rootType, id: rootId, label: rootLabel }],
    nodes: [
      { type: rootType, id: rootId, label: rootLabel },
      { type: 'task', id: taskId, label: 'Bound the retry loop' },
      { type: 'task', id: 'task-neighbour', label: 'Serve the retry limit' },
      { type: 'blueprint', id: 'task-anchor', label: 'Retry blueprint' },
      { type: 'blueprint_version', id: 'task-anchor:v2', label: 'Retry blueprint v2' },
      { type: 'planning_session', id: 'session-retries', label: 'Retry planning' },
      { type: 'planning_bundle', id: 'bundle-retries', label: 'Retry bundle' },
      { type: 'work_order', id: `${taskId}-implement-1`, label: 'implement work order for task-explorer' },
      { type: 'requirement', id: 'req-retries', label: 'Retry behavior' },
      { type: 'requirement_version', id: 'req-retries:v2', label: 'Retry behavior v2' },
      { type: 'system_design', id: 'design-dispatch', label: 'Dispatch ownership' },
      { type: 'system_design_version', id: 'design-dispatch:v3', label: 'Dispatch ownership v3' },
      { type: 'reference_document', id: 'overview-retries', label: 'Retry overview' },
      { type: 'reference_document_version', id: 'overview-retries:v4', label: 'Retry overview v4' },
      { type: 'decision', id: 'DEC-1', label: 'Usage telemetry is observational' },
      { type: 'repository_path', id: 'conveyor:internal/dispatch/**', label: 'conveyor:internal/dispatch/**' },
      { type: 'pull_request', id: 'kidus-tiliksew/conveyor#284', label: 'Pull request kidus-tiliksew/conveyor#284' },
      { type: 'commit_range', id: 'kidus-tiliksew/conveyor@abc1234..def5678', label: 'Range abc1234..def5678' },
      { type: 'verdict', id: `review:${taskId}-review-1`, label: 'Review verdict' },
      { type: 'work_order', id: `${taskId}-review-1`, label: 'review work order for task-explorer' },
      { type: 'evidence', id: 'artifact-1', label: 'proof screenshot.png' },
    ].filter(
      (node, index, all) => all.findIndex((other) => other.type === node.type && other.id === node.id) === index,
    ),
    links: [
      link(rootType, rootId, 'task', 'task-neighbour', 'serves', 1),
      link('task', taskId, 'work_order', `${taskId}-implement-1`, 'dispatches', 2),
      link('task', taskId, 'work_order', `${taskId}-review-1`, 'dispatches', 3),
      link('work_order', `${taskId}-review-1`, 'verdict', `review:${taskId}-review-1`, 'produced_verdict', 4),
      link('planning_session', 'session-retries', 'planning_bundle', 'bundle-retries', 'produced_bundle', 5),
      link(
        'system_design_version',
        'design-dispatch:v3',
        'repository_path',
        'conveyor:internal/dispatch/**',
        'governs',
        6,
      ),
    ],
    truncated: true,
    omitted_nodes: 3,
    omitted_links: 4,
    budget: { max_depth: 5, max_nodes: 32, max_links: 128 },
  }
}

function emptyGraph(rootType: string, rootId: string) {
  return {
    roots: [{ type: rootType, id: rootId, label: 'The record itself' }],
    // The root is always in the walk; on its own it means nothing is related.
    nodes: [{ type: rootType, id: rootId, label: 'The record itself' }],
    links: [],
    truncated: false,
    omitted_nodes: 0,
    omitted_links: 0,
  }
}

const requirement = {
  requirement: {
    id: 'req-retries',
    slug: 'retry-behavior',
    title: 'Retry behavior',
    statement_high_water_mark: 1,
    workspace: 'demo',
    created_at: createdAt,
    updated_at: createdAt,
  },
  current_version: {
    requirement_id: 'req-retries',
    version: 1,
    content: 'Keep retries bounded.',
    statements: [{ id: 'REQ-1', statement: 'Retries stop after a finite limit.' }],
    origin: 'operator',
    confirmed: true,
    workspace: 'demo',
    created_at: createdAt,
  },
  pending_versions: [],
  serving_tasks: [],
  serving_blueprints: [],
  planning_sessions: [],
  artifacts: [],
  lineage: [],
  migrated_seed: false,
  confirmation_eligible: true,
}

const designVersion = {
  document_id: 'design-dispatch',
  version: 1,
  content: '# Dispatch\n\nThe dispatcher owns durable stage transitions.',
  governs: [{ repository: 'conveyor', paths: ['internal/dispatch/**'] }],
  origin: 'operator',
  confirmed: true,
  workspace: 'demo',
  created_at: createdAt,
}

const design = {
  document: {
    id: 'design-dispatch',
    slug: 'dispatch',
    title: 'Dispatch ownership',
    category: 'Architecture',
    current_version: 1,
    workspace: 'demo',
    created_at: createdAt,
    updated_at: createdAt,
  },
  current_version: designVersion,
  pending_versions: [],
  versions: [designVersion],
  lineage: [],
  drift: [],
}

const designSummary = {
  document: design.document,
  current_version: {
    document_id: designVersion.document_id,
    version: designVersion.version,
    origin: designVersion.origin,
    confirmed: designVersion.confirmed,
    workspace: designVersion.workspace,
    created_at: designVersion.created_at,
  },
  pending_versions: [],
  pending_version_count: 0,
  drift_count: 0,
}

function task(id: string, parentTaskId?: string) {
  return {
    id,
    workspace: 'demo',
    source: 'operator',
    title: 'Bound the retry loop',
    body: '',
    class: 'feature',
    level: '',
    spec_approval: true,
    merge_approval: false,
    policy_version: 1,
    setup: 'default',
    setup_contract: {
      name: 'default',
      execution_settings: {
        control_plane: { triage: { model: 'control', timeout: '20m' } },
        spec: { harness: 'codex', model: 'gpt-spec', model_policy: 'explicit', timeout: '30m' },
        implementation: {
          harness: 'claude',
          model: 'claude-opus',
          model_policy: 'explicit',
          effort: 'high',
          timeout: '4h',
        },
        review: { execution: 'mcp', timeout: '1h' },
      },
      review: { seats: [] },
    },
    repo: 'conveyor',
    base_branch: 'main',
    branch: `conveyor/${id}`,
    state: 'running',
    parent_task_id: parentTaskId,
    origin_spec_version: parentTaskId ? 1 : undefined,
    created_at: createdAt,
  }
}

function taskActivity(id: string, parentTaskId?: string) {
  return {
    task: task(id, parentTaskId),
    jobs: [],
    events: [],
    interventions: [],
    work_orders: [],
    attachments: [],
    verification_evidence: [],
    needs_attention: false,
  }
}

async function initShell(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
}

interface Options {
  /** Lineage roots that answer with a walk carrying no related records. */
  empty?: boolean
  /**
   * Every lineage walk that delivered a response, with its auth header,
   * workspace, and includes. Reads consume the query's abort signal, so
   * StrictMode's discarded first mount aborts a read that never reaches the
   * page and is not counted.
   */
  reads?: Array<{ path: string; authorization?: string; workspace: string | null; include?: string | null }>
  parentTaskId?: string
  lineageDelayMs?: number
  lineageFailure?: boolean
  /** The impact read's answer, and every impact request with its query. */
  impact?: unknown
  impactReads?: Array<{ path: string; workspace: string | null; outdated: string | null }>
  /** Replaces the populated walk with a caller-built graph. */
  graph?: (rootType: string, rootId: string) => unknown
  /** Every evidence download, with the workspace it named. */
  downloads?: Array<{ path: string; workspace: string | null }>
  /** The blueprint projection; empty by default. */
  blueprints?: unknown[]
}

async function routeAPI(page: Page, options: Options = {}) {
  page.on('requestfinished', (request) => {
    const url = new URL(request.url())
    if (/^\/v1\/lineage\/[^/]+\/[^/]+\/impact$/.test(url.pathname)) {
      options.impactReads?.push({
        path: url.pathname,
        workspace: url.searchParams.get('workspace_id'),
        outdated: url.searchParams.get('outdated'),
      })
    } else if (/^\/v1\/lineage\/[^/]+\/.+$/.test(url.pathname)) {
      options.reads?.push({
        path: url.pathname,
        authorization: request.headers().authorization,
        workspace: url.searchParams.get('workspace_id'),
        include: url.searchParams.get('include'),
      })
    }
  })
  await serveNextActionsFromActivity(page)
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', repos: [{ name: 'conveyor', base: 'main' }] } })
    if (path === '/v1/activity') return route.fulfill({ json: [] })
    if (path === '/v1/workspace/config')
      return route.fulfill({
        json: {
          version: 1,
          document: {
            workspace: 'demo',
            planning_models: ['gpt-plan'],
            execution_settings: {
              control_plane: {
                triage: { model: 'control', timeout: '20m' },
                planning: { model: 'gpt-plan', effort: 'high', timeout: '30m' },
              },
            },
            routing: { stages: { review: {} } },
            review: { seats: [] },
            setups: [task(taskId).setup_contract],
            default_setup: 'default',
            execution: {},
            harnesses: [],
            repos: [{ name: 'conveyor', base: 'main' }],
          },
        },
      })
    const impact = /^\/v1\/lineage\/([^/]+)\/([^/]+)\/impact$/.exec(path)
    if (impact) {
      return route.fulfill({ json: options.impact ?? { items: [], total: 0, truncated: false } })
    }
    const lineage = /^\/v1\/lineage\/([^/]+)\/(.+)$/.exec(path)
    if (lineage) {
      const [rootType, rootId] = [decodeURIComponent(lineage[1]), decodeURIComponent(lineage[2])]
      if (options.lineageDelayMs) await new Promise((resolve) => setTimeout(resolve, options.lineageDelayMs))
      if (options.lineageFailure) {
        return route.fulfill({ status: 503, body: 'Lineage temporarily unavailable.' })
      }
      return route.fulfill({
        json: options.empty
          ? emptyGraph(rootType, rootId)
          : options.graph
            ? options.graph(rootType, rootId)
            : populatedGraph(rootType, rootId),
      })
    }
    const artifact = /^\/v1\/artifacts\/([^/]+)$/.exec(path)
    if (artifact) {
      options.downloads?.push({ path, workspace: url.searchParams.get('workspace_id') })
      return route.fulfill({ status: 200, contentType: 'image/png', body: 'png-bytes' })
    }
    const detail = /^\/v1\/tasks\/([^/]+)\/activity$/.exec(path)
    if (detail) return route.fulfill({ json: taskActivity(decodeURIComponent(detail[1]), options.parentTaskId) })
    if (path === '/v1/requirements') return route.fulfill({ json: [requirement] })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (path === '/v1/requirements/req-retries/versions') return route.fulfill({ json: [requirement.current_version] })
    if (path === '/v1/system-designs') return route.fulfill({ json: [designSummary] })
    if (path === '/v1/system-designs/design-dispatch') return route.fulfill({ json: design })
    if (path === '/v1/blueprints') return route.fulfill({ json: options.blueprints ?? [] })
    if (path.endsWith('/events/stream'))
      return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: '' })
    return route.fulfill({ json: [] })
  })
}

test('the task Knowledge explorer makes one canonical bounded read on demand', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  // On-demand: nothing is read until the affordance is used (REQ-3).
  await expect(page.getByRole('heading', { name: 'Bound the retry loop' })).toBeVisible()
  expect(reads).toHaveLength(0)

  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel).toBeVisible()

  // The read is the canonical lineage route, authenticated and workspace-scoped.
  await expect.poll(() => reads.length).toBe(1)
  expect(reads[0].path).toBe(`/v1/lineage/task/${taskId}`)
  expect(reads[0].authorization).toBeUndefined()
  expect(reads[0].workspace).toBe('demo')
  expect(reads[0].include).toBe('depth,path,status')

  // The server bounded the walk, and the panel says so.
  await expect(panel.getByText('This is a bounded view.')).toBeVisible()
  await expect(panel.getByText('3 further records and 4 further links were not read.')).toBeVisible()
  // Opening the panel is not navigation.
  expect(new URL(page.url()).pathname).toBe(`/w/demo/tasks/${taskId}/full`)
  await panel.getByRole('button', { name: 'Close Knowledge explorer' }).click()
  await expect(panel).toHaveCount(0)
})

// AC-3.1: every returned kind lands in one of the four groups and links to its
// surface; AC-3.2: every destination comes from the entry's own identity or
// from a link the same response returned.
test('lineage renders work documents delivery and evidence', async ({ page }) => {
  await initShell(page)
  const downloads: Options['downloads'] = []
  await routeAPI(page, { downloads })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('heading', { level: 3 })).toHaveText([/Work/, /Documents/, /Delivery/, /Evidence/])

  const work = panel.getByRole('region', { name: 'Work' })
  const current = work.locator('[aria-current="true"]')
  await expect(current).toContainText('Bound the retry loop')
  await expect(current).toContainText('Current')
  // Peers of the origin's own kind are listed too.
  await expect(work.getByRole('link', { name: /Serve the retry limit/ })).toHaveAttribute(
    'href',
    '/w/demo/tasks/task-neighbour/full',
  )
  await expect(work.getByRole('link', { name: /Retry blueprint/ })).toHaveAttribute(
    'href',
    '/w/demo/blueprints/task-anchor',
  )
  await expect(work.getByRole('link', { name: /Retry planning/ })).toHaveAttribute(
    'href',
    '/w/demo/planning?session=session-retries',
  )
  // The bundle resolves to its producing session through the returned link.
  await expect(work.getByRole('link', { name: /Retry bundle/ })).toHaveAttribute(
    'href',
    '/w/demo/planning?session=session-retries',
  )
  // Work orders resolve to the task named by the returned dispatches link.
  await expect(work.getByRole('link', { name: /implement work order for task-explorer/ })).toHaveAttribute(
    'href',
    `/w/demo/tasks/${taskId}/full`,
  )

  const documents = panel.getByRole('region', { name: 'Documents' })
  await expect(documents.getByRole('link', { name: /Retry behavior/ })).toHaveAttribute(
    'href',
    '/w/demo/requirements?requirement=req-retries',
  )
  await expect(documents.getByRole('link', { name: /Dispatch ownership/ })).toHaveAttribute(
    'href',
    '/w/demo/system-design?document=design-dispatch',
  )
  await expect(documents.getByRole('link', { name: /Retry overview/ })).toHaveAttribute(
    'href',
    '/w/demo/requirements#reference-overview-retries-v4',
  )
  // A decision opens its own page (req-document-operating-surfaces AC-8.9).
  await expect(documents.getByRole('link', { name: /Usage telemetry is observational/ })).toHaveAttribute(
    'href',
    '/w/demo/decisions/DEC-1',
  )
  // The governed path opens the governing design version the walk returned.
  await expect(documents.getByRole('link', { name: /internal\/dispatch/ })).toHaveAttribute(
    'href',
    '/w/demo/system-design?document=design-dispatch&target=3',
  )
  // Record and version nodes fold into one entry per document, and each
  // returned version is its own row under that entry.
  await expect(documents.getByRole('link')).toHaveCount(8)
  await expect(documents.getByRole('list', { name: 'Versions of Retry behavior' }).getByRole('link')).toHaveAttribute(
    'href',
    '/w/demo/requirements?requirement=req-retries&target=2',
  )
  // A reference document is labelled as one, not as a product overview.
  await expect(documents.getByRole('link', { name: /Retry overview/ })).toContainText('Reference document')
  await expect(panel.getByText('Product overview')).toHaveCount(0)

  const delivery = panel.getByRole('region', { name: 'Delivery' })
  const pullRequest = delivery.getByRole('link', { name: /conveyor#284/ })
  await expect(pullRequest).toHaveAttribute('href', 'https://github.com/kidus-tiliksew/conveyor/pull/284')
  await expect(pullRequest).toHaveAttribute('target', '_blank')
  await expect(pullRequest).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(delivery.getByRole('link', { name: /Range abc1234/ })).toHaveAttribute(
    'href',
    'https://github.com/kidus-tiliksew/conveyor/compare/abc1234...def5678',
  )
  await expect(delivery.getByRole('link', { name: /Review verdict/ })).toHaveAttribute(
    'href',
    `/w/demo/tasks/${taskId}/full`,
  )

  const evidence = panel.getByRole('region', { name: 'Evidence' })
  const download = page.waitForEvent('download')
  await evidence.getByRole('button', { name: /proof screenshot\.png/ }).click()
  expect((await download).suggestedFilename()).toMatch(/^artifact-1/)
  expect(downloads).toEqual([{ path: '/v1/artifacts/artifact-1', workspace: 'demo' }])
  await expect(panel.locator('[data-destination="none"]')).toHaveCount(0)
})

for (const origin of [
  { kind: 'requirement', path: '/w/demo/requirements', heading: 'Retry behavior', group: 'Documents' },
  { kind: 'system_design', path: '/w/demo/system-design', heading: 'Dispatch ownership', group: 'Documents' },
]) {
  test(`lineage renders work documents delivery and evidence from the ${origin.kind} canvas`, async ({ page }) => {
    await initShell(page)
    const reads: Options['reads'] = []
    await routeAPI(page, { reads })

    await page.goto(origin.path)
    await expect(page.getByRole('heading', { name: origin.heading })).toBeVisible()
    await page.getByRole('button', { name: 'Knowledge explorer' }).click()
    const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
    await expect(panel.getByRole('heading', { level: 3 })).toHaveText([/Work/, /Documents/, /Delivery/, /Evidence/])
    await expect(panel.getByRole('region', { name: origin.group }).locator('[aria-current="true"]')).toContainText(
      origin.heading,
    )
    // From a document, tasks are ordinary links to their full detail.
    await expect(
      panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /Bound the retry loop/ }),
    ).toHaveAttribute('href', `/w/demo/tasks/${taskId}/full`)
    await expect(
      panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /Serve the retry limit/ }),
    ).toHaveAttribute('href', '/w/demo/tasks/task-neighbour/full')
    const rootId = origin.kind === 'requirement' ? 'req-retries' : 'design-dispatch'
    await expect.poll(() => reads.map((read) => read.path)).toEqual([`/v1/lineage/${origin.kind}/${rootId}`])
  })
}

// AC-3.2: the panel never guesses an owner or builds a link from an identity
// it cannot validate. Each unresolved entry still renders and says why.
test('lineage links only returned ownership', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, {
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Bound the retry loop' }],
      nodes: [
        { type: rootType, id: rootId, label: 'Bound the retry loop' },
        // The ID looks like it belongs to the current task, but no link says so.
        { type: 'work_order', id: `${taskId}-implement-9`, label: 'Orphaned work order' },
        { type: 'verdict', id: `review:${taskId}-review-9`, label: 'Orphaned verdict' },
        { type: 'planning_bundle', id: 'bundle-orphan', label: 'Orphaned bundle' },
        { type: 'repository_path', id: 'conveyor:internal/orphan/**', label: 'Ungoverned path' },
        { type: 'reference_document', id: 'overview-unversioned', label: 'Unversioned overview' },
        { type: 'pull_request', id: '42', label: 'Legacy pull request 42' },
        { type: 'pull_request', id: 'conveyor#284', label: 'Pull request without owner' },
        { type: 'pull_request', id: '../evil#1', label: 'Traversing pull request' },
        { type: 'pull_request', id: 'acme/app#1"><script>', label: 'Injected pull request' },
        { type: 'commit_range', id: 'acme/app@not-a-sha..def5678', label: 'Malformed range' },
      ],
      // A link of another kind between the same nodes is not ownership.
      links: [link('task', taskId, 'work_order', `${taskId}-implement-9`, 'consulted', 1)],
      truncated: false,
      omitted_nodes: 0,
      omitted_links: 0,
    }),
  })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('heading', { level: 3 })).toHaveText([/Work/, /Documents/, /Delivery/])
  // Nothing but the current record, and no link at all.
  await expect(panel.getByRole('link')).toHaveCount(0)
  const unresolved = panel.locator('[data-destination="none"]')
  await expect(unresolved).toHaveCount(10)
  for (const [label, reason] of [
    ['Orphaned work order', 'No returned link names the task that dispatched this work order.'],
    ['Orphaned verdict', 'No returned links name the work order and task behind this verdict.'],
    ['Orphaned bundle', 'No returned link names the planning session that produced this bundle.'],
    ['Ungoverned path', 'No returned System Design version governs this path.'],
    ['Unversioned overview', 'No returned version of this reference document can be opened.'],
    ['Legacy pull request 42', 'Not a canonical owner/repository#number identity'],
    ['Pull request without owner', 'Not a canonical owner/repository#number identity'],
    ['Traversing pull request', 'Not a canonical owner/repository#number identity'],
    ['Injected pull request', 'Not a canonical owner/repository#number identity'],
    ['Malformed range', 'Not a canonical owner/repository@base..head identity'],
  ]) {
    await expect(unresolved.filter({ hasText: label })).toContainText(reason)
  }
})

// Versions fold into their record, peers stay, the origin is marked, labels
// fall back to IDs, and a bounded walk says so.
test('lineage preserves versions peers and bounded notices', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, {
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Retry behavior' }],
      nodes: [
        { type: rootType, id: rootId, label: 'Retry behavior' },
        { type: 'requirement_version', id: 'req-retries:v2', label: 'Retry behavior v2' },
        { type: 'requirement_version', id: 'req-retries:v1', label: 'Retry behavior v1' },
        // A peer requirement the walk returned only as versions opens the
        // newest one it returned.
        { type: 'requirement_version', id: 'req-budget:v5', label: 'Budget limits v5' },
        { type: 'requirement_version', id: 'req-budget:v3', label: 'Budget limits v3' },
        { type: 'system_design_version', id: 'design-only-version:v7' },
        { type: 'task', id: 'task-unlabelled' },
      ],
      links: [],
      truncated: true,
      omitted_nodes: 0,
      omitted_links: 0,
    }),
  })

  await page.goto('/w/demo/requirements')
  await expect(page.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  const documents = panel.getByRole('region', { name: 'Documents' })

  const current = documents.locator('[aria-current="true"]')
  await expect(current).toHaveCount(1)
  await expect(current).toContainText('Retry behavior')
  // Each returned version is listed on its own row, newest first.
  await expect(documents.getByRole('list', { name: 'Versions of Retry behavior' }).locator(':scope > li')).toHaveText([
    /v2/,
    /v1/,
  ])

  const peer = documents.getByRole('link', { name: /Budget limits/ })
  await expect(peer).toHaveAttribute('href', '/w/demo/requirements?requirement=req-budget&target=5')
  await expect(peer).toContainText('Opens v5')
  // A version without a label falls back to its record ID.
  await expect(documents.getByRole('link', { name: /design-only-version/ })).toHaveAttribute(
    'href',
    '/w/demo/system-design?document=design-only-version&target=7',
  )
  await expect(
    panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /task-unlabelled/ }),
  ).toHaveAttribute('href', '/w/demo/tasks/task-unlabelled/full')
  // Truncated without omitted counts still says the view is bounded.
  await expect(panel.getByText('This is a bounded view.')).toBeVisible()
})

// A walk node as the server renders it with include=depth,path,status: its
// depth counts the edge path's links from the root.
function walked(type: string, id: string, label: string, path: unknown[], status?: unknown) {
  return { type, id, label, depth: path.length, edge_path: path, ...(status ? { status } : {}) }
}

// Root-relative grouping: a task's own verdict and pull request stay with the
// record, while a sibling task's, reached through a shared requirement, are
// listed under "Through other tasks" and name that task. Verdicts show their
// returned outcome, round, and seat.
test('lineage groups entries relative to the root and names the task they come through', async ({ page }) => {
  await initShell(page)
  const toRequirement = link('requirement', 'req-retries', 'task', taskId, 'serves', 1)
  const toSibling = link('requirement', 'req-retries', 'task', 'task-neighbour', 'serves', 2)
  const ownOrder = link('task', taskId, 'work_order', `${taskId}-review-1`, 'dispatches', 3)
  const ownVerdict = link(
    'work_order',
    `${taskId}-review-1`,
    'verdict',
    `review:${taskId}-review-1`,
    'produced_verdict',
    4,
  )
  const ownPull = link('task', taskId, 'pull_request', 'kidus-tiliksew/conveyor#284', 'opened', 5)
  const siblingOrder = link('task', 'task-neighbour', 'work_order', 'task-neighbour-review-1', 'dispatches', 6)
  const siblingVerdict = link(
    'work_order',
    'task-neighbour-review-1',
    'verdict',
    'review:task-neighbour-review-1',
    'produced_verdict',
    7,
  )
  const siblingPull = link('task', 'task-neighbour', 'pull_request', 'kidus-tiliksew/conveyor#300', 'opened', 8)
  await routeAPI(page, {
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Bound the retry loop' }],
      nodes: [
        walked(rootType, rootId, 'Bound the retry loop', [], { state: 'running' }),
        walked('requirement', 'req-retries', 'Retry behavior', [toRequirement]),
        walked('task', 'task-neighbour', 'Serve the retry limit', [toRequirement, toSibling], { state: 'merged' }),
        walked('work_order', `${taskId}-review-1`, 'review work order for task-explorer', [ownOrder]),
        walked('verdict', `review:${taskId}-review-1`, `Review verdict ${taskId}-review-1`, [ownOrder, ownVerdict], {
          outcome: 'approve',
          review_round: 2,
          review_seat: 1,
        }),
        walked('pull_request', 'kidus-tiliksew/conveyor#284', 'Pull request kidus-tiliksew/conveyor#284', [ownPull]),
        walked('work_order', 'task-neighbour-review-1', 'review work order for task-neighbour', [
          toRequirement,
          toSibling,
          siblingOrder,
        ]),
        walked(
          'verdict',
          'review:task-neighbour-review-1',
          'Review verdict task-neighbour-review-1',
          [toRequirement, toSibling, siblingOrder, siblingVerdict],
          { outcome: 'changes_requested', review_round: 1, review_seat: 2 },
        ),
        walked('pull_request', 'kidus-tiliksew/conveyor#300', 'Pull request kidus-tiliksew/conveyor#300', [
          toRequirement,
          toSibling,
          siblingPull,
        ]),
      ],
      links: [toRequirement, toSibling, ownOrder, ownVerdict, ownPull, siblingOrder, siblingVerdict, siblingPull],
      truncated: false,
    }),
  })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  const delivery = panel.getByRole('region', { name: 'Delivery', exact: true })
  const deliveryThrough = panel.getByRole('region', { name: 'Delivery through other tasks' })

  // The task's own verdict shows its returned outcome, round, and seat
  // instead of "Review verdict <id>", and no "via".
  const ownVerdictEntry = delivery.getByRole('link', { name: /^Review verdict Approved/ })
  await expect(ownVerdictEntry).toContainText('Review round 2, seat 1')
  await expect(ownVerdictEntry).not.toContainText('via')
  await expect(ownVerdictEntry).toHaveAttribute('href', `/w/demo/tasks/${taskId}/full`)
  await expect(delivery.getByText(`Review verdict ${taskId}-review-1`)).toHaveCount(0)
  await expect(deliveryThrough.getByRole('link', { name: /conveyor#284/ })).toHaveCount(0)

  // The sibling's verdict and pull request come through the sibling task.
  const siblingVerdictEntry = deliveryThrough.getByRole('link', { name: /Changes requested/ })
  await expect(siblingVerdictEntry).toContainText('Review round 1, seat 2')
  await expect(siblingVerdictEntry).toContainText('via Serve the retry limit')
  await expect(siblingVerdictEntry).toHaveAttribute('href', '/w/demo/tasks/task-neighbour/full')
  await expect(deliveryThrough.getByRole('link', { name: /conveyor#300/ })).toContainText('via Serve the retry limit')

  // The sibling task itself is reached through the requirement, not through
  // another task, so it stays in the group's own list; its work order does not.
  const work = panel.getByRole('region', { name: 'Work', exact: true })
  await expect(work.getByRole('link', { name: /Serve the retry limit/ }).first()).toHaveAttribute(
    'href',
    '/w/demo/tasks/task-neighbour/full',
  )
  await expect(
    panel.getByRole('region', { name: 'Work through other tasks' }).getByRole('link', {
      name: /review work order for task-neighbour/,
    }),
  ).toContainText('via Serve the retry limit')
  await expect(panel.getByRole('region', { name: 'Documents through other tasks' })).toHaveCount(0)
})

// Each returned version is listed with its returned status and the tasks a
// returned serves link from it names, with their state. Requirement detail
// offers the impact read's "merged on an older version" view.
test('lineage lists each version with its status and served tasks and offers the impact view', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  const impactReads: Options['impactReads'] = []
  const v1 = link('requirement', 'req-retries', 'requirement_version', 'req-retries:v1', 'versions', 1)
  const v2 = link('requirement', 'req-retries', 'requirement_version', 'req-retries:v2', 'versions', 2)
  const v3 = link('requirement', 'req-retries', 'requirement_version', 'req-retries:v3', 'versions', 3)
  const servesOld = link('requirement_version', 'req-retries:v1', 'task', 'task-old', 'serves', 4)
  const servesNew = link('requirement_version', 'req-retries:v2', 'task', taskId, 'serves', 5)
  const peer = link('task', taskId, 'system_design_version', 'design-retired:v2', 'governed_by', 6)
  await routeAPI(page, {
    reads,
    impactReads,
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Retry behavior' }],
      nodes: [
        walked(rootType, rootId, 'Retry behavior', [], { current_version: 2 }),
        walked('requirement_version', 'req-retries:v1', 'Retry behavior v1', [v1], {
          version_status: 'superseded',
          current_version: 2,
        }),
        walked('requirement_version', 'req-retries:v2', 'Retry behavior v2', [v2], {
          version_status: 'current',
          current_version: 2,
        }),
        walked('requirement_version', 'req-retries:v3', 'Retry behavior v3', [v3], {
          version_status: 'dismissed',
          current_version: 2,
        }),
        walked('task', 'task-old', 'Old retry task', [v1, servesOld], { state: 'merged' }),
        walked('task', taskId, 'Bound the retry loop', [v2, servesNew], { state: 'running' }),
        walked('system_design_version', 'design-retired:v2', 'Retired design v2', [v2, servesNew, peer], {
          version_status: 'superseded',
          current_version: 3,
          archived: true,
        }),
      ],
      links: [v1, v2, v3, servesOld, servesNew, peer],
      truncated: false,
    }),
    impact: {
      root: { type: 'requirement', id: 'req-retries' },
      current_version: 2,
      outdated_only: true,
      items: [
        {
          version: 1,
          version_status: 'superseded',
          relation: 'serves',
          task: { type: 'task', id: 'task-old', label: 'Old retry task' },
          task_state: 'merged',
          created_by_event_id: 4,
        },
      ],
      total: 3,
      limit: 100,
      offset: 0,
      next_offset: 1,
      truncated: true,
      exhaustion_reasons: ['tasks'],
    },
  })

  await page.goto('/w/demo/requirements')
  await expect(page.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  const documents = panel.getByRole('region', { name: 'Documents', exact: true })

  const versions = documents.getByRole('list', { name: 'Versions of Retry behavior' })
  const rows = versions.locator(':scope > li')
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0)).toContainText('v3 dismissed')
  await expect(rows.nth(1)).toContainText('v2 current')
  await expect(rows.nth(2)).toContainText('v1 superseded')
  await expect(rows.nth(2).getByRole('link', { name: /v1/ })).toHaveAttribute(
    'href',
    '/w/demo/requirements?requirement=req-retries&target=1',
  )
  // Each version names the tasks it serves, with their returned state.
  const onV1 = versions.getByRole('list', { name: 'Tasks on v1' })
  await expect(onV1.getByRole('link', { name: 'Old retry task' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/task-old/full',
  )
  await expect(onV1).toContainText('Merged')
  await expect(versions.getByRole('list', { name: 'Tasks on v2' })).toContainText('Running')
  await expect(versions.getByRole('list', { name: 'Tasks on v3' })).toHaveCount(0)

  // An archived peer says so beside its returned version status.
  const retired = documents.getByRole('link', { name: /Retired design/ })
  await expect(retired).toContainText('Archived')
  await expect(documents.getByRole('list', { name: 'Versions of Retired design v2' })).toContainText('v2 superseded')

  // The impact view is offered, not read, until asked for.
  expect(impactReads).toHaveLength(0)
  await documents.getByRole('button', { name: 'Show work merged on an older version' }).click()
  const impact = documents.getByRole('region', { name: 'Merged on an older version' })
  await expect(impact.getByRole('link', { name: 'Old retry task' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/task-old/full',
  )
  await expect(impact).toContainText('v1 superseded · Merged')
  await expect(impact).toContainText('Showing 1 of 3.')
  await expect(impact).toContainText('The impact read was bounded by the task limit.')
  await expect
    .poll(() => impactReads)
    .toEqual([{ path: '/v1/lineage/requirement/req-retries/impact', workspace: 'demo', outdated: 'true' }])
  expect(reads).toHaveLength(1)
})

test('task detail does not offer the impact view', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('region', { name: 'Work', exact: true })).toBeVisible()
  await expect(panel.getByRole('button', { name: 'Show work merged on an older version' })).toHaveCount(0)
})

// A truncated walk names each exhausted budget axis with its limit, and an
// entry whose destination link is missing says the budget may have omitted
// it rather than that it does not exist.
test('a truncated walk names its exhausted limits and budget-omitted destinations', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, {
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Bound the retry loop' }],
      nodes: [
        { type: rootType, id: rootId, label: 'Bound the retry loop' },
        { type: 'work_order', id: 'task-far-implement-1', label: 'Far work order' },
        { type: 'verdict', id: 'review:task-far-review-1', label: 'Far verdict' },
        { type: 'repository_path', id: 'conveyor:internal/far/**', label: 'Far path' },
      ],
      links: [],
      truncated: true,
      budget: { max_depth: 5, max_nodes: 32, max_links: 128, max_fanout: 8 },
      omitted_nodes: 2,
      omitted_links: 4,
      exhaustion_reasons: ['depth', 'fanout', 'links'],
      omissions: { depth_nodes: 2, node_cap_nodes: 0, fanout_nodes: 1, links: 4 },
    }),
  })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByText('This is a bounded view.')).toBeVisible()
  await expect(panel.getByRole('list', { name: 'Exhausted limits' }).getByRole('listitem')).toHaveText([
    'Depth limit of 5 steps reached: at least 2 records lie deeper.',
    'Per-record link limit of 8 links reached: 1 record has more links than were read.',
    'Link limit of 128 links reached: 4 links were dropped.',
  ])
  await expect(panel.getByText('2 further records and 4 further links were not read.')).toBeVisible()

  const unresolved = panel.locator('[data-destination="none"]')
  for (const [label, reason] of [
    [
      'Far work order',
      'This bounded view may have omitted the link that names the task that dispatched this work order.',
    ],
    [
      'Far verdict',
      'This bounded view may have omitted the links that name the work order and task behind this verdict.',
    ],
    [
      'Far path',
      'This bounded view may have omitted the link that names the System Design version governing this path.',
    ],
  ]) {
    await expect(unresolved.filter({ hasText: label })).toContainText(reason)
  }
  await expect(panel.getByText(/No returned link/)).toHaveCount(0)
})

// The blueprint detail page has no version selector: it shows the blueprint's
// governing version. A walk that returned only an older blueprint version must
// therefore link the record and say the returned version is not selected,
// rather than promise to open it.
test('a version-only blueprint links its record without promising the returned version', async ({ page }) => {
  await initShell(page)
  const anchorId = 'task-anchor-only'
  await routeAPI(page, {
    graph: (rootType, rootId) => ({
      roots: [{ type: rootType, id: rootId, label: 'Bound the retry loop' }],
      nodes: [
        { type: rootType, id: rootId, label: 'Bound the retry loop' },
        { type: 'blueprint_version', id: `${anchorId}:v2`, label: 'Retry blueprint v2' },
      ],
      links: [],
      truncated: false,
      omitted_nodes: 0,
      omitted_links: 0,
    }),
    blueprints: [
      {
        task: { ...task(anchorId), title: 'Retry blueprint' },
        spec: {
          task_id: anchorId,
          version: 3,
          content: '## Plan\n\nThe governing plan.',
          acceptance_count: 0,
          acceptance: [],
          decomposition: [],
          approved: true,
          created_at: createdAt,
          approved_at: createdAt,
        },
        governing_version: 3,
        children: [],
        delivery: { state: 'in_delivery', total: 0, merged: 0, closed: 0, open: 0 },
        serves: [],
        events: [],
        artifacts: [],
      },
    ],
  })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  const entry = panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /Retry blueprint/ })
  // The record route, with no version selection.
  await expect(entry).toHaveAttribute('href', `/w/demo/blueprints/${anchorId}`)
  await expect(entry).toContainText('Returned v2; opens the blueprint record without selecting a version')
  await expect(entry).not.toContainText('Opens v2')

  // The record shows its governing version, which differs from the returned one.
  await entry.click()
  await expect(page).toHaveURL(new RegExp(`/w/demo/blueprints/${anchorId}$`))
  await expect(page.getByText('Blueprint v3', { exact: true })).toBeVisible()
  await expect(page.getByText('Blueprint v2', { exact: true })).toHaveCount(0)
})

// A record with no related entries says so while retaining its current anchor.
const detailSurfaces = [
  { kind: 'task detail', path: `/w/demo/tasks/${taskId}/full` },
  { kind: 'requirement detail', path: '/w/demo/requirements' },
  { kind: 'System Design detail', path: '/w/demo/system-design' },
]

for (const surface of detailSurfaces) {
  test(`the explorer on ${surface.kind} accurately communicates an empty result`, async ({ page }) => {
    await initShell(page)
    await routeAPI(page, { empty: true })

    await page.goto(surface.path)
    await page.getByRole('button', { name: 'Knowledge explorer' }).click()
    const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
    await expect(
      panel.getByText('No related work, documents, delivery, or evidence are linked to this record yet.'),
    ).toBeVisible()
    await expect(panel.locator('[aria-current="true"]')).toHaveCount(1)
    await expect(panel.getByText('This is a bounded view')).toHaveCount(0)
  })
}

// AC-3.1: the task panel is the second task-detail surface, and the explorer
// opens over it without dismissing the panel underneath.
test('the explorer opens from the task detail panel without closing it', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto(`/w/demo/tasks/${taskId}`)
  const taskPanel = page.getByRole('dialog', { name: 'Task detail' })
  await expect(taskPanel).toBeVisible()

  await taskPanel.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('region', { name: 'Work' })).toBeVisible()
  await expect.poll(() => reads.length).toBe(1)

  // Escape dismisses the panel the operator opened — and only that one.
  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(taskPanel).toBeVisible()
})

test('the Knowledge explorer exposes loading and error states with updated accessible names', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, { lineageDelayMs: 200, lineageFailure: true })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('status', { name: 'Loading Knowledge explorer' })).toBeVisible()
  await expect(panel.getByText('Lineage temporarily unavailable.')).toBeVisible()
})

// The error state offers Retry, which reads the same walk again.
test('the Knowledge explorer error state retries the same read', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  // The route reads the toggle per request, so walks fail until it flips.
  const options: Options = { reads, lineageFailure: true }
  await routeAPI(page, options)

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByText('Lineage temporarily unavailable.')).toBeVisible()
  const failedReads = reads.length
  expect(failedReads).toBeGreaterThan(0)

  options.lineageFailure = false
  await panel.getByRole('button', { name: 'Retry' }).click()
  await expect(panel.getByRole('region', { name: 'Work', exact: true })).toBeVisible()
  await expect(panel.getByText('Lineage temporarily unavailable.')).toHaveCount(0)
  // Retry made exactly one more read of the same walk.
  await expect.poll(() => reads.length).toBe(failedReads + 1)
  expect(new Set(reads.map((read) => `${read.path}?${read.include}&${read.workspace}`))).toEqual(
    new Set([`/v1/lineage/task/${taskId}?depth,path,status&demo`]),
  )
})

// The walk names the shell's workspace context, the one its query key holds,
// even when the stored selection changes underneath the page.
test('the Knowledge explorer reads the context workspace explicitly', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(page.getByRole('heading', { name: 'Bound the retry loop' })).toBeVisible()
  await page.evaluate(() => localStorage.setItem('conveyor-workspace', 'elsewhere'))
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  await expect(
    page.getByRole('dialog', { name: 'Knowledge explorer' }).getByRole('region', { name: 'Work' }),
  ).toBeVisible()
  await expect.poll(() => reads.map((read) => read.workspace)).toEqual(['demo'])
})

// req-document-operating-surfaces AC-4.1 first half and AC-4.2: the
// navigation carries exactly the accepted operating surface set, with
// Decisions directly after System Design.
test('primary navigation shows exactly the accepted operating surfaces', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)

  await page.goto('/')
  const nav = page.getByRole('navigation', { name: 'Primary' })
  await expect(nav.getByRole('link')).toHaveText([
    'Board',
    /^Inbox/,
    'Workspace',
    'Requirements',
    'System Design',
    'Decisions',
    'Monitor',
    'Settings',
  ])
  await expect(nav.getByRole('link', { name: 'Planning' })).toHaveCount(0)
  await expect(nav.getByRole('link', { name: 'Blueprint history' })).toHaveCount(0)
})

// AC-4.1 second half: parked is not deleted — every withdrawn route still
// resolves by deep link, and blueprint history reaches from task detail.
test('the parked routes stay reachable by deep link and from blueprint-era task detail', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, { parentTaskId: 'blueprint-anchor' })

  await page.goto('/w/demo/planning')
  await expect(page.getByRole('button', { name: 'New session' })).toBeVisible()

  await page.goto('/w/demo/blueprints')
  await expect(page.getByRole('heading', { name: 'Blueprint history' })).toBeVisible()

  await page.goto('/w/demo/blueprints/blueprint-anchor')
  await expect(page.getByRole('link', { name: 'Back to blueprints' })).toBeVisible()

  // A task materialized from a blueprint keeps both its anchor and the history
  // that the sidebar no longer offers.
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(page.getByRole('link', { name: /blueprint-anchor/ })).toHaveAttribute(
    'href',
    '/w/demo/blueprints/blueprint-anchor',
  )
  await page.getByRole('link', { name: 'Blueprint history' }).click()
  await expect(page).toHaveURL(/\/blueprints$/)
})
