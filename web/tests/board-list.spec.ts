import { expect, type Page, type Route } from '@playwright/test'
import type { TaskFlow, TaskFlowStage, TaskFlowStatus, TaskOperationsGroup } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

// Task detail opens on its Timeline tab; pin it so a persisted Flow choice
// can never hide the activity these cases check (component-web-task-surfaces,
// Activity tabs and the task flow view).
test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', 'timeline'))
})

// UI coverage for the Board's list view. The projection is mocked so each
// assertion reads the rendering of durable authority rather than a live
// pipeline. Every fixture states the server's next action, because the
// server groups the list by its actor (req-operator-next-action AC-6.5).
const operations = [
  {
    task: {
      id: 'task-blocked',
      workspace: 'demo',
      source: 'planning_bundle:bundle-1',
      title: 'Wire the Tasks view',
      repo: 'conveyor',
      branch: 'conveyor/task-blocked',
      state: 'queued',
      next_stage: 'implement',
      created_at: '2026-08-06T10:00:00Z',
      blocking_task_ids: ['task-dead', 'task-open'],
      dependencies: [
        { id: 'task-dead', title: 'Abandoned prerequisite', state: 'closed' },
        { id: 'task-done', title: 'Delivered prerequisite', state: 'merged' },
        { id: 'task-open', title: 'Running prerequisite', state: 'running' },
      ],
      context: {
        requirements: [{ id: 'req-tasks-view', title: 'Task-centric operations view', version: 1 }],
        // A real document title is written for the document, not for a table
        // column, so the fixture carries one long enough to press on the Context
        // cell the way the corpus does.
        designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle and MCP surface', version: 16 }],
      },
    },
    latest_stage: 'spec',
    last_event_at: '2026-08-06T11:00:00Z',
    needs_attention: false,
    unsatisfiable_task_ids: ['task-dead'],
    plan: { state: 'pending_gate', version: 2 },
    next_action: {
      label: 'Waiting on another task',
      actor: 'dependency',
      kind: 'blocked',
      judgment: false,
      summary: 'Running prerequisite has to merge before this task can start.',
      deadline: null,
    },
  },
  {
    task: {
      id: 'task-anchor',
      workspace: 'demo',
      source: 'cli',
      title: 'Historical anchor',
      repo: 'conveyor',
      branch: 'conveyor/task-anchor',
      state: 'running',
      created_at: '2026-08-05T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-06T09:00:00Z',
    needs_attention: false,
    child_rollup: { total: 3, merged: 1, closed: 1, open: 1 },
    plan: { state: 'approved', version: 1, legacy: true },
    next_action: {
      label: 'Implementing',
      actor: 'agent',
      kind: 'working',
      judgment: false,
      summary: 'An agent is implementing the approved plan.',
      deadline: null,
    },
  },
  {
    task: {
      id: 'task-shipped',
      workspace: 'demo',
      source: 'cli',
      title: 'Shipped web change',
      repo: 'web',
      branch: 'web/task-shipped',
      state: 'merged',
      assignee: {
        user_id: 'usr-assigned',
        email: 'assigned@example.test',
        display_name: 'Assigned User',
      },
      created_at: '2026-08-04T10:00:00Z',
    },
    last_event_at: '2026-08-05T09:00:00Z',
    needs_attention: false,
    plan: { state: 'none' },
    pull_request: { url: 'https://example.test/web/pull/42', number: 42 },
    next_action: {
      label: 'Merged',
      actor: 'none',
      kind: 'terminal',
      judgment: false,
      summary: 'The task merged.',
      deadline: null,
    },
  },
  {
    task: {
      id: 'task-stuck',
      workspace: 'demo',
      source: 'cli',
      title: 'Stuck conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-stuck',
      state: 'queued',
      next_stage: 'implement',
      assignee: {
        user_id: 'usr-assigned',
        email: 'assigned@example.test',
        display_name: 'Assigned User',
      },
      created_at: '2026-08-02T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-03T09:00:00Z',
    // The list-scoped summary: the reason, never the work order
    // (component-web-task-surfaces). The detail surfaces are where the order itself renders.
    stalled: {
      needed: true,
      reason: 'dispatch is failing repeatedly',
      last_failure: 'harness exited before completing work order',
    },
    needs_attention: true,
    plan: { state: 'approved', version: 1 },
    next_action: {
      label: 'Implementation keeps failing',
      actor: 'operator',
      kind: 'job_failure',
      judgment: true,
      summary: 'The implementation order failed three times; recover it with direction or close the task.',
      deadline: null,
    },
  },
  {
    task: {
      id: 'task-bounced',
      workspace: 'demo',
      source: 'cli',
      title: 'Bounced web plan',
      repo: 'web',
      branch: 'web/task-bounced',
      state: 'queued',
      next_stage: 'spec',
      created_at: '2026-08-03T10:00:00Z',
    },
    last_event_at: '2026-08-04T09:00:00Z',
    needs_attention: false,
    plan: { state: 'redirected', version: 1 },
    next_action: {
      label: 'Queued for planning',
      actor: 'factory',
      kind: 'queued',
      judgment: false,
      summary: 'The plan order is queued for an eligible agent.',
      deadline: null,
    },
  },
]

type OperationFixture = {
  task: { id: string; state: string; created_at: string } & Record<string, unknown>
  next_action?: { actor: string } & Record<string, unknown>
} & Record<string, unknown>

// The server's actor-to-group table (component-http-api, Grouped
// task-operations read). The mock stands in for it so the browser's own
// rendering is what each case reads.
function groupOf(item: OperationFixture): TaskOperationsGroup {
  switch (item.next_action?.actor) {
    case 'operator':
      return 'operator'
    case 'agent':
      return 'agent'
    case 'factory':
    case 'dependency':
      return 'waiting'
    case 'none':
      return 'terminal'
    default:
      return ['merged', 'closed'].includes(item.task.state) ? 'terminal' : 'waiting'
  }
}

const groupHeaders: Record<TaskOperationsGroup, string> = {
  operator: 'X-Conveyor-Group-Operator-Total',
  agent: 'X-Conveyor-Group-Agent-Total',
  waiting: 'X-Conveyor-Group-Waiting-Total',
  terminal: 'X-Conveyor-Group-Terminal-Total',
}

// Answers one grouped read the way the server does: the selected group's page
// in the given order, its total, and all four group counts over the whole
// matched set. `counts` overrides the stated counts so a case can prove the
// headings read the headers rather than the rows.
function fulfillGrouped(
  route: Route,
  items: OperationFixture[],
  options: { limit?: number; counts?: Partial<Record<TaskOperationsGroup, number>> } = {},
) {
  const params = new URL(route.request().url()).searchParams
  const group = params.get('group') as TaskOperationsGroup | null
  const offset = Number(params.get('offset') ?? '0')
  const limit = options.limit ?? Number(params.get('limit') ?? String(items.length))
  const counts: Record<TaskOperationsGroup, number> = { operator: 0, agent: 0, waiting: 0, terminal: 0 }
  for (const item of items) counts[groupOf(item)]++
  const members = group ? items.filter((item) => groupOf(item) === group) : items
  const stated = { ...counts, ...options.counts }
  const headers: Record<string, string> = {
    'content-type': 'application/json',
    'X-Conveyor-Total': String(members.length),
    'X-Conveyor-Limit': String(limit),
    'X-Conveyor-Offset': String(offset),
  }
  if (group)
    for (const name of Object.keys(groupHeaders) as TaskOperationsGroup[])
      headers[groupHeaders[name]] = String(stated[name])
  return route.fulfill({ body: JSON.stringify(members.slice(offset, offset + limit)), headers })
}

function stage(
  id: TaskFlowStage['id'],
  label: string,
  status: TaskFlowStatus,
  gate: TaskFlowStage['gate'] = null,
): TaskFlowStage {
  return {
    id,
    label,
    gate,
    gate_label: gate ? (gate === 'plan_approval' ? 'Plan approval' : 'Merge approval') : '',
    gate_status: gate ? 'upcoming' : null,
    gate_status_label: '',
    gate_status_text: '',
    status,
    status_label: {
      done: 'Done',
      in_progress: 'In progress',
      waiting: 'Waiting',
      needs_operator: 'Your approval',
      off: 'Skipped',
      upcoming: 'Upcoming',
    }[status],
    status_text: '',
    description: '',
    missed_check_in: '',
    traversed: status !== 'upcoming' && status !== 'off',
    gate_traversed: false,
    attempts: [],
    current_attempt: null,
  }
}

// Each fixture's stage path, as the flow read serves it (feature-operator-next-action
// NA-7). The row draws exactly these statuses and never derives one.
const flowStatuses: Record<string, { current: TaskFlowStage['id'] | null; statuses: TaskFlowStatus[] }> = {
  'task-anchor': { current: 'implement', statuses: ['done', 'done', 'in_progress', 'off', 'upcoming', 'upcoming'] },
  'task-blocked': { current: 'implement', statuses: ['done', 'done', 'waiting', 'off', 'upcoming', 'upcoming'] },
  'task-stuck': { current: 'implement', statuses: ['done', 'done', 'needs_operator', 'off', 'upcoming', 'upcoming'] },
  'task-bounced': { current: 'spec', statuses: ['done', 'waiting', 'upcoming', 'off', 'upcoming', 'upcoming'] },
}

function flowFor(taskId: string): TaskFlow {
  const { current, statuses } = flowStatuses[taskId] ?? flowStatuses['task-anchor']
  const ids: [TaskFlowStage['id'], string, TaskFlowStage['gate']][] = [
    ['triage', 'Triage', null],
    ['spec', 'Plan', 'plan_approval'],
    ['implement', 'Implement', null],
    ['verify', 'Verify', null],
    ['review', 'Review', null],
    ['merge', 'Merged', 'merge_approval'],
  ]
  return {
    stages: ids.map(([id, label, gate], index) => stage(id, label, statuses[index], gate)),
    loops: [],
    side: { blocked_by: [], retrying: null, parked: false, closed: false, agent_question: false, labels: {} },
    current_stage: current,
    as_of: '2026-08-06T12:00:00Z',
    display: {
      legend: [],
      side_labels: { blocked_by: 'b', retrying: 'r', parked: 'p', closed: 'c', agent_question: 'q' },
      skipped_label: 'skipped',
      plan_bypass_label: 'bypass',
      from_any_step_label: 'any',
    },
    next_action: {
      label: 'x',
      actor: 'agent',
      kind: 'working',
      judgment: false,
      summary: 'x',
      deadline: null,
      choices: [],
    },
    raw: { state: 'running', next_stage: 'implement', recovery_stage: '', gate_kind: '', order_ids: [] },
  } as TaskFlow
}
// The confirmed corpora the shared filter family reads its requirement and
// design options from (AC-2.4). Only a confirmed document can be attached to a
// task, so only a confirmed document is offered.
const requirementCorpus = [
  { requirement: { id: 'req-tasks-view', title: 'Task-centric operations view' }, current_version: { version: 1 } },
  { requirement: { id: 'req-draft', title: 'Unconfirmed idea' }, current_version: null },
]
const designCorpus = [
  {
    document: { id: 'design-lifecycle', title: 'Work-order lifecycle and MCP surface' },
    current_version: { version: 16 },
  },
]

// The mock stands in for the server-side predicate, so a surface that filtered
// in the browser instead would return the whole fixture and fail (AC-2.3).
function matchOperations(url: string) {
  const params = new URL(url).searchParams
  // Each list member repeats its parameter and matches on any listed value,
  // exactly as parseTaskFilter reads it on the server (AC-2.4).
  const states = params.getAll('state')
  const repositories = params.getAll('repository')
  const requirements = params.getAll('serves_requirement')
  const designs = params.getAll('governing_design')
  // Assignee is single-valued: empty is inactive, "unassigned" selects the
  // tasks nobody holds, anything else is an exact user ID (REQ-4).
  const assignee = params.get('assignee') ?? ''
  const needle = (params.get('q') ?? '').toLowerCase()
  // Updated is last activity: the row's last event, or creation before any.
  const from = params.get('updated_from') ?? ''
  const to = params.get('updated_to') ?? ''
  return operations
    .filter((item) => {
      if (states.length && !states.includes(item.task.state)) return false
      if (repositories.length && !repositories.includes(item.task.repo)) return false
      if (assignee === 'unassigned' && item.task.assignee) return false
      if (assignee && assignee !== 'unassigned' && item.task.assignee?.user_id !== assignee) return false
      if (
        needle &&
        ![item.task.title, item.task.id, item.task.source, item.task.branch]
          .filter(Boolean)
          .some((field) => field.toLowerCase().includes(needle))
      ) {
        return false
      }
      const updated = item.last_event_at || item.task.created_at
      if (from && updated < from) return false
      // The upper bound is exclusive, exactly as the store evaluates it.
      if (to && updated >= to) return false
      if (
        requirements.length &&
        !(item.task.context?.requirements ?? []).some((entry) => requirements.includes(entry.id))
      ) {
        return false
      }
      if (designs.length && !(item.task.context?.designs ?? []).some((entry) => designs.includes(entry.id))) {
        return false
      }
      return true
    })
    .sort((a, b) => {
      const createdOrder = new Date(b.task.created_at).getTime() - new Date(a.task.created_at).getTime()
      return createdOrder || a.task.id.localeCompare(b.task.id)
    })
}

async function routeTasksSurface(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  // The repository filter reads the configured repositories, so this fixture
  // carries the real config.Repo shape rather than bare names.
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: {
        workspace: 'demo',
        repos: [
          { name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' },
          { name: 'web', url: 'https://example.test/web', base: 'main' },
        ],
      },
    }),
  )
  await page.route('**/v1/requirements**', (route) => route.fulfill({ json: requirementCorpus }))
  await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: designCorpus }))
  // The signed-in user, which is what "My tasks" resolves "my" against.
  await page.route('**/v1/me**', (route) =>
    route.fulfill({
      json: callerIdentity(
        {
          id: 'usr-assigned',
          email: 'assigned@example.test',
          display_name: 'Assigned User',
          role: 'operator',
        },
        route.request(),
      ),
    }),
  )
  await page.route('**/v1/workspaces/*/members**', (route) =>
    route.fulfill({
      json: [
        {
          workspace_id: 'demo',
          user_id: 'usr-assigned',
          display_name: 'Assigned User',
          role: 'contributor',
          created_at: '2026-08-02T10:00:00Z',
        },
        {
          workspace_id: 'demo',
          user_id: 'usr-other',
          display_name: 'Other Member',
          role: 'contributor',
          created_at: '2026-08-02T10:00:00Z',
        },
      ],
    }),
  )
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/tasks/*/activity**', (route) => {
    const taskId = /\/v1\/tasks\/([^/]+)\/activity/.exec(new URL(route.request().url()).pathname)?.[1] ?? ''
    const match = operations.find((item) => item.task.id === taskId)
    return route.fulfill({
      json: {
        task: match?.task ?? { id: taskId, workspace: 'demo', title: taskId, state: 'queued' },
        jobs: [],
        events: [],
        interventions: [],
        work_orders: [],
        checkout_available: false,
        checkout_guidance: '',
        needs_attention: false,
      },
    })
  })
  await page.route('**/v1/task-operations?**', (route) => {
    expect(route.request().headers().authorization).toBeUndefined()
    // Mirror the server contract: every filter narrows the set before it is
    // grouped and paged, and the page bounds and group counts travel in the
    // additive response headers.
    return fulfillGrouped(route, matchOperations(route.request().url()))
  })
  await page.route('**/v1/tasks/*/flow**', (route) => {
    const taskId = /\/v1\/tasks\/([^/]+)\/flow/.exec(new URL(route.request().url()).pathname)?.[1] ?? ''
    return route.fulfill({ json: { flow: flowFor(taskId) } })
  })
}

