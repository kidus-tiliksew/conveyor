import { expect, type Locator, type Page } from '@playwright/test'
import type { NextAction, WorkOrder } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

// The timeline names who ran each order, seat, and decision by the person's
// avatar on the rail, never by name text in a card. The name shows only in
// the avatar's label, which opens on hover, keyboard focus, and tap
// (req-operator-next-action REQ-7, AC-7.6; req-accounts-and-membership AC-4.3;
// task 261110-96f5ed). A past claimant comes from `summaries.job_results`,
// which the fake server folds from the complete ledger the way the server
// does (component-web-task-surfaces invariant 25).

const t0 = '2026-11-10T08:00:00Z'
const taskId = 'attribution-task'
const minutes = (n: number) => new Date(Date.parse(t0) + n * 60_000).toISOString()

// Three members: one with a display name, one with a display name and email,
// and one known only by email. usr_ghost is not on the roster.
const members = [
  {
    workspace_id: 'demo',
    user_id: 'usr_meron',
    display_name: 'Meron Tesfaye',
    email: 'meron@example.test',
    role: 'operator',
    created_at: t0,
  },
  {
    workspace_id: 'demo',
    user_id: 'usr_kidus',
    display_name: 'Kidus Alemu',
    email: 'kidus@example.test',
    role: 'maintainer',
    created_at: t0,
  },
  { workspace_id: 'demo', user_id: 'usr_selam', email: 'selam@example.test', role: 'executor', created_at: t0 },
]
const memberNames = ['Meron Tesfaye', 'Kidus Alemu', 'selam@example.test', 'usr_ghost']

function job(id: string, stage: string, state: string, start: number, end?: number) {
  return {
    id,
    task_id: taskId,
    stage,
    harness: 'external-mcp',
    model_tier: 'claude-opus-5-5',
    runner: 'external',
    confinement: 'none',
    tokens_in: 0,
    tokens_out: 0,
    state,
    started_at: minutes(start),
    ...(end === undefined ? {} : { ended_at: minutes(end) }),
  }
}

function order(id: string, extra: Partial<WorkOrder>): WorkOrder {
  return {
    id,
    task_id: taskId,
    job_id: id,
    stage: 'implement',
    state: 'claimed',
    claimable: false,
    queue_entered_at: minutes(1),
    created_at: minutes(1),
    updated_at: minutes(2),
    agent: 'claude-code',
    model: 'claude-opus-5-5',
    ...extra,
  } as WorkOrder
}

let eventId = 0
function event(kind: string, at: number, payload: Record<string, unknown>, jobId?: string) {
  eventId += 1
  return {
    id: eventId,
    task_id: taskId,
    ...(jobId ? { job_id: jobId } : {}),
    kind,
    actor_id: 'system',
    actor_role: 'system',
    payload,
    at: minutes(at),
  }
}

const claim = (jobId: string, stage: string, at: number, claimant: string) =>
  event('work_order.claimed', at, { id: jobId, stage, state: 'claimed', claimed_by: claimant }, jobId)
const summary = (jobId: string, at: number, text: string) => event('job.summary', at, { summary: text }, jobId)

const plan = `${taskId}-spec-1`
const attempt1 = `${taskId}-implement-1`
const attempt2 = `${taskId}-implement-2`
const attempt3 = `${taskId}-implement-3`
const seat1 = `${taskId}-review-1-seat-1`
const seat2 = `${taskId}-review-1-seat-2`

// A plan by Meron, Kidus's plan approval, implementation attempt 1 by Meron
// that failed, attempt 2 by Selam that was submitted, a two-seat review round
// whose seat 1 Kidus claimed and whose seat 2 no agent ran, and a running
// attempt 3 by a claimant missing from the roster.
const item = {
  task: {
    id: taskId,
    workspace: 'demo',
    title: 'Show who ran each order',
    state: 'running',
    repo: 'conveyor',
    branch: `conveyor/${taskId}`,
    created_at: t0,
  },
  jobs: [
    job(plan, 'spec', 'done', 1, 4),
    job(attempt1, 'implement', 'failed', 6, 12),
    job(attempt2, 'implement', 'done', 14, 30),
    job(seat1, 'review', 'running', 32),
    job(attempt3, 'implement', 'running', 50),
  ],
  events: [
    claim(plan, 'spec', 1, 'run:usr_meron'),
    summary(plan, 4, 'Planned the avatar rail in four steps.'),
    claim(attempt1, 'implement', 6, 'run:usr_meron'),
    event('job.failed', 12, { error: 'The build failed.' }, attempt1),
    // A release between attempts clears the order's claimant; the ledger
    // still names attempt 1's person.
    event('work_order.released', 13, { id: attempt1, state: 'queued' }, attempt1),
    claim(attempt2, 'implement', 14, 'run:usr_selam'),
    summary(attempt2, 30, 'Drew the claimant avatar on the rail.'),
    claim(seat1, 'review', 32, 'run:usr_kidus'),
    // A note with no person.
    event('job.timeout', 48, { stage: 'review', timeout: '1h' }),
    claim(attempt3, 'implement', 50, 'run:usr_ghost'),
  ],
  work_orders: [
    order(seat1, {
      stage: 'review',
      state: 'submitted',
      queue_entered_at: minutes(31),
      created_at: minutes(31),
      review_round: 1,
      review_seat: 1,
      model: 'gpt-6',
      agent: 'codex',
    }),
    order(seat2, {
      stage: 'review',
      state: 'queued',
      claimable: true,
      queue_entered_at: minutes(31),
      created_at: minutes(31),
      review_round: 1,
      review_seat: 2,
    }),
    order(attempt3, {
      claimed_by: 'run:usr_ghost',
      attempt_id: 'attempt-3',
      session_id: 'sess-3',
      execution_started_at: minutes(50),
    }),
  ],
  reviews: [],
  interventions: [
    {
      id: 1,
      task_id: taskId,
      actor_id: 'user:usr_kidus',
      actor_role: 'human',
      action: 'approve',
      reason_code: 'plan-approved',
      comment: '',
      at: minutes(5),
    },
  ],
  latest_stage: 'implement',
  plan: { state: 'approved', version: 1 },
  last_event_at: minutes(50),
  needs_attention: false,
}

