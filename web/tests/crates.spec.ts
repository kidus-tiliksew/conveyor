// The crates list, the crate page, and task creation from a crate
// (req-crates REQ-3, REQ-5; component-crates invariants 17 to 19). A fake
// server serves the crate rows and crate reads in the shapes
// internal/httpapi/crates.go writes; the dashboard derives none of the
// counts, the shipped flag, or the next action labels.
import { expect, type Page, type Route } from '@playwright/test'
import type {
  Crate,
  CrateActivityEvent,
  CrateDetail,
  CrateInsight,
  CrateInsightTask,
  CratePhase,
  CrateReplayStep,
  CrateStation,
  CrateTask,
} from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

const createdAt = '2026-10-01T09:00:00Z'

// A calendar date `days` from the viewer's today, as the server writes a
// target date (YYYY-MM-DD).
function dateFromToday(days: number) {
  const date = new Date()
  date.setDate(date.getDate() + days)
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

function crateRow(overrides: Partial<Crate> & Pick<Crate, 'id' | 'name'>): Crate {
  return {
    target_date: null,
    state: 'open',
    shipped: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    closed_at: null,
    open_task_count: 0,
    merged_task_count: 0,
    total_task_count: 0,
    ...overrides,
  }
}

// The served order of the open list: soonest target date first, then crates
// with no date, then name (AC-5.1). Names are chosen so that no client-side
// sort by name or count could reproduce it.
const openCrates: Crate[] = [
  crateRow({
    id: 'crate-zeta',
    name: 'Zeta rollout',
    target_date: dateFromToday(3),
    open_task_count: 1,
    merged_task_count: 0,
    total_task_count: 1,
  }),
  crateRow({
    id: 'crate-billing',
    name: 'Billing v2',
    target_date: dateFromToday(12),
    open_task_count: 2,
    merged_task_count: 3,
    total_task_count: 5,
  }),
  crateRow({ id: 'crate-alpha', name: 'Alpha cleanup', open_task_count: 4, merged_task_count: 1, total_task_count: 6 }),
]

const closedCrates: Crate[] = [
  crateRow({
    id: 'crate-closed',
    name: 'Spring migration',
    state: 'closed',
    target_date: '2026-03-01',
    closed_at: '2026-03-04T12:00:00Z',
    open_task_count: 0,
    merged_task_count: 2,
    total_task_count: 3,
  }),
]

const nextActionVerbatim = 'Waiting on review seat 2 of 3 (served text)'

const billing: CrateDetail = {
  ...openCrates[1],
  requirements: [
    { id: 'req-billing', title: 'Customers pay by invoice', task_count: 2 },
    { id: 'req-refunds', title: 'Operators issue refunds', task_count: 0 },
  ],
  undeclared_requirements: [{ id: 'req-audit', title: 'Every money movement is audited', task_count: 1 }],
  tasks: [
    {
      id: '261101-aaaaaa',
      title: 'Send invoices by email',
      state: 'running',
      branch: 'conveyor/task-261101-aaaaaa',
      pull_request: { number: 412, url: 'https://github.com/acme/app/pull/412', repository: 'acme/app' },
      next_action: {
        label: nextActionVerbatim,
        actor: 'agent',
        kind: 'working',
        judgment: false,
        summary: 'An agent is reviewing.',
        deadline: null,
      },
    },
    {
      id: '261101-bbbbbb',
      title: 'Record invoice payments',
      state: 'merged',
      branch: 'conveyor/task-261101-bbbbbb',
      pull_request: { number: 398, url: 'https://github.com/acme/app/pull/398', repository: 'acme/app' },
      next_action: {
        label: 'Merged',
        actor: 'none',
        kind: 'terminal',
        judgment: false,
        summary: 'The task merged.',
        deadline: null,
      },
    },
  ],
}

// A member row whose next action label is the one the server states for a
// task in that state.
function member(id: string, title: string, state: CrateTask['state'], label: string): CrateTask {
  const done = state === 'merged' || state === 'closed'
  return {
    id,
    title,
    state,
    branch: `conveyor/task-${id}`,
    pull_request:
      state === 'closed'
        ? null
        : { number: Number.parseInt(id.slice(-3), 16), url: `https://github.com/acme/app/pull/${id}` },
    next_action: {
      label,
      actor: done ? 'none' : 'agent',
      kind: done ? 'terminal' : 'working',
      judgment: false,
      summary: label,
      deadline: null,
    },
  }
}

const shipped: CrateDetail = {
  ...crateRow({
    id: 'crate-shipped',
    name: 'Onboarding refresh',
    target_date: dateFromToday(-2),
    shipped: true,
    open_task_count: 0,
    merged_task_count: 2,
    total_task_count: 3,
  }),
  requirements: [{ id: 'req-onboarding', title: 'New users finish setup in one visit', task_count: 2 }],
  undeclared_requirements: [],
  tasks: [
    member('261020-a01', 'Shorten the welcome checklist', 'merged', 'Merged'),
    member('261020-a02', 'Remember the setup step a user reached', 'merged', 'Merged'),
    member('261020-a03', 'Send a setup reminder email', 'closed', 'Closed'),
  ],
}

const closed: CrateDetail = {
  ...closedCrates[0],
  requirements: [],
  undeclared_requirements: [],
  tasks: [
    member('260210-b01', 'Move tasks to the new schema', 'merged', 'Merged'),
    member('260210-b02', 'Backfill task events', 'merged', 'Merged'),
    member('260210-b03', 'Drop the old task table', 'closed', 'Closed'),
  ],
}

const undated: CrateDetail = {
  ...openCrates[2],
  requirements: [],
  undeclared_requirements: [],
  tasks: [
    member('261110-c01', 'Remove the retired shim flags', 'running', 'Agent working'),
    member('261110-c02', 'Delete unused board styles', 'queued', 'Queued'),
    member('261110-c03', 'Drop the legacy task list route', 'queued', 'Queued'),
    member('261110-c04', 'Rename stale test fixtures', 'running', 'Agent working'),
    member('261110-c05', 'Remove the old map prototype', 'merged', 'Merged'),
    member('261110-c06', 'Retire the planning bundle page', 'closed', 'Closed'),
  ],
}

// A closed crate that never had a target date.
const closedUndated: CrateDetail = {
  ...crateRow({
    id: 'crate-closed-undated',
    name: 'Old experiments',
    state: 'closed',
    closed_at: '2026-02-10T12:00:00Z',
    open_task_count: 0,
    merged_task_count: 0,
    total_task_count: 0,
  }),
  requirements: [],
  undeclared_requirements: [],
  tasks: [],
}

// Newest first, as the server pages them (AC-5.5).
const billingActivity: CrateActivityEvent[] = [
  {
    id: 30,
    kind: 'pull_request.opened',
    at: '2026-11-18T10:00:00Z',
    actor_id: 'usr_agent',
    actor_role: 'executor',
    task_id: '261101-aaaaaa',
    task_title: 'Send invoices by email',
    pull_request: { number: 412, url: 'https://github.com/acme/app/pull/412', repository: 'acme/app' },
  },
  {
    id: 25,
    kind: 'review.round_completed',
    at: '2026-11-17T12:00:00Z',
    actor_id: 'usr_reviewer',
    actor_role: 'executor',
    task_id: '261101-aaaaaa',
    task_title: 'Send invoices by email',
    verdict: 'changes_requested',
  },
  {
    id: 20,
    kind: 'merge.confirmed',
    at: '2026-11-17T10:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261101-bbbbbb',
    task_title: 'Record invoice payments',
  },
  {
    id: 10,
    kind: 'task.crate_set',
    at: '2026-11-10T10:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261101-aaaaaa',
    task_title: 'Send invoices by email',
  },
]

// An open crate with a task at every station, in the served order of
// component-crates invariant 28: the judgments first, then the agent's work,
// then the other open tasks, then merged, then closed. Neither an ID sort
// nor a title sort reproduces it, and it holds more than five tasks.
function judged(id: string, title: string, kind: string, label: string): CrateTask {
  return {
    id,
    title,
    state: 'awaiting_human',
    branch: `conveyor/task-${id}`,
    pull_request: null,
    next_action: { label, actor: 'operator', kind, judgment: true, summary: label, deadline: null },
  }
}

const lineCrate: CrateDetail = {
  ...crateRow({
    id: 'crate-line',
    name: 'Search relaunch',
    target_date: dateFromToday(20),
    open_task_count: 6,
    merged_task_count: 1,
    total_task_count: 8,
  }),
  requirements: [{ id: 'req-search', title: 'Shoppers find products by name', task_count: 8 }],
  undeclared_requirements: [],
  tasks: [
    judged(
      '261115-f07',
      'Approve the ranking change for merge',
      'merge_approval',
      'Merge approval waits on you (served)',
    ),
    judged('261115-a03', 'Approve the plan for synonym search', 'plan_approval', 'Plan approval waits on you (served)'),
    member('261115-c11', 'Index product reviews', 'running', 'Agent building (served)'),
    member('261115-b02', 'Score results by freshness', 'running', 'Agent reviewing (served)'),
    member('261115-e05', 'Add search analytics events', 'queued', 'Waiting for an agent (served)'),
    {
      ...member('261115-d09', 'Show spelling suggestions', 'queued', 'Blocked by another task (served)'),
      pull_request: null,
      next_action: {
        label: 'Blocked by another task (served)',
        actor: 'dependency',
        kind: 'blocked',
        judgment: false,
        summary: 'Blocked',
        deadline: null,
      },
    },
    member('261115-a01', 'Drop the legacy search index', 'merged', 'Merged'),
    member('261115-g04', 'Try a second ranking model', 'closed', 'Closed'),
  ],
}

// An open crate with no member task.
const emptyCrate: CrateDetail = {
  ...crateRow({ id: 'crate-empty', name: 'Next quarter' }),
  requirements: [],
  undeclared_requirements: [],
  tasks: [],
}

// The insight view as internal/crateinsight writes it (component-crates,
// Insight view): times in RFC 3339 UTC, empty lists as [], and absent times
// as null.
const lineNow = '2026-11-19T12:00:00Z'

function insightTask(
  id: string,
  phase: CratePhase,
  station: CrateStation,
  createdAt: string,
  extra: Partial<CrateInsightTask> = {},
): CrateInsightTask {
  return {
    id,
    phase,
    station,
    base_branch: 'main',
    created_at: createdAt,
    pull_request_opened_at: [],
    changes_requested_at: [],
    review_rounds: 0,
    merged_at: null,
    closed_at: null,
    ...extra,
  }
}

function step(at: string, taskId: string, phase: CratePhase, station: CrateStation): CrateReplayStep {
  return { at, task_id: taskId, phase, station }
}

function emptyProgress(): CrateInsight['progress'] {
  return {
    daily: [],
    forecast: null,
    weekly: [],
    delivered: 0,
    total: 0,
    open: 0,
    throughput_per_week: 0,
    median_cycle_seconds: null,
    status_mix: { delivered: 0, closed: 0, open: [] },
  }
}

// The line crate's history, in (at, task ID) order. Each task's last step is
// its entry in `tasks`.
const lineSteps: CrateReplayStep[] = [
  step('2026-11-10T09:00:00Z', '261115-a01', 'queued', 'intake'),
  step('2026-11-10T12:00:00Z', '261115-a01', 'building', 'build'),
  step('2026-11-11T09:00:00Z', '261115-c11', 'queued', 'intake'),
  step('2026-11-11T10:00:00Z', '261115-a01', 'review', 'review'),
  step('2026-11-12T09:00:00Z', '261115-b02', 'queued', 'intake'),
  step('2026-11-12T15:00:00Z', '261115-c11', 'building', 'build'),
  step('2026-11-13T09:00:00Z', '261115-a01', 'merged', 'crate'),
  step('2026-11-13T10:00:00Z', '261115-f07', 'queued', 'intake'),
  step('2026-11-14T09:00:00Z', '261115-a03', 'queued', 'intake'),
  step('2026-11-14T11:00:00Z', '261115-f07', 'building', 'build'),
  step('2026-11-15T09:00:00Z', '261115-g04', 'queued', 'intake'),
  step('2026-11-15T12:00:00Z', '261115-b02', 'building', 'build'),
  step('2026-11-16T09:00:00Z', '261115-d09', 'queued', 'intake'),
  step('2026-11-16T10:00:00Z', '261115-g04', 'closed', 'aside'),
  step('2026-11-16T14:00:00Z', '261115-a03', 'needs_you', 'plan'),
  step('2026-11-17T09:00:00Z', '261115-e05', 'queued', 'intake'),
  step('2026-11-17T10:00:00Z', '261115-b02', 'review', 'review'),
  step('2026-11-17T16:00:00Z', '261115-f07', 'review', 'review'),
  step('2026-11-18T08:00:00Z', '261115-f07', 'needs_you', 'gate'),
  step('2026-11-18T09:00:00Z', '261115-d09', 'blocked', 'intake'),
]

const lineInsight: CrateInsight = {
  tasks: [
    insightTask('261115-f07', 'needs_you', 'gate', '2026-11-13T10:00:00Z', { base_branch: 'develop' }),
    insightTask('261115-a03', 'needs_you', 'plan', '2026-11-14T09:00:00Z'),
    insightTask('261115-c11', 'building', 'build', '2026-11-11T09:00:00Z'),
    insightTask('261115-b02', 'review', 'review', '2026-11-12T09:00:00Z', {
      pull_request_opened_at: ['2026-11-17T10:00:00Z'],
    }),
    insightTask('261115-e05', 'queued', 'intake', '2026-11-17T09:00:00Z'),
    insightTask('261115-d09', 'blocked', 'intake', '2026-11-16T09:00:00Z'),
    insightTask('261115-a01', 'merged', 'crate', '2026-11-10T09:00:00Z', {
      pull_request_opened_at: ['2026-11-11T10:00:00Z'],
      review_rounds: 1,
      merged_at: '2026-11-13T09:00:00Z',
    }),
    insightTask('261115-g04', 'closed', 'aside', '2026-11-15T09:00:00Z', { closed_at: '2026-11-16T10:00:00Z' }),
  ],
  branches: ['main', 'develop'],
  whereabouts: [
    { key: 'not_started', branch: null, task_ids: ['261115-e05', '261115-d09'] },
    {
      key: 'task_branch',
      branch: null,
      task_ids: ['261115-f07', '261115-a03', '261115-c11', '261115-b02'],
    },
    { key: 'merged', branch: 'main', task_ids: ['261115-a01'] },
  ],
  progress: { ...emptyProgress(), delivered: 1, total: 7, open: 6 },
  first_review: { approved: 1, reviewed: 1 },
  forecast_days_before_target: null,
  replay: { start: '2026-11-10T09:00:00Z', end: lineNow, steps: lineSteps },
}

// A plain insight view for a crate whose test does not look at the line:
// each member at the place its state suggests, from one step at creation.
function plainInsight(crate: CrateDetail): CrateInsight {
  const place = (task: CrateTask): [CratePhase, CrateStation] =>
    task.state === 'merged'
      ? ['merged', 'crate']
      : task.state === 'closed'
        ? ['closed', 'aside']
        : task.state === 'awaiting_human'
          ? ['needs_you', 'gate']
          : task.state === 'running'
            ? ['building', 'build']
            : ['queued', 'intake']
  const start = '2026-11-01T09:00:00Z'
  return {
    tasks: crate.tasks.map((task) => insightTask(task.id, ...place(task), start)),
    branches: crate.tasks.length ? ['main'] : [],
    whereabouts: [],
    progress: emptyProgress(),
    first_review: { approved: 0, reviewed: 0 },
    forecast_days_before_target: null,
    replay: {
      start: crate.tasks.length ? start : lineNow,
      end: lineNow,
      steps: crate.tasks.map((task) => step(start, task.id, ...place(task))),
    },
  }
}

type Role = 'viewer' | 'maintainer' | 'operator'

const memberNextAction = {
  label: 'Agent working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'The agent is working.',
  deadline: null,
}

async function serveCrates(page: Page, options: { role?: Role; insight?: 'fail' } = {}) {
  const role = options.role ?? 'operator'
  const details: Record<string, CrateDetail> = {
    [billing.id]: structuredClone(billing),
    [shipped.id]: structuredClone(shipped),
    [closed.id]: structuredClone(closed),
    [undated.id]: structuredClone(undated),
    [closedUndated.id]: structuredClone(closedUndated),
    [lineCrate.id]: structuredClone(lineCrate),
    [emptyCrate.id]: structuredClone(emptyCrate),
  }
  const insightRequests: string[] = []
  const listRequests: string[] = []
  const created: Record<string, unknown>[] = []
  const patched: Record<string, unknown>[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()
    if (path === '/v1/workspaces')
      return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: createdAt }] })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: `usr_${role}`, role }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: {
          workspace: 'demo',
          database: 'postgres',
          max_bounces: 2,
          default_plan_state: 'needed',
          repos: [{ name: 'conveyor', base: 'main' }],
        },
      })
    if (path === '/v1/crates' && method === 'GET') {
      const state = url.searchParams.get('state') ?? 'open'
      listRequests.push(state)
      return route.fulfill({ json: state === 'closed' ? closedCrates : openCrates })
    }
    const activity = path.match(/^\/v1\/crates\/([^/]+)\/activity$/)
    if (activity) {
      return route.fulfill({ json: { events: activity[1] === billing.id ? billingActivity : [] } })
    }
    const insight = path.match(/^\/v1\/crates\/([^/]+)\/insight$/)
    if (insight) {
      const id = decodeURIComponent(insight[1])
      insightRequests.push(id)
      const crate = details[id]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate_not_found', crate_id: id } })
      if (options.insight === 'fail') return route.fulfill({ status: 500, json: { error: 'internal error' } })
      return route.fulfill({ json: id === lineCrate.id ? lineInsight : plainInsight(crate) })
    }
    const one = path.match(/^\/v1\/crates\/([^/]+)$/)
    if (one) {
      const crate = details[decodeURIComponent(one[1])]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate not found' } })
      if (method === 'PATCH') {
        const body = request.postDataJSON() as Partial<CrateDetail>
        patched.push(body as Record<string, unknown>)
        Object.assign(crate, body)
        return route.fulfill({ json: crate })
      }
      return route.fulfill({ json: crate })
    }
    if (path === '/v1/requirements')
      return route.fulfill({
        json: [
          ['req-billing', 'Customers pay by invoice'],
          ['req-refunds', 'Operators issue refunds'],
          ['req-other', 'Operators see a weekly digest'],
        ].map(([id, title]) => ({
          requirement: { id, title, current_version: 1 },
          current_version: { requirement_id: id, version: 1, confirmed: true },
          pending_versions: [],
          serving_blueprints: [],
          planning_sessions: [],
          artifacts: [],
        })),
      })
    if (path === '/v1/tasks' && method === 'POST') {
      // The intake form posts multipart data with the task as a JSON part.
      const task = (request.postData() ?? '').match(/name="task"\r\n\r\n(.*)\r\n/)
      created.push(JSON.parse(task?.[1] ?? '{}') as Record<string, unknown>)
      return route.fulfill({
        status: 201,
        json: {
          id: '261119-created',
          workspace: 'demo',
          title: 'Generated title',
          body: '',
          repo: 'conveyor',
          state: 'queued',
          created_at: createdAt,
        },
      })
    }
    // Task detail for any member task; each one belongs to Billing v2.
    const taskRead = path.match(/^\/v1\/tasks\/([^/]+)\/(activity|next-action)$/)
    if (taskRead && method === 'GET') {
      const id = decodeURIComponent(taskRead[1])
      if (taskRead[2] === 'next-action')
        return route.fulfill({ json: { ...memberNextAction, choices: [], details: {} } })
      return route.fulfill({
        json: {
          task: {
            id,
            workspace: 'demo',
            title: billing.tasks.find((task) => task.id === id)?.title ?? 'Send invoices by email',
            body: 'Send each invoice by email.',
            repo: 'conveyor',
            state: 'running',
            branch: `conveyor/task-${id}`,
            created_at: createdAt,
            crate: { id: billing.id, name: billing.name },
          },
          jobs: [],
          events: [],
          interventions: [],
          work_orders: [],
          reviews: [],
          latest_stage: 'implement',
          next_action: memberNextAction,
          checkout_available: false,
          checkout_guidance: '',
          needs_attention: false,
        },
      })
    }
    return route.fulfill({ json: [] })
  })
  return { listRequests, created, patched, insightRequests }
}