// Opens the list view. Done starts collapsed (AC-6.5); most cases read every
// group, so they expand it first.
async function openTasks(page: Page, { done = true }: { done?: boolean } = {}) {
  await routeTasksSurface(page)
  await page.goto('/?view=list')
  await expect(page.getByRole('button', { name: 'List', pressed: true })).toBeVisible()
  if (done) await expandDone(page)
}

function table(page: Page) {
  return page.getByRole('region', { name: 'Tasks table' })
}

function groupRegion(page: Page, title: string) {
  return table(page).getByRole('region', { name: title, exact: true })
}

function groupToggle(page: Page, title: string) {
  return groupRegion(page, title).getByRole('button', { name: new RegExp(`^${title}`) })
}

async function expandDone(page: Page) {
  const toggle = groupToggle(page, 'Done')
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await toggle.click()
  await expect(toggle).toHaveAttribute('aria-expanded', 'true')
}

// Every row across the open groups, in the order the groups present them.
function rows(page: Page) {
  return page.getByRole('list', { name: 'Tasks', exact: true }).locator(':scope > li')
}

// One group's rows; a row's own stage-progress list is not a row.
function groupRows(page: Page, title: string) {
  return groupRegion(page, title).getByRole('list', { name: 'Tasks', exact: true }).locator(':scope > li')
}

function row(page: Page, title: string) {
  return rows(page).filter({ has: page.getByRole('link', { name: title, exact: true }) })
}

// AC-6.5: four groups in the order operator, executing agent, factory or
// blocking task, and terminal. Each heading states the server's count for the
// whole filtered workspace, read from the group headers rather than counted
// from the rows in hand.
test('the list groups tasks by who acts next and heads each group with the server count', async ({ page }) => {
  await routeTasksSurface(page)
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(route, matchOperations(route.request().url()), {
      counts: { operator: 7, agent: 12, waiting: 30, terminal: 650 },
    }),
  )
  await page.goto('/?view=list')
  const groups = table(page).locator('[data-group]')
  await expect(groups).toHaveCount(4)
  expect(await groups.evaluateAll((elements) => elements.map((element) => element.getAttribute('data-group')))).toEqual(
    ['operator', 'agent', 'waiting', 'terminal'],
  )
  for (const [title, count] of [
    ['Needs you', '7'],
    ['In flight', '12'],
    ['Waiting', '30'],
    ['Done', '650'],
  ]) {
    await expect(groupRegion(page, title).locator('[data-group-count]')).toHaveText(count)
  }
  // The rows each group holds are the server's members of that group.
  await expect(groupRows(page, 'Needs you')).toHaveCount(1)
  await expect(groupRows(page, 'In flight')).toHaveCount(1)
  await expect(groupRows(page, 'Waiting')).toHaveCount(2)
  // Only the "Needs you" heading carries the attention tone.
  await expect(groupToggle(page, 'Needs you').getByText('Needs you', { exact: true })).toHaveClass(/text-attention/)
  for (const title of ['In flight', 'Waiting', 'Done']) {
    await expect(groupToggle(page, title).getByText(title, { exact: true })).not.toHaveClass(/text-attention/)
  }
  // The group headings replace every other count on the surface.
  await expect(page.getByLabel('Matching tasks')).toHaveCount(0)
  await expect(page.getByText('All tasks', { exact: true })).toHaveCount(0)
  await expect(table(page).getByText(/^\d+ tasks?$/)).toHaveCount(0)
})