const action: NextAction = {
  label: 'Working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'An agent is implementing this task.',
  deadline: null,
  choices: [],
  details: {},
  current_order: {
    id: attempt3,
    stage: 'implement',
    state: 'claimed',
    queue_deadline: null,
    execution_deadline: null,
    lease_expires_at: minutes(110),
  },
  attempt: { harness: 'claude-code', model: 'claude-opus-5-5', user: 'usr_ghost', claimed_by: 'run:usr_ghost' },
}

async function mockAPI(page: Page, roster: object[] = members) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_meron', email: 'meron@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/workspaces/demo/members') return route.fulfill({ json: roster })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/next-action`) return route.fulfill({ json: action })
    if (path === `/v1/tasks/${taskId}/flow`) return route.fulfill({ status: 500, json: {} })
    if (path === `/v1/tasks/${taskId}/verification`)
      return route.fulfill({
        json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
      })
    if (path === `/v1/tasks/${taskId}/activity`) return route.fulfill({ json: item })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/activity')
      return route.fulfill({
        json: [{ ...item, next_action: { ...action, choices: undefined } }],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    return route.fulfill({ json: [] })
  })
}

async function openTimeline(page: Page) {
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByRole('article', { name: 'Implementation attempt 3' })).toBeVisible()
  return timeline
}

// The rail slot of the timeline entry that holds `card`.
const railOf = (card: Locator) => card.locator('xpath=ancestor::li[1]').locator(':scope > [data-rail-people]')

async function expectLabel(page: Page, avatar: Locator, label: string) {
  await avatar.hover()
  await expect(page.getByRole('tooltip').filter({ hasText: label })).toBeVisible()
  await page.mouse.move(0, 0)
  await avatar.focus()
  await expect(page.getByRole('tooltip').filter({ hasText: label })).toBeVisible()
  // The open label can change the text a locator matched, so blur through the
  // document.
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
}

