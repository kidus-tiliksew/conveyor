import { expect, type Locator, type Page, type Route } from '@playwright/test'
import type { ActivityItem, TaskPlanCycle } from '../src/lib/types'
import { type CompleteEvent, test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { activityFrame, controlTaskStreams } from './helpers/task-streams'

// Plan history reads as one timeline entry per plan cycle, built only from
// `summaries.plan_cycles` (component-web-task-surfaces invariants 41 and 67 to
// 71; DEC-130; req-260809-a40c75 AC-4.1, AC-4.2). Complete fixtures pass
// through the fake server in helpers/bounded-detail.ts, which folds the cycles
// as internal/httpapi does; the Go suites own the fold itself.

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', 'timeline'))
})

const members = [
  {
    workspace_id: 'demo',
    user_id: 'usr_meron',
    display_name: 'Meron Tesfaye',
    email: 'meron@example.test',
    role: 'operator',
    created_at: '2026-11-01T00:00:00Z',
  },
  {
    workspace_id: 'demo',
    user_id: 'usr_kidus',
    display_name: 'Kidus Alemu',
    email: 'kidus@example.test',
    role: 'operator',
    created_at: '2026-11-01T00:00:00Z',
  },
]

const longRationale =
  'The approved plan reads the cases file from the release branch, but the check now runs on develop, where the ' +
  'file moved under a new package. Following the plan would test a stale copy and pass for the wrong reason. ' +
  'I need a revised plan that names the new location, adds the fixture the moved file depends on, and updates ' +
  'the CI job that pins the old path, so the check fails when the cases drift again.'

const longComment =
  'Split the reader change from the CI change, so each lands with its own test. Name the fixture file and the ' +
  'package it loads from. Keep the old path working for one release with a warning, then remove it. State the ' +
  'rollback step if the moved package fails to import on the release branch, and say which job proves it.'

const day1 = (time: string) => `2026-11-10T${time}:00Z`
const day2 = (time: string) => `2026-11-11T${time}:00Z`

function baseTask(id: string, state: string) {
  return {
    id,
    workspace: 'demo',
    title: `Repair the cases check (${id})`,
    body: 'Repair the cases check on develop.',
    class: 'bug',
    level: '',
    plan_state: 'completed',
    merge_approval: false,
    policy_version: 1,
    repo: 'conveyor',
    base_branch: 'main',
    branch: `conveyor/task-${id}`,
    state,
    next_stage: 'implement',
    source: 'api',
    created_at: day1('09:00'),
  }
}

const working = {
  label: 'Working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'An agent is implementing this task.',
  deadline: null,
}

type Fixture = {
  task: ReturnType<typeof baseTask>
  jobs: []
  events: CompleteEvent[]
  interventions: ActivityItem['interventions']
  work_orders: []
  checkout_available: boolean
  checkout_guidance: string
  needs_attention: boolean
  at_merge_gate: boolean
  attachments: []
  verification_evidence: []
  next_action: Record<string, unknown>
}

// A ledger builder: each decision records its intervention and the matching
// intervention.<action> event, as every backend writes them.
function ledger(taskId: string) {
  const events: CompleteEvent[] = []
  const interventions: ActivityItem['interventions'] = []
  let nextId = 1
  return {
    events,
    interventions,
    event(at: string, kind: string, payload: Record<string, unknown>, actor = 'system:dispatcher') {
      events.push({
        id: nextId++,
        task_id: taskId,
        kind,
        actor_id: actor,
        actor_role: actor.startsWith('user:') ? 'human' : 'system',
        payload,
        at,
      })
    },
    decide(at: string, action: 'approve' | 'redirect' | 'reject', reason: string, comment: string, actor: string) {
      const id = interventions.length + 101
      interventions.push({
        id,
        task_id: taskId,
        actor_id: actor,
        actor_role: actor.startsWith('user:') ? 'human' : 'system',
        action,
        reason_code: reason,
        comment,
        at,
      })
      this.event(at, `intervention.${action}`, { reason_code: reason, comment }, actor)
      return id
    },
  }
}