// A browser with no WebGL: every WebGL context request answers null, as on a
// machine without a graphics driver.
async function withoutWebGL(page: Page) {
  await page.addInitScript(() => {
    const getContext = HTMLCanvasElement.prototype.getContext
    HTMLCanvasElement.prototype.getContext = function (this: HTMLCanvasElement, kind: string, ...rest: unknown[]) {
      if (/webgl/i.test(kind)) return null
      return (getContext as (...args: unknown[]) => unknown).call(this, kind, ...rest)
    } as typeof getContext
  })
}

// The station counts the page states in text, in line order.
async function stationCounts(page: Page) {
  return page
    .locator('[data-line-counts] [data-station]')
    .evaluateAll((items) =>
      Object.fromEntries(
        items.map((item) => [item.getAttribute('data-station'), item.querySelector('[data-count]')?.textContent]),
      ),
    )
}

// Moves the replay slider to the slider step nearest `at`: the slider takes
// only whole steps from the start of the served span.
async function replayTo(page: Page, at: string) {
  const slider = page.getByRole('slider', { name: "Moment in the crate's history" })
  const min = Number(await slider.getAttribute('min'))
  const step = Number(await slider.getAttribute('step'))
  await slider.fill(String(min + Math.round((Date.parse(at) - min) / step) * step))
}

