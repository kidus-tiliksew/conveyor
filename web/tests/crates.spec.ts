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

// Open crates for the completion pie (component-crates invariants 9 and 17;
// req-crates AC-5.1). The share is merged over merged and open members, so
// a closed member counts toward neither. Each total includes closed members
// that a share over the total would wrongly count.
const completionCrates: Crate[] = [
  crateRow({
    id: 'crate-partial',
    name: 'Payments cleanup',
    target_date: dateFromToday(5),
    open_task_count: 3,
    merged_task_count: 2,
    total_task_count: 6,
  }),
  crateRow({
    id: 'crate-full',
    name: 'Docs refresh',
    target_date: dateFromToday(9),
    shipped: true,
    open_task_count: 0,
    merged_task_count: 4,
    total_task_count: 5,
  }),
  crateRow({
    id: 'crate-closed-only',
    name: 'Legacy sunset',
    shipped: true,
    open_task_count: 0,
    merged_task_count: 0,
    total_task_count: 2,
  }),
  crateRow({ id: 'crate-no-members', name: 'Quiet backlog' }),
  crateRow({
    id: 'crate-almost',
    name: 'Search revamp',
    open_task_count: 1,
    merged_task_count: 199,
    total_task_count: 200,
  }),
]

// The accessible name and tooltip text the pie of each completion crate
// states, from the served counts alone.
const completionLabels: Record<string, string> = {
  'crate-partial': '40% done · 2 of 5 merged',
  'crate-full': '100% done · 4 of 4 merged',
  'crate-closed-only': 'No open or merged tasks',
  'crate-no-members': 'No open or merged tasks',
  // 199 of 200 rounds to 100%, but a partial share never reads as done.
  'crate-almost': '99% done · 199 of 200 merged',
}