function fixture(id: string, state: string, events: CompleteEvent[], interventions: ActivityItem['interventions']) {
  return {
    task: baseTask(id, state),
    jobs: [],
    events,
    interventions,
    work_orders: [],
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: false,
    at_merge_gate: false,
    attachments: [],
    verification_evidence: [],
    next_action: working,
  } satisfies Fixture
}

// A merged task with three settled plan cycles and a merge-gate approval no
// cycle names. The agent's drafts carry the operator's account as actor, as a
// self-claimed session records them.
function settledTask(id = 'plan-cycles-settled', extraHuman = 0) {
  const l = ledger(id)
  l.event(day1('09:00'), 'spec.version_created', { version: 1, origin: 'intake' }, 'user:usr_meron')
  l.decide(day1('09:00'), 'approve', 'intake-plan-approved', '', 'user:usr_meron')
  l.event(day1('09:00'), 'spec.version_approved', { version: 1 })
  l.event(
    day1('10:00'),
    'work_order.plan_revision_requested',
    { rationale: longRationale, plan_version: 1 },
    'user:usr_meron',
  )
  l.decide(day1('10:30'), 'redirect', 'plan-revision-approved', 'Agreed — split the reader first.', 'user:usr_kidus')
  l.event(day1('11:00'), 'spec.version_created', { version: 2, origin: 'plan_order' }, 'user:usr_meron')
  l.decide(day1('11:20'), 'redirect', 'changes-requested', longComment, 'user:usr_meron')
  l.event(day1('12:00'), 'spec.version_created', { version: 3, origin: 'plan_order' }, 'user:usr_meron')
  l.decide(day2('09:00'), 'approve', '', '', 'user:usr_meron')
  l.event(day2('09:00'), 'spec.version_approved', { version: 3 })
  l.event(
    day2('13:00'),
    'work_order.plan_revision_requested',
    { rationale: 'The fixture path moved again.', plan_version: 3 },
    'user:usr_meron',
  )
  l.decide(day2('13:10'), 'redirect', 'plan-revision-approved', 'Fine.', 'user:usr_kidus')
  l.event(day2('14:00'), 'spec.version_created', { version: 4, origin: 'plan_order' }, 'user:usr_meron')
  l.decide(day2('14:05'), 'approve', '', 'Ship this one.', 'user:usr_kidus')
  for (let index = 0; index < extraHuman; index++)
    l.event(day2(`15:${String(index % 60).padStart(2, '0')}`), 'job.timeout', { timeout: '1h' })
  const mergeApproval = l.decide(day2('18:00'), 'approve', '', 'Merging after review.', 'user:usr_meron')
  l.event(day2('18:01'), 'task.state_changed', { from: 'approved', to: 'merged', command: 'merge.recover' })
  return { item: fixture(id, 'merged', l.events, l.interventions), mergeApproval }
}

// A task waiting on the operator's plan-revision decision.
function waitingTask(id = 'plan-cycles-waiting') {
  const l = ledger(id)
  l.event(day1('09:00'), 'spec.version_created', { version: 1, origin: 'intake' }, 'user:usr_meron')
  l.decide(day1('09:00'), 'approve', 'intake-plan-approved', '', 'user:usr_meron')
  l.event(day1('09:00'), 'spec.version_approved', { version: 1 })
  l.event(
    day2('10:00'),
    'work_order.plan_revision_requested',
    {
      work_order_id: `${id}-implement-1`,
      attempt_id: 'attempt-1',
      session_id: 'session-1',
      rationale: longRationale,
      plan_version: 1,
    },
    'user:usr_meron',
  )
  const item = fixture(id, 'awaiting_human', l.events, l.interventions)
  item.next_action = {
    label: 'Approve plan revision',
    actor: 'operator',
    kind: 'plan_revision',
    judgment: true,
    summary: 'Implementation asked to revise the plan.',
    deadline: null,
  }
  return item
}

