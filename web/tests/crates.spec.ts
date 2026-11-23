// The crates list, the crate page, and task creation from a crate
// (req-crates REQ-3, REQ-5; component-crates invariants 17 to 19). A fake
// server serves the crate rows and crate reads in the shapes
// internal/httpapi/crates.go writes; the dashboard derives none of the
// counts, the shipped flag, or the next action labels.
import { expect, type Page, type Route } from '@playwright/test'
import type {
  Crate,
  CrateActivityEvent,
  CrateBriefing,
  CrateBriefingView,
  CrateDetail,
  CrateInsight,
  CrateInsightTask,
  CrateListRow,
  CratePhase,
  CrateReplayStep,
  CrateStation,
  CrateTask,
} from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { controlWorkspaceChanges } from './helpers/workspace-changes'

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

// No count or percentage reads as row text at rest: the row's rendered text
// (innerText, which leaves out hidden text) holds none.
const countText = /\d+\s*(open|merged|total)\b|%/

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

// A readable task that belongs to no crate, for a `task` search value that
// names a task outside the crate's member list.
const outsideTask = { id: '261115-zzz', title: 'Tidy the release notes' }

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

// The stored briefing as internal/httpapi/crate_briefing.go writes it. The
// page shows none of `model`, `generated_at`, or `generated_by`.
function storedBriefing(overrides: Partial<CrateBriefing> = {}): CrateBriefing {
  return {
    headline: 'Two tasks wait on a person before the crate can ship.',
    account: 'One task merged this week. Review moves slowly, and the ranking change waits on a merge approval.',
    look_first: [],
    model: 'served-model-name',
    generated_at: '2026-11-19T10:00:00Z',
    generated_by: 'usr_operator',
    current: true,
    ...overrides,
  }
}

// The headline the fake list serves for a row that states no briefing.
const listHeadline = 'The crate moves as planned.'

// What the fake server answers to one briefing POST, when a spec sets it.
type BriefingReply = { status: number; json?: unknown; body?: string; contentType?: string }