// No horizontal page scroll: neither the document nor any scroll container
// under `root` is wider than the viewport. The create form checks only its
// sheet, because the Tasks kanban behind it scrolls sideways by design.
async function expectNoHorizontalScroll(page: Page, root = 'body') {
  const overflow = await page.evaluate((root) => {
    const width = document.documentElement.clientWidth
    const wide: string[] = []
    if (document.documentElement.scrollWidth > width) wide.push('document')
    for (const element of Array.from(document.querySelectorAll<HTMLElement>(`${root} *`))) {
      const style = getComputedStyle(element)
      if (!/(auto|scroll)/.test(style.overflowX)) continue
      // A deliberately scrolling strip, such as a segmented control, is fine.
      if (element.closest('fieldset')) continue
      if (element.scrollWidth > element.clientWidth + 1 && element.clientWidth >= width - 32)
        wide.push(`${element.tagName.toLowerCase()}.${element.className} ${element.scrollWidth}>${element.clientWidth}`)
    }
    return wide
  }, root)
  expect(overflow).toEqual([])
}

test('primary navigation opens the crates list directly after Tasks', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/inbox')
  const nav = page.getByRole('navigation', { name: 'Primary' })
  // evaluateAll reads once without waiting; every entry renders in one commit.
  await expect(nav.getByRole('link', { name: 'Crates', exact: true })).toBeVisible()
  const labels = await nav
    .locator('a')
    .evaluateAll((links) => links.map((link) => link.textContent?.replace(/\d+$/, '').trim()))
  expect(labels.slice(0, 2)).toEqual(['Tasks', 'Crates'])
  await nav.getByRole('link', { name: 'Crates', exact: true }).click()
  await expect(page).toHaveURL(/\/w\/demo\/crates$/)
  await expect(page.getByRole('heading', { name: 'Crates', exact: true })).toBeVisible()
  await expect(nav.getByRole('link', { name: 'Crates', exact: true })).toHaveAttribute('aria-current', 'page')
})