async function mockServer(page: Page, details: Record<string, unknown>[], role: 'viewer' | 'operator' = 'operator') {
  const byId = new Map(details.map((item) => [(item as { task: { id: string } }).task.id, item]))
  const reads = new Map<string, number>()
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: 'usr_meron', role }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    if (path === '/v1/workspaces/demo/members') return route.fulfill({ json: members })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    const detailPath = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detailPath) {
      const id = decodeURIComponent(detailPath[1])
      reads.set(id, (reads.get(id) ?? 0) + 1)
      const item = byId.get(id)
      if (!item) return route.fulfill({ status: 404, json: { error: 'not_found' } })
      return route.fulfill({ json: item })
    }
    const nextAction = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (nextAction) {
      const item = byId.get(decodeURIComponent(nextAction[1])) as { next_action?: unknown } | undefined
      return route.fulfill({ json: { ...((item?.next_action as object) ?? working), choices: [], details: {} } })
    }
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  })
  return {
    set: (item: Record<string, unknown>) => byId.set((item as { task: { id: string } }).task.id, item),
    reads: (id: string) => reads.get(id) ?? 0,
  }
}

function timelineOf(page: Page) {
  return page.getByRole('region', { name: 'Execution event timeline' })
}

function cycles(timeline: Locator) {
  return timeline.locator('li[data-plan-cycle]')
}

const shortTime = /[A-Z][a-z]{2} \d{1,2}, \d{1,2}:\d{2}\s?[AP]M$/

// A closed cycle shows its span as a runtime and its step count, and no clock
// time. Focus opens the first and last step times (invariant 70; DEC-127(1)).
async function expectClosedSpan(cycle: Locator, runtime: string, steps: string) {
  const span = cycle.locator('[data-plan-cycle-span]')
  await expect(span).toHaveText(`${runtime}·${steps}`)
  const trigger = span.locator('[data-plan-cycle-runtime]')
  await expect(trigger).toHaveText(runtime)
  await expect(trigger).not.toHaveAttribute('data-runtime-live')
  await expect(trigger).toHaveAccessibleName(
    new RegExp(`^Took ${runtime}\\. ${steps}\\. First step .*[AP]M\\. Last step .*[AP]M\\.$`),
  )
  await trigger.focus()
  await expect(span.locator('[data-plan-cycle-first]')).toHaveText(new RegExp(`^First step ${shortTime.source}`))
  await expect(span.locator('[data-plan-cycle-last]')).toHaveText(new RegExp(`^Last step ${shortTime.source}`))
  await expect(span.locator('[data-plan-cycle-open]')).toHaveCount(0)
  await trigger.blur()
}