const completionFills: Record<string, string> = {
  'crate-partial': 'partial',
  'crate-full': 'full',
  'crate-closed-only': 'empty',
  'crate-no-members': 'empty',
  'crate-almost': 'partial',
}

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
async function serveCrates(
  page: Page,
  options: {
    role?: Role
    insight?: 'fail'
    insights?: Record<string, CrateInsight>
    openList?: Crate[]
    briefings?: Record<string, CrateBriefing | null>
    briefingPost?: (crateId: string, count: number) => Promise<BriefingReply | undefined> | BriefingReply | undefined
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
      return route.fulfill({ json: state === 'closed' ? closedCrates : (options.openList ?? openCrates) })
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
      const served = options.insights?.[id] ?? (id === lineCrate.id ? lineInsight : plainInsight(crate))
      return route.fulfill({ json: served })
    }
    // The briefing routes of component-crates, Public surfaces.
    const briefing = path.match(/^\/v1\/crates\/([^/]+)\/briefing$/)
    if (briefing) {
      const id = decodeURIComponent(briefing[1])
      const crate = details[id]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate_not_found', crate_id: id } })
      const view = (): CrateBriefingView => ({
        briefing: id in briefings ? briefings[id] : storedBriefing(),
        fingerprint: 'fingerprint-live',
        can_generate: role !== 'viewer',
      })
      if (method === 'POST') {
        briefingPosts.push(id)
        const reply = await options.briefingPost?.(id, briefingPosts.length)
        if (reply) return route.fulfill(reply)
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
  return { listRequests, created, patched, insightRequests, briefingGets, briefingPosts }
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

// The open crates list as the page shows it: the list card and each row.
function completionList(page: Page) {
  const list = page.getByRole('list', { name: 'Open crates' })
  return { list, card: list.locator('..'), row: (id: string) => list.locator(`[data-crate-row="${id}"]`) }
}

type Box = { x: number; y: number; width: number; height: number }

// True when `inner` lies inside `outer`, allowing half a pixel of rounding.
function inside(inner: Box | null, outer: Box | null) {
  if (!inner || !outer) return false
  return (
    inner.x >= outer.x - 0.5 &&
    inner.y >= outer.y - 0.5 &&
    inner.x + inner.width <= outer.x + outer.width + 0.5 &&
    inner.y + inner.height <= outer.y + outer.height + 0.5
  )
}

// Pointing at a row's pie shows its tooltip with the same text as the pie's
// accessible name. The tooltip stays inside the list card, which clips its
// content, and inside the viewport.
async function expectTooltipOnHover(page: Page, id: string) {
  const { card, row } = completionList(page)
  const tooltip = row(id).locator('[data-completion-tooltip]')
  await expect(tooltip).toBeHidden()
  await row(id).locator('[data-completion-pie]').hover()
  await expect(tooltip).toBeVisible()
  await expect(tooltip).toHaveText(completionLabels[id])
  const viewport = page.viewportSize()
  await expect
    .poll(async () => {
      const box = await tooltip.boundingBox()
      return (
        inside(box, await card.boundingBox()) &&
        inside(box, viewport && { x: 0, y: 0, width: viewport.width, height: viewport.height })
      )
    })
    .toBe(true)
}

// AC-5.1; component-crates invariants 9 and 17: the crates list completion
// pie fills to merged over merged and open members, and its accessible name
// states the share, the merged count, and the counted tasks. A crate with no
// counted task shows an empty pie that states it has none.
test('crates list completion pie names the merged share of partial, full, and closed-only crates', async ({ page }) => {
  await serveCrates(page, { openList: completionCrates })
  await page.goto('/w/demo/crates')
  const { list, row } = completionList(page)
  await expect(list.locator('[data-crate-name]')).toHaveText(completionCrates.map((crate) => crate.name))
  for (const crate of completionCrates) {
    const pie = row(crate.id).locator('[data-completion-pie]')
    await expect(pie.getByRole('img', { name: completionLabels[crate.id], exact: true })).toBeVisible()
    await expect(pie).toHaveAttribute('data-completion-pie', completionFills[crate.id])
    await expect(row(crate.id).locator('[data-completion-tooltip]')).toBeHidden()
    await expect(row(crate.id).locator('[data-count]')).toHaveCount(0)
    await expect(row(crate.id)).not.toContainText(countText, { useInnerText: true })
  }
  // The full share is a full disc, and a partial share draws one wedge.
  await expect(row('crate-full').locator('[data-completion-pie] path')).toHaveCount(0)
  await expect(row('crate-full').locator('[data-completion-pie] circle')).toHaveCount(2)
  await expect(row('crate-partial').locator('[data-completion-pie] path')).toHaveCount(1)
  await expect(row('crate-closed-only').locator('[data-completion-pie] path')).toHaveCount(0)
  await expect(row('crate-closed-only').locator('[data-completion-pie] circle')).toHaveCount(1)
  // The column that holds the pie is headed Done.
  const header = page.locator('[data-crate-list-header] > span', { hasText: /^Done$/ })
  await expect(header).toBeVisible()
  await expect(page.locator('[data-crate-list-header] > span', { hasText: /^Tasks$/ })).toHaveCount(0)
})

test('crates list completion pie shows its share on hover and hides it when the pointer leaves', async ({ page }) => {
  await serveCrates(page, { openList: completionCrates })
  await page.goto('/w/demo/crates')
  const { row } = completionList(page)
  await expectTooltipOnHover(page, 'crate-partial')
  // The tooltip is the only place the share reads as text.
  await expect(row('crate-partial')).toContainText('40% done · 2 of 5 merged', { useInnerText: true })
  await page.mouse.move(0, 0)
  await expect(row('crate-partial').locator('[data-completion-tooltip]')).toBeHidden()
  await expect(row('crate-partial')).not.toContainText(countText, { useInnerText: true })
  await expectTooltipOnHover(page, 'crate-closed-only')
  // The last row's tooltip is not cut off at the bottom of the list card.
  await expectTooltipOnHover(page, 'crate-almost')
})

test('crates list completion pie sits in the Done column at a desktop width', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await serveCrates(page, { openList: completionCrates })
  await page.goto('/w/demo/crates')
  const { row } = completionList(page)
  const header = page.locator('[data-crate-list-header] > span', { hasText: /^Done$/ })
  await expect(header).toBeVisible()
  await expect(row('crate-partial').locator('[data-completion-pie]')).toBeVisible()
  await expect
    .poll(async () => {
      const done = await header.boundingBox()
      const pie = await row('crate-partial').locator('[data-completion-pie]').boundingBox()
      return done && pie ? Math.abs(done.x - pie.x) : Number.POSITIVE_INFINITY
    })
    .toBeLessThanOrEqual(1)
  // The desktop date has its own column; the date under the name is hidden.
  await expect(row('crate-partial').locator('[data-crate-row-main] [data-target-date]')).toBeHidden()
  await expect(row('crate-partial').locator('[data-target-date]:visible')).toHaveCount(1)
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

  test('crates list completion pie sits at the right with the date under the name', async ({ page }) => {
    await serveCrates(page, { openList: completionCrates })
    await page.goto('/w/demo/crates')
    const { row } = completionList(page)
    const partial = row('crate-partial')
    const main = partial.locator('[data-crate-row-main]')
    const pie = partial.locator('[data-completion-pie]')
    await expect(main.locator('svg').first()).toBeVisible()
    await expect(main.locator('[data-target-date]')).toBeVisible()
    await expect(pie.getByRole('img', { name: completionLabels['crate-partial'], exact: true })).toBeVisible()
    await expect
      .poll(async () => {
        const name = await partial.locator('[data-crate-name]').boundingBox()
        const date = await main.locator('[data-target-date]').boundingBox()
        const piece = await pie.boundingBox()
        const box = await partial.boundingBox()
        if (!name || !date || !piece || !box) return 'unmeasured'
        if (date.y < name.y + name.height - 0.5) return 'date not under the name'
        if (piece.x < name.x + name.width) return 'pie not right of the name'
        if (piece.x + piece.width < box.x + box.width * 0.75) return 'pie not at the right'
        return 'ok'
      })
      .toBe('ok')
    await expect(partial).not.toContainText(countText, { useInnerText: true })
    await expectNoHorizontalScroll(page)
  })

  test('crates list completion pie tooltip is not clipped at the right edge', async ({ page }) => {
    await serveCrates(page, { openList: completionCrates })
    await page.goto('/w/demo/crates')
    for (const crate of completionCrates) await expectTooltipOnHover(page, crate.id)
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

    // AC-1.5 after the first render: when the browser takes the line's WebGL
    // context away, the counts become visible text and the rest of the page
    // keeps working (component-crates invariant 34). The spec needs WebGL to
    // lose, so it skips where the browser has none.
    test('a lost WebGL context turns the line into visible station counts', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const band = page.locator('[data-line-mode]')
      test.skip((await band.getAttribute('data-line-mode')) !== 'scene', 'this browser has no WebGL')
      // The first frame places the station signs.
      await expect(page.locator('[data-station-sign="build"]')).toBeVisible()
      const lost = await page.locator('[data-line-scene] canvas').evaluate((canvas) => {
        const element = canvas as HTMLCanvasElement
        const context = element.getContext('webgl2') ?? element.getContext('webgl')
        const extension = context?.getExtension('WEBGL_lose_context')
        extension?.loseContext()
        return extension != null
      })
      expect(lost).toBe(true)

      await expect(band).toHaveAttribute('data-line-mode', 'text')
      await expect(page.locator('canvas')).toHaveCount(0)
      const counts = page.getByRole('list', { name: 'Tasks at each station' })
      await expect(counts).toBeVisible()
      for (const item of await counts.getByRole('listitem').all()) await expect(item).toBeVisible()
      await expect(counts.getByRole('listitem')).toHaveText([
        'Queued: 2',
        'Plan: 1',
        'Build: 1',
        'Review: 1',
        'Merge gate: 1',
        'Merged: 1',
      ])

      // The task list still expands and collapses.
      const toggle = page.locator('[data-crate-tasks-toggle]')
      await toggle.click()
      await expect(toggle).toHaveAttribute('aria-expanded', 'true')
      await expect(page.locator('[data-crate-task]')).toHaveCount(8)
      await toggle.click()
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
      // The branch map sits between the line and the task list, so a desktop
      // page needs the height that keeps the line and the rows in view
      // together.
      if (viewport.width >= 1024) await page.setViewportSize({ width: viewport.width, height: 1900 })
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
    // row, or names the task beside the list control when the collapsed
    // list hides the row, and shows the block's card. Activating the block
    // opens task detail. The scene needs WebGL and sits beside the list only
    // from lg, so the spec runs where both hold.
    test('pointing at a block marks its row, and activating it opens task detail', async ({ page }) => {
      test.skip(viewport.width < 1024, 'the line and the task list share the screen only from lg')
      // Tall enough that the line and every expanded row stay in view with
      // the branch map between them, so the page never scrolls the line away.
      await page.setViewportSize({ width: viewport.width, height: 1900 })
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-line')
      const mode = await page.locator('[data-line-mode]').getAttribute('data-line-mode')
      test.skip(mode !== 'scene', 'this browser has no WebGL')
      await expect(page.locator('[data-station-sign="gate"]')).toBeVisible()
      const scene = page.locator('[data-line-scene]')

      // A row's emphasis places its block's card just above the block, so
      // the block lies below the card's point.
      async function blockOffset(id: string) {
        await page.locator(`[data-crate-task="${id}"]`).hover()
        const card = page.locator(`[data-line-card="${id}"]`)
        await expect(card).toBeVisible()
        // The scene moves the card on its next frames, so the first read can
        // still hold the place of the block emphasized before. Wait until
        // two reads in a row agree.
        let transform = ''
        await expect
          .poll(
            async () => {
              const next = await card.evaluate((element) => (element as HTMLElement).style.transform)
              const settled = next !== '' && next === transform
              transform = next
              return settled
            },
            { intervals: [150] },
          )
          .toBe(true)
        const point = transform
          .match(/translate\(([-\d.]+)px, ([-\d.]+)px\)$/)
          ?.slice(1)
          .map(Number)
        if (!point) throw new Error(`the card of ${id} has no place over the scene`)
        return { x: point[0], y: point[1] + 22 }
      }
      async function pointAt(offset: { x: number; y: number }) {
        const box = await scene.boundingBox()
        if (!box) throw new Error('the scene has no box')
        const at = { x: box.x + offset.x, y: box.y + offset.y }
        await page.mouse.move(at.x, at.y)
        return at
      }

      // The sixth task's row is hidden while the list is collapsed.
      const toggle = page.locator('[data-crate-tasks-toggle]')
      await toggle.click()
      const hiddenBlock = await blockOffset('261115-d09')
      await toggle.click()
      await expect(page.locator('[data-crate-task]')).toHaveCount(5)
      await pointAt(hiddenBlock)
      await expect(page.locator('[data-emphasized-hidden="261115-d09"]')).toHaveText(
        'Show spelling suggestions is in the hidden rows.',
      )
      await expect(page.locator('[data-emphasized]')).toHaveCount(0)

      const row = page.locator('[data-crate-task="261115-f07"]')
      const block = await pointAt(await blockOffset('261115-f07'))
      await expect(row).toHaveAttribute('data-emphasized', 'true')
      await expect(page.locator('[data-emphasized-hidden]')).toHaveCount(0)
      const card = page.locator('[data-line-card="261115-f07"]')
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

// The branch map and the delivery numbers (req-crate-insights REQ-2 to
// REQ-4; component-crates invariants 26, 27, and 33). The fake server serves
// every lane, time, group, and number; the page places and formats them.

// The billing crate on one branch: one task in review after a round that
// asked for changes, and one merged task.
const billingInsight: CrateInsight = {
  tasks: [
    insightTask('261101-aaaaaa', 'review', 'review', '2026-11-12T09:00:00Z', {
      pull_request_opened_at: ['2026-11-13T15:00:00Z'],
      changes_requested_at: ['2026-11-14T11:00:00Z'],
      review_rounds: 2,
    }),
    insightTask('261101-bbbbbb', 'merged', 'crate', '2026-11-10T09:00:00Z', {
      pull_request_opened_at: ['2026-11-11T09:00:00Z'],
      review_rounds: 1,
      merged_at: '2026-11-12T16:00:00Z',
    }),
  ],
  branches: ['main'],
  whereabouts: [
    { key: 'task_branch', branch: null, task_ids: ['261101-aaaaaa'] },
    { key: 'merged', branch: 'main', task_ids: ['261101-bbbbbb'] },
  ],
  progress: { ...emptyProgress(), delivered: 1, total: 2, open: 1 },
  first_review: { approved: 1, reviewed: 2 },
  forecast_days_before_target: null,
  replay: {
    start: '2026-11-10T09:00:00Z',
    end: lineNow,
    steps: [
      step('2026-11-10T09:00:00Z', '261101-bbbbbb', 'queued', 'intake'),
      step('2026-11-11T09:00:00Z', '261101-bbbbbb', 'review', 'review'),
      step('2026-11-12T09:00:00Z', '261101-aaaaaa', 'queued', 'intake'),
      step('2026-11-12T16:00:00Z', '261101-bbbbbb', 'merged', 'crate'),
      step('2026-11-13T15:00:00Z', '261101-aaaaaa', 'review', 'review'),
    ],
  },
}

// Ten served days of scope, open, and merged tasks, oldest first.
const deliveryDaily = Array.from({ length: 10 }, (_, index) => ({
  day: `2026-11-${String(10 + index).padStart(2, '0')}`,
  scope: Math.min(7, 2 + index),
  open: Math.min(7, 2 + index) - (index >= 3 ? 1 : 0),
  delivered: index >= 3 ? 1 : 0,
}))

// Eight served ISO weeks, oldest first. The last one is in progress.
const deliveryWeekly = Array.from({ length: 8 }, (_, index) => {
  const start = new Date(Date.UTC(2026, 8, 28 + index * 7))
  return {
    week_start: start.toISOString().slice(0, 10),
    iso_year: 2026,
    iso_week: 40 + index,
    delivered: [0, 1, 0, 2, 1, 0, 1, 1][index],
    in_progress: index === 7,
  }
})

// The line crate with served delivery numbers and a round that asked for
// changes: a forecast that falls five days before the target date.
const deliveryInsight: CrateInsight = {
  ...lineInsight,
  tasks: lineInsight.tasks.map((task) =>
    task.id === '261115-b02' ? { ...task, changes_requested_at: ['2026-11-16T15:00:00Z'], review_rounds: 1 } : task,
  ),
  progress: {
    ...emptyProgress(),
    daily: deliveryDaily,
    forecast: { day: '2026-12-01', rate_per_day: 0.5 },
    weekly: deliveryWeekly,
    delivered: 1,
    total: 7,
    open: 6,
    throughput_per_week: 0.5,
    median_cycle_seconds: 172_800,
  },
  first_review: { approved: 1, reviewed: 2 },
  forecast_days_before_target: 5,
}

// The line crate with two rounds that asked for changes on the merged task,
// one hour after its creation and one hour before its merge. On both map
// widths each falls inside a bend of the line.
const bendInsight: CrateInsight = {
  ...deliveryInsight,
  tasks: deliveryInsight.tasks.map((task) =>
    task.id === '261115-a01'
      ? { ...task, changes_requested_at: ['2026-11-10T10:00:00Z', '2026-11-13T08:00:00Z'], review_rounds: 3 }
      : task,
  ),
}

// The x where a line leaves its lane: the first point of its drawn path.
async function lineStart(page: Page, id: string) {
  const d = await page.locator(`[data-branch-task="${id}"] path`).first().getAttribute('d')
  const match = d?.match(/^M([-\d.]+),/)
  if (!match) throw new Error(`the line of ${id} has no path`)
  return Number(match[1])
}

for (const viewport of [
  { name: '375 px', width: 375, height: 812 },
  { name: 'desktop', width: 1280, height: 900 },
]) {
  test.describe(`branch map and delivery numbers at ${viewport.name}`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height } })

    // AC-2.1 to AC-2.4 on a crate whose tasks merge into two branches.
    test('the branch map shows a lane per served branch, a line per task, and the served counts', async ({ page }) => {
      await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      await expect(map.locator('[data-lane]')).toHaveCount(2)
      expect(await map.locator('[data-lane]').evaluateAll((lanes) => lanes.map((lane) => lane.dataset.lane))).toEqual([
        'main',
        'develop',
      ])
      await expect(map.locator('[data-lane-name]')).toHaveText(['main', 'develop'])
      await expect(map.locator('[data-branch-task]')).toHaveCount(8)

      // Each line ends as its served times say: a merge returns to the lane
      // with a mark, a close ends with a mark, and an open line ends at the
      // present with the title and what the task is doing.
      const line = (id: string) => map.locator(`[data-branch-task="${id}"]`)
      await expect(line('261115-a01')).toHaveAttribute('data-ends', 'merged')
      await expect(line('261115-a01').locator('[data-merge-mark]')).toHaveCount(1)
      await expect(line('261115-g04')).toHaveAttribute('data-ends', 'closed')
      await expect(line('261115-g04').locator('[data-close-mark]')).toHaveCount(1)
      for (const id of ['261115-f07', '261115-a03', '261115-c11', '261115-b02', '261115-e05', '261115-d09'])
        await expect(line(id)).toHaveAttribute('data-ends', 'open')
      // Below 900 px the label column is narrower and a long title is cut
      // short. The line's name and card keep the whole title.
      await expect(line('261115-c11').locator('[data-open-label]')).toHaveText(
        viewport.width >= 900 ? 'Index product reviews · Building' : 'Index product… · Building',
      )
      await expect(line('261115-c11')).toHaveAttribute(
        'aria-label',
        /^Index product reviews: Agent building \(served\)/,
      )
      await expect(line('261115-f07').locator('[data-open-label]')).toContainText('Waits on a person')
      // A task whose work has not started is a broken line.
      await expect(map.locator('[data-not-started]')).toHaveCount(2)
      await expect(line('261115-e05')).toHaveAttribute('data-not-started', 'true')
      await expect(line('261115-d09')).toHaveAttribute('data-not-started', 'true')
      // AC-2.3: one mark for the round that asked for changes.
      await expect(line('261115-b02').locator('[data-change-mark]')).toHaveCount(1)
      await expect(map.locator('[data-change-mark]')).toHaveCount(1)

      // The time axis: day ticks and the present.
      await expect(map.locator('[data-now]')).toHaveText('Now')
      const ticks = await map.locator('[data-tick]').count()
      expect(ticks).toBeGreaterThan(0)
      expect(ticks).toBeLessThanOrEqual(8)
      // Lines leave their lanes in the order of their served creation times.
      expect(await lineStart(page, '261115-a01')).toBeLessThan(await lineStart(page, '261115-c11'))
      expect(await lineStart(page, '261115-c11')).toBeLessThan(await lineStart(page, '261115-b02'))
      expect(await lineStart(page, '261115-b02')).toBeLessThan(await lineStart(page, '261115-e05'))

      // AC-2.4: the served counts, in the served order.
      const counts = map.locator('[data-whereabouts]')
      await expect(counts).toHaveText(['2not started', '4on task branches', '1merged into main'])

      await expect(map.locator('[data-legend]')).toHaveText([
        'Merged into the branch',
        'Review asked for changes',
        'Not started',
      ])
      // No release mark or release legend entry.
      await expect(map.getByText(/released/i)).toHaveCount(0)

      // Below 680 px the map scrolls sideways inside its card, and the page
      // does not.
      const scroll = map.locator('[data-branch-map-scroll]')
      const sizes = await scroll.evaluate((element) => ({ scroll: element.scrollWidth, client: element.clientWidth }))
      if (viewport.width < 680) expect(sizes.scroll).toBeGreaterThan(sizes.client)
      else expect(sizes.scroll).toBeLessThanOrEqual(sizes.client)
      await expectNoHorizontalScroll(page)
    })

    test('a crate on one branch shows one lane with its lines', async ({ page }) => {
      await serveCrates(page, { insights: { [billing.id]: billingInsight } })
      await page.goto('/w/demo/crates/crate-billing')
      const map = page.getByRole('region', { name: 'Where the work is' })
      await expect(map.locator('[data-lane-name]')).toHaveText(['main'])
      await expect(map.locator('[data-branch-task]')).toHaveCount(2)
      await expect(map.locator('[data-branch-task="261101-aaaaaa"] [data-open-label]')).toHaveText(
        viewport.width >= 900 ? 'Send invoices by email · In review' : 'Send invoice… · In review',
      )
      await expect(map.locator('[data-branch-task="261101-bbbbbb"]')).toHaveAttribute('data-ends', 'merged')
      await expect(map.locator('[data-whereabouts]')).toHaveText(['1on task branches', '1merged into main'])
    })

    // AC-2.4: pointing at a count emphasizes the lines of its served tasks.
    test('pointing at a count emphasizes its tasks lines', async ({ page }) => {
      await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      await expect(map.locator('[data-branch-task]')).toHaveCount(8)
      const lit = () =>
        map
          .locator('[data-branch-task]:not([data-dimmed])')
          .evaluateAll((lines) => lines.map((line) => line.getAttribute('data-branch-task')).sort())

      await map.locator('[data-whereabouts="not_started"]').hover()
      await expect.poll(lit).toEqual(['261115-d09', '261115-e05'])
      await page.mouse.move(0, 0)
      await expect(map.locator('[data-dimmed]')).toHaveCount(0)

      // From the keyboard too, and the group's members come from the served
      // IDs, not from the lines' own state.
      await map.locator('[data-whereabouts="merged"]').focus()
      await expect.poll(lit).toEqual(['261115-a01'])
      await map.locator('[data-whereabouts="task_branch"]').focus()
      await expect.poll(lit).toEqual(['261115-a03', '261115-b02', '261115-c11', '261115-f07'])
      await map.locator('[data-whereabouts="task_branch"]').blur()
      await expect(map.locator('[data-dimmed]')).toHaveCount(0)
    })

    // AC-2.5 and REQ-3: focus or pointing shows the card and links the line
    // with the task list. Enter and Space open task detail.
    test('a line shows its card on focus, links with the task list, and opens from the keyboard', async ({ page }) => {
      await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      const open = map.locator('[data-branch-task="261115-b02"]')
      await open.focus()
      const card = map.locator('[data-branch-card="261115-b02"]')
      await expect(card).toBeVisible()
      // The card is in view, also where the map scrolls sideways.
      await expect
        .poll(async () => {
          const box = await card.boundingBox()
          return box !== null && box.x >= 0 && box.x + box.width <= viewport.width
        })
        .toBe(true)
      await expect(card).toContainText('Score results by freshness')
      await expect(card.locator('[data-branch-card-label]')).toHaveText('Agent reviewing (served)')
      await expect(card.locator('[data-branch-card-pull]')).toHaveText(' · #2818')
      await expect(card.locator('[data-branch-card-rounds]')).toHaveText(' · 1 review round')
      await expect(card.locator('[data-branch-card-branch]')).toHaveText('conveyor/task-261115-b02 → main')
      await expect(open).toHaveAttribute('data-map-emphasized', 'true')
      await expect(map.locator('[data-focus-ring]')).toHaveCount(1)
      // The line's task is marked in the task list.
      await expect(page.locator('[data-crate-task="261115-b02"]')).toHaveAttribute('data-emphasized', 'true')

      // Pointing at another line and away again leaves the focused line
      // emphasized.
      await map.locator('[data-branch-task="261115-c11"] [data-open-label]').hover()
      await expect(map.locator('[data-branch-card="261115-c11"]')).toBeVisible()
      await expect(page.locator('[data-crate-task="261115-c11"]')).toHaveAttribute('data-emphasized', 'true')
      await page.mouse.move(0, 0)
      await expect(card).toBeVisible()
      await expect(page.locator('[data-crate-task="261115-b02"]')).toHaveAttribute('data-emphasized', 'true')

      // A merged task's card names the branch that holds it and the merge.
      await map.locator('[data-branch-task="261115-a01"]').focus()
      const merged = map.locator('[data-branch-card="261115-a01"]')
      await expect(merged.locator('[data-branch-card-branch]')).toContainText('in main since Nov 13')
      await expect(merged.locator('[data-branch-card-rounds]')).toHaveText(' · 1 review round')

      // Pointing at a row emphasizes its line in the map.
      await page.locator('[data-crate-task="261115-c11"]').hover()
      await expect(map.locator('[data-branch-task="261115-c11"]')).toHaveAttribute('data-map-emphasized', 'true')
      await expect(map.locator('[data-branch-task="261115-b02"]')).toHaveAttribute('data-dimmed', 'true')

      // Space opens task detail and does not scroll the page.
      const prevented = await map.locator('[data-branch-task="261115-e05"]').evaluate((element) => {
        const event = new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true })
        element.dispatchEvent(event)
        return event.defaultPrevented
      })
      expect(prevented).toBe(true)
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-e05$/)

      await page.goto('/w/demo/crates/crate-line')
      await map.locator('[data-branch-task="261115-c11"]').focus()
      await page.keyboard.press('Enter')
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-c11$/)
    })

    // AC-2.3: a round that asked for changes close to the task's creation or
    // its merge sits inside a bend. Its mark keeps the served x and sits on
    // the drawn line there, not at the row's height beside the bend.
    test('a change-request mark near a bend sits on its line at the served time', async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await serveCrates(page, { insights: { [lineCrate.id]: bendInsight } })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      const merged = map.locator('[data-branch-task="261115-a01"]')
      await expect(merged.locator('[data-change-mark]')).toHaveCount(2)
      // Every mark on the map meets its own drawn line.
      const marks = await map.locator('[data-branch-task]').evaluateAll((lines) =>
        lines.flatMap((line) => {
          const path = line.querySelector<SVGPathElement>('[data-line-path]')
          if (!path) return []
          const d = path.getAttribute('d') ?? ''
          const lane = Number(d.match(/^M[-\d.]+,([-\d.]+)/)?.[1])
          const row = Number(d.match(/ L[-\d.]+,([-\d.]+)/)?.[1])
          return Array.from(line.querySelectorAll<SVGCircleElement>('[data-change-mark]')).map((mark) => {
            const x = Number(mark.getAttribute('cx'))
            const y = Number(mark.getAttribute('cy'))
            return {
              id: line.getAttribute('data-branch-task'),
              onLine: path.isPointInStroke(new DOMPoint(x, y)),
              x,
              y,
              lane,
              row,
            }
          })
        }),
      )
      expect(marks).toHaveLength(3)
      for (const mark of marks) expect(mark, `the mark of ${mark.id} at x ${mark.x}`).toMatchObject({ onLine: true })
      // The two marks of the merged task sit inside its bends: below the lane
      // and above the row, so a mark at the row's height would miss the line.
      const bent = marks.filter((mark) => mark.id === '261115-a01').sort((a, b) => a.x - b.x)
      for (const mark of bent) {
        expect(mark.y).toBeGreaterThan(mark.lane)
        expect(mark.y).toBeLessThan(mark.row)
      }
      // The served times keep their order on the axis: one after creation,
      // one before the merge mark.
      const mergeX = Number(await merged.locator('[data-merge-mark]').getAttribute('cx'))
      expect(bent[0].x).toBeGreaterThan(await lineStart(page, '261115-a01'))
      expect(bent[1].x).toBeLessThan(mergeX)
    })

    // AC-2.5: pointing at another task row and moving off it gives the card
    // and the row mark back to the map line that still has focus.
    test('a focused line keeps its card and row mark after the pointer leaves another task row', async ({ page }) => {
      await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      const focused = map.locator('[data-branch-task="261115-b02"]')
      await focused.focus()
      await expect(map.locator('[data-branch-card="261115-b02"]')).toBeVisible()
      await expect(page.locator('[data-crate-task="261115-b02"]')).toHaveAttribute('data-emphasized', 'true')

      // The pointer rests on another row: that task takes the emphasis.
      await page.locator('[data-crate-task="261115-c11"]').hover()
      await expect(page.locator('[data-crate-task="261115-c11"]')).toHaveAttribute('data-emphasized', 'true')
      await expect(map.locator('[data-branch-card="261115-c11"]')).toBeVisible()

      // The pointer moves off the row without a click.
      await page.mouse.move(0, 0)
      await expect(focused).toBeFocused()
      await expect(map.locator('[data-branch-card="261115-b02"]')).toBeVisible()
      await expect(map.locator('[data-branch-card]')).toHaveCount(1)
      await expect(page.locator('[data-crate-task="261115-b02"]')).toHaveAttribute('data-emphasized', 'true')
      await expect(page.locator('[data-crate-task="261115-c11"]')).not.toHaveAttribute('data-emphasized', 'true')
      await expect(focused).toHaveAttribute('data-map-emphasized', 'true')

      // When the line loses focus, nothing stays emphasized.
      await focused.blur()
      await expect(map.locator('[data-branch-card]')).toHaveCount(0)
      await expect(page.locator('[data-emphasized]')).toHaveCount(0)
    })

    // AC-2.5: the pointer rests on one task while keyboard focus moves to
    // another map line. The focused task takes the card and the row mark
    // with no pointer movement, and Enter opens that same task.
    for (const resting of ['a task row', 'a map line'] as const) {
      test(`keyboard focus that moves to a map line while the pointer rests on ${resting} shows the focused task`, async ({
        page,
      }) => {
        test.skip(resting === 'a map line' && viewport.width < 1024, 'the pointer rests on a map line at desktop width')
        // Tall enough that the map and every row stay in view, so moving focus
        // never scrolls the page. A scroll would put another element under the
        // resting pointer, and the browser would report that as pointer
        // movement.
        await page.setViewportSize({ width: viewport.width, height: 2400 })
        await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
        await page.goto('/w/demo/crates/crate-line')
        const map = page.getByRole('region', { name: 'Where the work is' })
        await expect(map.locator('[data-branch-task]')).toHaveCount(8)
        // Every row shows, so each task's row mark can be checked.
        await page.locator('[data-crate-tasks-toggle]').click()
        await expect(page.locator('[data-crate-task]')).toHaveCount(8)
        const card = (id: string) => map.locator(`[data-branch-card="${id}"]`)
        const row = (id: string) => page.locator(`[data-crate-task="${id}"]`)
        async function showsOnly(id: string) {
          await expect(card(id)).toBeVisible()
          await expect(map.locator('[data-branch-card]')).toHaveCount(1)
          await expect(row(id)).toHaveAttribute('data-emphasized', 'true')
          await expect(page.locator('[data-crate-task][data-emphasized]')).toHaveCount(1)
          await expect(map.locator(`[data-branch-task="${id}"]`)).toHaveAttribute('data-map-emphasized', 'true')
        }

        // The pointer comes to rest on one task and stays there.
        if (resting === 'a task row') await row('261115-c11').hover()
        else await map.locator('[data-branch-task="261115-c11"] [data-open-label]').hover()
        await showsOnly('261115-c11')
        const scrolled = () =>
          page.evaluate(() =>
            Array.from(document.querySelectorAll('*')).reduce(
              (sum, element) => sum + element.scrollTop,
              window.scrollY,
            ),
          )
        const before = await scrolled()

        // Keyboard focus moves to a map line, then on to the next one by Tab.
        const lines = await map
          .locator('[data-branch-task]')
          .evaluateAll((items) => items.map((item) => item.getAttribute('data-branch-task') ?? ''))
        // A line whose next line in tab order is not the hovered task.
        const first = lines.find(
          (id, index) => id !== '261115-c11' && lines[index + 1] && lines[index + 1] !== '261115-c11',
        ) as string
        await map.locator(`[data-branch-task="${first}"]`).focus()
        await showsOnly(first)
        await page.keyboard.press('Tab')
        const next = await page.evaluate(() => document.activeElement?.getAttribute('data-branch-task') ?? '')
        expect(lines).toContain(next)
        expect(next).not.toBe('261115-c11')
        await showsOnly(next)
        // Nothing scrolled, so the pointer still rests where it was.
        expect(await scrolled()).toBe(before)

        // The pointer then enters another task and leaves it: the focused
        // line gets the emphasis back (round 2).
        await row('261115-b02').hover()
        await showsOnly('261115-b02')
        await page.mouse.move(0, 0)
        await showsOnly(next)

        // Enter opens the task the card shows.
        await page.keyboard.press('Enter')
        await expect(page).toHaveURL(new RegExp(`/w/demo/tasks/${next}$`))
      })
    }

    test('clicking a line opens task detail', async ({ page }) => {
      await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      await page.locator('[data-branch-task="261115-a03"] [data-open-label]').click()
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-a03$/)
    })

    // AC-4.1, AC-4.2, AC-4.4, and AC-4.5 in the app's sheet.
    test('Stats opens the served delivery numbers and charts, and Escape closes the sheet', async ({ page }) => {
      await serveCrates(page, { role: 'viewer', insights: { [lineCrate.id]: deliveryInsight } })
      await page.goto('/w/demo/crates/crate-line')
      await expect(page.locator('[data-branch-task]')).toHaveCount(8)
      const stats = page.getByRole('button', { name: 'Stats' })
      await stats.click()
      const sheet = page.getByRole('dialog', { name: 'Delivery stats' })
      await expect(sheet).toBeVisible()
      await expect(sheet.locator('[data-figure="merged"] dd')).toHaveText('1/7')
      await expect(sheet.locator('[data-figure="open"] dd')).toHaveText('6')
      await expect(sheet.locator('[data-figure="throughput"] dd')).toHaveText('0.5/wk')
      await expect(sheet.locator('[data-figure="cycle"] dd')).toHaveText('2d')
      await expect(sheet.locator('[data-first-review-share]')).toHaveText('50%')
      await expect(sheet.locator('[data-first-review-count]')).toHaveText('2 tasks have been reviewed.')
      await expect(sheet.locator('[data-forecast]')).toHaveText(
        'At this rate the open tasks finish around Dec 1, 2026, 5 days before the target date.',
      )

      // The burn chart with its burn-down and burn-up choice.
      const burn = sheet.getByRole('region', { name: 'Burn-down' })
      await expect(burn).toBeVisible()
      await burn.getByRole('button', { name: 'Burn-up' }).click()
      const up = sheet.getByRole('region', { name: 'Burn-up' })
      await expect(up.getByRole('button', { name: 'Burn-up' })).toHaveAttribute('aria-pressed', 'true')
      await up.getByText('Table', { exact: true }).click()
      // One row per served day, then the served forecast day.
      await expect(up.locator('tbody tr')).toHaveCount(deliveryDaily.length + 1)
      await expect(up.locator('tbody tr').last()).toContainText('Forecast · Dec 1, 2026')

      // Eight ISO weeks with the current one in progress, and its table.
      const weekly = sheet.getByRole('region', { name: 'Merged per week' })
      await expect(weekly.locator('[data-week]')).toHaveCount(8)
      await expect(weekly.locator('[data-in-progress]')).toHaveCount(1)
      await weekly.getByText('Table', { exact: true }).click()
      await expect(weekly.locator('tbody tr')).toHaveCount(8)
      await expect(weekly.locator('tbody tr').last()).toContainText('2026-W47')
      await expect(weekly.locator('tbody tr').last()).toContainText('1 (in progress)')
      await expectNoHorizontalScroll(page, '[role="dialog"]')

      await page.keyboard.press('Escape')
      await expect(sheet).toHaveCount(0)
      await expect(stats).toBeFocused()
    })

    // AC-4.3: every form of the forecast statement, from served values.
    test('the sheet states the served forecast in each form', async ({ page }) => {
      const insights: Record<string, CrateInsight> = {}
      await serveCrates(page, { insights })
      const cases: Array<{ insight: CrateInsight; text: string }> = [
        {
          insight: { ...deliveryInsight, forecast_days_before_target: 1 },
          text: 'At this rate the open tasks finish around Dec 1, 2026, 1 day before the target date.',
        },
        {
          insight: { ...deliveryInsight, forecast_days_before_target: -3 },
          text: 'At this rate the open tasks finish around Dec 1, 2026, 3 days after the target date.',
        },
        {
          insight: { ...deliveryInsight, forecast_days_before_target: 0 },
          text: 'At this rate the open tasks finish around Dec 1, 2026, on the target date.',
        },
        {
          insight: { ...deliveryInsight, forecast_days_before_target: null },
          text: 'At this rate the open tasks finish around Dec 1, 2026.',
        },
        {
          insight: {
            ...deliveryInsight,
            progress: { ...deliveryInsight.progress, forecast: null },
            forecast_days_before_target: null,
          },
          text: 'No forecast: no task in this crate merged in the last 14 days.',
        },
        {
          insight: {
            ...deliveryInsight,
            progress: { ...deliveryInsight.progress, forecast: null, open: 0, delivered: 7 },
            forecast_days_before_target: null,
          },
          text: 'Every task is merged or closed.',
        },
      ]
      for (const { insight, text } of cases) {
        insights[lineCrate.id] = insight
        await page.goto('/w/demo/crates/crate-line')
        await expect(page.locator('[data-branch-task]')).toHaveCount(8)
        await page.getByRole('button', { name: 'Stats' }).click()
        await expect(page.getByRole('dialog', { name: 'Delivery stats' }).locator('[data-forecast]')).toHaveText(text)
      }
    })

    test('an empty crate shows no branch map and its sheet states no open task', async ({ page }) => {
      await serveCrates(page)
      await page.goto('/w/demo/crates/crate-empty')
      await expect.poll(() => stationCounts(page)).toMatchObject({ intake: '0' })
      await expect(page.locator('[data-branch-map]')).toHaveCount(0)
      await expect(page.locator('[data-branch-map-loading]')).toHaveCount(0)
      await page.getByRole('button', { name: 'Stats' }).click()
      const sheet = page.getByRole('dialog', { name: 'Delivery stats' })
      await expect(sheet.locator('[data-first-review-share]')).toHaveText('none')
      await expect(sheet.locator('[data-figure="cycle"] dd')).toHaveText('none')
      await expect(sheet.locator('[data-forecast]')).toHaveText('Every task is merged or closed.')
    })
  })
}