// AC-6.5: Done starts collapsed and asks the server for nothing until it is
// expanded; the open groups make one request each.
test('Done starts collapsed and sends no terminal request until it is expanded', async ({ page }) => {
  const groups: string[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/v1/task-operations') groups.push(url.searchParams.get('group') ?? '')
  })
  await openTasks(page, { done: false })
  await expect(groupRows(page, 'Needs you')).toHaveCount(1)
  await expect(groupToggle(page, 'Done')).toHaveAttribute('aria-expanded', 'false')
  await expect(groupRows(page, 'Done')).toHaveCount(0)
  // The heading still counts Done from an open group's response.
  await expect(groupRegion(page, 'Done').locator('[data-group-count]')).toHaveText('1')
  expect(groups.toSorted()).toEqual(['agent', 'operator', 'waiting'])

  await groupToggle(page, 'Done').click()
  await expect(row(page, 'Shipped web change')).toBeVisible()
  expect(groups.filter((group) => group === 'terminal')).toHaveLength(1)

  // Collapsing an open group unmounts its rows; reopening reads it again.
  await groupToggle(page, 'In flight').click()
  await expect(groupRows(page, 'In flight')).toHaveCount(0)
  await expect(groupRegion(page, 'In flight').locator('[data-group-count]')).toHaveText('1')
})

// AC-6.5 and AC-6.2: each group pages on its own at the server's page size,
// newest created first, and any filter change sends every group back to its
// first page.
test('each group pages independently and a filter change resets every group', async ({ page }) => {
  const reads: URL[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/v1/task-operations') reads.push(url)
  })
  const agents = ['one', 'two', 'three'].map((name, index) => ({
    ...operations[1],
    task: {
      ...operations[1].task,
      id: `task-agent-${name}`,
      title: `Agent task ${name}`,
      created_at: `2026-08-0${7 + index}T10:00:00Z`,
    },
  }))
  const items = [...operations, ...agents]
  await routeTasksSurface(page)
  await page.route('**/v1/task-operations?**', (route) => {
    const needle = (new URL(route.request().url()).searchParams.get('q') ?? '').toLowerCase()
    const matched = items
      .filter((item) => !needle || item.task.title.toLowerCase().includes(needle))
      .toSorted((a, b) => b.task.created_at.localeCompare(a.task.created_at))
    return fulfillGrouped(route, matched, { limit: 2 })
  })
  await page.goto('/?view=list')
  const inFlight = groupRegion(page, 'In flight')
  await expect(groupRows(page, 'In flight')).toHaveCount(2)
  // Newest created first inside the group.
  await expect(groupRows(page, 'In flight')).toContainText(['Agent task three', 'Agent task two'])
  await expect(inFlight.getByText('1–2 of 4')).toBeVisible()
  await expect(inFlight.getByRole('button', { name: 'Previous' })).toBeDisabled()

  await inFlight.getByRole('button', { name: 'Next' }).click()
  await expect(inFlight.getByText('3–4 of 4')).toBeVisible()
  await expect(groupRows(page, 'In flight')).toContainText(['Agent task one', 'Historical anchor'])
  // The other groups kept their own first page: no other group read moved.
  expect(
    reads
      .filter((url) => url.searchParams.get('group') !== 'agent')
      .every((url) => url.searchParams.get('offset') === '0'),
  ).toBe(true)
  expect(reads.some((url) => url.searchParams.get('group') === 'agent' && url.searchParams.get('offset') === '2')).toBe(
    true,
  )
  await expect(groupRegion(page, 'Waiting').getByText('1–2 of 2')).toHaveCount(0)

  // Narrowing returns the paged group to its first page with the new filter.
  const before = reads.length
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('agent task')
  await expect(inFlight.getByText('1–2 of 3')).toBeVisible()
  const filtered = reads.slice(before).filter((url) => url.searchParams.get('q') === 'agent task')
  expect(filtered.length).toBeGreaterThan(0)
  expect(filtered.every((url) => url.searchParams.get('offset') === '0')).toBe(true)
})

// AC-6.2: within each group the server's newest-created-first order is kept.
test('tasks view preserves newest-created-first API order within each group', async ({ page }) => {
  await openTasks(page)
  await expect(rows(page)).toContainText([
    'Stuck conveyor change',
    'Historical anchor',
    'Wire the Tasks view',
    'Bounced web plan',
    'Shipped web change',
  ])
  await expect(groupRows(page, 'Waiting')).toContainText(['Wire the Tasks view', 'Bounced web plan'])
})

// AC-1.1: the view is a filterable list, not stage columns, and filters by
// state, repository, and free text. State and repository are multi-select:
// checking a second value widens the member rather than replacing it. Every
// filter reaches every group (AC-6.2).
test('tasks view lists every task and filters by state, repository, and free text', async ({ page }) => {
  await openTasks(page)
  await expect(rows(page)).toHaveCount(5)
  // The board's stage columns are not this surface.
  await expect(page.getByRole('heading', { name: 'Needs operator' })).toHaveCount(0)

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('bounced')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Bounced web plan' })).toBeVisible()
  await expect(groupRegion(page, 'Needs you').locator('[data-group-count]')).toHaveText('0')
  await expect(groupRegion(page, 'Waiting').locator('[data-group-count]')).toHaveText('1')

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('')
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Repository' }).click()
  await page.getByRole('option', { name: 'web' }).click()
  await expect(rows(page)).toHaveCount(2)

  await page.getByRole('tab', { name: 'Status' }).click()
  await page.getByRole('option', { name: 'Merged' }).click()
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Shipped web change' })).toBeVisible()

  // Both checks hold at once: merged-or-queued over the web repository is the
  // two web tasks, and the menu stays open for the next adjustment.
  await page.getByRole('option', { name: 'Queued' }).click()
  await expect(rows(page)).toHaveCount(2)
  await expect(page.getByRole('option', { name: 'Merged' })).toHaveAttribute('aria-selected', 'true')

  // Narrowing to a pair no seeded task satisfies empties the list rather than
  // falling back to the unfiltered workspace; every heading still stands at 0.
  await page.getByRole('option', { name: 'Merged' }).click()
  await page.getByRole('option', { name: 'Queued' }).click()
  await page.getByRole('option', { name: 'Running' }).click()
  await expect(rows(page)).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'No tasks match these filters' })).toBeVisible()
  for (const title of ['Needs you', 'In flight', 'Waiting', 'Done']) {
    await expect(groupRegion(page, title).locator('[data-group-count]')).toHaveText('0')
  }
})

// AC-1.2: child rollups render from durable authority, and their absence
// renders as absence. The per-dependency breakdown lives in the task's own
// detail panel.
test('tasks view shows child rollup state', async ({ page }) => {
  await openTasks(page)
  const anchor = row(page, 'Historical anchor')
  await expect(anchor.getByText('3 child tasks · 1 merged · 1 closed · 1 open')).toBeVisible()

  // A task with no children states that rather than tallying zeroes.
  await expect(row(page, 'Shipped web change').getByText('child task')).toHaveCount(0)
})

// AC-1.3: the row links its first served requirement, or its first governing
// System Design when it serves none, counts the rest, and lists and links
// every attached document on hover and on keyboard focus.
test('the context cell links the first document, counts the rest, and lists every document on hover', async ({
  page,
}) => {
  await openTasks(page)
  const blocked = row(page, 'Wire the Tasks view')
  const chip = blocked.getByRole('link', { name: 'Task-centric operations view v1', exact: true })
  await expect(chip).toHaveAttribute('href', '/requirements?requirement=req-tasks-view')
  await expect(chip).toHaveClass(/bg-raised/)
  await expect(blocked.getByText('+1', { exact: true })).toBeVisible()
  // The second document is not on the row itself until the popover opens.
  await expect(page.getByRole('group', { name: 'Attached context' })).toHaveCount(0)

  await chip.hover()
  const popover = page.getByRole('group', { name: 'Attached context' })
  await expect(popover).toBeVisible()
  await expect(popover.getByRole('link', { name: /Task-centric operations view v1/ })).toHaveAttribute(
    'href',
    '/requirements?requirement=req-tasks-view',
  )
  await expect(popover.getByRole('link', { name: /Work-order lifecycle and MCP surface v16/ })).toHaveAttribute(
    'href',
    '/system-design?document=design-lifecycle',
  )
  // The pointer can move into the popover and it stays open for its links.
  await popover.getByRole('link', { name: /Work-order lifecycle/ }).hover()
  await expect(popover).toBeVisible()
  await page.mouse.move(5, 5)
  await expect(popover).toHaveCount(0)

  // Each row hands off to the task's own detail sheet over the list rather
  // than restating it, at an address that can be shared.
  await expect(blocked.getByRole('link', { name: 'Wire the Tasks view' })).toHaveAttribute(
    'href',
    '/tasks/task-blocked?view=list',
  )
  await expect(row(page, 'Shipped web change').getByText('No context', { exact: true })).toBeVisible()
  await expect(row(page, 'Shipped web change').getByText('No context', { exact: true })).toHaveClass(/text-faint/)
})