test('settled plan history reads as one entry per cycle, folded except the newest', async ({ page }) => {
  const { item } = settledTask()
  await mockServer(page, [item])
  await page.goto('/w/demo/tasks/plan-cycles-settled/full')
  const timeline = timelineOf(page)

  // One entry per cycle (invariant 68).
  await expect(cycles(timeline)).toHaveCount(3)
  const [first, second, newest] = [cycles(timeline).nth(0), cycles(timeline).nth(1), cycles(timeline).nth(2)]
  await expect(first.getByRole('article', { name: 'Plan v1' })).toBeVisible()
  await expect(second.getByRole('article', { name: 'Plan revision v1 → v3' })).toBeVisible()
  await expect(newest.getByRole('article', { name: 'Plan revision v3 → v4' })).toBeVisible()

  // Older settled cycles fold to the header button, one preview line, and the
  // span as a runtime with the step count; their steps are not mounted
  // (invariant 70). The runtime sits beside the fold button, not inside it.
  const secondHeader = second.getByRole('button', { name: /Plan revision/ })
  await expect(secondHeader).toHaveAttribute('aria-expanded', 'false')
  const controls = await secondHeader.getAttribute('aria-controls')
  expect(controls).toBeTruthy()
  await expect(second.locator('[data-plan-cycle-step]')).toHaveCount(0)
  const summary = second.locator('[data-plan-cycle-summary]')
  await expect(summary).toContainText('The approved plan reads the cases file')
  await expectClosedSpan(second, '23h 00m', '6 steps')
  await expect(secondHeader.locator('[data-plan-cycle-runtime]')).toHaveCount(0)
  await expectClosedSpan(first, '0s', '2 steps')
  await expect(first.locator('[data-plan-cycle-step]')).toHaveCount(0)
  // The newest cycle is not folded and still shows its span.
  await expectClosedSpan(newest, '1h 05m', '4 steps')

  // The newest cycle never folds and names the approved version.
  await expect(newest.getByRole('button', { name: /Plan revision/ })).toHaveCount(0)
  const newestSteps = newest.locator('[data-plan-cycle-step]')
  await expect(newestSteps).toHaveCount(4)
  await expect(newestSteps.nth(3)).toContainText('Approved plan v4')
  await expect(newestSteps.nth(3)).toContainText('Ship this one.')
  await expect(newest.getByText('Approved', { exact: true })).toBeVisible()

  // Activating the header unfolds it and mounts its steps in order.
  await secondHeader.click()
  await expect(secondHeader).toHaveAttribute('aria-expanded', 'true')
  await expect(page.locator(`[id="${controls}"]`)).toBeVisible()
  const steps = second.locator('[data-plan-cycle-step]')
  await expect(steps).toHaveCount(6)
  await expect(steps.nth(0)).toContainText('Agent asked to revise plan v1')
  await expect(steps.nth(1)).toContainText('Allowed a revision — back to planning')
  await expect(steps.nth(2)).toContainText('Plan v2 ready')
  await expect(steps.nth(3)).toContainText('Asked for changes to plan v2')
  await expect(steps.nth(4)).toContainText('Plan v3 ready')
  await expect(steps.nth(5)).toContainText('Approved plan v3')
  await first.getByRole('button', { name: /Plan v1/ }).click()
  await expect(first.locator('[data-plan-cycle-step]').nth(0)).toContainText('Plan v1 filed with the task')
  await expect(first.locator('[data-plan-cycle-step]').nth(1)).toContainText('Approved plan v1')

  // No absorbed approval or revision decision renders as its own card; the
  // merge-gate approval no step names keeps its entry (invariant 68; AC-4.2).
  await expect(timeline.locator('li > div strong', { hasText: /^Approved$/ })).toHaveCount(1)
  await expect(timeline.getByText('Merging after review.')).toBeVisible()
  await expect(timeline.getByText('Plan revision approved — returning to planning')).toHaveCount(0)
  await expect(timeline.getByText('The agent asked to revise plan v1')).toHaveCount(0)
  await expect(timeline.getByText('Requested changes')).toHaveCount(0)

  // People appear as avatars, never as printed names; the rail shows the
  // cycle's last deciding person (invariant 69).
  await expect(steps.nth(1).locator('[data-person-avatar="usr_kidus"]')).toHaveCount(1)
  await expect(steps.nth(2).locator('[data-person-avatar]')).toHaveCount(0)
  await expect(newest.locator(':scope > [data-rail-people] [data-person-avatar="usr_kidus"]')).toHaveCount(1)
  for (const cycle of [first, second, newest]) {
    await expect(cycle).not.toContainText('Meron Tesfaye')
    await expect(cycle).not.toContainText('Kidus Alemu')
  }

  // A step on the same day as the one before shows the time alone, with the
  // full date in its title and dateTime.
  const sameDay = steps.nth(1).locator('time')
  await expect(sameDay).toHaveAttribute('datetime', day1('10:30'))
  await expect(sameDay).not.toContainText('Nov')
  await expect(sameDay).toHaveAttribute('title', /2026/)
  await expect(steps.nth(5).locator('time')).toContainText('Nov')
})

