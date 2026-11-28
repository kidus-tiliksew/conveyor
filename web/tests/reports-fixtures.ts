// A fake server for feedback reports (component-feedback-reports v4, Public
// surfaces; docs/feedback-reports.md). It answers in the shapes
// internal/httpapi writes: the served report on `GET /v1/feedback-reports`
// with the list headers, on `GET /v1/feedback-reports/{id}`, and on each of the
// five act routes. It also serves the task, crate, environment, and monitor
// reads that the three pages and the create sheet need. Every value is
// synthetic. A spec can replace the answer of any act or read.
import type { Page, Request, Route } from '@playwright/test'
import type {
  CrateDetail,
  CrateEnvironmentLine,
  EnvironmentRow,
  FeedbackReport,
  FeedbackReportLink,
} from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { controlWorkspaceChanges, type WorkspaceChangeControl } from './helpers/workspace-changes'

export const createdAt = '2026-11-01T09:00:00Z'
export const now = new Date('2026-11-27T12:00:00Z')

export function environment(id: string, name: string, listening: boolean, lastSignalAt: string | null) {
  return { id, name, listening, last_signal_at: lastSignalAt }
}

export const production = environment('env-production', 'production', true, '2026-11-27T11:59:00Z')
export const staging = environment('env-staging', 'staging', false, '2026-11-26T18:00:00Z')
export const qa = environment('env-qa', 'qa', false, null)

export function taskLink(
  id: string,
  taskId: string,
  title: string,
  overrides: Partial<FeedbackReportLink> = {},
  crate: { id: string; name: string } | null = null,
): FeedbackReportLink {
  return { id, state: 'possible', task: { id: taskId, title, crate }, evidence: [], ...overrides }
}

export const lineEvidence = {
  kind: 'line',
  file: 'internal/checkout/pay.go',
  line: 214,
  commit: '3f9c2ab1d4e5f60718293a4b5c6d7e8f90a1b2c3',
}
export const timingEvidence = {
  kind: 'timing',
  service: 'api',
  version: { repository: 'app', commit: '3f9c2ab1d4e5', version: 'v1.4.2' },
  deployed_at: '2026-11-27T11:40:00Z',
  source: 'github',
  elapsed_minutes: 18,
}
export const priorSignalEvidence = {
  ...timingEvidence,
  elapsed_minutes: 0,
  prior_signal_at: '2026-11-27T11:30:00Z',
}
export const deploy = {
  service: 'api',
  version: { repository: 'app', commit: '9a8b7c6d5e4f', version: 'v1.4.3' },
  deployed_at: '2026-11-27T10:00:00Z',
  source: 'signal',
}

export function report(id: string, overrides: Partial<FeedbackReport> = {}): FeedbackReport {
  return {
    id,
    kind: 'error',
    key: `checkout.timeout.${id}`,
    summary: `Checkout request timed out (${id})`,
    environment: production,
    service: 'api',
    count: 12,
    first_seen: '2026-11-27T11:58:00Z',
    last_seen: '2026-11-27T11:59:40Z',
    file: null,
    line: null,
    first: { repository: 'app', commit: '3f9c2ab', version: null },
    latest: { repository: 'app', commit: '3f9c2ab', version: null },
    state: 'open',
    closed: null,
    reopened_at: null,
    in_inbox: true,
    links: [],
    links_pending: false,
    notes: [],
    fix_task: null,
    created_at: '2026-11-27T11:58:05Z',
    ...overrides,
  }
}

export function environmentRow(id: string, name: string): EnvironmentRow {
  return {
    id,
    name,
    tracked_branches: { conveyor: 'main' },
    github_environments: [name],
    version_rule: null,
    timing_window_minutes: 60,
    archived: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    keys: [],
    listening: true,
    last_signal_at: '2026-11-27T11:59:00Z',
    newest_unresolved: null,
  }
}

const nextAction = {
  label: 'Finished',
  actor: 'none',
  kind: 'terminal',
  judgment: false,
  summary: 'This task is finished.',
  deadline: null,
}

export function task(id: string, title: string, state = 'merged') {
  return {
    id,
    workspace: 'demo',
    title,
    body: 'Send each invoice by email.',
    repo: 'conveyor',
    base_branch: 'main',
    state,
    branch: `conveyor/task-${id}`,
    created_at: createdAt,
  }
}