// AC-1.3: the popover is reachable and dismissable by keyboard alone.
test('the context popover opens on keyboard focus, takes Tab into its links, and closes on Escape', async ({
  page,
}) => {
  await openTasks(page)
  const chip = row(page, 'Wire the Tasks view').getByRole('link', {
    name: 'Task-centric operations view v1',
    exact: true,
  })
  await chip.focus()
  const popover = page.getByRole('group', { name: 'Attached context' })
  await expect(popover).toBeVisible()
  await page.keyboard.press('Tab')
  await expect(popover.getByRole('link', { name: /Task-centric operations view v1/ })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(popover.getByRole('link', { name: /Work-order lifecycle and MCP surface v16/ })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(popover).toHaveCount(0)
  await expect(chip).toBeFocused()
  // Focus that leaves the cell closes it too.
  await chip.focus()
  await page.keyboard.press('Escape')
  await expect(popover).toHaveCount(0)
  await page.keyboard.press('Shift+Tab')
  await chip.focus()
  await expect(popover).toBeVisible()
  await row(page, 'Wire the Tasks view').getByRole('link', { name: 'Wire the Tasks view' }).focus()
  await expect(popover).toHaveCount(0)
})

// A row that serves no requirement leads with its first governing design.
test('the context chip falls back to the first governing design', async ({ page }) => {
  await routeTasksSurface(page)
  const designOnly = {
    ...operations[1],
    task: {
      ...operations[1].task,
      context: { designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle and MCP surface', version: 16 }] },
    },
  }
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, [designOnly]))
  await page.goto('/?view=list')
  const anchor = row(page, 'Historical anchor')
  await expect(
    anchor.getByRole('link', { name: 'Work-order lifecycle and MCP surface v16', exact: true }),
  ).toHaveAttribute('href', '/system-design?document=design-lifecycle')
  await expect(anchor.getByText(/^\+\d+$/)).toHaveCount(0)
})

// AC-1.4: a pending-gate or redirected plan stays visible on its row; the
// row never shows "No plan", "Approved", or the historical plan-gate badge.
test('plan status shows only while it asks for someone and never as a No plan badge', async ({ page }) => {
  await routeTasksSurface(page)
  const gate = {
    ...operations[0],
    task: {
      ...operations[0].task,
      id: 'task-plan-gate',
      title: 'Plan gate task',
      created_at: '2026-08-08T10:00:00Z',
      context: {},
    },
    plan: { state: 'pending_gate', version: 3 },
    next_action: {
      label: 'Plan awaiting approval',
      actor: 'operator',
      kind: 'plan_approval',
      judgment: true,
      summary: 'Read plan v3 and approve it, request changes, or reject it.',
      deadline: null,
    },
  }
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(route, [gate, ...matchOperations(route.request().url())]),
  )
  await page.goto('/?view=list')
  await expandDone(page)
  // A plan that is not what the next action asks about rides the Progress label.
  await expect(row(page, 'Wire the Tasks view').locator('[data-progress-label]')).toContainText(
    'Plan awaiting approval v2',
  )
  await expect(row(page, 'Bounced web plan').locator('[data-progress-label]')).toContainText(
    'Plan changes requested v1',
  )
  // When the plan gate is the next action, the operator line states it once.
  const gateRow = row(page, 'Plan gate task')
  await expect(gateRow.locator('[data-operator-reason]')).toContainText('Plan awaiting approval')
  await expect(gateRow.locator('[data-progress-label]')).not.toContainText('Plan awaiting approval')
  // Retired badges are gone from every row.
  for (const retired of ['No plan', 'Historical plan gate', 'Plan approved']) {
    await expect(table(page).getByText(retired, { exact: false })).toHaveCount(0)
  }
})

// AC-6.2: the header reads Name, Progress, Context, and Updated, with an
// unlabelled assignee column and no sort affordance; the State and Stage
// columns, the drag handle, and the restart badge are gone.
test('the list header has no sort affordance and rows carry no State column, drag handle, or restart badge', async ({
  page,
}) => {
  await routeTasksSurface(page)
  const items = [
    { ...operations[0], task: { ...operations[0].task, superseded_by: 'task-anchor' } },
    { ...operations[1], task: { ...operations[1].task, supersedes: 'task-blocked' } },
    ...operations.slice(2),
  ]
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, items))
  await page.goto('/?view=list')
  await expandDone(page)
  const header = page.getByTestId('tasks-table-header')
  expect(await header.locator(':scope > span').allTextContents()).toEqual([
    'Name',
    'Progress',
    'Context',
    '',
    'Updated',
  ])
  await expect(header.locator('svg')).toHaveCount(0)
  await expect(header.getByRole('button')).toHaveCount(0)
  await expect(table(page).getByText('⠿')).toHaveCount(0)
  await expect(table(page).getByText('State', { exact: true })).toHaveCount(0)
  await expect(table(page).getByText('Stage', { exact: true })).toHaveCount(0)
  await expect(table(page).getByText('restarted', { exact: true })).toHaveCount(0)
  await expect(table(page).getByLabel('Needs operator')).toHaveCount(0)
})

// The 14 px glyph that leads every row names its group, and only the
// operator's carries a colour (AC-6.6).
test('every row leads with its state glyph', async ({ page }) => {
  await routeTasksSurface(page)
  const closed = {
    ...operations[2],
    task: { ...operations[2].task, id: 'task-closed', title: 'Closed web change', state: 'closed' },
    pull_request: undefined,
  }
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(route, [...matchOperations(route.request().url()), closed]),
  )
  await page.goto('/?view=list')
  await expandDone(page)
  for (const [title, glyph] of [
    ['Stuck conveyor change', 'Needs you'],
    ['Historical anchor', 'In flight'],
    ['Wire the Tasks view', 'Waiting'],
    ['Shipped web change', 'Merged'],
    ['Closed web change', 'Closed'],
  ]) {
    const mark = row(page, title).getByRole('img', { name: glyph, exact: true })
    await expect(mark).toBeVisible()
    expect(await mark.boundingBox()).toMatchObject({ width: 14, height: 14 })
  }
  await expect(row(page, 'Stuck conveyor change').getByRole('img', { name: 'Needs you' })).toHaveClass(
    /bg-attention-dot/,
  )
  for (const title of ['Historical anchor', 'Wire the Tasks view']) {
    await expect(row(page, title).locator('[role="img"]').first()).not.toHaveClass(/attention|primary/)
  }
})

// AC-6.6: an operator row states the server's label and summary beneath its
// title and stands apart from the rows nobody is waiting on the operator for.
// This line replaces the former stalled footer.
test('an operator row shows the next action label and summary with an attention inset rule', async ({ page }) => {
  await openTasks(page)
  const stuck = row(page, 'Stuck conveyor change')
  const reason = stuck.locator('[data-operator-reason]')
  await expect(reason.getByText('Implementation keeps failing', { exact: true })).toHaveClass(/text-attention/)
  await expect(
    reason.getByText('— The implementation order failed three times; recover it with direction or close the task.'),
  ).toHaveClass(/text-faint/)
  await expect(stuck).toHaveCSS('box-shadow', /inset/)
  await expect(stuck.getByText(/Stalled —/)).toHaveCount(0)
  await expect(stuck).not.toContainText('harness exited before completing work order')
  // The operator line takes the identity line's place on that row only.
  await expect(stuck.getByText('task-stuck', { exact: true })).toHaveCount(0)

  const healthy = row(page, 'Historical anchor')
  await expect(healthy.locator('[data-operator-reason]')).toHaveCount(0)
  await expect(healthy.getByText('task-anchor', { exact: true })).toHaveClass(/font-mono/)
  await expect(healthy).toHaveCSS('box-shadow', 'none')
  await expect(healthy.locator('.text-attention')).toHaveCount(0)
})

// A deliberate checkpoint release also matches the stalled row, but its release
// message is not a failure: the row prints the server's agent-question label
// and summary as every operator row does (component-web-task-surfaces,
// Board and Tasks list; feature-operator-next-action NA-6; AC-6.6).
test('an agent-question row states the server question instead of a stalled failure', async ({ page }) => {
  await routeTasksSurface(page)
  const stuck = operations.find((item) => item.task.id === 'task-stuck')
  if (!stuck) throw new Error('Expected the stalled fixture row.')
  const checkpoint = {
    ...stuck,
    task: {
      ...stuck.task,
      id: 'task-checkpoint',
      title: 'Checkpointed conveyor change',
      created_at: '2026-08-07T10:00:00Z',
    },
    stalled: { needed: true, reason: 'retry suppressed', last_failure: 'operator checkpoint reached' },
    next_action: {
      label: 'Cited revisions need a decision',
      actor: 'operator',
      kind: 'agent_question',
      judgment: true,
      summary:
        'The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.',
      deadline: null,
    },
  }
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, [checkpoint, stuck]))
  await page.goto('/?view=list')
  const checkpointRow = row(page, 'Checkpointed conveyor change')
  const reason = checkpointRow.locator('[data-operator-reason]')
  await expect(reason).toContainText(`Cited revisions need a decision — ${checkpoint.next_action.summary}`)
  await expect(reason.getByText('Cited revisions need a decision', { exact: true })).toHaveClass(/text-attention/)
  await expect(checkpointRow.getByText(/Stalled —/)).toHaveCount(0)
  await expect(checkpointRow).not.toContainText('operator checkpoint reached')
  await expect(checkpointRow.locator('.text-failure')).toHaveCount(0)
  // An ordinary stalled row prints no failure footer either: its operator line
  // carries the server's statement.
  const ordinary = row(page, 'Stuck conveyor change')
  await expect(ordinary.locator('[data-operator-reason]')).toContainText('Implementation keeps failing')
  await expect(ordinary.getByText(/Stalled —/)).toHaveCount(0)
})