test('long requests and comments keep their full text one activation away', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 900 })
  const { item } = settledTask()
  await mockServer(page, [item])
  await page.goto('/w/demo/tasks/plan-cycles-settled/full')
  const second = cycles(timelineOf(page)).nth(1)
  await second.getByRole('button', { name: /Plan revision/ }).click()
  const steps = second.locator('[data-plan-cycle-step]')

  for (const [index, name] of [
    [0, 'Show full request'],
    [3, 'Show full comment'],
  ] as const) {
    const step = steps.nth(index)
    const text = step.locator('[data-plan-cycle-text]')
    const control = step.getByRole('button', { name })
    await expect(control).toHaveAttribute('aria-expanded', 'false')
    const box = await control.boundingBox()
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(32)
    expect(await text.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeGreaterThan(1)
    await control.click()
    const expanded = step.getByRole('button', { name: 'Show less' })
    await expect(expanded).toHaveAttribute('aria-expanded', 'true')
    expect(await text.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeLessThanOrEqual(1)
  }
  // A short comment shows whole, with no control.
  await expect(steps.nth(1).getByRole('button', { name: /Show/ })).toHaveCount(0)

  // Below the sm breakpoint the time sits on its own line under the label.
  const label = steps.nth(2).locator('span').first()
  const time = steps.nth(2).locator('time')
  const labelBox = await label.boundingBox()
  const timeBox = await time.boundingBox()
  expect((timeBox?.y ?? 0) + 0.5).toBeGreaterThanOrEqual((labelBox?.y ?? 0) + (labelBox?.height ?? 0))
})

test('a cycle waiting on a plan-revision decision stays open with the attention marker', async ({ page }) => {
  const item = waitingTask()
  await mockServer(page, [item])
  await page.goto('/w/demo/tasks/plan-cycles-waiting/full')
  const timeline = timelineOf(page)
  await expect(cycles(timeline)).toHaveCount(2)
  const waiting = cycles(timeline).nth(1)
  await expect(waiting.getByRole('article', { name: 'Plan revision' })).toBeVisible()
  await expect(waiting.getByText('Needs your decision')).toBeVisible()
  await expect(waiting.getByRole('button', { name: /Plan revision/ })).toHaveCount(0)
  await expect(waiting.locator('[data-plan-cycle-step]')).toHaveCount(1)
  await expect(waiting.locator('[data-plan-cycle-step]').first()).toContainText('Agent asked to revise plan v1')
  await expect(waiting.getByText('Waiting for your decision below')).toBeVisible()
  await expect(waiting.locator(':scope > [data-plan-cycle-attention]')).toHaveCount(1)
  await expect(waiting.locator(':scope > [data-rail-people]')).toHaveCount(0)
  // The settled intake cycle folds.
  await expect(
    cycles(timeline)
      .nth(0)
      .getByRole('button', { name: /Plan v1/ }),
  ).toHaveAttribute('aria-expanded', 'false')

  // The plan-revision decision card stays below the timeline (invariant 44).
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.getByText('The approved plan reads the cases file', { exact: false })).toBeVisible()
  const cycleBox = await waiting.boundingBox()
  const gateBox = await gate.boundingBox()
  expect(gateBox?.y ?? 0).toBeGreaterThan(cycleBox?.y ?? 0)
})