function activity(id: string, title: string, state: string) {
  return {
    task: task(id, title, state),
    jobs: [],
    events: [],
    interventions: [],
    work_orders: [],
    reviews: [],
    latest_stage: 'implement',
    next_action: nextAction,
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: false,
  }
}

export function crateLine(overrides: Partial<CrateEnvironmentLine> = {}): CrateEnvironmentLine {
  return {
    environment: { id: 'env-production', name: 'production' },
    live_task_count: 1,
    merged_task_count: 2,
    newest_live_at: '2026-11-25T14:00:00Z',
    listening: true,
    last_signal_at: '2026-11-27T11:59:00Z',
    as_of: '2026-11-27T11:59:00Z',
    ...overrides,
  }
}

export function crate(lines: CrateEnvironmentLine[] = [crateLine()]): CrateDetail {
  return {
    id: 'crate-billing',
    name: 'Billing v2',
    target_date: null,
    state: 'open',
    shipped: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    closed_at: null,
    open_task_count: 0,
    merged_task_count: 2,
    total_task_count: 2,
    requirements: [],
    undeclared_requirements: [],
    tasks: [
      {
        id: '261120-a01',
        title: 'Send invoices by email',
        state: 'merged',
        branch: 'conveyor/task-261120-a01',
        pull_request: null,
        next_action: { label: 'Finished', actor: 'none', kind: 'terminal' },
      },
      {
        id: '261120-b02',
        title: 'Store invoice numbers',
        state: 'merged',
        branch: 'conveyor/task-261120-b02',
        pull_request: null,
        next_action: { label: 'Finished', actor: 'none', kind: 'terminal' },
      },
    ],
    environments: lines,
  } as unknown as CrateDetail
}

function crateInsight(detail: CrateDetail) {
  const start = '2026-11-10T09:00:00Z'
  return {
    tasks: detail.tasks.map((item) => ({
      id: item.id,
      phase: 'merged',
      station: 'crate',
      base_branch: 'main',
      created_at: start,
      pull_request_opened_at: [],
      changes_requested_at: [],
      review_rounds: 0,
      merged_at: null,
      closed_at: null,
      depends_on: [],
      wait_depth: 0,
      waited_on_by: [],
    })),
    branches: ['main'],
    whereabouts: [],
    progress: {
      daily: [],
      forecast: null,
      weekly: [],
      delivered: 2,
      total: 2,
      open: 0,
      throughput_per_week: 0,
      median_cycle_seconds: null,
      status_mix: { delivered: 2, closed: 0, open: [] },
    },
    first_review: { approved: 0, reviewed: 0 },
    forecast_days_before_target: null,
    replay: { start, end: '2026-11-26T10:00:00Z', steps: [] },
  }
}

export interface Act {
  method: string
  path: string
  body: unknown
}

/** A replacement answer: a status and JSON body, 'abort' for no answer, or undefined for the default. */
export type Answer = { status: number; json?: unknown; headers?: Record<string, string> } | 'abort' | undefined

export interface ServerOptions {
  reports?: FeedbackReport[]
  role?: 'operator' | 'viewer' | 'contributor'
  crate?: CrateDetail
  // The merged tasks the link form searches.
  mergedTasks?: ReturnType<typeof task>[]
}

export interface Server {
  reports: FeedbackReport[]
  gets: string[]
  acts: Act[]
  created: Record<string, unknown>[]
  stream: WorkspaceChangeControl
  crate: CrateDetail
  // Replaces the answer of the next act requests, in order. An empty queue
  // answers by the default rules.
  actAnswers: Array<Answer | ((act: Act) => Answer)>
  // Holds every act answer until the promise resolves.
  actGate: Promise<void> | null
  // Replaces the answer of a list read.
  listAnswer: ((url: URL) => Answer) | null
  // Replaces the answer of a single report read.
  readAnswer: ((id: string) => Answer) | null
  // Holds every report list answer until the promise resolves.
  listGate: Promise<void> | null
  // Holds every single report answer until the promise resolves.
  readGate: Promise<void> | null
  // Fails the merged-task search of the link form.
  failTaskSearch: boolean
  // Holds every task intake answer until the promise resolves.
  createGate: Promise<void> | null
  // The intake key of each task intake request, in order.
  intakeKeys: string[]
  // The number of next task intake requests whose answer is lost.
  loseCreateAnswers: number
}