// Without WebGL the branch map and the sheet still render: neither draws
// with WebGL.
test('without WebGL the branch map and the delivery numbers render', async ({ page }) => {
  await withoutWebGL(page)
  await serveCrates(page, { insights: { [lineCrate.id]: deliveryInsight } })
  await page.goto('/w/demo/crates/crate-line')
  await expect(page.locator('[data-line-mode]')).toHaveAttribute('data-line-mode', 'text')
  await expect(page.locator('[data-branch-task]')).toHaveCount(8)
  await page.getByRole('button', { name: 'Stats' }).click()
  await expect(page.getByRole('dialog', { name: 'Delivery stats' }).locator('[data-figure="open"] dd')).toHaveText('6')
})

// The map and the sheet read the insight view the line reads. When that read
// fails, each names the failure, the rest of the page renders, and Try again
// reads it again.
test('a failed insight read names the failure in the branch map and the sheet', async ({ page }) => {
  const { insightRequests } = await serveCrates(page, { insight: 'fail' })
  await page.goto('/w/demo/crates/crate-billing')
  const failed = page.locator('[data-branch-map-error]')
  await expect(failed).toContainText('The branch map did not load.')
  await expect(failed).toContainText('internal error')
  await expect(page.locator('[data-crate-task]')).toHaveCount(2)
  const before = insightRequests.length
  await failed.getByRole('button', { name: 'Try again' }).click()
  await expect.poll(() => insightRequests.length).toBeGreaterThan(before)

  await page.getByRole('button', { name: 'Stats' }).click()
  const sheet = page.getByRole('dialog', { name: 'Delivery stats' })
  await expect(sheet.locator('[data-stats-error]')).toContainText('The delivery numbers did not load.')
  const again = insightRequests.length
  await sheet.getByRole('button', { name: 'Try again' }).click()
  await expect.poll(() => insightRequests.length).toBeGreaterThan(again)
  await sheet.getByRole('button', { name: 'Close' }).click()
  await expect(sheet).toHaveCount(0)
})