// A cycle waiting on a decision is still open, so its span reads "Open for …"
// and advances once a second without a read. Its step keeps its own time
// (invariant 70; DEC-127(1)).
test('an open cycle shows its running span, which advances each second', async ({ page }) => {
  const server = await mockServer(page, [waitingTask()])
  await page.clock.install({ time: new Date(day2('10:03')) })
  await page.goto('/w/demo/tasks/plan-cycles-waiting/full')
  const waiting = cycles(timelineOf(page)).nth(1)
  await expect(waiting.locator('[data-plan-cycle-step]')).toHaveCount(1)
  await page.clock.pauseAt(new Date(day2('10:04')))
  const span = waiting.locator('[data-plan-cycle-span]')
  const trigger = span.locator('[data-plan-cycle-runtime]')
  await expect(span).toHaveText('Open for 4m 00s·1 step')
  await expect(trigger).toHaveAttribute('data-runtime-live', '')
  await expect(trigger).toHaveAccessibleName(
    /^Open for 4m 00s\. 1 step\. First step .*[AP]M\. Last step .*[AP]M\. Still open\.$/,
  )
  const reads = server.reads('plan-cycles-waiting')
  await page.clock.runFor(2_000)
  await expect(trigger).toHaveText('Open for 4m 02s')
  expect(server.reads('plan-cycles-waiting')).toBe(reads)
  // Focus and hover open the first and last step times.
  await trigger.focus()
  await expect(span.locator('[data-plan-cycle-first]')).toHaveText(new RegExp(`^First step ${shortTime.source}`))
  await expect(span.locator('[data-plan-cycle-open]')).toHaveText('Still open')
  await trigger.blur()
  await trigger.hover()
  await expect(span.locator('[data-plan-cycle-last]')).toBeVisible()
  // The step row keeps its own clock time.
  await expect(waiting.locator('[data-plan-cycle-time]')).toHaveText(shortTime)
  // The settled intake cycle measures to its last step and starts no timer.
  await expect(cycles(timelineOf(page)).nth(0).locator('[data-plan-cycle-runtime]')).toHaveText('0s')
  await expect(page.locator('[data-runtime-live]')).toHaveCount(1)
})

test.describe('on a coarse pointer at 375 px', () => {
  test.use({ viewport: { width: 375, height: 1400 }, isMobile: true, hasTouch: true })

  test('a cycle span opens its first and last step times on tap, apart from the fold', async ({ page }) => {
    const { item } = settledTask()
    await mockServer(page, [item])
    await page.goto('/w/demo/tasks/plan-cycles-settled/full')
    const second = cycles(timelineOf(page)).nth(1)
    const header = second.getByRole('button', { name: /Plan revision/ })
    await expect(header).toHaveAttribute('aria-expanded', 'false')
    const trigger = second.locator('[data-plan-cycle-runtime]')
    const bounds = await trigger.boundingBox()
    expect(bounds!.height).toBeGreaterThanOrEqual(40)
    await trigger.tap()
    await expect(second.locator('[data-plan-cycle-first]')).toBeVisible()
    await expect(second.locator('[data-plan-cycle-last]')).toBeVisible()
    // The tap opened the times and left the fold as it was.
    await expect(header).toHaveAttribute('aria-expanded', 'false')
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0)
  })
})

// A viewer who cannot act on the gate sees no decision panel, so the waiting
// cycle names the wait without directing them below (invariant 69).
test('a viewer without the gate capability sees neutral waiting wording', async ({ page }) => {
  await mockServer(page, [waitingTask()], 'viewer')
  await page.goto('/w/demo/tasks/plan-cycles-waiting/full')
  const waiting = cycles(timelineOf(page)).nth(1)
  await expect(waiting.getByText('Waiting for a decision', { exact: true })).toBeVisible()
  await expect(waiting.getByText('Waiting for an operator decision')).toBeVisible()
  await expect(waiting.getByText('Needs your decision')).toHaveCount(0)
  await expect(waiting.getByText('Waiting for your decision below')).toHaveCount(0)
  await expect(waiting.locator(':scope > [data-plan-cycle-attention]')).toHaveCount(1)
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
})