test('each entry shows its claimant avatar with the name only in the label', async ({ page }) => {
  await mockAPI(page)
  const timeline = await openTimeline(page)

  // The plan job and the approval.
  const planCard = timeline.locator('li article').filter({ hasText: 'Planned the avatar rail in four steps.' })
  const planAvatar = railOf(planCard).getByRole('button', { name: 'Meron Tesfaye', exact: true })
  await expect(planAvatar).toBeVisible()
  await expectLabel(page, planAvatar, 'Meron Tesfaye · ran this plan')
  const approval = timeline.locator('li').filter({ hasText: 'Plan approved' })
  const approver = approval.locator(':scope > [data-rail-people]').getByRole('button', { name: 'Kidus Alemu' })
  await expect(approver).toBeVisible()
  await expectLabel(page, approver, 'Kidus Alemu · decided')
  // The intervention drops the generic person icon when an avatar shows.
  await expect(approval.locator('svg.lucide-user-round')).toHaveCount(0)

  // Attempt 1 failed under Meron after a release; attempt 2 was Selam's.
  const first = timeline.getByRole('article', { name: 'Implementation attempt 1' })
  await expect(first.getByText('Failed', { exact: true })).toBeVisible()
  const firstAvatar = railOf(first).getByRole('button', { name: 'Meron Tesfaye' })
  await expectLabel(page, firstAvatar, 'Meron Tesfaye · ran this implementation')
  const second = timeline.getByRole('article', { name: 'Implementation attempt 2' })
  await expect(second.getByText('Submitted', { exact: true })).toBeVisible()
  // Selam has no display name: the roster's email names her.
  const secondAvatar = railOf(second).getByRole('button', { name: 'selam@example.test' })
  await expectLabel(page, secondAvatar, 'selam@example.test · ran this implementation')
  await expect(secondAvatar).not.toHaveAttribute('data-live')

  // The running attempt's claimant is not on the roster: the user ID names
  // them, and the avatar is live.
  const third = timeline.getByRole('article', { name: 'Implementation attempt 3' })
  await expect(third.getByText('Running', { exact: true })).toBeVisible()
  const live = railOf(third).getByRole('button', { name: 'usr_ghost' })
  await expect(live).toHaveAttribute('data-live', '')
  await expectLabel(page, live, 'usr_ghost · implementing now')

  // The review panel: seat 1's person on the rail and in the seat row, and
  // no avatar for the seat no agent ran.
  const panel = timeline.locator('li').filter({ has: page.getByText('Panel of 2') })
  const reviewer = panel.locator(':scope > [data-rail-people]').getByRole('button', { name: 'Kidus Alemu' })
  await expect(reviewer).toHaveAttribute('data-live', '')
  await expectLabel(page, reviewer, 'Kidus Alemu · reviewing now')
  await expect(panel.locator(':scope > [data-rail-people] [data-person-avatar]')).toHaveCount(1)
  const seatOne = panel.getByText('Seat 1', { exact: true }).locator('..')
  const seatAvatar = seatOne.getByRole('button', { name: 'Kidus Alemu' })
  await expect(seatAvatar).toBeVisible()
  await expectLabel(page, seatAvatar, 'Kidus Alemu · seat 1')
  const seatTwo = panel.getByText('Seat 2', { exact: true }).locator('..')
  await expect(seatTwo.locator('[data-person-avatar]')).toHaveCount(0)

  // The Working banner shows the current order's live claimant before the
  // server's summary, which it prints unchanged.
  const banner = page.locator('[data-passive-banner]')
  await expect(banner.getByRole('heading', { name: 'An agent is implementing this task.' })).toBeVisible()
  const bannerAvatar = banner.getByRole('button', { name: 'usr_ghost' })
  await expect(bannerAvatar).toHaveAttribute('data-live', '')
  await expectLabel(page, bannerAvatar, 'usr_ghost · working on this order')

  // No card prints a member's name as text while every label is closed.
  for (const name of memberNames) await expect(timeline.getByText(name)).toHaveCount(0)
})

test('an avatar label opens on tap', async ({ browser }) => {
  const context = await browser.newContext({ hasTouch: true, viewport: { width: 375, height: 812 } })
  const page = await context.newPage()
  const { installBoundedDetail } = await import('./helpers/bounded-detail')
  await installBoundedDetail(page)
  await mockAPI(page)
  const timeline = await openTimeline(page)
  const second = timeline.getByRole('article', { name: 'Implementation attempt 2' })
  // At 375 px a seat row with its 16 px avatar keeps the label, avatar,
  // model, and state on one line.
  const panel = timeline.locator('li').filter({ has: page.getByText('Panel of 2') })
  const row = panel.getByText('Seat 1', { exact: true }).locator('..')
  // Vertical centres: a touch target can be taller than what it draws.
  const top = async (locator: Locator) => {
    const box = await locator.boundingBox()
    return box ? box.y + box.height / 2 : Number.NaN
  }
  const labelTop = await top(row.getByText('Seat 1', { exact: true }))
  for (const part of [
    row.getByRole('button', { name: 'Kidus Alemu' }),
    row.getByTitle('gpt-6', { exact: true }).first(),
    row.getByText('Deliberating'),
  ])
    expect(Math.abs((await top(part)) - labelTop)).toBeLessThanOrEqual(4)
  await railOf(second).getByRole('button', { name: 'selam@example.test' }).tap()
  await expect(
    page.getByRole('tooltip').filter({ hasText: 'selam@example.test · ran this implementation' }),
  ).toBeVisible()
  await context.close()
})

test('an entry with no person keeps its dot', async ({ page }) => {
  await mockAPI(page)
  const timeline = await openTimeline(page)
  const note = timeline.locator('li').filter({ hasText: 'Timed out before finishing' })
  await expect(note).toHaveCount(1)
  await expect(note.locator(':scope > span.rounded-full')).toHaveCount(1)
  await expect(note.locator('[data-person-avatar]')).toHaveCount(0)
  // An entry with a person shows the avatar in place of the dot.
  const second = timeline.getByRole('article', { name: 'Implementation attempt 2' }).locator('xpath=ancestor::li[1]')
  await expect(second.locator(':scope > span.rounded-full')).toHaveCount(0)
  await expect(second.locator(':scope > [data-rail-people]')).toHaveCount(1)
})

test('a failed roster read names each person by user ID and keeps every avatar', async ({ page }) => {
  await mockAPI(page)
  await page.route('**/v1/workspaces/demo/members', (route) => route.fulfill({ status: 500, json: {} }))
  const timeline = await openTimeline(page)
  const first = timeline.getByRole('article', { name: 'Implementation attempt 1' })
  await expect(railOf(first).getByRole('button', { name: 'usr_meron' })).toBeVisible()
  const second = timeline.getByRole('article', { name: 'Implementation attempt 2' })
  await expect(railOf(second).getByRole('button', { name: 'usr_selam' })).toBeVisible()
})