// AC-5.1: the order the server returns, each open crate's name, target date,
// and open, merged, and total counts; closed crates only on request.
test('crates list keeps the served order and shows closed crates only on request', async ({ page }) => {
  const { listRequests } = await serveCrates(page)
  await page.goto('/w/demo/crates')

  const open = page.getByRole('list', { name: 'Open crates' })
  await expect(open.locator('[data-crate-name]')).toHaveText(['Zeta rollout', 'Billing v2', 'Alpha cleanup'])
  const row = open.locator('[data-crate-row="crate-billing"]')
  await expect(row.locator('[data-count="open"]')).toHaveText('2 open')
  await expect(row.locator('[data-count="merged"]')).toHaveText('3 merged')
  await expect(row.locator('[data-count="total"]')).toHaveText('5 total')
  await expect(row.locator('[data-target-date]:visible')).toHaveCount(1)
  await expect(row.getByText('12 days left').first()).toBeAttached()
  await expect(open.locator('[data-crate-row="crate-alpha"]').getByText('No target date').first()).toBeAttached()
  await expect(page.getByText('Spring migration')).toHaveCount(0)
  expect(listRequests).toEqual(['open'])

  await page.getByRole('group', { name: 'Crate state' }).getByRole('button', { name: 'Closed' }).click()
  await expect(page).toHaveURL(/show=closed/)
  const closedList = page.getByRole('list', { name: 'Closed crates' })
  await expect(closedList.locator('[data-crate-name]')).toHaveText(['Spring migration'])
  expect(listRequests).toContain('closed')

  await closedList.getByRole('link').first().click()
  await expect(page).toHaveURL(/\/w\/demo\/crates\/crate-closed$/)
})

// The open list never stands in for the closed list while it loads.
test('switching to closed crates shows no open crate while the closed list loads', async ({ page }) => {
  await serveCrates(page)
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route(/\/v1\/crates\?state=closed/, async (route) => {
    await held
    await route.fallback()
  })
  await page.goto('/w/demo/crates')
  await expect(page.locator('[data-crate-row]')).toHaveCount(3)
  await page.getByRole('group', { name: 'Crate state' }).getByRole('button', { name: 'Closed' }).click()
  await expect(page).toHaveURL(/show=closed/)
  await expect(page.locator('[data-crate-row]')).toHaveCount(0)
  release()
  await expect(page.getByRole('list', { name: 'Closed crates' }).locator('[data-crate-name]')).toHaveText([
    'Spring migration',
  ])
})