// A cycle seeds its fold state on mount; when a newer cycle opens on a live
// refresh, the former newest cycle folds (invariant 70).
test('a cycle folds when a newer cycle opens on a live refresh', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  const id = 'plan-cycles-live'
  const later = waitingTask(id)
  const earlier = {
    ...later,
    task: { ...later.task, state: 'running' },
    events: later.events.slice(0, 3),
    next_action: working,
  }
  const server = await mockServer(page, [earlier])
  await page.clock.install({ time: new Date('2026-11-12T08:00:00Z') })
  await page.goto(`/w/demo/tasks/${id}/full`)
  const timeline = timelineOf(page)
  await expect(cycles(timeline)).toHaveCount(1)
  await expect(cycles(timeline).nth(0).locator('[data-plan-cycle-step]')).toHaveCount(2)
  await expect.poll(() => streams.countFor(id)).toBeGreaterThan(0)
  await streams.open()
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
  const reads = server.reads(id)
  server.set(later)
  await streams.send(activityFrame)
  await page.clock.runFor(250)
  await expect.poll(() => server.reads(id)).toBeGreaterThan(reads)
  // Query results arrive through timers, so the clock runs again to render them.
  await page.clock.resume()
  await expect(cycles(timeline)).toHaveCount(2)
  const former = cycles(timeline).nth(0)
  await expect(former.getByRole('button', { name: /Plan v1/ })).toHaveAttribute('aria-expanded', 'false')
  await expect(former.locator('[data-plan-cycle-step]')).toHaveCount(0)
})

// The dashboard reads cycles, versions, outcomes, and people only from the
// summary, never from events or intervention times (invariant 71). This
// fixture is already in the bounded shape: its events page contradicts the
// summary on purpose, and steps name an omitted event and an intervention.
test('cycles render from the summary alone and absorb the events and interventions they name', async ({ page }) => {
  const id = 'plan-cycles-summary-only'
  const planCycles: TaskPlanCycle[] = [
    {
      opened_by: 'plan',
      contested_version: null,
      first_version: 5,
      final_version: 5,
      outcome: 'planning',
      steps: [
        {
          kind: 'plan_attached',
          at: day1('09:00'),
          event_id: '9007199254740993',
          plan_version: 5,
          user_id: 'usr_kidus',
        },
        {
          kind: 'plan_sent_back',
          at: day1('09:30'),
          event_id: '9007199254740995',
          plan_version: 5,
          user_id: 'usr_meron',
          intervention_id: 41,
          text: 'Plan the reader first.',
        },
      ],
    },
    {
      opened_by: 'plan',
      contested_version: null,
      first_version: 6,
      final_version: 6,
      outcome: 'approved',
      steps: [
        { kind: 'plan_drafted', at: day1('11:00'), event_id: '9007199254740997', plan_version: 6 },
        {
          kind: 'plan_approved',
          at: day1('11:30'),
          event_id: '9007199254740999',
          plan_version: 6,
          intervention_id: 42,
        },
      ],
    },
  ]
  const bounded = {
    ...fixture(id, 'running', [], []),
    interventions: [
      {
        id: 41,
        task_id: id,
        actor_id: 'user:usr_meron',
        actor_role: 'human',
        action: 'redirect',
        reason_code: 'changes-requested',
        comment: 'Plan the reader first.',
        at: day1('09:30'),
      },
      {
        id: 42,
        task_id: id,
        actor_id: 'user:usr_meron',
        actor_role: 'human',
        action: 'approve',
        reason_code: '',
        comment: '',
        at: day1('11:30'),
      },
      {
        id: 43,
        task_id: id,
        actor_id: 'user:usr_meron',
        actor_role: 'human',
        action: 'approve',
        reason_code: '',
        comment: 'Merge gate.',
        at: day1('16:00'),
      },
    ],
    events: [
      // Named by a step, with its payload omitted: absorbed, never a note.
      {
        id: '9007199254740995',
        task_id: id,
        kind: 'task.state_changed',
        actor_id: 'user:usr_meron',
        actor_role: 'human',
        payload: null,
        payload_omitted: true,
        payload_index: {},
        at: day1('09:30'),
      },
      // Contradicts the summary; the summary decides.
      {
        id: '9007199254741001',
        task_id: id,
        kind: 'work_order.plan_revision_requested',
        actor_id: 'agent',
        actor_role: 'agent',
        payload: { rationale: 'Not in any cycle.', plan_version: 9 },
        at: day1('12:00'),
      },
    ],
    event_page: { limit: 100, returned: 2, kinds: 'human', has_older: false },
    summaries: { run_implementation: false, plan_cycles: planCycles },
  }
  await mockServer(page, [bounded])
  await page.goto(`/w/demo/tasks/${id}/full`)
  const timeline = timelineOf(page)
  await expect(cycles(timeline)).toHaveCount(2)
  // A planning cycle never folds, even when it is not the newest.
  const planning = cycles(timeline).nth(0)
  await expect(planning.getByRole('article', { name: 'Plan v5' })).toBeVisible()
  await expect(planning.getByText('Planning', { exact: true })).toBeVisible()
  await expect(planning.getByRole('button', { name: /Plan v5/ })).toHaveCount(0)
  await expect(planning.locator('[data-plan-cycle-step]').nth(0)).toContainText('Plan v5 attached')
  await expect(planning.locator('[data-plan-cycle-step]').nth(1)).toContainText('Sent back for a new plan')
  await expect(cycles(timeline).nth(1).locator('[data-plan-cycle-step]').nth(1)).toContainText('Approved plan v6')
  // Absorbed by ID: the omitted event and both named interventions.
  await expect(timeline.getByText('Details omitted from this page')).toHaveCount(0)
  await expect(timeline.locator('li > div strong', { hasText: /^(Approved|Requested changes)$/ })).toHaveCount(1)
  await expect(timeline.getByText('Merge gate.')).toBeVisible()
  // An event no step names keeps rendering as today.
  await expect(timeline.getByText('Not in any cycle.')).toBeVisible()
})