// AC-6.7: a non-terminal row's Progress cell names the current stage and marks
// each stage on the served path, in the colours the card stripe shares, with
// the current segment widened. AC-3.3's card preference is off here.
test('progress names the current stage and draws each served stage status', async ({ page }) => {
  await openTasks(page)
  const expectations: [string, string, string, TaskFlowStatus[]][] = [
    ['Historical anchor', 'Implement', 'text-foreground', flowStatuses['task-anchor'].statuses],
    ['Wire the Tasks view', 'Implement', 'text-foreground', flowStatuses['task-blocked'].statuses],
    ['Stuck conveyor change', 'Implement', 'text-attention', flowStatuses['task-stuck'].statuses],
  ]
  for (const [title, name, tone, statuses] of expectations) {
    const progress = row(page, title)
    const label = progress.locator('[data-progress-label]')
    await expect(label.getByText(name, { exact: true })).toHaveClass(new RegExp(tone))
    const segments = progress.getByRole('list', { name: 'Stage progress' }).getByRole('img')
    await expect(segments).toHaveCount(6)
    expect(
      await segments.evaluateAll((elements) => elements.map((element) => element.getAttribute('data-status'))),
    ).toEqual(statuses)
  }
  // A waiting current stage says so in the faint tone.
  await expect(row(page, 'Wire the Tasks view').locator('[data-progress-label] .text-faint')).toContainText('waiting')
  await expect(row(page, 'Historical anchor').locator('[data-progress-label]')).not.toContainText('waiting')
  // Each segment is named for its stage and the server's status word.
  const anchor = row(page, 'Historical anchor').getByRole('list', { name: 'Stage progress' })
  await expect(anchor.getByRole('img', { name: 'Implement — In progress' })).toHaveClass(/bg-primary/)
  await expect(anchor.getByRole('img', { name: 'Triage — Done' })).toHaveClass(/bg-primary/)
  await expect(anchor.getByRole('img', { name: 'Review — Upcoming' })).toHaveClass(/bg-raised/)
  await expect(anchor.getByRole('img', { name: 'Verify — Skipped' })).toHaveClass(/border-dashed/)
  await expect(row(page, 'Wire the Tasks view').getByRole('img', { name: 'Implement — Waiting' })).toHaveClass(
    /bg-primary\/55/,
  )
  await expect(row(page, 'Stuck conveyor change').getByRole('img', { name: 'Implement — Your approval' })).toHaveClass(
    /bg-attention-dot/,
  )
  // The current segment is about 1.6 times as wide as the others.
  const current = await anchor.locator('[data-current="true"]').boundingBox()
  const other = await anchor.getByRole('img', { name: 'Review — Upcoming' }).boundingBox()
  const ratio = (current?.width ?? 0) / (other?.width ?? 1)
  expect(ratio).toBeGreaterThan(1.4)
  expect(ratio).toBeLessThan(1.8)
  // No gate diamonds on list rows; the card preference stayed off.
  await expect(table(page).locator('[data-gate]')).toHaveCount(0)
  expect(await page.evaluate(() => localStorage.getItem('conveyor-show-stage-progress'))).toBeNull()
})

// AC-6.7: the in-progress segment's sheen moves, and reduced motion stops it.
test('the in-progress sheen stops under reduced motion', async ({ page }) => {
  await openTasks(page)
  const segment = row(page, 'Historical anchor').getByRole('img', { name: 'Implement — In progress' })
  await expect(segment).toHaveCSS('animation-name', 'progress-sheen')
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await expect(segment).toHaveCSS('animation-name', 'none')
  // The fill stays primary with the motion stopped.
  await expect(segment).toHaveClass(/bg-primary/)
})

// The flow is read only for rows on screen, once each, so a page of rows does
// not fan out a flow read per task up front (AC-6.7; as StageStripe does).
test('progress reads the flow only for rows on screen', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 640 })
  // Completed reads: the dev server's StrictMode remount aborts a reader's
  // first request and repeats it, which is not a second reader.
  const flowReads: string[] = []
  page.on('requestfinished', (request) => {
    const match = /\/v1\/tasks\/([^/]+)\/flow/.exec(new URL(request.url()).pathname)
    if (match) flowReads.push(match[1])
  })
  const many = Array.from({ length: 25 }, (_, index) => ({
    ...operations[1],
    task: {
      ...operations[1].task,
      id: `task-many-${String(index).padStart(2, '0')}`,
      title: `Running task ${String(index).padStart(2, '0')}`,
      created_at: new Date(Date.UTC(2026, 7, 30, 10, 0, 0) - index * 60_000).toISOString(),
    },
  }))
  await routeTasksSurface(page)
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, many))
  await page.goto('/?view=list')
  await expect(row(page, 'Running task 00').getByRole('list', { name: 'Stage progress' })).toBeVisible()
  await expect.poll(() => flowReads.length).toBeGreaterThan(0)
  expect(flowReads).not.toContain('task-many-24')
  const firstScreen = new Set(flowReads).size
  expect(firstScreen).toBeLessThan(25)
  // Each row reads its flow once: the label and the segments share one reader.
  expect(flowReads.length).toBe(firstScreen)

  await row(page, 'Running task 24').scrollIntoViewIfNeeded()
  await expect(row(page, 'Running task 24').getByRole('list', { name: 'Stage progress' })).toBeVisible()
  expect(flowReads).toContain('task-many-24')
})

// AC-6.8: a terminal row names how it ended, links its pull request when the
// server states one, and mounts no flow reader.
test('a merged row reads Merged and links its pull request', async ({ page }) => {
  const flowReads: string[] = []
  page.on('request', (request) => {
    const match = /\/v1\/tasks\/([^/]+)\/flow/.exec(new URL(request.url()).pathname)
    if (match) flowReads.push(match[1])
  })
  await routeTasksSurface(page)
  const closed = {
    ...operations[2],
    task: { ...operations[2].task, id: 'task-closed', title: 'Closed web change', state: 'closed' },
    pull_request: undefined,
  }
  const unnumbered = {
    ...operations[2],
    task: { ...operations[2].task, id: 'task-unnumbered', title: 'Unnumbered merge' },
    pull_request: { url: 'https://example.test/web/pull/77' },
  }
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(route, [...matchOperations(route.request().url()), closed, unnumbered]),
  )
  await page.goto('/?view=list')
  await expandDone(page)
  const shipped = row(page, 'Shipped web change')
  await expect(shipped.getByText('Merged', { exact: true })).toHaveClass(/text-foreground/)
  await expect(shipped.getByText('Merged', { exact: true })).not.toHaveClass(/positive|green/)
  await expect(shipped.getByRole('link', { name: 'PR #42' })).toHaveAttribute(
    'href',
    'https://example.test/web/pull/42',
  )
  await expect(shipped.locator('[data-list-progress="terminal"]')).toHaveText('Merged · PR #42')
  await expect(shipped.getByRole('list', { name: 'Stage progress' })).toHaveCount(0)

  const closedRow = row(page, 'Closed web change')
  await expect(closedRow.getByText('Closed', { exact: true })).toHaveClass(/text-faint/)
  await expect(closedRow.getByRole('link', { name: /^PR/ })).toHaveCount(0)
  // A pull request without a stated number is still linked, never numbered from its URL.
  await expect(row(page, 'Unnumbered merge').getByRole('link', { name: 'PR', exact: true })).toHaveAttribute(
    'href',
    'https://example.test/web/pull/77',
  )
  expect(flowReads.filter((id) => ['task-shipped', 'task-closed', 'task-unnumbered'].includes(id))).toEqual([])
})

// The table is read at the width the navigation leaves beside it, so its
// columns have to hold there: header and rows stay on the same tracks, the
// context chip stops at its own column instead of running under the
// timestamp, and nothing is cut off past the table's edge.
test('tasks table stays aligned and readable at a constrained width', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 })
  await openTasks(page)
  const header = page.getByTestId('tasks-table-header')
  const blocked = row(page, 'Wire the Tasks view')

  // Header and row read one track definition: the Progress column starts at
  // the same offset in both.
  const headerProgress = await header.getByText('Progress', { exact: true }).boundingBox()
  const rowProgress = await blocked.locator('[data-list-progress]').boundingBox()
  expect(Math.abs((headerProgress?.x ?? 0) - (rowProgress?.x ?? 0))).toBeLessThan(1)

  // The context chip is held inside its own column rather than overlapping
  // the timestamp beside it, and keeps the full title reachable.
  const context = blocked.getByRole('link', { name: 'Task-centric operations view v1', exact: true })
  const contextBox = await context.boundingBox()
  const updatedBox = await blocked.getByRole('button', { name: /^Updated/ }).boundingBox()
  expect((contextBox?.x ?? 0) + (contextBox?.width ?? 0)).toBeLessThanOrEqual(updatedBox?.x ?? 0)
  await expect(context).toHaveAttribute('title', 'Task-centric operations view v1')

  // The progress label holds its column rather than spilling across Context.
  const label = await blocked.locator('[data-progress-label]').boundingBox()
  expect((label?.x ?? 0) + (label?.width ?? 0)).toBeLessThanOrEqual(contextBox?.x ?? 0)

  // The timestamp is fully inside the table rather than clipped past its edge.
  const tableBox = await table(page).boundingBox()
  expect((updatedBox?.x ?? 0) + (updatedBox?.width ?? 0)).toBeLessThanOrEqual(
    (tableBox?.x ?? 0) + (tableBox?.width ?? 0),
  )
  // Updated is a short elapsed time; the disclosure holds the full timestamp.
  await expect(blocked.getByRole('button', { name: /^Updated/ })).toHaveText(/^(now|\d+[mhd])$/)
})