// AC-5.2 to AC-5.5 on one open crate with tasks.
test('crate page shows the header, requirements with the uncovered mark, verbatim next actions, and activity', async ({
  page,
}) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')

  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByRole('heading', { level: 1 })).toHaveText('Billing v2')
  await expect(hero.locator('[data-stat="merged"]')).toHaveText('3')
  await expect(hero.locator('[data-stat="total"]')).toHaveText('5')
  await expect(hero.locator('[data-stat="open"]')).toHaveText('2')
  await expect(hero.locator('p').first()).toHaveText(/^3 of 5 merged\s*·\s*,\s*2 open\s*60%$/)
  await expect(hero.getByText('Target date', { exact: true })).toBeVisible()
  await expect(hero.getByText('12 days left')).toBeVisible()
  // The merged share of the served counts.
  await expect(hero.locator('[data-merged-share]')).toHaveText('60%')
  await expect(hero.locator('[data-shipped-statement]')).toHaveCount(0)

  // Declared requirements with their task counts and the uncovered mark, and
  // the undeclared requirement apart (AC-5.4).
  const declared = page.getByRole('list', { name: 'Declared requirements' })
  await expect(declared.locator('[data-requirement="req-billing"]')).toContainText('2 tasks')
  await expect(declared.locator('[data-requirement="req-billing"] [data-uncovered-mark]')).toHaveCount(0)
  await expect(declared.locator('[data-requirement="req-refunds"] [data-uncovered-mark]')).toHaveText('Uncovered')
  await expect(page.getByText('1 declared requirement has no task yet.')).toBeVisible()
  const undeclared = page.getByRole('list', { name: 'Undeclared requirements' })
  await expect(undeclared.locator('[data-requirement]')).toHaveCount(1)
  await expect(undeclared.locator('[data-requirement="req-audit"]')).toContainText('1 task')
  await expect(undeclared.locator('[data-uncovered-mark]')).toHaveCount(0)

  // Member rows print the served label verbatim, the state, the branch, and
  // the pull request, and open the task (AC-5.3).
  const member = page.locator('[data-crate-task="261101-aaaaaa"]')
  await expect(member.locator('[data-next-action-label]')).toHaveText(nextActionVerbatim)
  await expect(member.locator('[data-task-state]')).toHaveAttribute('data-task-state', 'running')
  await expect(member.locator('[data-task-branch]')).toHaveText('conveyor/task-261101-aaaaaa')
  await expect(member.getByRole('link', { name: '#412' })).toHaveAttribute(
    'href',
    'https://github.com/acme/app/pull/412',
  )
  await expect(page.locator('[data-crate-task="261101-bbbbbb"] [data-next-action-label]')).toHaveText('Merged')

  // Activity newest first, with the pull request entry linked (AC-5.5).
  const activity = page.getByRole('region', { name: 'Activity' })
  await expect(activity.locator('[data-activity-kind]')).toHaveCount(4)
  expect(
    await activity
      .locator('[data-activity-kind]')
      .evaluateAll((items) => items.map((item) => item.dataset.activityKind)),
  ).toEqual(['pull_request.opened', 'review.round_completed', 'merge.confirmed', 'task.crate_set'])
  // The served verdict value reads as plain text.
  await expect(activity.locator('[data-activity-kind="review.round_completed"]')).toContainText(
    'Review finished: changes requested',
  )
  await expect(activity.getByRole('link', { name: /Pull request #412 opened/ })).toHaveAttribute(
    'href',
    'https://github.com/acme/app/pull/412',
  )

  await member.locator('[data-task-row]').click()
  await expect(page).toHaveURL(/\/w\/demo\/tasks\/261101-aaaaaa$/)
})

test('a shipped crate shows the shipped statement and no merged share', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-shipped')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.locator('[data-shipped-statement]')).toHaveText(
    'Shipped. Every task in this crate is merged or closed.',
  )
  await expect(hero.locator('[data-merged-share]')).toHaveCount(0)
  await expect(hero.locator('[data-stat="merged"]')).toHaveText('2')
  await expect(hero.locator('[data-stat="total"]')).toHaveText('3')
  // A shipped crate is never late.
  await expect(hero.getByText('Target date', { exact: true })).toBeVisible()
  await expect(hero.getByText(/days? late/)).toHaveCount(0)
})

// AC-5.2: a closed crate still shows its saved target date, read-only, with
// no hint and no "Set a target date".
test('a closed crate states its closing date, shows its saved target date read-only, and offers reopening', async ({
  page,
}) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-closed')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByText('Closed on Mar 4, 2026')).toBeVisible()
  await expect(hero.getByText('Target date', { exact: true })).toBeVisible()
  await expect(hero.locator('[data-target-date]')).toHaveText('Mar 1, 2026')
  await expect(hero.getByText(/days? (late|left)|Due today/)).toHaveCount(0)
  await expect(hero.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
  await expect(hero.locator('[data-merged-share]')).toHaveCount(0)
  await expect(hero.locator('[data-stat="open"]')).toHaveText('0')
  await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Reopen crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Add an existing task' })).toHaveCount(0)
})

test('a closed crate with no target date shows no date block and no set-date control', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-closed-undated')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByText('Closed on Feb 10, 2026')).toBeVisible()
  await expect(hero.getByText('Target date', { exact: true })).toHaveCount(0)
  await expect(hero.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
})

// A closed crate takes no new task (AC-2.1), so the form never sends the
// request that names it; "Not in a crate" creates the task without it.
test('task creation from a closed crate sends no crate_id', async ({ page }) => {
  const { created } = await serveCrates(page)
  await page.goto('/w/demo/?create=true&crate=crate-closed')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  await expect(sheet.locator('[data-intake-crate-name]')).toHaveText('Spring migration')
  await expect(sheet.getByText('This crate is closed, so a new task cannot join it.', { exact: false })).toBeVisible()
  await sheet.getByRole('textbox', { name: 'Task description' }).fill('Clean up after the migration')
  const create = sheet.getByRole('button', { name: 'Create task' })
  await expect(create).toBeDisabled()

  await sheet.getByRole('button', { name: 'Create this task outside the crate' }).click()
  await expect(sheet.locator('[data-intake-crate-name]')).toHaveCount(0)
  await expect(create).toBeEnabled()
  await create.click()
  await expect.poll(() => created.length).toBe(1)
  expect(created[0]).not.toHaveProperty('crate_id')
})

// The ghost button opens the crate form on the date field; after the save,
// focus lands on the new date block, and closing a dialog returns focus to
// its opener.
test('"Set a target date" opens the form on the date field and focus moves to the saved date', async ({ page }) => {
  const { patched } = await serveCrates(page)
  await page.goto('/w/demo/crates/crate-alpha')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByText('Target date', { exact: true })).toHaveCount(0)
  const setDate = hero.getByRole('button', { name: 'Set a target date' })

  await setDate.click()
  const dialog = page.getByRole('dialog', { name: 'Edit crate' })
  await expect(dialog.getByLabel('Target date')).toBeFocused()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(setDate).toBeFocused()

  await setDate.click()
  await expect(dialog.getByLabel('Target date')).toBeFocused()
  const date = dateFromToday(0)
  await dialog.getByLabel('Target date').fill(date)
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toHaveCount(0)
  expect(patched.at(-1)).toMatchObject({ name: 'Alpha cleanup', target_date: date })

  await expect(hero.locator('[data-target-date]')).toBeVisible()
  await expect(hero.getByText('Due today')).toBeVisible()
  await expect(hero.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.querySelector('[data-target-date]') != null))
    .toBe(true)
})

