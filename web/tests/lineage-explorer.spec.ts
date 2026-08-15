import { expect, type Page, type Route, test } from '@playwright/test'

// The Knowledge explorer and the parked navigation.
// Task, requirement, and System Design detail each open a focused right panel
// derived only from the canonical lineage read. REQ-4: primary navigation carries exactly the accepted
// operating surfaces while the parked routes stay reachable by deep link
// (AC-4.1).

const createdAt = '2026-08-06T09:00:00Z'
const taskId = 'task-explorer'

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
      { type: 'task', id: 'task-neighbour', label: 'Serve the retry limit' },
      { type: 'work_order', id: `${taskId}-implement-1`, label: 'implement work order for task-explorer' },
      { type: 'requirement', id: 'req-retries', label: 'Retry behavior' },
      { type: 'requirement_version', id: 'req-retries:v2', label: 'Retry behavior v2' },
      { type: 'system_design', id: 'design-dispatch', label: 'Dispatch ownership' },
      { type: 'system_design_version', id: 'design-dispatch:v3', label: 'Dispatch ownership v3' },
      { type: 'decision', id: 'DEC-1', label: 'Usage telemetry is observational' },
      { type: 'pull_request', id: 'kidus-tiliksew/conveyor#284', label: 'Pull request kidus-tiliksew/conveyor#284' },
      { type: 'repository_path', id: 'conveyor:internal/dispatch/**', label: 'conveyor:internal/dispatch/**' },
      { type: 'evidence', id: 'artifact-1', label: 'proof screenshot.png' },
      { type: 'verdict', id: 'review:order-1', label: 'Review verdict order-1' },
    ],
    // Links are the walk's own record. The panel reads nodes and never renders
    // an edge, so these exist to prove it does not start.
    links: [
      {
        workspace: 'demo',
        src_type: rootType,
        src_id: rootId,
        dst_type: 'task',
        dst_id: 'task-neighbour',
        kind: 'serves',
        created_by_event_id: 1,
        created_at: createdAt,
      },
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
    sessionStorage.setItem('conveyor-token', 'test-token')
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
        json: options.empty ? emptyGraph(rootType, rootId) : populatedGraph(rootType, rootId),
      })
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

test('the task Knowledge explorer shows its focused hierarchy and canonical bounded read', async ({ page }) => {
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
  expect(reads[0].authorization).toBe('Bearer test-token')
  expect(reads[0].workspace).toBe('demo')

  await expect(panel.getByRole('heading', { level: 3 })).toHaveText([/Requirements/, /System Designs/, /Tasks/])

  const requirements = panel.getByRole('region', { name: 'Requirements' })
  await expect(requirements.getByRole('link', { name: /Retry behavior/ })).toHaveAttribute(
    'href',
    '/requirements?requirement=req-retries',
  )
  // Durable and version nodes normalize to one destination.
  await expect(requirements.getByRole('link')).toHaveCount(1)

  const designs = panel.getByRole('region', { name: 'System Designs' })
  await expect(designs.getByRole('link', { name: /Dispatch ownership/ })).toHaveAttribute(
    'href',
    '/system-design?document=design-dispatch',
  )
  await expect(designs.getByRole('link')).toHaveCount(1)

  const tasks = panel.getByRole('region', { name: 'Tasks' })
  const current = tasks.locator('[aria-current="true"]')
  await expect(current).toContainText('Bound the retry loop')
  await expect(current).toContainText('Current')
  await expect(tasks.getByRole('link')).toHaveCount(0)

  // Task context ends at the current task, and unrelated lineage kinds stay out.
  await expect(panel.getByText('Serve the retry limit')).toHaveCount(0)
  await expect(panel.getByText('Usage telemetry is observational')).toHaveCount(0)
  await expect(panel.getByText('Pull request kidus-tiliksew/conveyor#284')).toHaveCount(0)
  await expect(panel.getByText('proof screenshot.png')).toHaveCount(0)

  // The server bounded the walk, and the panel says so (7 = 3 nodes + 4 links).
  await expect(panel.getByText('This is a bounded view: 7 further connections were not read.')).toBeVisible()

  // Opening the panel is not navigation.
  expect(new URL(page.url()).pathname).toBe(`/tasks/${taskId}/full`)

  await panel.getByRole('button', { name: 'Close Knowledge explorer' }).click()
  await expect(panel).toHaveCount(0)
})

// A record with no focused relations says so while retaining its current anchor.
const detailSurfaces = [
  { kind: 'task detail', path: `/tasks/${taskId}/full` },
  { kind: 'requirement detail', path: '/requirements' },
  { kind: 'System Design detail', path: '/system-design' },
]

for (const surface of detailSurfaces) {
  test(`the explorer on ${surface.kind} accurately communicates an empty focused result`, async ({ page }) => {
    await initShell(page)
    await routeAPI(page, { empty: true })

    await page.goto(surface.path)
    await page.getByRole('button', { name: 'Knowledge explorer' }).click()
    const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
    await expect(panel.getByText('No related requirements, System Designs, or tasks are linked')).toBeVisible()
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
  await expect(panel.getByRole('region', { name: 'Tasks' })).toBeVisible()
  await expect(reads).toHaveLength(1)

  // Escape dismisses the panel the operator opened — and only that one.
  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(taskPanel).toBeVisible()
})

// AC-3.1 on the requirement canvas.
test('the requirement canvas opens the explorer for its own document', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto('/requirements')
  await expect(page.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()

  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('region', { name: 'Requirements' }).locator('[aria-current="true"]')).toContainText(
    'Retry behavior',
  )
  await expect.poll(() => reads.map((read) => read.path)).toEqual(['/v1/lineage/requirement/req-retries'])
})

// AC-3.1 on the System Design canvas.
test('the System Design canvas opens the explorer for its own document', async ({ page }) => {
  await initShell(page)
  const reads: Options['reads'] = []
  await routeAPI(page, { reads })

  await page.goto('/system-design')
  await expect(page.getByRole('heading', { name: 'Dispatch ownership' })).toBeVisible()
  await page.getByRole('button', { name: 'Knowledge explorer' }).click()

  const panel = page.getByRole('dialog', { name: 'Knowledge explorer' })
  await expect(panel.getByRole('heading', { level: 3 })).toHaveText([/Requirements/, /System Designs/, /Tasks/])
  const current = panel.getByRole('region', { name: 'System Designs' }).locator('[aria-current="true"]')
  await expect(current).toContainText('Dispatch ownership')
  await expect(
    panel.getByRole('region', { name: 'Tasks' }).getByRole('link', { name: /Serve the retry limit/ }),
  ).toHaveAttribute('href', '/tasks/task-neighbour/full')
  await expect.poll(() => reads.map((read) => read.path)).toEqual(['/v1/lineage/system_design/design-dispatch'])
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