// The list-first surface keeps the empty and error behavior the other document
// surfaces have, so an unreachable projection never reads as an empty factory.
test('tasks view distinguishes an empty workspace from a failed load', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: { workspace: 'demo', repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }] },
    }),
  )
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, []))
  await page.goto('/?view=list')
  await expect(page.getByRole('heading', { name: 'No tasks yet' })).toBeVisible()
  // Every heading keeps its place with the server's zero.
  for (const title of ['Needs you', 'In flight', 'Waiting', 'Done']) {
    await expect(groupRegion(page, title).locator('[data-group-count]')).toHaveText('0')
  }

  await page.unrouteAll({ behavior: 'ignoreErrors' })
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/workspace?**', (route) =>
    route.fulfill({
      json: { workspace: 'demo', repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }] },
    }),
  )
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/task-operations?**', (route) =>
    route.fulfill({ status: 500, body: 'task operations unavailable' }),
  )
  await page.reload()
  // Each open group states its own failure, and no heading invents a count.
  await expect(groupRegion(page, 'Needs you').getByText('task operations unavailable')).toBeVisible()
  await expect(groupRegion(page, 'Waiting').getByText('task operations unavailable')).toBeVisible()
  await expect(groupRegion(page, 'Needs you').locator('[data-group-count]')).toHaveText('–')
  await expect(page.getByRole('heading', { name: 'No tasks yet' })).toHaveCount(0)
})

// REQ-4/AC-4.3 and AC-1.5: assignment is claim-eligibility routing, so the row
// names who holds the task in a neutral initials circle. An unassigned row is
// silent about it rather than spending width on an empty label.
test('tasks rows carry an assignee chip only where someone holds the task', async ({ page }) => {
  await openTasks(page)
  const assigned = row(page, 'Stuck conveyor change')
  const chip = assigned.getByTitle('Assigned to Assigned User — assigned@example.test · usr-assigned')
  await expect(chip).toBeVisible()
  // The name is the chip's accessible text; the account identifiers stay in the tooltip.
  await expect(chip).toContainText('Assigned User')
  await expect(chip).toHaveText('AUAssigned User')
  await expect(chip.locator('[aria-hidden="true"]')).toHaveClass(/bg-raised/)
  await expect(chip.locator('[aria-hidden="true"]')).not.toHaveClass(/primary/)
  await expect(assigned).not.toContainText('usr-assigned')

  const unassigned = row(page, 'Bounced web plan')
  await expect(unassigned.getByTitle(/^Assigned to/)).toHaveCount(0)
  await expect(unassigned).not.toContainText('Unassigned')
})

// AC-1.5: no barred field appears on the surface, and none is offered as a
// filter or sort control.
test('tasks view exposes no priority or declared-phase field', async ({ page }) => {
  await openTasks(page)
  const surface = (await page.locator('main').innerText()).toLowerCase()
  for (const barred of ['priority', 'phase']) {
    expect(surface).not.toContain(barred)
  }
  await expect(page.getByLabel('Filter by priority')).toHaveCount(0)
})

// The selected row takes the inset-rule position in primary over a light
// tint, and the operator line and chips stay visible on it.
test('the selected row carries the primary rule and tint', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('link', { name: 'Stuck conveyor change' }).click()
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
  const selected = row(page, 'Stuck conveyor change')
  await expect(selected).toHaveClass(/bg-primary\/5/)
  await expect(selected).toHaveCSS('box-shadow', /rgb\(51, 85, 245\).*inset|inset.*rgb\(51, 85, 245\)/)
  await expect(selected.locator('[data-operator-reason]')).toBeVisible()
  await expect(selected.getByRole('img', { name: 'Needs you' })).toBeVisible()
  await expect(row(page, 'Historical anchor')).not.toHaveClass(/bg-primary\/5/)
})

// At 390 px the rows stack and the page never scrolls sideways; the context
// popover stays inside the viewport.
test('the grouped list has no horizontal overflow at 390 px', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await openTasks(page)
  await expect(row(page, 'Wire the Tasks view')).toBeVisible()
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(overflow).toBeLessThanOrEqual(0)
  await expect(page.getByTestId('tasks-table-header')).toBeHidden()
  const chip = row(page, 'Wire the Tasks view').getByRole('link', {
    name: 'Task-centric operations view v1',
    exact: true,
  })
  await chip.focus()
  const popover = page.getByRole('group', { name: 'Attached context' })
  await expect(popover).toBeVisible()
  const box = await popover.boundingBox()
  expect(box?.x ?? -1).toBeGreaterThanOrEqual(0)
  expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(390)
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth),
  ).toBeLessThanOrEqual(0)
})