// `insights` replaces the served insight view of the crates it names. A spec
// may change it between page loads: the fake server reads it per request.
// `briefings` holds the stored briefing of the crates it names, null for none.
// A crate it leaves out has a current briefing with no task to look at first.
// The fake server reads and writes the object per request, so a spec can
// change it while the page is open. `briefingPost` answers a POST in place of
// the default, which stores a new current briefing. `openList` replaces the
// served open crates list; without it the fake server serves `openCrates`.
// `closedList` does the same for the closed list and `closedCrates`.
// A served row that states no `briefing` carries one generated as the list
// is read.
async function serveCrates(
  page: Page,
  options: {
    role?: Role
    insight?: 'fail'
    insights?: Record<string, CrateInsight>
    openList?: Crate[]
    closedList?: Crate[]
    briefings?: Record<string, CrateBriefing | null>
    briefingPost?: (crateId: string, count: number) => Promise<BriefingReply | undefined> | BriefingReply | undefined
    activity?: Record<string, CrateActivityEvent[]>
  } = {},
) {
  const role = options.role ?? 'operator'
  const briefings = options.briefings ?? {}
  const briefingGets: string[] = []
  const briefingPosts: string[] = []
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
  // The workspace-wide activity read the Board's task order comes from.
  const boardActivityReads: string[] = []
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
    if (path === '/v1/activity') boardActivityReads.push(url.search)
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
      const rows: Crate[] = state === 'closed' ? (options.closedList ?? closedCrates) : (options.openList ?? openCrates)
      // A row that states no briefing carries one generated as the list is
      // read, so the list asks for none (component-crates invariant 35).
      return route.fulfill({
        json: rows.map(
          (row): CrateListRow => ({
            briefing: { headline: listHeadline, generated_at: new Date().toISOString() },
            ...row,
          }),
        ),
      })
    }
    const activity = path.match(/^\/v1\/crates\/([^/]+)\/activity$/)
    if (activity) {
      const events = options.activity?.[activity[1]] ?? (activity[1] === billing.id ? billingActivity : [])
      return route.fulfill({ json: { events } })
    }
    const insight = path.match(/^\/v1\/crates\/([^/]+)\/insight$/)
    if (insight) {
      const id = decodeURIComponent(insight[1])
      insightRequests.push(id)
      const crate = details[id]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate_not_found', crate_id: id } })
      if (options.insight === 'fail') return route.fulfill({ status: 500, json: { error: 'internal error' } })
      const served = options.insights?.[id] ?? (id === lineCrate.id ? lineInsight : plainInsight(crate))
      return route.fulfill({ json: served })
    }
    // The briefing routes of component-crates, Public surfaces.
    const briefing = path.match(/^\/v1\/crates\/([^/]+)\/briefing$/)
    if (briefing) {
      const id = decodeURIComponent(briefing[1])
      // `briefingPost` answers first, so it can answer for a crate that only
      // a list serves.
      let posted = false
      if (method === 'POST') {
        briefingPosts.push(id)
        posted = true
        const reply = await options.briefingPost?.(id, briefingPosts.length)
        if (reply) return route.fulfill(reply)
      }
      const crate = details[id]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate_not_found', crate_id: id } })
      const view = (): CrateBriefingView => ({
        briefing: id in briefings ? briefings[id] : storedBriefing(),
        fingerprint: 'fingerprint-live',
        can_generate: role !== 'viewer',
      })
      if (posted) {
        if (crate.tasks.length === 0)
          return route.fulfill({ status: 409, json: { error: 'crate_empty', crate_id: id, message: 'Empty.' } })
        briefings[id] = storedBriefing({
          headline: 'A new briefing came back from the server.',
          generated_at: '2026-11-19T12:00:00Z',
        })
        return route.fulfill({ json: view() })
      }
      briefingGets.push(id)
      return route.fulfill({ json: view() })
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
    // Task detail for any member task, read with the title and crate of the
    // crate that holds it. A task no crate holds reads as Billing v2's first
    // task, except the task outside every crate.
    const taskRead = path.match(/^\/v1\/tasks\/([^/]+)\/(activity|next-action)$/)
    if (taskRead && method === 'GET') {
      const id = decodeURIComponent(taskRead[1])
      if (taskRead[2] === 'next-action')
        return route.fulfill({ json: { ...memberNextAction, choices: [], details: {} } })
      const owner = Object.values(details).find((crate) => crate.tasks.some((task) => task.id === id))
      const outside = id === outsideTask.id
      const holder = outside ? undefined : (owner ?? billing)
      return route.fulfill({
        json: {
          task: {
            id,
            workspace: 'demo',
            title: outside
              ? outsideTask.title
              : (owner?.tasks.find((task) => task.id === id)?.title ?? 'Send invoices by email'),
            body: 'Send each invoice by email.',
            repo: 'conveyor',
            state: 'running',
            branch: `conveyor/task-${id}`,
            created_at: createdAt,
            crate: holder ? { id: holder.id, name: holder.name } : undefined,
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
  return { listRequests, created, patched, insightRequests, briefingGets, briefingPosts, boardActivityReads }
}

// No horizontal page scroll: neither the document nor any scroll container
// under `root` is wider than the viewport.
async function expectNoHorizontalScroll(page: Page, root = 'body') {
  const overflow = await page.evaluate((root) => {
    const width = document.documentElement.clientWidth
    const wide: string[] = []
    if (document.documentElement.scrollWidth > width) wide.push('document')
    for (const element of Array.from(document.querySelectorAll<HTMLElement>(`${root} *`))) {
      const style = getComputedStyle(element)
      if (!/(auto|scroll)/.test(style.overflowX)) continue
      // A deliberately scrolling strip, such as a segmented control, is fine.
      // So is the branch map, which scrolls sideways inside its card below
      // 680 px (req-crate-insights REQ-2).
      if (element.closest('fieldset, [data-branch-map-scroll]')) continue
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
// and completion pie with no count text; closed crates only on request.
test('crates list keeps the served order and shows closed crates only on request', async ({ page }) => {
  const { listRequests } = await serveCrates(page)
  await page.goto('/w/demo/crates')

  const open = page.getByRole('list', { name: 'Open crates' })
  await expect(open.locator('[data-crate-name]')).toHaveText(['Zeta rollout', 'Billing v2', 'Alpha cleanup'])
  const row = open.locator('[data-crate-row="crate-billing"]')
  await expect(row.getByRole('img', { name: '60% done · 3 of 5 merged', exact: true })).toBeVisible()
  await expect(row.locator('[data-count]')).toHaveCount(0)
  await expect(row).not.toContainText(countText, { useInnerText: true })
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

  // A member row opens the task's detail in the sheet over the crate page
  // (AC-5.8).
  await member.locator('[data-task-row]').click()
  await expect(page).toHaveURL(/\/w\/demo\/crates\/crate-billing\?task=261101-aaaaaa$/)
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
  await expect(hero).toBeVisible()
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

// Tasks to look at first: two members of the line crate and one task that
// has left it, which gets no row.
const lookFirst: CrateBriefing['look_first'] = [
  { task_id: '261115-c11', tone: 'attention', reason: 'Its build has run for three days.' },
  { task_id: '261115-gone', tone: 'info', reason: 'A task that left the crate.' },
  { task_id: '261115-b02', tone: 'positive', reason: 'Review is close to done.' },
]

// A promise the spec resolves to let a held POST answer.
function held() {
  let release = () => {}
  const promise = new Promise<void>((resolve) => {
    release = resolve
  })
  return { promise, release }
}

const briefingSection = (page: Page) => page.getByRole('region', { name: 'Briefing' })

// Every state the briefing reads still renders when another part of the page
// fails: a failed insight read leaves the briefing in place, and a failed
// briefing read leaves the rest of the page.
test('the briefing and the rest of the page each survive the other failing', async ({ page }) => {
  await serveCrates(page, {
    insight: 'fail',
    briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst }) },
  })
  await page.goto('/w/demo/crates/crate-line')
  const section = briefingSection(page)
  await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
  await expect(section.locator('[data-briefing-task]')).toHaveCount(2)

  // A store failure on the briefing read answers the generic plain-text 500
  // (DEC-105(1)).
  await page.route(
    (url) => /^\/v1\/crates\/[^/]+\/briefing$/.test(url.pathname),
    (route) =>
      route.fulfill({ status: 500, contentType: 'text/plain; charset=utf-8', body: 'internal server error\n' }),
  )
  await page.goto('/w/demo/crates/crate-billing')
  await expect(briefingSection(page).locator('[data-briefing-read-failed]')).toHaveText(
    'The briefing did not load: internal server error',
  )
  await expect(page.locator('[data-crate-task]')).toHaveCount(2)
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
})

// The crates list headline and its briefing requests (component-crates
// invariants 17 and 35; req-crates AC-5.1; req-crate-insights AC-5.2;
// DEC-144(4)(c), (7)). A fixed clock reads the list at listReadTime.
const listReadTime = new Date('2026-11-22T12:00:00Z')

// A served briefing summary generated `minutes` and `seconds` before
// listReadTime.
function listSummary(headline: string, minutes: number, seconds = 0) {
  return { headline, generated_at: new Date(listReadTime.getTime() - (minutes * 60 + seconds) * 1000).toISOString() }
}

// The rows of the request specs, in served order. Four rows want a
// briefing: two with none, one generated 30 minutes and one second before
// the read, and a shipped crate with none. One generated 29 minutes and 59
// seconds before the read and one with no member want none.
function requestCrates(): CrateListRow[] {
  return [
    {
      ...crateRow({ id: 'crate-missing-1', name: 'Alpha missing', open_task_count: 1, total_task_count: 1 }),
      briefing: null,
    },
    {
      ...crateRow({ id: 'crate-old', name: 'Bravo old', open_task_count: 2, total_task_count: 2 }),
      briefing: listSummary('An old headline stays until a new one arrives.', 30, 1),
    },
    {
      ...crateRow({ id: 'crate-young', name: 'Charlie young', open_task_count: 1, total_task_count: 1 }),
      briefing: listSummary('A headline from just under 30 minutes before the read.', 29, 59),
    },
    { ...crateRow({ id: 'crate-no-member', name: 'Delta empty' }), briefing: null },
    {
      ...crateRow({ id: 'crate-missing-2', name: 'Echo missing', open_task_count: 3, total_task_count: 3 }),
      briefing: null,
    },
    {
      ...crateRow({
        id: 'crate-missing-3',
        name: 'Foxtrot missing',
        shipped: true,
        merged_task_count: 1,
        total_task_count: 1,
      }),
      briefing: null,
    },
  ]
}
const wantedCrates = ['crate-missing-1', 'crate-missing-2', 'crate-missing-3', 'crate-old']

function generatedView(headline: string): CrateBriefingView {
  return {
    briefing: storedBriefing({ headline, generated_at: listReadTime.toISOString() }),
    fingerprint: 'fingerprint-live',
    can_generate: true,
  }
}

// Invariant 35: one opening posts once for each open crate it shows with a
// member and a missing or old briefing, at most two at a time, and
// refetches the list after each success. Refetches from the workspace change
// stream and filter changes never post again. A new visit decides again.
test('crates list posts once for each missing or old briefing, two at a time, and refetches after each success', async ({
  page,
}) => {
  const changes = await controlWorkspaceChanges(page)
  await page.clock.setFixedTime(listReadTime)
  const served = requestCrates()
  const gates = new Map<string, () => void>()
  let inFlight = 0
  let maxInFlight = 0
  const server = await serveCrates(page, {
    openList: served,
    briefingPost: async (id) => {
      inFlight += 1
      maxInFlight = Math.max(maxInFlight, inFlight)
      const gate = held()
      gates.set(id, gate.release)
      await gate.promise
      inFlight -= 1
      const row = served.find((crate) => crate.id === id)
      const headline = `A new headline for ${row?.name ?? id}.`
      if (row) row.briefing = { headline, generated_at: listReadTime.toISOString() }
      return { status: 200, json: generatedView(headline) }
    },
  })
  await page.goto('/w/demo/crates')
  const list = page.getByRole('list', { name: 'Open crates' })
  await expect(list.locator('[data-crate-row]')).toHaveCount(6)

  await expect.poll(() => server.briefingPosts.length).toBe(2)
  await page.waitForTimeout(400)
  expect(server.briefingPosts).toHaveLength(2)
  expect(inFlight).toBe(2)

  const before = server.listRequests.length
  gates.get(server.briefingPosts[0])?.()
  await expect.poll(() => server.listRequests.length).toBeGreaterThan(before)
  await expect.poll(() => server.briefingPosts.length).toBe(3)
  gates.get(server.briefingPosts[1])?.()
  await expect.poll(() => server.briefingPosts.length).toBe(4)
  gates.get(server.briefingPosts[2])?.()
  gates.get(server.briefingPosts[3])?.()
  await expect.poll(() => inFlight).toBe(0)
  expect([...server.briefingPosts].sort()).toEqual(wantedCrates)
  expect(maxInFlight).toBe(2)
  for (const id of wantedCrates) {
    const name = served.find((crate) => crate.id === id)?.name
    await expect(list.locator(`[data-crate-row="${id}"] [data-crate-headline]`)).toHaveText(
      `A new headline for ${name}.`,
    )
  }

  // Refetches from the change stream post nothing more.
  const reads = server.listRequests.length
  await changes.ready()
  await expect.poll(() => server.listRequests.length).toBeGreaterThan(reads)
  await changes.activity('task-in-a-crate')
  await expect.poll(() => server.listRequests.length).toBeGreaterThan(reads + 1)
  // A filter change and back post nothing more either.
  await page.getByRole('group', { name: 'Crate state' }).getByRole('button', { name: 'Closed' }).click()
  await expect(page.getByRole('list', { name: 'Closed crates' })).toBeVisible()
  await page.getByRole('group', { name: 'Crate state' }).getByRole('button', { name: 'Open' }).click()
  await expect(list.locator('[data-crate-row]')).toHaveCount(6)
  await page.waitForTimeout(400)
  expect(server.briefingPosts).toHaveLength(4)

  // A new visit decides again on its own read.
  const young = served.find((crate) => crate.id === 'crate-young')
  if (young) young.briefing = null
  await page.getByRole('link', { name: 'Tasks', exact: true }).first().click()
  await expect(page).not.toHaveURL(/crates/)
  await page.goBack()
  await expect.poll(() => server.briefingPosts.length).toBe(5)
  expect(server.briefingPosts[4]).toBe('crate-young')
  gates.get('crate-young')?.()
  await expect(list.locator('[data-crate-row="crate-young"] [data-crate-headline]')).toHaveText(
    'A new headline for Charlie young.',
  )
  await page.waitForTimeout(400)
  expect(server.briefingPosts).toHaveLength(5)
})

// component-crates v4, Verification: the crates list and a crate page render at
// 375 px and at a desktop width without a horizontal page scroll.
for (const width of [375, 1280]) {
  test(`the crates list and a crate page render without horizontal scroll at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await serveCrates(page)
    await page.goto('/w/demo/crates')
    await expect(page.locator('[data-crate-row]')).toHaveCount(3)
    await expectNoHorizontalScroll(page)

    await page.goto('/w/demo/crates/crate-billing')
    await expect(page.locator('[data-crate-task]')).toHaveCount(2)
    await expect(page.getByRole('region', { name: 'Activity' }).locator('[data-activity-kind]')).toHaveCount(4)
    await expectNoHorizontalScroll(page)
  })
}

// The task detail sheet over the crate page (req-crates AC-5.8;
// component-crates invariant 37; component-web-task-surfaces invariants 32
// and 39). The line crate serves its members in an order no sort
// reproduces, and holds more than five so the member list can expand.
const lineOrder = lineCrate.tasks.map((task) => task.id)
const lineTitle = (id: string) => lineCrate.tasks.find((task) => task.id === id)?.title ?? id

// Newest first, for two members of the line crate.
const lineActivity: CrateActivityEvent[] = [
  {
    id: 41,
    kind: 'task.crate_set',
    at: '2026-11-18T09:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261115-d09',
    task_title: 'Show spelling suggestions',
  },
  {
    id: 40,
    kind: 'merge.confirmed',
    at: '2026-11-13T09:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261115-a01',
    task_title: 'Drop the legacy search index',
  },
]

const taskSheet = (page: Page) => page.getByRole('dialog', { name: 'Task detail' })

// The sheet shows the task's title as the body's heading.
async function expectSheetTask(page: Page, id: string) {
  await expect(page).toHaveURL(new RegExp(`/w/demo/crates/crate-line\\?task=${id}$`))
  await expect(taskSheet(page).getByRole('heading', { level: 2 }).first()).toHaveText(lineTitle(id))
}

// The crate page stays open with no sheet and no `task` value.
async function expectCrateOnly(page: Page) {
  await expect(page).toHaveURL(/\/w\/demo\/crates\/crate-line$/)
  await expect(taskSheet(page)).toHaveCount(0)
}

// A briefing the page still wants after its one attempt failed, so a remount
// of the crate page would post again (component-crates invariant 35).
const failingBriefing = {
  briefings: { [lineCrate.id]: null },
  briefingPost: () => ({ status: 500, json: { error: 'internal error' } }),
  activity: { [lineCrate.id]: lineActivity },
}

test('a crate task opens in the task sheet over the crate page, which stays mounted', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 })
  const server = await serveCrates(page, failingBriefing)
  await page.goto('/w/demo/crates/crate-line')
  await expect.poll(() => server.briefingPosts.length).toBe(1)
  await expect(page.locator('[data-crate-task]')).toHaveCount(5)
  await page.locator('[data-crate-tasks-toggle]').click()
  await expect(page.locator('[data-crate-task]')).toHaveCount(8)

  // Mark the page's own DOM and scroll it, so a remount would show. A click
  // scrolls its target into view, so each step reads the scroll position
  // after the sheet opens and expects the same position after it closes.
  const scroller = page.locator('[data-crate-scroll]')
  await page.locator('[data-crate-main]').evaluate((element) => {
    element.dataset.probe = 'kept'
  })
  await scroller.evaluate((element) => {
    element.scrollTop = 240
  })
  const scrollTop = () => scroller.evaluate((element) => element.scrollTop)
  expect(await scrollTop()).toBe(240)
  const expectCrateKept = async (top: number) => {
    expect(top).toBeGreaterThan(0)
    await expect(page.locator('[data-crate-main]')).toHaveAttribute('data-probe', 'kept')
    expect(await scrollTop()).toBe(top)
    await expect(page.locator('[data-crate-tasks-toggle]')).toHaveAttribute('aria-expanded', 'true')
    await expect(page.locator('[data-crate-task]')).toHaveCount(8)
  }

  // The member list opens the sheet, and Previous and Next step through the
  // served member order on the crate page.
  await page.locator('[data-crate-task="261115-b02"] [data-task-row]').click()
  await expectSheetTask(page, '261115-b02')
  let top = await scrollTop()
  const sheet = taskSheet(page)
  await expect(sheet.getByRole('note')).toHaveCount(0)
  await sheet.getByRole('button', { name: 'Next task' }).click()
  await expectSheetTask(page, '261115-e05')
  await sheet.getByRole('button', { name: 'Previous task' }).click()
  await expectSheetTask(page, '261115-b02')
  await sheet.getByRole('button', { name: 'Previous task' }).click()
  await expectSheetTask(page, '261115-c11')
  await expectCrateKept(top)

  // The Close button clears `task` and keeps the crate page.
  await sheet.getByRole('button', { name: 'Close panel' }).click()
  await expectCrateOnly(page)
  await expectCrateKept(top)

  // The activity feed opens the sheet, and Escape closes it.
  const activity = page.getByRole('region', { name: 'Activity' })
  await activity.getByRole('link', { name: 'Show spelling suggestions' }).click()
  await expectSheetTask(page, '261115-d09')
  top = await scrollTop()
  await page.keyboard.press('Escape')
  await expectCrateOnly(page)
  await expectCrateKept(top)

  // The branch map opens the sheet from the keyboard (its lines overlap, so
  // a pointer can land on a neighbour), and the scrim closes it.
  const map = page.getByRole('region', { name: 'Where the work is' })
  await map.getByRole('button', { name: /^Index product reviews:/ }).focus()
  await page.keyboard.press('Enter')
  await expectSheetTask(page, '261115-c11')
  top = await scrollTop()
  await page.mouse.click(20, 400)
  await expectCrateOnly(page)
  await expectCrateKept(top)

  // No Board activity read, and no second briefing post.
  await page.waitForTimeout(300)
  expect(server.boardActivityReads).toEqual([])
  expect(server.briefingPosts).toEqual([lineCrate.id])
})

test('the briefing opens a task in the sheet over the crate page', async ({ page }) => {
  const server = await serveCrates(page, { briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst }) } })
  await page.goto('/w/demo/crates/crate-line')
  await briefingSection(page).locator('[data-briefing-task="261115-c11"]').click()
  await expectSheetTask(page, '261115-c11')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  expect(server.briefingPosts).toEqual([])
})

test('a crate link with a task opens the sheet, and Previous and Next stop at the ends of the crate', async ({
  page,
}) => {
  const server = await serveCrates(page)
  // The first served member has no previous task.
  await page.goto(`/w/demo/crates/crate-line?task=${lineOrder[0]}`)
  await expectSheetTask(page, lineOrder[0])
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  const sheet = taskSheet(page)
  await expect(sheet.getByRole('button', { name: 'Previous task' })).toBeDisabled()
  await expect(sheet.getByRole('button', { name: 'Next task' })).toBeEnabled()

  // The last served member has no next task.
  const last = lineOrder[lineOrder.length - 1]
  await page.goto(`/w/demo/crates/crate-line?task=${last}`)
  await expectSheetTask(page, last)
  await expect(sheet.getByRole('button', { name: 'Previous task' })).toBeEnabled()
  await expect(sheet.getByRole('button', { name: 'Next task' })).toBeDisabled()
  await sheet.getByRole('button', { name: 'Previous task' }).click()
  await expectSheetTask(page, lineOrder[lineOrder.length - 2])

  // A task outside the crate still reads, with both controls disabled.
  await page.goto(`/w/demo/crates/crate-line?task=${outsideTask.id}`)
  await expect(sheet.getByRole('heading', { level: 2 }).first()).toHaveText(outsideTask.title)
  await expect(sheet.getByRole('button', { name: 'Previous task' })).toBeDisabled()
  await expect(sheet.getByRole('button', { name: 'Next task' })).toBeDisabled()
  await expect(sheet.getByRole('note')).toHaveCount(0)

  // An empty `task` value opens no sheet.
  await page.goto('/w/demo/crates/crate-line?task=%20%20')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  await expect(page.locator('[data-crate-task]')).toHaveCount(5)
  await expect(taskSheet(page)).toHaveCount(0)
  expect(server.boardActivityReads).toEqual([])
})

test('the full task page opens from the sheet over a crate page', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-line?task=261115-c11')
  await expectSheetTask(page, '261115-c11')
  await taskSheet(page).getByRole('link', { name: 'Open full task page' }).click()
  await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-c11\/full$/)
})

// Visual record for the pull request: the task sheet open over a crate page
// at 375 px and a desktop width. The task scratch directory named by
// CONVEYOR_SCREENSHOT_DIR keeps a local copy, and the Playwright report
// carries each attachment.
for (const width of [375, 1280]) {
  test(`the task sheet over a crate page screenshot at ${width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 })
    await serveCrates(page, { activity: { [lineCrate.id]: lineActivity } })
    await page.goto('/w/demo/crates/crate-line?task=261115-b02')
    await expectSheetTask(page, '261115-b02')
    await expect(taskSheet(page).getByRole('button', { name: 'Next task' })).toBeEnabled()
    await page.evaluate(() => document.fonts.ready)
    await expectNoHorizontalScroll(page)
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    const file = `crate-task-sheet-${width}.png`
    const image = await page.screenshot({ path: shots ? `${shots}/${file}` : undefined })
    await testInfo.attach(file, { body: image, contentType: 'image/png' })
  })
}