function servedOrder(a: FeedbackReport, b: FeedbackReport) {
  return b.last_seen.localeCompare(a.last_seen) || a.id.localeCompare(b.id)
}

function belongsToCrate(item: FeedbackReport, crateId: string) {
  return item.links.some((link) => link.crate?.id === crateId || link.task?.crate?.id === crateId)
}

function inSomeCrate(item: FeedbackReport) {
  return item.links.some((link) => link.crate || link.task?.crate)
}

/** The report as the default rules change it for one act, or an answer for a refusal. */
function applyAct(
  state: Server,
  method: string,
  path: string,
  body: Record<string, unknown>,
): { status: number; json: unknown } {
  const match = path.match(/^\/v1\/feedback-reports\/([^/]+)\/(links|close|reopen|fix-task)(?:\/([^/]+))?$/)
  if (!match) return { status: 404, json: { error: 'not_found' } }
  const id = decodeURIComponent(match[1])
  const index = state.reports.findIndex((item) => item.id === id)
  if (index < 0) return { status: 404, json: { error: 'report_not_found', message: 'The report was not found.' } }
  const current = structuredClone(state.reports[index])
  const kind = match[2]
  if (current.state === 'closed' && kind !== 'reopen')
    return { status: 409, json: { error: 'report_closed', message: 'The report is closed. Reopen it first.' } }
  const at = '2026-11-27T12:00:30Z'
  if (kind === 'links' && method === 'POST') {
    const taskId = typeof body.task_id === 'string' ? body.task_id : ''
    const crateId = typeof body.crate_id === 'string' ? body.crate_id : ''
    const existing = current.links.find((link) => (taskId ? link.task?.id === taskId : link.crate?.id === crateId))
    if (existing) {
      existing.state = 'confirmed'
      existing.confirmed_by = 'usr_operator'
      existing.confirmed_at = at
    } else if (taskId) {
      current.links.push(
        taskLink(`lnk-${taskId}`, taskId, `Task ${taskId}`, {
          state: 'confirmed',
          confirmed_by: 'usr_operator',
          confirmed_at: at,
        }),
      )
    } else {
      current.links.push({
        id: `lnk-${crateId}`,
        state: 'confirmed',
        crate: { id: crateId, name: `Crate ${crateId}` },
        evidence: [],
        confirmed_by: 'usr_operator',
        confirmed_at: at,
      })
    }
  } else if (kind === 'links' && method === 'DELETE') {
    const linkId = decodeURIComponent(match[3] ?? '')
    if (!current.links.some((link) => link.id === linkId))
      return {
        status: 404,
        json: { error: 'link_not_found', field: 'link_id', message: 'must name a link of the report' },
      }
    current.links = current.links.filter((link) => link.id !== linkId)
  } else if (kind === 'close') {
    current.state = 'closed'
    current.closed = { at, by: 'usr_operator', reason: String(body.reason ?? '') }
  } else if (kind === 'reopen') {
    if (current.state === 'open') return { status: 200, json: current }
    current.state = 'open'
    current.closed = null
    current.reopened_at = at
  } else if (kind === 'fix-task') {
    current.fix_task = { id: String(body.task_id), title: `Task ${String(body.task_id)}` }
    // Each cause is confirmed as a link act confirms it, keeping its evidence.
    for (const cause of Array.isArray(body.cause_task_ids) ? body.cause_task_ids : []) {
      const existing = current.links.find((link) => link.task?.id === cause)
      if (existing) {
        existing.state = 'confirmed'
        existing.confirmed_by = 'usr_operator'
        existing.confirmed_at = at
      } else {
        current.links.push(
          taskLink(`lnk-${String(cause)}`, String(cause), `Task ${String(cause)}`, {
            state: 'confirmed',
            confirmed_by: 'usr_operator',
            confirmed_at: at,
          }),
        )
      }
    }
  }
  current.in_inbox = false
  state.reports[index] = current
  return { status: 200, json: current }
}