// The address moves from one loaded crate straight to another, as browser
// history can. An edit form open on the first never stays open on the second.
test('moving to another crate closes an open edit form', async ({ page }) => {
  const { patched } = await serveCrates(page)
  await page.goto('/w/demo/crates/crate-alpha')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toContainText('Alpha cleanup')
  await page.getByRole('link', { name: 'Crates', exact: true }).first().click()
  await page.locator('[data-crate-row="crate-billing"]').click()
  await expect(page.getByRole('region', { name: 'Crate summary' })).toContainText('Billing v2')
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await page.getByRole('menuitem', { name: 'Edit crate' }).click()
  await expect(page.getByRole('dialog', { name: 'Edit crate' }).getByLabel('Crate name')).toHaveValue('Billing v2')

  // The router follows a history move to the other, already loaded crate.
  await page.evaluate(() => {
    window.history.pushState(window.history.state, '', '/w/demo/crates/crate-alpha')
    window.dispatchEvent(new PopStateEvent('popstate', { state: window.history.state }))
  })
  await expect(page.getByRole('region', { name: 'Crate summary' })).toContainText('Alpha cleanup')
  await expect(page.getByRole('dialog', { name: 'Edit crate' })).toHaveCount(0)
  expect(patched).toEqual([])
})

test('the edit form focuses the name field first', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await page.getByRole('menuitem', { name: 'Edit crate' }).click()
  await expect(page.getByRole('dialog', { name: 'Edit crate' }).getByLabel('Crate name')).toBeFocused()
})

// AC-3.1 and AC-3.2: the form names the crate, lists its declared
// requirements first, preselects none, and sends crate_id with only the
// picked requirement_ids.
test('a task created from a crate sends crate_id and only the selected requirements', async ({ page }) => {
  const { created } = await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')
  await page.getByRole('link', { name: 'New task' }).click()
  await expect(page).toHaveURL(/create=true/)
  await expect(page).toHaveURL(/crate=crate-billing/)

  const sheet = page.getByRole('dialog', { name: 'New task' })
  const notice = sheet.getByRole('region', { name: 'Crate' })
  await expect(notice.locator('[data-intake-crate-name]')).toHaveText('Billing v2')
  const declared = notice.getByRole('list', { name: 'Requirements the crate declares' })
  await expect(declared.getByRole('button')).toHaveCount(2)
  for (const chip of await declared.getByRole('button').all())
    await expect(chip).toHaveAttribute('aria-pressed', 'false')

  await sheet.getByRole('combobox', { name: 'Search context' }).click()
  const listbox = sheet.getByRole('listbox', { name: 'Context' })
  const groups = listbox.getByRole('group')
  await expect(groups.first()).toHaveAttribute('aria-label', 'Declared by Billing v2')
  await expect(listbox.getByRole('group', { name: 'Declared by Billing v2' }).getByRole('option')).toHaveCount(2)
  await expect(listbox.getByRole('option', { selected: true })).toHaveCount(0)
  // The other requirements group leaves out the declared ones.
  const other = listbox.getByRole('group', { name: 'Other requirements' })
  await expect(other.getByRole('option')).toHaveCount(1)
  await expect(other.getByRole('option', { name: /Operators see a weekly digest/ })).toBeVisible()
  await page.keyboard.press('Escape')

  await declared.getByRole('button', { name: /Operators issue refunds/ }).click()
  await expect(declared.getByRole('button', { name: /Operators issue refunds/ })).toHaveAttribute(
    'aria-pressed',
    'true',
  )
  await sheet.getByRole('textbox', { name: 'Task description' }).fill('Let operators refund an invoice')
  await sheet.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(() => created.length).toBe(1)
  expect(created[0].crate_id).toBe('crate-billing')
  expect(created[0].requirement_ids).toEqual(['req-refunds'])
})

// Every act renders only for a user with create_tasks; the server checks it
// again.
test('a viewer without create_tasks sees no crate acts', async ({ page }) => {
  await serveCrates(page, { role: 'viewer' })
  await page.goto('/w/demo/crates')
  await expect(page.locator('[data-crate-row]')).toHaveCount(3)
  await expect(page.getByRole('button', { name: 'New crate' })).toHaveCount(0)

  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  await expect(page.locator('[data-crate-task]')).toHaveCount(2)
  await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Crate actions' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Remove .* from this crate/ })).toHaveCount(0)

  await page.goto('/w/demo/crates/crate-alpha')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add an existing task' })).toHaveCount(0)
})