test('earlier history pages absorb the events a cycle names', async ({ page }) => {
  const { item } = settledTask('plan-cycles-paged', 120)
  await mockServer(page, [item])
  await page.goto('/w/demo/tasks/plan-cycles-paged/full')
  const timeline = timelineOf(page)
  // The cycles fold from the whole ledger even though their events left the page.
  await expect(cycles(timeline)).toHaveCount(3)
  const earlier = timeline.getByRole('button', { name: 'Earlier history' })
  await earlier.click()
  const rows = timeline.getByRole('list', { name: 'Earlier history events' })
  await expect(rows.locator('li').first()).toBeVisible()
  await expect(rows.getByText(/asked to revise plan/)).toHaveCount(0)
  await expect(rows.getByText('Timed out before finishing').first()).toBeVisible()
})

// Visual record for the pull request: settled and waiting timelines at 375 px
// and at a desktop width. Each image is attached to the Playwright report,
// which CI uploads, and is also written to PLAN_CYCLE_SCREENSHOT_DIR when it is
// set, else to the test's output directory. No image is committed.
for (const width of [375, 1280]) {
  test(`plan cycle screenshots at ${width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 1400 })
    const { item } = settledTask()
    await mockServer(page, [item, waitingTask()])
    const directory = process.env.PLAN_CYCLE_SCREENSHOT_DIR
    const target = (name: string) => (directory ? `${directory}/${name}` : testInfo.outputPath(name))
    await page.goto('/w/demo/tasks/plan-cycles-settled/full')
    const settled = timelineOf(page)
    await expect(cycles(settled)).toHaveCount(3)
    await cycles(settled)
      .nth(1)
      .getByRole('button', { name: /Plan revision/ })
      .click()
    const settledName = `plan-cycles-settled-${width}.png`
    await settled.screenshot({ path: target(settledName) })
    await testInfo.attach(settledName, { path: target(settledName), contentType: 'image/png' })
    await page.goto('/w/demo/tasks/plan-cycles-waiting/full')
    const waiting = timelineOf(page)
    await expect(waiting.getByText('Waiting for your decision below')).toBeVisible()
    const waitingName = `plan-cycles-waiting-${width}.png`
    await page.screenshot({ path: target(waitingName), fullPage: true })
    await testInfo.attach(waitingName, { path: target(waitingName), contentType: 'image/png' })
  })
}