async function fulfill(route: Route, answer: Exclude<Answer, undefined>) {
  if (answer === 'abort') return route.abort('connectionreset')
  return route.fulfill({ status: answer.status, json: answer.json ?? {}, headers: answer.headers })
}

function parseBody(request: Request): Record<string, unknown> {
  const text = request.postData()
  if (!text) return {}
  try {
    return JSON.parse(text) as Record<string, unknown>
  } catch {
    return {}
  }
}

export async function mockReportServer(page: Page, options: ServerOptions = {}): Promise<Server> {
  const stream = await controlWorkspaceChanges(page)
  const state: Server = {
    reports: structuredClone(options.reports ?? []),
    gets: [],
    acts: [],
    created: [],
    stream,
    crate: options.crate ?? crate(),
    actAnswers: [],
    actGate: null,
    listAnswer: null,
    readAnswer: null,
    listGate: null,
    readGate: null,
    failTaskSearch: false,
    createGate: null,
    intakeKeys: [],
    loseCreateAnswers: 0,
  }
  const role = options.role ?? 'operator'
  const mergedTasks = options.mergedTasks ?? [
    task('261120-a01', 'Send invoices by email'),
    task('261120-b02', 'Store invoice numbers'),
  ]
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-theme', 'light')
    localStorage.setItem('conveyor-task-activity-view', 'timeline')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()
    if (method === 'GET') state.gets.push(`${path}${url.search}`)
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: {
          workspace: 'demo',
          database: 'postgres',
          max_bounces: 2,
          repos: [
            { name: 'conveyor', base: 'main' },
            { name: 'app', base: 'trunk' },
          ],
        },
      })
    if (path === '/v1/environments')
      return route.fulfill({
        json: [environmentRow('env-production', 'production'), environmentRow('env-staging', 'staging')],
      })
    if (path === '/v1/feedback-reports' && method === 'GET') {
      if (state.listGate) await state.listGate
      const replaced = state.listAnswer?.(url)
      if (replaced) return fulfill(route, replaced)
      const params = url.searchParams
      const wanted = params.get('state') ?? 'open'
      // The second workspace holds no report.
      let items = params.get('workspace_id') === 'other' ? [] : state.reports.filter((item) => item.state === wanted)
      const environmentId = params.get('environment_id')
      if (environmentId) items = items.filter((item) => item.environment.id === environmentId)
      const taskId = params.get('task_id')
      if (taskId) items = items.filter((item) => item.links.some((link) => link.task?.id === taskId))
      const crateId = params.get('crate_id')
      if (crateId) items = items.filter((item) => belongsToCrate(item, crateId))
      if (params.get('no_crate') === 'true') items = items.filter((item) => !inSomeCrate(item))
      items.sort(servedOrder)
      const limit = Number(params.get('limit') ?? 100)
      const offset = Number(params.get('offset') ?? 0)
      // A read the page cancelled while it waited has no one to answer.
      return route
        .fulfill({
          json: items.slice(offset, offset + limit),
          headers: {
            'X-Conveyor-Total': String(items.length),
            'X-Conveyor-Limit': String(limit),
            'X-Conveyor-Offset': String(offset),
          },
        })
        .catch(() => undefined)
    }
    const single = path.match(/^\/v1\/feedback-reports\/([^/]+)$/)
    if (single && method === 'GET') {
      const id = decodeURIComponent(single[1])
      // The answer is chosen when the request arrives, before any gate, so a
      // held request keeps the answer of its own moment.
      const replaced = state.readAnswer?.(id)
      if (state.readGate) await state.readGate
      if (replaced) return fulfill(route, replaced)
      const found = state.reports.find((item) => item.id === id)
      if (!found)
        return route.fulfill({ status: 404, json: { error: 'report_not_found', message: 'The report was not found.' } })
      return route.fulfill({ json: found })
    }
    if (path.startsWith('/v1/feedback-reports/') && method !== 'GET') {
      const body = parseBody(request)
      const act: Act = { method, path, body: request.postData() ? body : undefined }
      state.acts.push(act)
      if (state.actGate) await state.actGate
      const next = state.actAnswers.shift()
      const replaced = typeof next === 'function' ? next(act) : next
      if (replaced) return fulfill(route, replaced)
      const answer = applyAct(state, method, path, body)
      return route.fulfill({ status: answer.status, json: answer.json })
    }
    if (path === '/v1/tasks' && method === 'POST') {
      const form = request.postData() ?? ''
      const match = form.match(/name="task"\r\n\r\n([\s\S]*?)\r\n--/)
      state.created.push(match ? (JSON.parse(match[1]) as Record<string, unknown>) : {})
      // Intake with a key it saw before answers with the same task, as the
      // server does; a new key makes a new task.
      const intakeKey = form.match(/name="idempotency_key"\r\n\r\n([\s\S]*?)\r\n--/)?.[1] ?? ''
      state.intakeKeys.push(intakeKey)
      const distinct = [...new Set(state.intakeKeys)]
      const index = distinct.indexOf(intakeKey)
      const id = index === 0 ? '261127-fix' : `261127-fix${index + 1}`
      if (state.createGate) await state.createGate
      if (state.loseCreateAnswers > 0) {
        state.loseCreateAnswers--
        return route.abort('connectionreset')
      }
      // A page that reloaded meanwhile has no one to answer.
      return route
        .fulfill({ status: 201, json: { ...task(id, 'Fix the checkout timeout', 'queued') } })
        .catch(() => undefined)
    }
    if (path === '/v1/tasks' && method === 'GET') {
      if (state.failTaskSearch) return route.fulfill({ status: 500, json: { error: 'internal error' } })
      const query = (url.searchParams.get('q') ?? '').toLowerCase()
      return route.fulfill({
        json: mergedTasks.filter(
          (item) => !query || item.title.toLowerCase().includes(query) || item.id.includes(query),
        ),
      })
    }
    const taskRead = path.match(/^\/v1\/tasks\/([^/]+)(?:\/(activity|next-action))?$/)
    if (taskRead) {
      const id = decodeURIComponent(taskRead[1])
      const known = mergedTasks.find((item) => item.id === id)
      const title = known?.title ?? (id === '261127-fix' ? 'Fix the checkout timeout' : 'Send invoices by email')
      const taskState = id === '261127-fix' || id.startsWith('running') ? 'queued' : 'merged'
      if (taskRead[2] === 'next-action') return route.fulfill({ json: { ...nextAction, choices: [], details: {} } })
      if (taskRead[2] === 'activity') return route.fulfill({ json: activity(id, title, taskState) })
      return route.fulfill({ json: { ...task(id, title, taskState), environments: [] } })
    }
    if (path === '/v1/crates' && method === 'GET')
      return route.fulfill({
        json: [
          { ...state.crate, briefing: null },
          { ...state.crate, id: 'crate-search', name: 'Search', briefing: null },
        ],
      })
    if (path === '/v1/crates/crate-billing') return route.fulfill({ json: state.crate })
    if (path === '/v1/crates/crate-billing/insight') return route.fulfill({ json: crateInsight(state.crate) })
    if (path === '/v1/crates/crate-billing/activity') return route.fulfill({ json: { events: [] } })
    if (path === '/v1/crates/crate-billing/briefing')
      return route.fulfill({ json: { briefing: null, fingerprint: 'none', can_generate: false } })
    if (path === '/v1/monitor')
      return route.fulfill({
        json: { enabled: true, drift_count: 0, drift: [], observations: [], last_successful_observation: createdAt },
      })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })
  return state
}

const pathOf = (read: string) => read.split('?')[0]

/** The report list reads, each with its query. */
export function listReads(state: Server) {
  return state.gets.filter((read) => pathOf(read) === '/v1/feedback-reports')
}

export function reportReads(state: Server, id: string) {
  return state.gets.filter((read) => pathOf(read) === `/v1/feedback-reports/${id}`).length
}

export function crateReads(state: Server) {
  return state.gets.filter((read) => pathOf(read) === '/v1/crates/crate-billing').length
}

/** A gate that holds answers until `open` runs. */
export function gate() {
  let open = () => {}
  const promise = new Promise<void>((resolve) => {
    open = resolve
  })
  return { promise, open }
}