test('a maintainer with create_tasks sees every crate act', async ({ page }) => {
  await serveCrates(page, { role: 'maintainer' })
  await page.goto('/w/demo/crates')
  await expect(page.getByRole('button', { name: 'New crate' })).toBeVisible()
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('link', { name: 'New task' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add task' })).toBeVisible()
  await expect(page.getByRole('button', { name: /Remove .* from this crate/ })).toHaveCount(2)
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Edit crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Close crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Add an existing task' })).toBeVisible()
})

// AC-5.6: task detail names the crate and links to its page.
test('task detail shows the crate fact linked to the crate page', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/tasks/261119-member')
  const fact = page.locator('[data-task-crate="crate-billing"]')
  await expect(fact).toHaveText('Billing v2')
  await expect(fact).toHaveAttribute('href', '/w/demo/crates/crate-billing')
})

test.describe('at 375 px', () => {
  test.use({ viewport: { width: 375, height: 812 } })

  test('the crates list, the crate page, and the create form have no horizontal page scroll', async ({ page }) => {
    await serveCrates(page)
    await page.goto('/w/demo/crates')
    await expect(page.locator('[data-crate-row]')).toHaveCount(3)
    await expectNoHorizontalScroll(page)

    await page.goto('/w/demo/crates/crate-billing')
    await expect(page.locator('[data-crate-task]')).toHaveCount(2)
    await expect(page.getByRole('region', { name: 'Activity' }).locator('[data-activity-kind]')).toHaveCount(4)
    await expectNoHorizontalScroll(page)

    await page.goto('/w/demo/?create=true&crate=crate-billing')
    await expect(page.getByRole('dialog', { name: 'New task' }).locator('[data-intake-crate-name]')).toHaveText(
      'Billing v2',
    )
    await expectNoHorizontalScroll(page, '[role="dialog"]')
  })
})

// The served order of the line crate's task rows (component-crates
// invariant 28).
const lineOrder = lineCrate.tasks.map((task) => task.id)

async function rowOrder(page: Page) {
  return page.locator('[data-crate-task]').evaluateAll((rows) => rows.map((row) => row.getAttribute('data-crate-task')))
}

// The crate's line, its replay, the task list, and the page layout, at a
// phone width and a desktop width (req-crate-insights REQ-1, REQ-3;
// req-crates AC-5.7; component-crates invariants 33 and 34). Every assertion
// reads page elements, so these specs pass in a browser with no WebGL.
for (const viewport of [
  { name: '375 px', width: 375, height: 812 },
  { name: 'desktop', width: 1280, height: 900 },
]) {
  test.describe(`crate line at ${viewport.name}`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height } })

    // AC-1.1: the count at each of the six stations, from the served view.
    test('the header states the served count at each station', async ({ page }) => {
      const { insightRequests } = await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const hero = page.getByRole('region', { name: 'Crate summary' })
      await expect(hero.getByRole('heading', { level: 1 })).toHaveText('Search relaunch')
      await expect(hero.locator('[data-stat="merged"]')).toHaveText('1')
      await expect(hero.locator('[data-stat="total"]')).toHaveText('8')
      await expect(hero.locator('[data-stat="open"]')).toHaveText('6')
      const counts = hero.getByRole('list', { name: 'Tasks at each station' })
      await expect(counts.getByRole('listitem')).toHaveText([
        'Queued: 2',
        'Plan: 1',
        'Build: 1',
        'Review: 1',
        'Merge gate: 1',
        'Merged: 1',
      ])
      expect(await stationCounts(page)).toEqual({
        intake: '2',
        plan: '1',
        build: '1',
        review: '1',
        gate: '1',
        crate: '1',
      })
      expect(insightRequests).toContain('crate-line')
      // With the scene drawn, the legend names each colour the line uses.
      if ((await hero.locator('[data-line-mode]').getAttribute('data-line-mode')) === 'scene') {
        await expect(hero.locator('[data-line-legend] [data-phase]')).toHaveText([
          'Queued',
          'Building',
          'In review',
          'Waits on a person',
          'Blocked by a task',
          'Merged',
          'Closed',
        ])
      }
    })

    // AC-5.7: the served order, five rows, and every row on request.
    test('the task list keeps the served order, shows five rows, and expands to every row', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      await expect(page.locator('[data-crate-task]')).toHaveCount(5)
      expect(await rowOrder(page)).toEqual(lineOrder.slice(0, 5))
      const toggle = page.locator('[data-crate-tasks-toggle]')
      await expect(toggle).toHaveAttribute('aria-expanded', 'false')
      await expect(toggle).toHaveAttribute('aria-controls', 'crate-task-list')
      await expect(toggle).toContainText('Show all 8 tasks')
      await expect(toggle).toContainText('3 more')

      await toggle.click()
      await expect(toggle).toHaveAttribute('aria-expanded', 'true')
      await expect(page.locator('[data-crate-task]')).toHaveCount(8)
      expect(await rowOrder(page)).toEqual(lineOrder)
      await expect(page.locator('[data-crate-task="261115-f07"] [data-next-action-label]')).toHaveText(
        'Merge approval waits on you (served)',
      )

      await toggle.click()
      await expect(toggle).toHaveAttribute('aria-expanded', 'false')
      await expect(page.locator('[data-crate-task]')).toHaveCount(5)
    })

    // AC-1.4: each task at its last served step at or before the moment, no
    // task before its first step, and the present when the replay ends.
    test('a replay moves a task between stations and ends at the present', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const time = page.locator('[data-replay-time]')
      await expect(time).toHaveText('Now')

      await replayTo(page, '2026-11-10T10:00:00Z')
      await expect(time).not.toHaveText('Now')
      await expect
        .poll(() => stationCounts(page))
        .toEqual({
          intake: '1',
          plan: '0',
          build: '0',
          review: '0',
          gate: '0',
          crate: '0',
        })

      // "Score results by freshness" waits at the start of the line ...
      await replayTo(page, '2026-11-13T12:00:00Z')
      await expect
        .poll(() => stationCounts(page))
        .toEqual({
          intake: '2',
          plan: '0',
          build: '1',
          review: '0',
          gate: '0',
          crate: '1',
        })
      // ... and is in review four days later.
      await replayTo(page, '2026-11-17T12:00:00Z')
      await expect
        .poll(() => stationCounts(page))
        .toEqual({
          intake: '2',
          plan: '1',
          build: '2',
          review: '1',
          gate: '0',
          crate: '1',
        })

      // Play and pause.
      await page.getByRole('button', { name: 'Continue the replay' }).click()
      await expect(page.getByRole('button', { name: 'Pause the replay' })).toBeVisible()
      await page.getByRole('button', { name: 'Pause the replay' }).click()
      await expect(page.getByRole('button', { name: 'Continue the replay' })).toBeVisible()
      await expect(time).not.toHaveText('Now')

      // Played to its end, the replay shows the present again.
      await replayTo(page, '2026-11-19T11:30:00Z')
      await page.getByRole('button', { name: 'Continue the replay' }).click()
      await expect(time).toHaveText('Now')
      await expect(page.getByRole('button', { name: 'Replay this crate from its first task' })).toBeVisible()
      await expect
        .poll(() => stationCounts(page))
        .toEqual({
          intake: '2',
          plan: '1',
          build: '1',
          review: '1',
          gate: '1',
          crate: '1',
        })
    })

    // AC-1.5: without WebGL the counts are visible text and every other part
    // renders.
    test('without WebGL the station counts are visible text and the rest of the page renders', async ({ page }) => {
      await withoutWebGL(page)
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const hero = page.getByRole('region', { name: 'Crate summary' })
      await expect(hero.locator('[data-line-mode]')).toHaveAttribute('data-line-mode', 'text')
      const counts = hero.getByRole('list', { name: 'Tasks at each station' })
      await expect(counts).toBeVisible()
      for (const item of await counts.getByRole('listitem').all()) await expect(item).toBeVisible()
      await expect(counts.locator('[data-station="build"]')).toHaveText('Build: 1')
      await expect(page.locator('canvas')).toHaveCount(0)
      await expect(hero.getByRole('heading', { level: 1 })).toHaveText('Search relaunch')
      await expect(page.getByRole('region', { name: 'Requirements' })).toBeVisible()
      await expect(page.getByRole('region', { name: 'Tasks' })).toBeVisible()
      await expect(page.getByRole('region', { name: 'Activity' })).toBeVisible()
      await expect(page.locator('[data-crate-task]')).toHaveCount(5)
      // The replay still moves the counts.
      await replayTo(page, '2026-11-10T10:00:00Z')
      await expect(counts.locator('[data-station="intake"]')).toHaveText('Queued: 1')
      await expect(counts.locator('[data-station="build"]')).toHaveText('Build: 0')
      await expectNoHorizontalScroll(page)
    })

    // REQ-3: pointing at a row emphasizes the task, and the row keeps its
    // link to task detail.
    test('pointing at a task row marks it and emphasizes its block', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const row = page.locator('[data-crate-task="261115-c11"]')
      await row.hover()
      await expect(row).toHaveAttribute('data-emphasized', 'true')
      await expect(page.locator('[data-emphasized]')).toHaveCount(1)
      // Where the scene is drawn beside the list, the block's card prints the
      // title, the label verbatim, and the base branch. On a narrow screen
      // the line is scrolled away while the row is in view.
      const mode = await page.locator('[data-line-mode]').getAttribute('data-line-mode')
      if (mode === 'scene' && viewport.width >= 1024) {
        await expect(page.locator('[data-line-scene]')).toBeVisible()
        const card = page.locator('[data-line-card="261115-c11"]')
        await expect(card).toContainText('Index product reviews')
        await expect(card.locator('[data-line-card-label]')).toHaveText('Agent building (served)')
        await expect(card).toContainText('→ main')
      }
      await page.mouse.move(0, 0)
      await expect(page.locator('[data-emphasized]')).toHaveCount(0)
      await row.locator('[data-task-row]').click()
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-c11$/)
    })

    // AC-1.3 and REQ-3 from the line's side: pointing at a block marks its
    // row and shows its card, and activating the block opens task detail.
    // The scene needs WebGL and sits beside the list only from lg, so the
    // spec runs where both hold.
    test('pointing at a block marks its row, and activating it opens task detail', async ({ page }) => {
      test.skip(viewport.width < 1024, 'the line and the task list share the screen only from lg')
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const mode = await page.locator('[data-line-mode]').getAttribute('data-line-mode')
      test.skip(mode !== 'scene', 'this browser has no WebGL')
      await expect(page.locator('[data-station-sign="gate"]')).toBeVisible()
      // The row's emphasis places the block's card over the block.
      const row = page.locator('[data-crate-task="261115-f07"]')
      await row.hover()
      const card = page.locator('[data-line-card="261115-f07"]')
      await expect(card).toBeVisible()
      const anchor = await card.evaluate((element) =>
        (element as HTMLElement).style.transform
          .match(/translate\(([-\d.]+)px, ([-\d.]+)px\)$/)
          ?.slice(1)
          .map(Number),
      )
      const scene = await page.locator('[data-line-scene]').boundingBox()
      if (!anchor || !scene) throw new Error('the card has no place over the scene')
      // The card stands just above the block, so the block is below its point.
      const block = { x: scene.x + anchor[0], y: scene.y + anchor[1] + 22 }
      await page.mouse.move(block.x, block.y)
      await expect(row).toHaveAttribute('data-emphasized', 'true')
      await expect(card.locator('[data-line-card-label]')).toHaveText('Merge approval waits on you (served)')
      await expect(card).toContainText('Approve the ranking change for merge')
      await expect(card).toContainText('→ develop')
      await page.mouse.click(block.x, block.y)
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-f07$/)
    })

    // Invariant 33: activity in the main column, requirements and then the
    // tasks beside it from lg, and before it below lg.
    test('activity takes the main column with requirements and tasks beside it or before it', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-billing')
      const hero = await page.getByRole('region', { name: 'Crate summary' }).boundingBox()
      const requirements = await page.getByRole('region', { name: 'Requirements' }).boundingBox()
      const tasks = await page.getByRole('region', { name: 'Tasks' }).boundingBox()
      const activity = await page.getByRole('region', { name: 'Activity' }).boundingBox()
      if (!hero || !requirements || !tasks || !activity) throw new Error('a page part did not render')
      expect(requirements.y).toBeGreaterThan(hero.y)
      expect(tasks.y).toBeGreaterThan(requirements.y)
      if (viewport.width >= 1024) {
        expect(requirements.x).toBeGreaterThan(activity.x + activity.width - 1)
        expect(Math.abs(activity.y - requirements.y)).toBeLessThan(2)
      } else {
        expect(activity.y).toBeGreaterThan(tasks.y)
      }
    })

    test('an empty crate shows its header and a line with no block', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-empty')
      const hero = page.getByRole('region', { name: 'Crate summary' })
      await expect(hero.getByRole('heading', { level: 1 })).toHaveText('Next quarter')
      expect(await stationCounts(page)).toEqual({
        intake: '0',
        plan: '0',
        build: '0',
        review: '0',
        gate: '0',
        crate: '0',
      })
      // A span of no length offers no replay.
      await expect(page.locator('[data-replay]')).toHaveCount(0)
      await expect(page.getByText('No tasks in this crate yet')).toBeVisible()
    })
  })
}