// The briefing (req-crate-insights REQ-5, AC-5.1, AC-5.2, AC-5.4, AC-5.5;
// component-crates invariants 33 and 35). The fake server counts every
// briefing GET and POST, so each case proves how many times one opening of
// the page posts. The screenshots attach to the Playwright report and are
// also written to CONVEYOR_SCREENSHOT_DIR when it is set.
const briefingShots = process.env.CONVEYOR_SCREENSHOT_DIR

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

for (const viewport of [
  { name: '375 px', slug: '375', width: 375, height: 812 },
  { name: 'desktop', slug: 'desktop', width: 1280, height: 900 },
]) {
  test.describe(`crate briefing at ${viewport.name}`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height } })

    async function shoot(page: Page, name: string) {
      const file = `crate-briefing-${name}-${viewport.slug}.png`
      // The page scrolls inside its own column, so a full-page shot shows
      // only the viewport. The shot takes a taller viewport at the same
      // width, which shows the page from the header to the task list.
      const size = page.viewportSize() ?? { width: viewport.width, height: viewport.height }
      await page.setViewportSize({ width: size.width, height: Math.max(size.height, 1600) })
      await page.evaluate(() => document.fonts.ready)
      const image = await page.screenshot({
        path: briefingShots ? `${briefingShots}/${file}` : undefined,
        animations: 'disabled',
      })
      await page.setViewportSize(size)
      await test.info().attach(file, { body: image, contentType: 'image/png' })
    }

    // Invariant 33: the briefing sits between the header with the line and
    // the branch map. Invariant 35: no generated marker, model name, or
    // generation time.
    test('a stored current briefing shows its text between the header and the branch map', async ({ page }) => {
      const server = await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst }) },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-account]')).toHaveText(storedBriefing().account)
      await expect(section.getByRole('heading', { name: 'Look at first' })).toBeVisible()
      // Only current members are rows, in the served order, with the member
      // title and the served reason.
      const rows = section.locator('[data-briefing-task]')
      await expect(rows).toHaveCount(2)
      expect(await rows.evaluateAll((items) => items.map((item) => item.getAttribute('data-briefing-task')))).toEqual([
        '261115-c11',
        '261115-b02',
      ])
      await expect(rows.first()).toContainText('Index product reviews')
      await expect(rows.first()).toContainText('Its build has run for three days.')
      await expect(rows.first().locator('[data-tone]')).toHaveAttribute('data-tone', 'attention')
      await expect(section).not.toContainText('A task that left the crate.')
      // Regenerate shares the heading's row and is disabled for a current
      // briefing.
      const regenerate = section.getByRole('button', { name: 'Regenerate' })
      await expect(regenerate).toBeDisabled()
      const sameRow = await section
        .locator('[data-briefing-heading-row]')
        .evaluate((row) => row.contains(row.querySelector('h3')) && row.contains(row.querySelector('button')))
      expect(sameRow).toBe(true)
      // No model name, generation time, or generated marker.
      await expect(section).not.toContainText('served-model-name')
      await expect(section).not.toContainText(/generated|2026-11-19/i)
      await expect(section.locator('time')).toHaveCount(0)
      // The page order: the header with the line, the briefing, the map.
      const order = await page.evaluate(() => {
        const parts = ['[aria-label="Crate summary"]', '[data-crate-briefing]', '[data-branch-map]']
        const found = parts.map((selector) => document.querySelector(selector))
        return (
          found.every(Boolean) &&
          found.every(
            (element, index) =>
              index === 0 ||
              Boolean(found[index - 1]!.compareDocumentPosition(element!) & Node.DOCUMENT_POSITION_FOLLOWING),
          )
        )
      })
      expect(order).toBe(true)
      expect(server.briefingPosts).toEqual([])
      await expectNoHorizontalScroll(page)
      await shoot(page, 'stored-current')
    })

    test('a stored briefing with no task to look at first puts Regenerate alone at the right', async ({ page }) => {
      const server = await serveCrates(page, {
        briefings: { [lineCrate.id]: storedBriefing({ current: false }) },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.getByRole('heading', { name: 'Look at first' })).toHaveCount(0)
      const regenerate = section.getByRole('button', { name: 'Regenerate' })
      await expect(regenerate).toBeEnabled()
      // Alone at the right of its row.
      const edges = await section.locator('[data-briefing-heading-row]').evaluate((row) => {
        const button = row.querySelector('button') as HTMLElement
        return { row: row.getBoundingClientRect().right, button: button.getBoundingClientRect().right }
      })
      expect(Math.abs(edges.row - edges.button)).toBeLessThan(1)
      expect(server.briefingPosts).toEqual([])
      await shoot(page, 'stored-no-look-first')
    })

    // AC-5.2: the first answered read of an opening posts once for an
    // eligible crate with no briefing, and the page shows a status while the
    // request runs.
    test('a first opening with no briefing posts once and shows the result', async ({ page }) => {
      const gate = held()
      const server = await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings: { [lineCrate.id]: null },
        briefingPost: async () => {
          await gate.promise
          return undefined
        },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      const status = section.getByRole('status', { name: 'Writing the briefing' })
      await expect(status).toBeVisible()
      await expect(status).toContainText('Reading 8 tasks and their activity')
      await expect.poll(() => server.briefingPosts).toEqual([lineCrate.id])
      await shoot(page, 'generating')
      gate.release()
      await expect(section.locator('[data-briefing-headline]')).toHaveText('A new briefing came back from the server.')
      await expect(status).toHaveCount(0)
      expect(server.briefingPosts).toEqual([lineCrate.id])
    })

    // Invariant 35: refetches never post again, a failed POST is not tried
    // again, and only a new opening decides again.
    test('a failed first post shows its notice, and a refetch in the same opening does not post', async ({ page }) => {
      const server = await serveCrates(page, {
        briefings: { [lineCrate.id]: null },
        briefingPost: () => ({
          status: 502,
          json: {
            error: 'crate_briefing_failed',
            detail: 'model timed out after 30s',
            message: 'The briefing could not be generated. The stored briefing did not change.',
          },
        }),
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      const notice = section.locator('[data-briefing-failed]')
      await expect(notice).toHaveText(
        'The briefing could not be generated (crate_briefing_failed): model timed out after 30s',
      )
      await expect(notice).toHaveAttribute('role', 'alert')
      expect(server.briefingPosts).toEqual([lineCrate.id])
      await shoot(page, 'failed-none-stored')

      // A workspace refresh refetches every crate read, the briefing too. It
      // still has no briefing, and the page does not post again.
      const gets = server.briefingGets.length
      await page.getByRole('button', { name: 'Crate actions' }).click()
      await page.getByRole('menuitem', { name: 'Close crate' }).click()
      await expect.poll(() => server.briefingGets.length).toBeGreaterThan(gets)
      // Any post would follow the refetch within a frame or two.
      await page.waitForTimeout(400)
      expect(server.briefingPosts).toEqual([lineCrate.id])

      // A new opening decides again, on its own read, not on the copy the
      // last visit left in the cache.
      await page.getByRole('link', { name: 'Crates', exact: true }).first().click()
      await expect(page.locator('[data-crate-row]').first()).toBeVisible()
      const before = server.briefingGets.length
      await page.goBack()
      await expect.poll(() => server.briefingPosts.length).toBe(2)
      expect(server.briefingGets.length).toBeGreaterThan(before)
    })

    // AC-5.4 and invariant 35: Regenerate is enabled only for a briefing
    // that is out of date, sends one request, and shows the server's answer.
    test('Regenerate is enabled only for an out-of-date briefing and posts once', async ({ page }) => {
      const gate = held()
      const briefings: Record<string, CrateBriefing | null> = {
        [lineCrate.id]: storedBriefing({ current: false, look_first: lookFirst }),
      }
      const server = await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings,
        briefingPost: async () => {
          await gate.promise
          return undefined
        },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      const regenerate = section.getByRole('button', { name: 'Regenerate' })
      await expect(regenerate).toBeEnabled()
      expect(server.briefingPosts).toEqual([])
      await shoot(page, 'stored-out-of-date')

      // Two quick clicks send one request.
      await regenerate.dblclick()
      await expect(regenerate).toBeDisabled()
      await expect.poll(() => server.briefingPosts).toEqual([lineCrate.id])
      // The stored text stays while the request runs, and a status says
      // that a new briefing is on its way.
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.getByRole('status')).toHaveText('Writing a new briefing…')
      await shoot(page, 'regenerating')
      const getsBefore = server.briefingGets.length
      gate.release()
      await expect(section.locator('[data-briefing-headline]')).toHaveText('A new briefing came back from the server.')
      // The page reads the briefing again after the answer. Nothing changed
      // in the crate, so the briefing stays current and Regenerate disabled.
      await expect.poll(() => server.briefingGets.length).toBeGreaterThan(getsBefore)
      await expect(regenerate).toBeDisabled()
      expect(server.briefingPosts).toEqual([lineCrate.id])
    })

    // Invariant 35: the answer to a POST compares the briefing with the crate
    // as it stood when generation started. When the crate changes while the
    // model runs, the read that follows the answer shows the briefing is out
    // of date, and Regenerate is enabled again.
    test('a crate change during generation leaves the new briefing out of date', async ({ page }) => {
      const gate = held()
      const briefings: Record<string, CrateBriefing | null> = {
        [lineCrate.id]: storedBriefing({ current: false }),
      }
      const server = await serveCrates(page, {
        briefings,
        briefingPost: async (id) => {
          await gate.promise
          // A member changed after the server built the model input: the
          // stored row's fingerprint no longer matches the live one.
          const generated = storedBriefing({ headline: 'Written before the crate changed.' })
          briefings[id] = { ...generated, current: false }
          return {
            status: 200,
            json: { briefing: generated, fingerprint: 'fingerprint-sent', can_generate: true },
          }
        },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      const regenerate = section.getByRole('button', { name: 'Regenerate' })
      await regenerate.click()
      await expect(regenerate).toBeDisabled()
      const getsBefore = server.briefingGets.length
      gate.release()
      await expect(section.locator('[data-briefing-headline]')).toHaveText('Written before the crate changed.')
      await expect.poll(() => server.briefingGets.length).toBeGreaterThan(getsBefore)
      await expect(regenerate).toBeEnabled()
      await expect(section.getByRole('alert')).toHaveCount(0)
      expect(server.briefingPosts).toEqual([lineCrate.id])
    })

    // Invariant 35: a 409 refetches the briefing and shows no notice.
    test('a 409 answer refetches the briefing without a notice', async ({ page }) => {
      const briefings: Record<string, CrateBriefing | null> = {
        [lineCrate.id]: storedBriefing({ current: false }),
      }
      const server = await serveCrates(page, {
        briefings,
        briefingPost: () => {
          // Another user generated a briefing first.
          briefings[lineCrate.id] = storedBriefing({ headline: 'Someone else wrote this briefing first.' })
          return {
            status: 409,
            json: {
              error: 'crate_briefing_current',
              crate_id: lineCrate.id,
              message: 'The stored briefing is current.',
            },
          }
        },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      const gets = server.briefingGets.length
      await section.getByRole('button', { name: 'Regenerate' }).click()
      await expect(section.locator('[data-briefing-headline]')).toHaveText('Someone else wrote this briefing first.')
      expect(server.briefingGets.length).toBeGreaterThan(gets)
      await expect(section.getByRole('alert')).toHaveCount(0)
      await expect(section.getByRole('button', { name: 'Regenerate' })).toBeDisabled()
      expect(server.briefingPosts).toEqual([lineCrate.id])
    })

    // AC-5.4: a 502 names its code and detail beside the stored briefing,
    // which does not change.
    test('a 502 answer shows its code and detail beside the unchanged stored briefing', async ({ page }) => {
      const server = await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings: { [lineCrate.id]: storedBriefing({ current: false, look_first: lookFirst }) },
        briefingPost: () => ({
          status: 502,
          json: {
            error: 'crate_briefing_failed',
            detail: 'the reply held a task ID',
            message: 'The briefing could not be generated. The stored briefing did not change.',
          },
        }),
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await section.getByRole('button', { name: 'Regenerate' }).click()
      const notice = section.getByRole('alert')
      await expect(notice).toHaveText(
        'The briefing could not be generated (crate_briefing_failed): the reply held a task ID',
      )
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-task]')).toHaveCount(2)
      expect(server.briefingPosts).toEqual([lineCrate.id])
      await expectNoHorizontalScroll(page)
      await shoot(page, 'failed-beside-stored')
    })

    // A store failure on the generation route answers the generic plain-text
    // 500 (DEC-105(1)). The page names it beside the stored briefing, which
    // does not change, and does not post again.
    test('a store failure on generation shows its notice and keeps the stored briefing', async ({ page }) => {
      const server = await serveCrates(page, {
        briefings: { [lineCrate.id]: storedBriefing({ current: false, look_first: lookFirst }) },
        briefingPost: () => ({
          status: 500,
          contentType: 'text/plain; charset=utf-8',
          body: 'internal server error\n',
        }),
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await section.getByRole('button', { name: 'Regenerate' }).click()
      await expect(section.getByRole('alert')).toHaveText(
        'The briefing could not be generated (HTTP 500): internal server error',
      )
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-task]')).toHaveCount(2)
      await expect(section.getByRole('button', { name: 'Regenerate' })).toBeEnabled()
      expect(server.briefingPosts).toEqual([lineCrate.id])
    })

    // AC-5.5: a user without create_tasks sees a stored briefing and no
    // control, and with none stored learns who can generate one. The page
    // never posts for this user.
    test('a viewer without create_tasks sees the stored briefing and no control', async ({ page }) => {
      const briefings: Record<string, CrateBriefing | null> = {
        [lineCrate.id]: storedBriefing({ current: false, look_first: lookFirst }),
        [billing.id]: null,
      }
      const server = await serveCrates(page, { role: 'viewer', briefings })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-task]')).toHaveCount(2)
      await expect(section.getByRole('button', { name: 'Regenerate' })).toHaveCount(0)
      await shoot(page, 'viewer-stored')

      await page.goto('/w/demo/crates/crate-billing')
      await expect(briefingSection(page).locator('[data-briefing-none]')).toHaveText(
        'No briefing yet. A user who can create tasks can generate one.',
      )
      await expect(briefingSection(page).getByRole('button')).toHaveCount(0)
      await expect.poll(() => server.briefingGets).toContain(billing.id)
      await page.waitForTimeout(400)
      expect(server.briefingPosts).toEqual([])
      await shoot(page, 'viewer-none')
    })

    // The feature design's rollout: a crate with no member has no briefing,
    // and the page neither reads nor posts one.
    test('an empty crate shows no briefing section and sends no briefing request', async ({ page }) => {
      const server = await serveCrates(page, { briefings: { [emptyCrate.id]: null } })
      await page.goto('/w/demo/crates/crate-empty')
      await expect.poll(() => stationCounts(page)).toMatchObject({ intake: '0' })
      await expect(page.locator('[data-crate-briefing]')).toHaveCount(0)
      await page.waitForTimeout(400)
      expect(server.briefingGets).toEqual([])
      expect(server.briefingPosts).toEqual([])
      await shoot(page, 'empty-crate')
    })

    // Invariant 35: the text renders as plain text, never markup.
    test('a headline holding markup renders as those characters', async ({ page }) => {
      const headline = 'Ship <b>now</b> & read <script>alert(1)</script> later'
      const account = 'An **account** with <i>tags</i> and a [link](https://example.com).'
      await serveCrates(page, {
        briefings: {
          [lineCrate.id]: storedBriefing({
            headline,
            account,
            look_first: [{ task_id: '261115-c11', tone: 'info', reason: 'A <em>reason</em> with tags.' }],
          }),
        },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(headline)
      await expect(section.locator('[data-briefing-account]')).toHaveText(account)
      await expect(section.locator('[data-briefing-reason]')).toHaveText('A <em>reason</em> with tags.')
      await expect(section.locator('b, i, em, script, a, strong')).toHaveCount(0)
    })

    // REQ-3 and AC-5.1: pointing at a row emphasizes its task in the line and
    // the branch map and marks its row in the task list. Activating the row
    // opens task detail.
    test('pointing at a Look at first row emphasizes its task across the page', async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: 2400 })
      await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst }) },
      })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      await expect(map.locator('[data-branch-task]')).toHaveCount(8)
      const row = page.locator('[data-briefing-task="261115-c11"]')
      await row.hover()
      await expect(row).toHaveAttribute('data-briefing-emphasized', 'true')
      await expect(map.locator('[data-branch-task="261115-c11"]')).toHaveAttribute('data-map-emphasized', 'true')
      await expect(map.locator('[data-branch-card="261115-c11"]')).toBeVisible()
      await expect(page.locator('[data-crate-task="261115-c11"]')).toHaveAttribute('data-emphasized', 'true')
      await expect(page.locator('[data-crate-task][data-emphasized]')).toHaveCount(1)
      // Where the scene draws, the line shows the task's card. Without WebGL
      // the line is text counts and has no card.
      const mode = await page.locator('[data-line-mode]').getAttribute('data-line-mode')
      if (mode === 'scene') await expect(page.locator('[data-line-card="261115-c11"]')).toHaveCount(1)
      await shoot(page, 'row-emphasis')

      await page.mouse.move(0, 0)
      await expect(page.locator('[data-crate-task][data-emphasized]')).toHaveCount(0)
      await expect(page.locator('[data-briefing-emphasized]')).toHaveCount(0)
      await row.click()
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-c11$/)
    })

    // The two orders of pointer and focus: a focused row keeps the emphasis
    // when the pointer leaves another target, and a row that takes focus
    // wins over a resting pointer. Enter opens the focused row's task.
    test('keyboard focus on a Look at first row and the pointer share the emphasis in both orders', async ({
      page,
    }) => {
      // Tall enough that moving focus never scrolls the page, which would put
      // another element under the resting pointer.
      await page.setViewportSize({ width: viewport.width, height: 2400 })
      await serveCrates(page, {
        insights: { [lineCrate.id]: deliveryInsight },
        briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst }) },
      })
      await page.goto('/w/demo/crates/crate-line')
      const map = page.getByRole('region', { name: 'Where the work is' })
      await expect(map.locator('[data-branch-task]')).toHaveCount(8)
      const briefingRow = (id: string) => page.locator(`[data-briefing-task="${id}"]`)
      const taskRow = (id: string) => page.locator(`[data-crate-task="${id}"]`)
      async function showsOnly(id: string) {
        await expect(taskRow(id)).toHaveAttribute('data-emphasized', 'true')
        await expect(page.locator('[data-crate-task][data-emphasized]')).toHaveCount(1)
        await expect(map.locator(`[data-branch-task="${id}"]`)).toHaveAttribute('data-map-emphasized', 'true')
        await expect(map.locator('[data-branch-card]')).toHaveCount(1)
      }

      // Order 1: the pointer rests on a task row, then focus moves to a
      // briefing row. The newly focused row wins.
      await taskRow('261115-f07').hover()
      await showsOnly('261115-f07')
      await briefingRow('261115-c11').focus()
      await showsOnly('261115-c11')
      await expect(briefingRow('261115-c11')).toHaveAttribute('data-briefing-emphasized', 'true')

      // Order 2: the pointer enters another briefing row and then leaves the
      // page. The focused row gets the emphasis back.
      await briefingRow('261115-b02').hover()
      await showsOnly('261115-b02')
      await page.mouse.move(0, 0)
      await expect(briefingRow('261115-c11')).toBeFocused()
      await showsOnly('261115-c11')

      // Tab moves focus to the next row, which takes the emphasis.
      await page.keyboard.press('Tab')
      await expect(briefingRow('261115-b02')).toBeFocused()
      await showsOnly('261115-b02')
      // When the row loses focus, nothing stays emphasized.
      await briefingRow('261115-b02').blur()
      await expect(page.locator('[data-crate-task][data-emphasized]')).toHaveCount(0)

      // Enter on a focused row opens its task.
      await briefingRow('261115-c11').focus()
      await page.keyboard.press('Enter')
      await expect(page).toHaveURL(/\/w\/demo\/tasks\/261115-c11$/)
    })

    // A later opening shows the copy an earlier visit cached while it reads
    // again. When that read fails, the page keeps the cached text and says
    // that the read failed, and it does not post.
    test('a failed read in a later opening shows its notice beside the cached briefing', async ({ page }) => {
      const server = await serveCrates(page, {
        briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst, current: false }) },
      })
      await page.goto('/w/demo/crates/crate-line')
      const section = briefingSection(page)
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-read-failed]')).toHaveCount(0)

      await page.getByRole('link', { name: 'Crates', exact: true }).first().click()
      await expect(page.locator('[data-crate-row]').first()).toBeVisible()
      // A store failure on the briefing read answers the generic plain-text
      // 500 (DEC-105(1)).
      await page.route(
        (url) => /^\/v1\/crates\/[^/]+\/briefing$/.test(url.pathname),
        (route) =>
          route.fulfill({ status: 500, contentType: 'text/plain; charset=utf-8', body: 'internal server error\n' }),
      )
      await page.goBack()
      const notice = section.locator('[data-briefing-read-failed]')
      await expect(notice).toHaveText('The briefing did not refresh: internal server error')
      await expect(notice).toHaveAttribute('role', 'alert')
      await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
      await expect(section.locator('[data-briefing-task]')).toHaveCount(2)
      // The cached view said can_generate, but this opening's read failed,
      // so the briefing offers no control.
      await expect(section.locator('[data-briefing-regenerate]')).toHaveCount(0)
      await shoot(page, 'read-failed-beside-cached')
      expect(server.briefingPosts).toEqual([])
    })

    // component-web-dashboard invariant 11: Regenerate shows only while a
    // successful identity read lists create_tasks. A caller who loses it, or
    // whose identity read then fails, keeps the stored text and loses the
    // control, and the page sends nothing.
    for (const change of [
      { name: 'an identity read without create_tasks', fulfill: { role: 'viewer' as const } },
      { name: 'a failed identity read', fulfill: { status: 500 } },
    ]) {
      test(`Regenerate disappears after ${change.name}`, async ({ page }) => {
        await page.clock.install({ time: new Date('2026-11-20T09:00:00Z') })
        const server = await serveCrates(page, {
          briefings: { [lineCrate.id]: storedBriefing({ look_first: lookFirst, current: false }) },
        })
        await page.goto('/w/demo/crates/crate-line')
        const section = briefingSection(page)
        await expect(section.getByRole('button', { name: 'Regenerate' })).toBeEnabled()

        await page.route(
          (url) => url.pathname === '/v1/me',
          (route, request) =>
            'status' in change.fulfill
              ? route.fulfill({ status: change.fulfill.status, json: { error: 'internal' } })
              : route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: change.fulfill.role }, request) }),
        )
        // The identity read stays fresh for five minutes. Past that, the tab
        // coming back into view reads it again.
        await page.clock.runFor(5 * 60_000 + 1)
        await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange', { bubbles: true })))
        await expect(section.locator('[data-briefing-regenerate]')).toHaveCount(0)
        await expect(section.locator('[data-briefing-headline]')).toHaveText(storedBriefing().headline)
        await expect(section.locator('[data-briefing-task]')).toHaveCount(2)
        expect(server.briefingPosts).toEqual([])
      })
    }
  })
}

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