// REQ-6 AC-6.1: the Board shows kanban by default and the list on request;
// the choice lives in the address, so a reload keeps it.
test('the board view switch keeps its view across a reload', async ({ page }) => {
  await routeTasksSurface(page)
  await page.goto('/')
  await expect(page.getByRole('button', { name: 'Kanban', pressed: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Task board' })).toBeVisible()
  expect(new URL(page.url()).search).toBe('')

  await page.getByRole('button', { name: 'List' }).click()
  await expect(page).toHaveURL(/\/\?view=list$/)
  await expect(page.getByRole('region', { name: 'Tasks table' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Task board' })).toHaveCount(0)

  await page.reload()
  await expect(page.getByRole('button', { name: 'List', pressed: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Tasks table' })).toBeVisible()

  await page.getByRole('button', { name: 'Kanban' }).click()
  await expect(page.getByRole('region', { name: 'Task board' })).toBeVisible()
  expect(new URL(page.url()).search).toBe('')
})

// REQ-6 AC-6.3: the retired Tasks address lands on the list view, keeping
// intake and every filter it names and dropping anything else.
test('the tasks address redirects to the list view with its filters and intake', async ({ page }) => {
  await routeTasksSurface(page)
  const requests: string[] = []
  page.on('request', (request) => {
    if (request.url().includes('/v1/task-operations?')) requests.push(request.url())
  })
  await page.goto('/tasks?create=true&q=shipped&state=queued&assignee=user-ada&serves_requirement=REQ-2&bogus=1')
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  const url = new URL(page.url())
  expect(url.pathname).toBe('/')
  expect(url.searchParams.get('view')).toBe('list')
  expect(url.searchParams.get('create')).toBe('true')
  expect(url.searchParams.has('bogus')).toBe(false)
  // The filter reached the server: the list reads it from the address once,
  // then the saved filter carries it and the address keeps only the view.
  await expect
    .poll(() =>
      requests.some((request) => {
        const params = new URL(request).searchParams
        return (
          params.get('q') === 'shipped' &&
          params.getAll('state').includes('queued') &&
          params.get('assignee') === 'user-ada' &&
          params.getAll('serves_requirement').includes('REQ-2')
        )
      }),
    )
    .toBe(true)
  await expect(page).not.toHaveURL(/q=shipped/)
})

// REQ-6: intake opens over whichever Board view asked for it, from the Board
// header and from the legacy /new address.
test('task intake opens over the board list view', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('link', { name: 'New task' }).click()
  const intake = page.getByRole('dialog', { name: 'New task' })
  await expect(intake).toBeVisible()
  await expect(intake.getByText(/triage and plan prompt/)).toBeVisible()
  await expect(intake.getByText(/Designs, plans, logs .* triage and plan agents/)).toBeVisible()
  // The list it files into stays behind the sheet rather than being replaced.
  await expect(page.getByRole('button', { name: 'List', pressed: true })).toBeVisible()

  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toHaveCount(0)
  await expect(page).toHaveURL(/\/\?view=list$/)

  await routeTasksSurface(page)
  await page.goto('/new')
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  expect(new URL(page.url()).searchParams.get('view')).toBe('list')
})

// A row opens the Board's task sheet over the list, and the legacy panel
// permalink lands on the same sheet.
test('selecting a row opens the task sheet over the list view', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('link', { name: 'Shipped web change' }).click()
  const panel = page.getByRole('dialog', { name: 'Task detail' })
  await expect(panel).toBeVisible()
  await expect(page).toHaveURL(/\/tasks\/task-shipped\?view=list$/)
  // The list stays behind the sheet.
  await expect(page.getByRole('region', { name: 'Tasks table' })).toBeVisible()
  await expect(page.getByRole('list', { name: 'Tasks' }).locator('a[aria-current]')).toHaveCount(1)
  await expect(panel.getByRole('link', { name: 'Open full task page' })).toHaveAttribute(
    'href',
    '/tasks/task-shipped/full',
  )

  await panel.getByRole('button', { name: 'Close panel' }).click()
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toHaveCount(0)
  await expect(page).toHaveURL(/\/\?view=list$/)

  await routeTasksSurface(page)
  await page.goto('/tasks?task=task-bounced')
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
  await expect(page).toHaveURL(/\/tasks\/task-bounced\?view=list$/)

  // The full route is unchanged.
  await page.goto('/tasks/task-bounced/full')
  await expect(page.getByRole('link', { name: 'Back to board' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/tasks/task-bounced/full')
})

test('full task detail identifies a task outside the loaded Board page', async ({ page }) => {
  await routeTasksSurface(page)
  await page.goto('/tasks/task-shipped/full')
  await expect(
    page.getByRole('note').filter({ hasText: 'This task is outside the loaded Board window.' }),
  ).toBeVisible()
})

// A blueprint anchor keeps its one canonical home. The panel
// hosts the task's own detail composition rather than a copy of it, so the rule
// arrives with the composition instead of being restated on this surface.
test('an anchor opened in the panel is sent to its canonical blueprint route', async ({ page }) => {
  await routeTasksSurface(page)
  await page.route('**/v1/tasks/task-anchor/activity**', (route) =>
    route.fulfill({
      json: {
        task: {
          id: 'task-anchor',
          workspace: 'demo',
          title: 'Historical anchor',
          state: 'running',
          children: [{ id: 'child-one', title: 'A child task', state: 'merged' }],
        },
        jobs: [],
        events: [],
        interventions: [],
        work_orders: [],
        checkout_available: false,
        checkout_guidance: '',
        needs_attention: false,
      },
    }),
  )
  await page.route('**/v1/blueprints**', (route) => route.fulfill({ json: [] }))
  await page.goto('/tasks?task=task-anchor')
  await expect(page).toHaveURL(/\/blueprints\/task-anchor$/)
})

// AC-2.4: the shared filter family — updated-at range, served requirement, and
// governing design — is applied by the server on the Tasks surface.
test('tasks view filters by updated-at range, served requirement, and governing design', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Requirement' }).click()
  await page.getByRole('option', { name: 'Task-centric operations view' }).click()
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Wire the Tasks view' })).toBeVisible()

  // An unconfirmed document is not an option: only confirmed documents can be
  // attached to a task, so only confirmed documents can narrow the list.
  await expect(page.getByRole('option', { name: 'Unconfirmed idea' })).toHaveCount(0)

  await page.getByRole('option', { name: 'Any requirement' }).click()
  await page.getByRole('tab', { name: 'System design' }).click()
  await page.getByRole('option', { name: 'Work-order lifecycle' }).click()
  await expect(rows(page)).toHaveCount(1)

  await page.getByRole('option', { name: 'Any system design' }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await page.getByRole('option', { name: 'Custom range' }).click()
  // Fixed dates rather than a preset, so the assertion does not depend on when
  // the suite runs. The end date is inclusive of its own day. Bounced web plan
  // was last active on 4 August; Shipped web change was created that day but
  // last active on the 5th, so it is not in this window.
  await page.getByLabel('Updated from').fill('2026-08-04')
  await page.getByLabel('Updated to').fill('2026-08-04')
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Bounced web plan' })).toBeVisible()

  await page.getByLabel('Updated to').fill('2026-08-03')
  await expect(page.getByText('Choose an end date on or after the start date.')).toBeVisible()

  await page.getByRole('button', { name: 'Reset filters' }).click()
  await expect(rows(page)).toHaveCount(5)
})

// REQ-4/AC-3.2: assignee is a member of the same server-applied filter family,
// single-valued because a task has one holder, and its options come from the
// workspace co-member read rather than any user directory. Filtering happens on
// the server, so a browser-side narrowing would return the whole fixture and
// fail here.
test('tasks view filters by assignee and by unassigned', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Assignee' }).click()

  await page.getByRole('option', { name: 'Assigned User' }).click()
  await expect(rows(page)).toContainText(['Stuck conveyor change', 'Shipped web change'])
  await expect(rows(page)).toHaveCount(2)

  // Unassigned is a distinct choice, and it replaces the member rather than
  // accumulating beside it.
  await page.getByRole('option', { name: 'Unassigned' }).click()
  await expect(rows(page).filter({ hasText: 'Stuck conveyor change' })).toHaveCount(0)
  await expect(rows(page).filter({ hasText: 'Bounced web plan' })).toHaveCount(1)

  // "Any assignee" hands the whole workspace back.
  await page.getByRole('option', { name: 'Any assignee' }).click()
  await expect(rows(page)).toHaveCount(operations.length)
})

// REQ-4: one click onto the work that is mine and still moving. It is a preset
// over the existing row, not a separate view — the filter row shows exactly
// what it selected — and it narrows by state, so an assignment that is already
// merged does not come back.
test('My tasks shows only the signed-in user assignments that still need them', async ({ page }) => {
  await openTasks(page)
  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Repository' }).click()
  await page.getByRole('option', { name: 'conveyor' }).click()
  await expect(rows(page)).toHaveCount(3)
  const preset = page.getByRole('button', { name: 'My tasks' })
  await expect(preset).toHaveAttribute('aria-pressed', 'false')

  await preset.click()
  await expect(preset).toHaveAttribute('aria-pressed', 'true')
  // Stuck is queued and mine; Shipped is mine but merged, so it is done being
  // routed and stays out.
  await expect(rows(page)).toContainText(['Stuck conveyor change'])
  await expect(rows(page)).toHaveCount(1)

  // The repo-narrowed subset is intentionally still the pressed preset. Turning
  // it off removes only the assignee and preset states; the repo survives.
  await expect(preset).toHaveAttribute('aria-pressed', 'true')
  await preset.click()
  await expect(preset).toHaveAttribute('aria-pressed', 'false')
  await expect(rows(page)).toHaveCount(3)
  await expect(rows(page)).toContainText(['Stuck conveyor change', 'Historical anchor', 'Wire the Tasks view'])
})

// The task that came back to the signed-in person, and what it carries: the
// framing, the feedback they wrote, and a link to the task itself. The marker
// reaches the dashboard only folded into needs_attention, so the entry is
// decided from the task's own durable events rather than inferred from state
// (REQ-6).
const returnedFeedback = 'The retry path still swallows the provider error — surface it before merging.'
const returnedNextStep =
  'This implementation order is queued. Pick it up with conveyor run task-returned, or an eligible worker can claim it.'

function returnedSummary() {
  return {
    task: {
      id: 'task-returned',
      workspace: 'demo',
      source: 'cli',
      title: 'Returned conveyor change',
      repo: 'conveyor',
      branch: 'conveyor/task-returned',
      state: 'queued',
      next_stage: 'implement',
      assignee: { user_id: 'usr-assigned', email: 'assigned@example.test', display_name: 'Assigned User' },
      created_at: '2026-08-06T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-08-06T11:00:00Z',
    needs_attention: true,
  }
}

function returnedDetail(claimantID: string, claimedAfter = false) {
  const events = [
    {
      id: 1,
      task_id: 'task-returned',
      kind: 'work_order.claimed',
      actor_id: claimantID,
      actor_role: 'runner',
      payload: { id: 'task-returned-implement-1', stage: 'implement', claimant_id: claimantID },
      at: '2026-08-06T10:55:00Z',
    },
    {
      id: 2,
      task_id: 'task-returned',
      kind: 'pipeline.bounced',
      actor_id: 'usr-assigned',
      actor_role: 'user',
      payload: { source: 'user-request-changes', feedback: returnedFeedback, from: 'review', to: 'implement' },
      at: '2026-08-06T11:00:00Z',
    },
  ]
  if (claimedAfter) {
    events.push({
      id: 3,
      task_id: 'task-returned',
      kind: 'work_order.claimed',
      actor_id: 'worker-1',
      actor_role: 'runner',
      payload: { id: 'task-returned-implement-2', stage: 'implement' },
      at: '2026-08-06T11:05:00Z',
    })
  }
  return {
    task: returnedSummary().task,
    jobs: [],
    events,
    interventions: [],
    work_orders: [],
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: true,
    at_merge_gate: false,
  }
}

async function openTasksWithReturn(page: Page, options: { claimantID: string; claimedAfter?: boolean }) {
  await routeTasksSurface(page)
  // Registered after the surface defaults, so these win.
  await page.route('**/v1/activity?**', (route) =>
    route.fulfill({
      headers: {
        'X-Conveyor-Total': '1',
        'X-Conveyor-Limit': '100',
        'X-Conveyor-Offset': '0',
      },
      json: [returnedSummary()],
    }),
  )
  await page.route('**/v1/tasks/task-returned/activity**', (route) =>
    route.fulfill({ json: returnedDetail(options.claimantID, options.claimedAfter) }),
  )
  await page.goto('/?view=list')
  await expect(page.getByRole('button', { name: 'List', pressed: true })).toBeVisible()
}

test("a run-claimed bounce says the queued order is the user's or an eligible worker's", async ({ page }) => {
  await openTasksWithReturn(page, { claimantID: 'run:usr-assigned' })
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention.getByText('Returned conveyor change came back to you with feedback')).toBeVisible()
  await expect(attention.getByText(returnedFeedback, { exact: false })).toBeVisible()
  await expect(attention.getByRole('link', { name: 'Open task' })).toHaveAttribute(
    'href',
    '/tasks/task-returned?view=list',
  )

  // Every bounce re-queues an ordinary eligible order (DEC-74(3)): the user
  // can resume it, or an eligible worker can claim it.
  await expect(attention.getByText(returnedNextStep, { exact: false })).toBeVisible()
  await expect(attention.getByText('Nothing is running this', { exact: false })).toHaveCount(0)
})

test('a worker-claimed bounce states the same queued next step', async ({ page }) => {
  await openTasksWithReturn(page, { claimantID: 'worker-1' })
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention.getByText('Returned conveyor change came back to you with feedback')).toBeVisible()
  await expect(attention.getByText(returnedNextStep, { exact: false })).toBeVisible()
  await expect(attention.getByText('The factory is already on it', { exact: false })).toHaveCount(0)
})

// A response from before the retirement may still carry hold: true. Neither
// the Board card nor the list row renders a hold badge (DEC-74(3)).
test('stale hold never renders a Held or On hold badge on the Board or Tasks list', async ({ page }) => {
  await routeTasksSurface(page)
  const stale = { ...returnedSummary(), task: { ...returnedSummary().task, hold: true } }
  await page.route('**/v1/activity?**', (route) =>
    route.fulfill({
      headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      json: [stale],
    }),
  )
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(
      route,
      operations.map((item) => ({ ...item, task: { ...item.task, hold: true } })),
    ),
  )
  await page.goto('/')
  await expect(page.getByRole('region', { name: 'Task board' })).toBeVisible()
  await expect(page.getByText('Returned conveyor change', { exact: true })).toBeVisible()
  await expect(page.getByText('Held', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'List' }).click()
  await expandDone(page)
  await expect(rows(page)).toHaveCount(operations.length)
  await expect(page.getByText('On hold', { exact: true })).toHaveCount(0)
})

// The signal clears exactly where the server clears its own marker: once an
// implementation run has claimed the work the return created. Presentation of
// an existing marker, with no derivation of its own.
test('the returned-with-feedback entry clears once implementation picks it up', async ({ page }) => {
  await openTasksWithReturn(page, { claimantID: 'worker-1', claimedAfter: true })
  await expect(page.getByRole('region', { name: 'Needs your attention' })).toHaveCount(0)
  // The list itself is untouched by the band above it.
  await expandDone(page)
  await expect(rows(page)).toHaveCount(operations.length)
})

test('Tasks panel renders lightweight detail before requesting selected audit', async ({ page }) => {
  await openTasks(page)
  const event = {
    id: 482,
    task_id: 'task-blocked',
    kind: 'work_order.updated',
    actor_id: 'runner',
    actor_role: 'runner',
    at: '2026-08-06T11:00:00Z',
    payload: { id: 'review-7' },
    audit_available: true,
  }
  let audits = 0
  await page.route('**/v1/tasks/task-blocked/activity**', (route) =>
    route.fulfill({
      json: {
        task: operations[0].task,
        jobs: [],
        events: [event],
        work_orders: [],
        interventions: [],
        needs_attention: false,
        spec: {
          task_id: 'task-blocked',
          version: 1,
          content: 'Retained approved plan',
          approved: true,
          acceptance: [],
          decomposition: [],
        },
      },
    }),
  )
  await page.route('**/v1/tasks/task-blocked/audit/event/482**', (route) => {
    audits++
    expect(new URL(route.request().url()).searchParams.get('workspace_id')).toBe('demo')
    return route.fulfill({
      json: {
        kind: 'event',
        event: { ...event, payload: { governance_snapshot: { content: 'Panel complete authority' } } },
      },
    })
  })
  await page.getByRole('link', { name: 'Wire the Tasks view' }).click()
  const panel = page.getByRole('dialog', { name: 'Task detail' })
  await expect(panel.getByText('Retained approved plan')).toBeVisible()
  await expect(panel.getByRole('link', { name: /Abandoned prerequisite/ })).toBeVisible()
  await expect(panel.getByRole('link', { name: 'Open full task page' })).toHaveAttribute(
    'href',
    '/tasks/task-blocked/full',
  )
  expect(audits).toBe(0)
  await panel.getByText('Show technical activity').click()
  expect(audits).toBe(0)
  await panel.getByText('Event payload', { exact: true }).click()
  await expect(panel.locator('pre').filter({ hasText: 'Panel complete authority' })).toBeVisible()
  expect(audits).toBe(1)
})

for (const hasCreator of [true, false]) {
  test(`Created by in the Tasks panel shows ${hasCreator ? 'the workspace member' : 'Unknown'}`, async ({ page }) => {
    await routeTasksSurface(page)
    await page.route('**/v1/tasks/task-shipped/activity**', (route) =>
      route.fulfill({
        json: {
          task: operations[2].task,
          jobs: [],
          events: hasCreator
            ? [
                {
                  id: 1,
                  kind: 'task.created',
                  actor_role: 'human',
                  actor_id: 'user:usr-other',
                  at: '2026-08-04T10:00:00Z',
                },
              ]
            : [],
          interventions: [],
          work_orders: [],
          needs_attention: false,
        },
      }),
    )
    await page.goto('/tasks?task=task-shipped')
    const panel = page.getByRole('dialog', { name: 'Task detail' })
    const label = panel.locator('dt').filter({ hasText: /^Created$/ })
    await expect(label).toHaveCount(1)
    await expect(
      label.locator('xpath=following-sibling::dd[1]').locator('[data-task-creator] > span').first(),
    ).toHaveText(hasCreator ? 'Other Member' : 'Unknown')
    await expect(label.locator('xpath=preceding-sibling::dt[1]')).toHaveText('Assignee')
  })
}

test('failed triage row states the server label and keeps truthful plan state', async ({ page }, testInfo) => {
  await routeTasksSurface(page)
  const failed = {
    ...operations[0],
    task: {
      ...operations[0].task,
      id: 'failed-triage',
      title: 'Triage failed before planning',
      state: 'awaiting_human',
      next_stage: '',
      recovery_stage: 'triage',
      blocking_task_ids: [],
      dependencies: [],
    },
    latest_stage: 'triage',
    needs_attention: true,
    plan: { state: 'none' },
    next_action: {
      label: 'Triage failed',
      actor: 'operator',
      kind: 'triage_failure',
      judgment: true,
      summary: 'Triage could not read the task; retry triage with direction or close the task.',
      deadline: null,
    },
  }
  const gate = {
    ...failed,
    task: {
      ...failed.task,
      id: 'spec-gate',
      title: 'Plan awaiting approval',
      next_stage: 'implement',
      recovery_stage: 'implement',
    },
    latest_stage: 'spec',
    plan: { state: 'pending_gate', version: 1 },
    next_action: {
      label: 'Plan awaiting approval',
      actor: 'operator',
      kind: 'plan_approval',
      judgment: true,
      summary: 'Read plan v1 and approve it, request changes, or reject it.',
      deadline: null,
    },
  }
  await page.route('**/v1/task-operations?**', (route) => fulfillGrouped(route, [failed, gate]))
  await page.goto('/?view=list')
  // The State column is gone (AC-6.2); the operator line states the server's
  // label instead (AC-6.6), and no "No plan" badge appears (AC-1.4).
  const failedRow = row(page, 'Triage failed before planning')
  await expect(failedRow.locator('[data-operator-reason]')).toContainText('Triage failed')
  await expect(failedRow).not.toContainText('No plan')
  await expect(failedRow).not.toContainText('Plan awaiting approval')
  const gateRow = row(page, 'Plan awaiting approval')
  await expect(gateRow).not.toContainText('Triage failed')
  await expect(gateRow.locator('[data-operator-reason]')).toContainText('Plan awaiting approval')
  await testInfo.attach('failed-triage-tasks', { body: await page.screenshot(), contentType: 'image/png' })
})

// AC-2.4: both surfaces send the same Updated window — the row's last activity,
// not its creation — as the same server parameters, with local-day half-open
// bounds. Tasks defaults to any time; an invalid range sends nothing; a filter
// change returns to the first page.
test('Tasks and Board share Updated range parameters', async ({ page }) => {
  const taskRequests: URL[] = []
  const boardRequests: URL[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/v1/task-operations') taskRequests.push(url)
    if (url.pathname === '/v1/activity') boardRequests.push(url)
  })
  await routeTasksSurface(page)
  // One row per page, so a page change is visible before the filter resets it.
  await page.route('**/v1/task-operations?**', (route) =>
    fulfillGrouped(route, matchOperations(route.request().url()), { limit: 1 }),
  )
  await page.goto('/?view=list')
  await expandDone(page)
  await expect(rows(page)).toHaveCount(4)
  // Tasks opens on any time: no Updated bound at all.
  expect(taskRequests.at(-1)?.searchParams.has('updated_from')).toBe(false)
  expect(taskRequests.at(-1)?.searchParams.has('updated_to')).toBe(false)
  await groupRegion(page, 'Waiting').getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('2–2 of 2')).toBeVisible()

  await page.getByRole('button', { name: 'Open filters' }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await page.getByRole('option', { name: 'Custom range' }).click()
  await page.getByLabel('Updated from').fill('2026-08-05')
  await page.getByLabel('Updated to').fill('2026-08-05')
  // An old task with new activity: Shipped web change was created on 4 August
  // and last active on the 5th, so the Updated window selects it.
  await expect(rows(page)).toHaveCount(1)
  await expect(page.getByRole('link', { name: 'Shipped web change' })).toBeVisible()
  await expect(page.getByText('2–2 of 2')).toHaveCount(0)

  const expectedFrom = await page.evaluate(() => new Date(2026, 7, 5).toISOString())
  const expectedTo = await page.evaluate(() => new Date(2026, 7, 6).toISOString())
  const filtered = taskRequests.at(-1)
  expect(filtered?.searchParams.get('updated_from')).toBe(expectedFrom)
  expect(filtered?.searchParams.get('updated_to')).toBe(expectedTo)
  expect(filtered?.searchParams.has('created_from')).toBe(false)
  expect(filtered?.searchParams.get('offset') ?? '0').toBe('0')

  // An inverted range shows its error in place and asks the server nothing.
  const before = taskRequests.length
  await page.getByLabel('Updated to').fill('2026-08-04')
  await expect(page.getByText('Choose an end date on or after the start date.')).toBeVisible()
  expect(taskRequests.length).toBe(before)

  // The Board offers the same member and sends the same parameters.
  await page.goto('/')
  await page.getByRole('button', { name: 'Filters', exact: true }).click()
  await page.getByRole('tab', { name: 'Updated' }).click()
  await expect(page.getByRole('option', { name: 'Last month' })).toHaveAttribute('aria-selected', 'true')
  expect(boardRequests.at(-1)?.searchParams.has('updated_from')).toBe(true)
  await page.getByRole('option', { name: 'Custom range' }).click()
  await page.getByLabel('Updated from').fill('2026-08-05')
  await page.getByLabel('Updated to').fill('2026-08-05')
  await expect
    .poll(() => {
      const last = boardRequests.at(-1)?.searchParams
      return [last?.get('updated_from'), last?.get('updated_to'), last?.has('created_from')]
    })
    .toEqual([expectedFrom, expectedTo, false])
})