// The insight read fails apart from the crate read: the header facts and
// every other part still render, with a way to try the line again.
test('a failed insight read keeps the rest of the page', async ({ page }) => {
  const { insightRequests } = await serveCrates(page, { insight: 'fail' })
  await page.goto('/w/demo/crates/crate-billing')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.locator('[data-line-error]')).toContainText('The line of stations did not load.')
  // The page prints the contract layer's message (component-web-dashboard
  // invariant 13).
  await expect(hero.locator('[data-line-error]')).toContainText('internal error')
  await expect(hero.locator('[data-stat="merged"]')).toHaveText('3')
  await expect(page.locator('[data-crate-task]')).toHaveCount(2)
  await expect(page.getByRole('region', { name: 'Activity' }).locator('[data-activity-kind]')).toHaveCount(4)
  const before = insightRequests.length
  await hero.getByRole('button', { name: 'Try again' }).click()
  await expect.poll(() => insightRequests.length).toBeGreaterThan(before)
})

test.describe('with reduced motion', () => {
  // AC-1.5: the line shows its tasks without animation.
  test('the line stands still and the counts stay served', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' })
    await serveCrates(page)
    await page.goto('/w/demo/crates/crate-line')
    const band = page.locator('[data-line-mode]')
    await expect(band).toHaveAttribute('data-motion', 'reduced')
    if ((await band.getAttribute('data-line-mode')) === 'scene')
      await expect(page.locator('[data-line-scene]')).toHaveAttribute('data-motion', 'still')
    await expect
      .poll(() => stationCounts(page))
      .toEqual({ intake: '2', plan: '1', build: '1', review: '1', gate: '1', crate: '1' })
    // The replay still moves the counts, with no animation between them.
    await replayTo(page, '2026-11-13T12:00:00Z')
    await expect
      .poll(() => stationCounts(page))
      .toEqual({
        intake: '2',
        plan: '0',
        build: '1',
        review: '0',
        gate: '0',
        crate: '1',
      })
  })
})
