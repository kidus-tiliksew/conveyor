import { expect, type Page, type Route, test } from '@playwright/test'

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
  /** Every lineage request the page made, with its auth header and workspace. */
  reads?: Array<{ path: string; authorization?: string; workspace: string | null }>
  parentTaskId?: string
  lineageDelayMs?: number
  lineageFailure?: boolean
  /** Replaces the populated walk with a caller-built graph. */
  graph?: (rootType: string, rootId: string) => unknown
  /** Every evidence download, with the workspace it named. */
  downloads?: Array<{ path: string; workspace: string | null }>
}

async function routeAPI(page: Page, options: Options = {}) {
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
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
    const lineage = /^\/v1\/lineage\/([^/]+)\/(.+)$/.exec(path)
    if (lineage) {
      const [rootType, rootId] = [decodeURIComponent(lineage[1]), decodeURIComponent(lineage[2])]
      options.reads?.push({
        path,
        authorization: request.headers().authorization,
        workspace: url.searchParams.get('workspace_id'),
      })
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
    if (path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path.endsWith('/events/stream'))
      return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: '' })
    return route.fulfill({ json: [] })
  })
}

test('the task Knowledge explorer makes one canonical bounded read on demand', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto(`/tasks/${taskId}/full`)
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

  // The server bounded the walk, and the panel says so (7 = 3 nodes + 4 links).
  await expect(panel.getByText('This is a bounded view: 7 further connections were not read.')).toBeVisible()
  // Opening the panel is not navigation.
  expect(new URL(page.url()).pathname).toBe(`/tasks/${taskId}/full`)
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

  await page.goto(`/tasks/${taskId}/full`)
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
    '/tasks/task-neighbour/full',
  )
  await expect(work.getByRole('link', { name: /Retry blueprint/ })).toHaveAttribute('href', '/blueprints/task-anchor')
  await expect(work.getByRole('link', { name: /Retry planning/ })).toHaveAttribute(
    'href',
    '/planning?session=session-retries',
  )
  // The bundle resolves to its producing session through the returned link.
  await expect(work.getByRole('link', { name: /Retry bundle/ })).toHaveAttribute(
    'href',
    '/planning?session=session-retries',
  )
  // Work orders resolve to the task named by the returned dispatches link.
  await expect(work.getByRole('link', { name: /implement work order for task-explorer/ })).toHaveAttribute(
    'href',
    `/tasks/${taskId}/full`,
  )

  const documents = panel.getByRole('region', { name: 'Documents' })
  await expect(documents.getByRole('link', { name: /Retry behavior/ })).toHaveAttribute(
    'href',
    '/requirements?requirement=req-retries',
  )
  await expect(documents.getByRole('link', { name: /Dispatch ownership/ })).toHaveAttribute(
    'href',
    '/system-design?document=design-dispatch',
  )
  await expect(documents.getByRole('link', { name: /Retry overview/ })).toHaveAttribute(
    'href',
    '/requirements#reference-overview-retries-v4',
  )
  await expect(documents.getByRole('link', { name: /Usage telemetry is observational/ })).toHaveAttribute(
    'href',
    '/system-design#decision-dec-1',
  )
  // The governed path opens the governing design version the walk returned.
  await expect(documents.getByRole('link', { name: /internal\/dispatch/ })).toHaveAttribute(
    'href',
    '/system-design?document=design-dispatch&target=3',
  )
  // Record and version nodes fold into one entry per document.
  await expect(documents.getByRole('link')).toHaveCount(5)

  const delivery = panel.getByRole('region', { name: 'Delivery' })
  const pullRequest = delivery.getByRole('link', { name: /conveyor#284/ })
  await expect(pullRequest).toHaveAttribute('href', 'https://github.com/kidus-tiliksew/conveyor/pull/284')
  await expect(pullRequest).toHaveAttribute('target', '_blank')
  await expect(pullRequest).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(delivery.getByRole('link', { name: /Range abc1234/ })).toHaveAttribute(
    'href',
    'https://github.com/kidus-tiliksew/conveyor/compare/abc1234...def5678',
  )
  await expect(delivery.getByRole('link', { name: /Review verdict/ })).toHaveAttribute('href', `/tasks/${taskId}/full`)

  const evidence = panel.getByRole('region', { name: 'Evidence' })
  const download = page.waitForEvent('download')
  await evidence.getByRole('button', { name: /proof screenshot\.png/ }).click()
  expect((await download).suggestedFilename()).toMatch(/^artifact-1/)
  expect(downloads).toEqual([{ path: '/v1/artifacts/artifact-1', workspace: 'demo' }])
  await expect(panel.locator('[data-destination="none"]')).toHaveCount(0)
})

for (const origin of [
  { kind: 'requirement', path: '/requirements', heading: 'Retry behavior', group: 'Documents' },
  { kind: 'system_design', path: '/system-design', heading: 'Dispatch ownership', group: 'Documents' },
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
    ).toHaveAttribute('href', `/tasks/${taskId}/full`)
    await expect(
      panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /Serve the retry limit/ }),
    ).toHaveAttribute('href', '/tasks/task-neighbour/full')
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

  await page.goto(`/tasks/${taskId}/full`)
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
    ['Unversioned overview', 'No returned version of this product overview can be opened.'],
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

  await page.goto('/requirements')
  await expect(page.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  const documents = panel.getByRole('region', { name: 'Documents' })

  const current = documents.locator('[aria-current="true"]')
  await expect(current).toHaveCount(1)
  await expect(current).toContainText('Retry behavior')
  await expect(current).toContainText('Versions v1, v2')

  const peer = documents.getByRole('link', { name: /Budget limits/ })
  await expect(peer).toHaveAttribute('href', '/requirements?requirement=req-budget&target=5')
  await expect(peer).toContainText('Opens v5')
  // A version without a label falls back to its record ID.
  await expect(documents.getByRole('link', { name: /design-only-version/ })).toHaveAttribute(
    'href',
    '/system-design?document=design-only-version&target=7',
  )
  await expect(
    panel.getByRole('region', { name: 'Work' }).getByRole('link', { name: /task-unlabelled/ }),
  ).toHaveAttribute('href', '/tasks/task-unlabelled/full')
  // Truncated without omitted counts still says the view is bounded.
  await expect(panel.getByText('This is a bounded view.')).toBeVisible()
})

// A record with no related entries says so while retaining its current anchor.
const detailSurfaces = [
  { kind: 'task detail', path: `/tasks/${taskId}/full` },
  { kind: 'requirement detail', path: '/requirements' },
  { kind: 'System Design detail', path: '/system-design' },
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

  await page.goto(`/tasks/${taskId}`)
  const taskPanel = page.getByRole('dialog', { name: 'Task detail' })
  await expect(taskPanel).toBeVisible()

  await taskPanel.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('region', { name: 'Work' })).toBeVisible()
  await expect(reads).toHaveLength(1)

  // Escape dismisses the panel the operator opened — and only that one.
  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(taskPanel).toBeVisible()
})

test('the Knowledge explorer exposes loading and error states with updated accessible names', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, { lineageDelayMs: 200, lineageFailure: true })

  await page.goto(`/tasks/${taskId}/full`)
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()
  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('status', { name: 'Loading Knowledge explorer' })).toBeVisible()
  await expect(panel.getByText('Lineage temporarily unavailable.')).toBeVisible()
})

// AC-4.1 first half: the navigation carries exactly the §21.61 surface set.
test('primary navigation shows exactly the accepted operating surfaces', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)

  await page.goto('/')
  const nav = page.getByRole('navigation', { name: 'Primary' })
  await expect(nav.getByRole('link')).toHaveText([
    /^Board/,
    'Tasks',
    'Workspace',
    'Requirements',
    'System Design',
    'Pending proposals',
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

  await page.goto('/planning')
  await expect(page.getByRole('button', { name: 'New session' })).toBeVisible()

  await page.goto('/blueprints')
  await expect(page.getByRole('heading', { name: 'Blueprint history' })).toBeVisible()

  await page.goto('/blueprints/blueprint-anchor')
  await expect(page.getByRole('link', { name: 'Back to blueprints' })).toBeVisible()

  // A task materialized from a blueprint keeps both its anchor and the history
  // that the sidebar no longer offers.
  await page.goto(`/tasks/${taskId}/full`)
  await expect(page.getByRole('link', { name: /blueprint-anchor/ })).toHaveAttribute(
    'href',
    '/blueprints/blueprint-anchor',
  )
  await page.getByRole('link', { name: 'Blueprint history' }).click()
  await expect(page).toHaveURL(/\/blueprints$/)
})
