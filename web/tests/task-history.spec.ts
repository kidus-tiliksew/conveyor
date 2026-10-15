import { expect, type Locator, type Page } from '@playwright/test'
import { humanTimelineEventKinds } from '../src/lib/activity'
import {
  boundedView,
  type CompleteActivity,
  type CompleteEvent,
  type GeneratedLedger,
  generateLedger,
  HistoryRefusal,
  historyPage,
  serverHumanTimelineEventKinds,
  test,
} from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { serveNextActionsFromActivity } from './helpers/next-action-stub'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'
import { activityFrame, controlTaskStreams, readyFrame } from './helpers/task-streams'

// Bounded task detail and history pages (component-web-task-surfaces, History
// pages, Live updates; component-http-api, Bounded detail view and Task history
// pages) over portable generated ledgers: 20,219 events of which 20,127 are
// `dispatch.reconciled`, and 100,000 events. Event IDs start above 2^53.

const ledgers = new Map<string, GeneratedLedger>()
function ledger(name: '20k' | '100k' | 'small' | 'triage' | 'other'): GeneratedLedger {
  const cached = ledgers.get(name)
  if (cached) return cached
  let generated: GeneratedLedger
  switch (name) {
    case '20k':
      generated = generateLedger({ taskId: 'ledger-20k', total: 20_219, reconciled: 20_127, human: 40 })
      break
    case '100k':
      generated = generateLedger({ taskId: 'ledger-100k', total: 100_000, reconciled: 99_000, human: 400 })
      break
    case 'small':
      generated = generateLedger({ taskId: 'ledger-small', total: 70, reconciled: 40, human: 8 })
      break
    case 'other':
      generated = generateLedger({
        taskId: 'ledger-other',
        total: 2_000,
        reconciled: 1_900,
        human: 20,
        title: 'Second long-running task',
      })
      break
    case 'triage':
      generated = failedTriageLedger()
      break
  }
  ledgers.set(name, generated)
  return generated
}

// A ledger whose latest triage job failed long before the newest events: the
// job.failed record is far outside every bounded page.
function failedTriageLedger(): GeneratedLedger {
  const generated = generateLedger({ taskId: 'ledger-triage', total: 20_219, reconciled: 20_127, human: 40 })
  const events = generated.item.events
  const triageJob = generated.item.jobs[0]
  events[3] = {
    ...events[3],
    kind: 'job.failed',
    job_id: triageJob.id,
    payload: { error: 'Responses API request failed: upstream service unavailable' },
  }
  // Nothing after the failure supersedes it.
  for (let index = 4; index <= 10; index++) {
    events[index] = { ...events[index], kind: 'dispatch.reconciled', job_id: undefined, payload: { pass: index } }
  }
  generated.item.jobs = [{ ...triageJob, state: 'failed', ended_at: events[3].at }]
  generated.item.work_orders = []
  generated.item.at_merge_gate = false
  generated.item.task = {
    ...generated.item.task,
    title: 'Triage failed long ago',
    state: 'awaiting_human',
    recovery_stage: 'triage',
    next_stage: undefined,
  }
  return generated
}

interface HistoryCall {
  taskId: string
  workspace: string | null
  selection: string | null
  cursor: string | null
  limit: string | null
}

type Surface = 'board sheet' | 'tasks panel' | 'full route'
const surfaces: Surface[] = ['board sheet', 'tasks panel', 'full route']

function surfaceURL(surface: Surface, taskId: string) {
  if (surface === 'board sheet') return `/tasks/${taskId}`
  if (surface === 'tasks panel') return `/tasks/${taskId}?view=list`
  return `/tasks/${taskId}/full`
}

function feedItem(item: CompleteActivity, workspace = 'demo') {
  return {
    task: { ...item.task, workspace },
    latest_stage: 'review',
    last_event_at: item.events.at(-1)?.at ?? item.task.created_at,
    needs_attention: item.needs_attention,
  }
}

// The fake server for one spec: the bounded view per task, history pages with
// opaque cursors, audit records, the workspace feed and Tasks list, and held
// task streams that never refresh unless a test opens them.
async function serveLedgers(page: Page, served: GeneratedLedger[]) {
  const calls: HistoryCall[] = []
  const detailCalls: string[] = []
  const auditCalls: string[] = []
  const failures: Array<{ status: number; body: unknown }> = []
  let gate: Promise<void> | undefined
  let release = () => {}
  const bounded = new Map(served.map((entry) => [entry.taskId, boundedView(entry.item)]))
  const streams = await controlTaskStreams(page)
  await serveNextActionsFromActivity(page)
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    // Records each history read the page aborts.
    const target = window as unknown as { __historyAborts: string[] }
    target.__historyAborts = []
    const nativeFetch = window.fetch.bind(window)
    window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      if (url.includes('/history')) init?.signal?.addEventListener('abort', () => target.__historyAborts.push(url))
      return nativeFetch(input, init)
    }
  })
  await page.route('**/v1/**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({
      json: [
        { id: 'demo', name: 'Demo' },
        { id: 'other', name: 'Other' },
      ],
    }),
  )
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }) }),
  )
  await page.route('**/v1/activity?**', (route) => {
    const workspace = new URL(route.request().url()).searchParams.get('workspace_id') ?? 'demo'
    return route.fulfill({ json: served.map((entry) => feedItem(entry.item, workspace)) })
  })
  await page.route('**/v1/task-operations?**', (route) => {
    const items = served.map((entry) => ({ ...feedItem(entry.item), plan: { state: 'approved', version: 1 } }))
    return route.fulfill({
      body: JSON.stringify(items),
      headers: {
        'content-type': 'application/json',
        'X-Conveyor-Total': String(items.length),
        'X-Conveyor-Limit': '25',
        'X-Conveyor-Offset': '0',
      },
    })
  })
  await page.route(/\/v1\/tasks\/[^/]+\/activity/, (route) => {
    const url = new URL(route.request().url())
    detailCalls.push(url.pathname + url.search)
    const taskId = decodeURIComponent(url.pathname.split('/')[3])
    const view = bounded.get(taskId)
    if (!view) return route.fulfill({ status: 404, json: { error: 'not_found', message: 'task not found' } })
    const workspace = url.searchParams.get('workspace_id') ?? 'demo'
    return route.fulfill({ json: { ...view, task: { ...view.task, workspace } } })
  })
  await page.route(/\/v1\/tasks\/[^/]+\/history/, async (route) => {
    const url = new URL(route.request().url())
    const taskId = decodeURIComponent(url.pathname.split('/')[3])
    calls.push({
      taskId,
      workspace: url.searchParams.get('workspace_id'),
      selection: url.searchParams.get('selection'),
      cursor: url.searchParams.get('cursor'),
      limit: url.searchParams.get('limit'),
    })
    if (gate) await gate
    const failure = failures.shift()
    try {
      if (failure) {
        await route.fulfill({ status: failure.status, json: failure.body })
        return
      }
      const events = served.find((entry) => entry.taskId === taskId)?.item.events ?? []
      try {
        await route.fulfill({
          json: historyPage(events, {
            taskId,
            selection: url.searchParams.get('selection'),
            cursor: url.searchParams.get('cursor'),
            limit: url.searchParams.get('limit'),
          }),
          headers: { 'Cache-Control': 'no-store' },
        })
      } catch (error) {
        if (!(error instanceof HistoryRefusal)) throw error
        await route.fulfill({ status: error.status, json: { error: error.code, message: error.code } })
      }
    } catch {
      // The page aborted the read; its late response has nowhere to go.
    }
  })
  await page.route(/\/v1\/tasks\/[^/]+\/audit\//, (route) => {
    const url = new URL(route.request().url())
    auditCalls.push(url.pathname)
    const [, , , taskId, , kind, recordId] = url.pathname.split('/')
    const events = served.find((entry) => entry.taskId === decodeURIComponent(taskId))?.item.events ?? []
    const event = kind === 'event' ? events.find((candidate) => String(candidate.id) === recordId) : undefined
    if (!event) return route.fulfill({ status: 404, json: { error: 'not_found', message: 'record not found' } })
    return route.fulfill({ json: { kind: 'event', event } })
  })
  return {
    streams,
    calls,
    detailCalls,
    auditCalls,
    hold() {
      gate = new Promise<void>((resolve) => {
        release = resolve
      })
    },
    release() {
      gate = undefined
      release()
    },
    failNext(status: number, body: unknown) {
      failures.push({ status, body })
    },
    aborts: () => page.evaluate(() => (window as unknown as { __historyAborts: string[] }).__historyAborts),
  }
}

function technicalRows(page: Page | Locator) {
  return page.getByRole('list', { name: 'Technical activity events' }).getByRole('listitem')
}

function technicalToggle(scope: Page | Locator) {
  return scope.getByRole('button', { name: 'Show technical activity' })
}

function pager(scope: Page | Locator, name: 'Technical activity history' | 'Earlier history pages') {
  return scope.getByRole('region', { name })
}

function detailScope(page: Page, surface: Surface): Locator {
  return surface === 'full route' ? page.locator('main') : page.getByRole('dialog', { name: 'Task detail' })
}

async function openDetail(page: Page, surface: Surface, generated: GeneratedLedger) {
  await page.goto(surfaceURL(surface, generated.taskId))
  const scope = detailScope(page, surface)
  await expect(scope.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
  await expect(scope.getByText(generated.item.task.title).first()).toBeVisible()
  return scope
}

// Every event row mounted anywhere on the page.
function mountedEventRows(page: Page) {
  return page.locator('[aria-label="Technical activity events"] > li, [aria-label="Earlier history events"] > li')
}

test('the dashboard human timeline kinds equal the server selection', () => {
  expect([...humanTimelineEventKinds].sort()).toEqual([...serverHumanTimelineEventKinds].sort())
})

for (const surface of surfaces) {
  test(`closed technical activity mounts no rows and reads no history on the ${surface}`, async ({ page }) => {
    const generated = ledger('100k')
    const server = await serveLedgers(page, [generated])
    const scope = await openDetail(page, surface, generated)
    expect(server.detailCalls.at(-1)).toContain('view=bounded')
    expect(server.detailCalls.at(-1)).toContain('workspace_id=demo')
    // The timeline always offers the closed disclosure.
    await expect(technicalToggle(scope)).toHaveAttribute('aria-expanded', 'false')
    await expect(mountedEventRows(page)).toHaveCount(0)
    await page.waitForTimeout(300)
    expect(server.calls).toEqual([])

    // Opening reads the newest technical page in the explicit workspace.
    await technicalToggle(scope).click()
    await expect(technicalRows(scope)).toHaveCount(50)
    expect(server.calls).toEqual([
      { taskId: generated.taskId, workspace: 'demo', selection: 'technical', cursor: null, limit: null },
    ])
    const newest = generated.item.events.at(-1) as CompleteEvent
    await expect(technicalRows(scope).last()).toContainText(`Event ${newest.id}`)
    await expect(mountedEventRows(page)).toHaveCount(50)
    expect(server.auditCalls).toEqual([])

    // Closing unmounts every row; reopening starts again at the newest page.
    await technicalToggle(scope).click()
    await expect(mountedEventRows(page)).toHaveCount(0)
  })
}

test('Older, Newer, and Newest replace the page and keep at most 50 rows', async ({ page }) => {
  const generated = ledger('100k')
  const server = await serveLedgers(page, [generated])
  const scope = await openDetail(page, 'full route', generated)
  await technicalToggle(scope).click()
  const history = pager(scope, 'Technical activity history')
  const newest = history.getByRole('button', { name: 'Newest' })
  const newer = history.getByRole('button', { name: 'Newer' })
  const older = history.getByRole('button', { name: 'Older' })
  await expect(technicalRows(scope)).toHaveCount(50)
  await expect(newest).toHaveAttribute('aria-disabled', 'true')
  await expect(newer).toHaveAttribute('aria-disabled', 'true')
  const ordered = generated.item.events
  const pageText = (index: number) => `Event ${ordered[ordered.length - 1 - index * 50].id}`

  for (let step = 1; step <= 3; step++) {
    await older.scrollIntoViewIfNeeded()
    const top = (await older.boundingBox())?.y
    await older.click()
    await expect(technicalRows(scope).last()).toContainText(pageText(step))
    await expect(mountedEventRows(page)).toHaveCount(50)
    await expect(older).toBeFocused()
    expect(Math.abs(((await older.boundingBox())?.y ?? 0) - (top ?? 0))).toBeLessThan(2)
  }
  expect(server.calls.slice(1).every((call) => call.cursor && call.selection === 'technical')).toBe(true)

  await newer.click()
  await expect(technicalRows(scope).last()).toContainText(pageText(2))
  await expect(newer).toBeFocused()
  await expect(mountedEventRows(page)).toHaveCount(50)

  await newest.click()
  await expect(technicalRows(scope).last()).toContainText(pageText(0))
  await expect(mountedEventRows(page)).toHaveCount(50)
  await expect(newest).toBeFocused()
  await expect(newest).toHaveAttribute('aria-disabled', 'true')
  expect(server.calls.at(-1)?.cursor).toBeNull()
})

test('history states: loading, empty, error with Retry, end of history, and restart', async ({ page }) => {
  const small = ledger('small')
  const empty: GeneratedLedger = {
    ...small,
    taskId: 'ledger-empty',
    item: { ...small.item, task: { ...small.item.task, id: 'ledger-empty', title: 'Empty ledger' }, events: [] },
  }
  const server = await serveLedgers(page, [small, empty])

  // Loading, then the newest page; the end of history after the oldest page.
  let scope = await openDetail(page, 'full route', small)
  server.hold()
  await technicalToggle(scope).click()
  await expect(pager(scope, 'Technical activity history').getByRole('status')).toHaveText('Loading activity…')
  server.release()
  await expect(technicalRows(scope)).toHaveCount(50)
  await expect(scope.getByText('End of history.')).toHaveCount(0)
  const older = pager(scope, 'Technical activity history').getByRole('button', { name: 'Older' })
  await older.click()
  await expect(technicalRows(scope)).toHaveCount(20)
  await expect(scope.getByText('End of history.')).toBeVisible()
  await expect(older).toHaveAttribute('aria-disabled', 'true')

  // A failed read shows its message and Retry reads the same page again.
  await pager(scope, 'Technical activity history').getByRole('button', { name: 'Newest' }).click()
  await expect(technicalRows(scope)).toHaveCount(50)
  server.failNext(500, { error: 'internal', message: 'history read failed' })
  await older.click()
  const alert = pager(scope, 'Technical activity history').getByRole('alert')
  await expect(alert).toContainText('Could not load activity: history read failed')
  await expect(technicalRows(scope)).toHaveCount(0)
  const failedCursor = server.calls.at(-1)?.cursor
  await alert.getByRole('button', { name: 'Retry' }).click()
  await expect(technicalRows(scope)).toHaveCount(20)
  expect(server.calls.at(-1)?.cursor).toBe(failedCursor)

  // A 409 history_restart offers a restart from the newest page.
  await pager(scope, 'Technical activity history').getByRole('button', { name: 'Newest' }).click()
  await expect(technicalRows(scope)).toHaveCount(50)
  server.failNext(409, { error: 'history_restart', message: 'task history changed; restart from the newest page' })
  await older.click()
  await expect(pager(scope, 'Technical activity history').getByRole('alert')).toContainText(
    'Activity changed while paging',
  )
  await pager(scope, 'Technical activity history').getByRole('button', { name: 'Restart from newest' }).click()
  await expect(technicalRows(scope)).toHaveCount(50)
  expect(server.calls.at(-1)?.cursor).toBeNull()

  // A task with no stored events says so.
  scope = await openDetail(page, 'full route', empty)
  await technicalToggle(scope).click()
  await expect(pager(scope, 'Technical activity history')).toContainText('No technical activity recorded.')
  await expect(technicalRows(scope)).toHaveCount(0)
})

test('closing, a task change, and a workspace change abort the read and drop the late response', async ({ page }) => {
  const first = ledger('100k')
  const second = ledger('other')
  const server = await serveLedgers(page, [first, second])

  // Close while the read is held.
  let scope = await openDetail(page, 'board sheet', first)
  server.hold()
  await technicalToggle(scope).click()
  await expect.poll(() => server.calls.length).toBe(1)
  await technicalToggle(scope).click()
  await expect.poll(() => server.aborts()).toHaveLength(1)
  server.release()
  await page.waitForTimeout(300)
  await expect(mountedEventRows(page)).toHaveCount(0)
  await expect(technicalToggle(scope)).toHaveAttribute('aria-expanded', 'false')

  // Change task inside the sheet while the read is held.
  server.hold()
  await technicalToggle(scope).click()
  await expect.poll(() => server.calls.length).toBe(2)
  const next = page.getByRole('button', { name: 'Next task' }).or(page.getByRole('link', { name: 'Next task' }))
  const previous = page
    .getByRole('button', { name: 'Previous task' })
    .or(page.getByRole('link', { name: 'Previous task' }))
  const neighbour = (await next.first().isEnabled()) ? next.first() : previous.first()
  await neighbour.click()
  await expect(scope.getByText(second.item.task.title).first()).toBeVisible()
  await expect.poll(() => server.aborts()).toHaveLength(2)
  server.release()
  await page.waitForTimeout(300)
  await expect(mountedEventRows(page)).toHaveCount(0)
  await expect(technicalToggle(scope)).toHaveAttribute('aria-expanded', 'false')

  // Switch workspace while the read is held.
  scope = await openDetail(page, 'full route', first)
  server.hold()
  await technicalToggle(scope).click()
  await expect.poll(() => server.calls.length).toBe(3)
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }).click()
  // The navigation above reloaded the page, which started a fresh abort log.
  await expect
    .poll(() => server.aborts())
    .toEqual([`/v1/tasks/${first.taskId}/history?selection=technical&workspace_id=demo`])
  server.release()
  await page.waitForTimeout(300)
  await expect(mountedEventRows(page)).toHaveCount(0)
  expect(server.calls.every((call) => call.workspace === 'demo')).toBe(true)
})

test('Earlier history pages the human selection and renders omitted payloads as notes', async ({ page }) => {
  const generated = ledger('100k')
  const view = boundedView(generated.item)
  expect(view.events.length).toBe(100)
  expect(view.event_page?.has_older).toBe(true)
  const server = await serveLedgers(page, [generated])
  const scope = await openDetail(page, 'full route', generated)

  // The newest page already holds one omitted-payload human event.
  const omittedNotes = scope.getByText('Details omitted from this page')
  await expect(omittedNotes).toHaveCount(1)
  const omitted = view.events.find((event) => event.payload_omitted)
  expect(omitted).toBeDefined()
  await omittedNotes.locator('..').getByText('Event payload', { exact: true }).click()
  await expect.poll(() => server.auditCalls).toEqual([`/v1/tasks/${generated.taskId}/audit/event/${omitted?.id}`])
  // A decimal ID above 2^53 reaches the audit read unchanged.
  expect(BigInt(omitted?.id ?? '0')).toBeGreaterThan(2n ** 53n)
  expect(String(Number(omitted?.id))).not.toBe(omitted?.id)
  await expect(scope.locator('pre').filter({ hasText: 'Dispatch failed with a long diagnostic' })).toBeVisible()

  // Earlier history ends the entries and pages from the view's cursor.
  const earlier = scope.getByRole('button', { name: 'Earlier history' })
  await expect(earlier).toHaveAttribute('aria-expanded', 'false')
  expect(server.calls).toEqual([])
  await earlier.click()
  const earlierRows = scope.getByRole('list', { name: 'Earlier history events' }).getByRole('listitem')
  await expect(earlierRows.first()).toBeVisible()
  expect(server.calls[0]).toEqual({
    taskId: generated.taskId,
    workspace: 'demo',
    selection: 'human',
    cursor: view.event_page?.older_cursor,
    limit: null,
  })
  expect(await mountedEventRows(page).count()).toBeLessThanOrEqual(50)
  const earlierPager = pager(scope, 'Earlier history pages')
  await expect(earlierPager.getByRole('button', { name: 'Newer' })).toHaveAttribute('aria-disabled', 'true')
  await expect(earlierPager.getByRole('button', { name: 'Newest' })).toHaveAttribute('aria-disabled', 'true')

  // Page back until the older omitted event and then the end of history.
  const olderButton = earlierPager.getByRole('button', { name: 'Older' })
  let sawOmitted = false
  for (let step = 0; step < 10; step++) {
    if ((await earlierPager.getByText('Details omitted from this page').count()) > 0) sawOmitted = true
    if ((await earlierPager.getByText('End of history.').count()) > 0) break
    const firstRow = (await earlierRows.first().textContent()) ?? ''
    await olderButton.click()
    await expect(earlierRows.first()).not.toHaveText(firstRow)
    expect(await mountedEventRows(page).count()).toBeLessThanOrEqual(50)
    await expect(olderButton).toBeFocused()
  }
  expect(sawOmitted).toBe(true)
  await expect(earlierPager.getByText('End of history.')).toBeVisible()
  // The oldest human page holds the pull request; the absorbed round stays hidden.
  await expect(earlierPager.getByText('Pull request opened')).toBeVisible()
  await expect(earlierPager.getByText(/Review panel:/)).toHaveCount(0)
  expect(server.calls.every((call) => call.selection === 'human' && call.workspace === 'demo')).toBe(true)
})

// The text the dashboard showed before the bounded view, derived from the
// complete ledger exactly as the previous client did.
function completeHistoryJobText(jobId: string, events: CompleteEvent[]) {
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.job_id === jobId && event.kind === 'job.summary' && typeof event.payload?.summary === 'string')
      return event.payload.summary
  }
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i]
    if (event.job_id !== jobId) continue
    const summary = event.payload?.summary
    if (
      (event.kind === 'triage.completed' || event.kind === 'review.completed') &&
      typeof summary === 'string' &&
      summary
    ) {
      const feedback =
        event.kind === 'review.completed' && typeof event.payload?.feedback === 'string'
          ? event.payload.feedback.trim()
          : ''
      return [summary, feedback ? `Reviewer feedback: ${feedback}` : undefined].filter(Boolean).join('\n\n')
    }
  }
  return undefined
}

function completeHistorySeatFeedback(orderId: string, events: CompleteEvent[]) {
  let feedback: string | undefined
  for (const event of events)
    if (event.kind === 'review.completed' && event.payload?.review_work_order_id === orderId)
      feedback = typeof event.payload.feedback === 'string' ? event.payload.feedback : ''
  return feedback
}

for (const surface of ['board sheet', 'full route'] as const) {
  test(`summaries keep the header, merge gate, review panel, and job text outside the page on the ${surface}`, async ({
    page,
  }) => {
    const generated = ledger('100k')
    const view = boundedView(generated.item)
    // None of the summary-bearing events is on the bounded page.
    const pageKinds = new Set(view.events.map((event) => event.kind))
    for (const kind of ['task.created', 'pull_request.opened', 'review.round_completed', 'job.summary'])
      expect(pageKinds.has(kind)).toBe(false)
    await serveLedgers(page, [generated])
    const scope = await openDetail(page, surface, generated)

    await expect(scope.getByText('User · usr_operator', { exact: true })).toBeVisible()
    await expect(scope.getByRole('link', { name: /acme\/widgets\/pull\/4242/ }).first()).toBeVisible()

    const gate = scope.getByRole('region', { name: 'What you are approving' })
    await expect(gate).toContainText('feedface')
    await expect(gate).toContainText('Factory review approved this')
    await expect(gate.getByRole('link', { name: /4242/ }).first()).toBeVisible()

    // Job and seat text equal what the complete-history fixture produced.
    for (const job of generated.item.jobs.filter((entry) => entry.stage !== 'review')) {
      const text = completeHistoryJobText(job.id, generated.item.events)
      expect(text).toBeTruthy()
      await expect(scope.getByText(text ?? '', { exact: true })).toBeVisible()
    }
    const order = generated.item.work_orders[0]
    const feedback = completeHistorySeatFeedback(order.id, generated.item.events)
    expect(feedback).toBeTruthy()
    await expect(scope.getByText(feedback ?? '', { exact: true })).toBeVisible()
    await expect(scope.getByText('Approved', { exact: true }).first()).toBeVisible()
  })
}

test('failed triage stays correct when its failure lies outside the page', async ({ page }) => {
  const generated = ledger('triage')
  const view = boundedView(generated.item)
  expect(view.events.some((event) => event.kind === 'job.failed')).toBe(false)
  expect(view.summaries?.triage_failure?.superseded).toBe(false)
  await serveLedgers(page, [generated])
  for (const surface of surfaces) {
    const scope = await openDetail(page, surface, generated)
    await expect(scope.getByText('Triage failed', { exact: true }).first()).toBeVisible()
    const recovery = scope.getByRole('region', { name: 'Triage recovery' })
    await expect(recovery).toContainText('Responses API request failed: upstream service unavailable')
    await expect(scope.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  }
})

test('the notify stream refreshes on ready, coalesces frames, reconnects, and never reads history', async ({
  page,
}) => {
  const generated = ledger('small')
  await installQueryClientProbe(page)
  const server = await serveLedgers(page, [generated])
  const counts = { feed: 0, nextAction: 0 }
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname
    if (path === '/v1/activity') counts.feed++
    if (path === `/v1/tasks/${generated.taskId}/next-action`) counts.nextAction++
  })
  await page.clock.install({ time: new Date('2026-07-15T12:30:00Z') })
  await openDetail(page, 'full route', generated)
  await expect.poll(() => server.streams.countFor(generated.taskId)).toBeGreaterThan(0)
  expect((await server.streams.urls()).at(-1)).toBe(
    `/v1/tasks/${generated.taskId}/events/stream?mode=notify&workspace_id=demo`,
  )
  await server.streams.open()
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)

  // Families the stream must never touch, and another workspace's inbox.
  const untouched = [
    ['next-actions', 'other', { band: null }],
    ['task-history', 'demo', generated.taskId, 'technical', ''],
    ['task-audit', 'demo', generated.taskId, 'event', '1'],
    ['verification-page', 'demo', generated.taskId, 'seeded-context', 'evidence', 0],
  ]
  const inbox = ['next-actions', 'demo', { band: null }]
  await withQueryClient(
    page,
    (client, keys) => {
      for (const key of keys) client.setQueryData(key, { seeded: true })
    },
    [...untouched, inbox],
  )
  const invalidated = () =>
    withQueryClient(page, (client, keys) => keys.map((key) => client.getQueryState(key)?.isInvalidated ?? null), [
      ...untouched,
      inbox,
    ])

  const details = server.detailCalls.length
  const feed = counts.feed
  const nextAction = counts.nextAction
  await server.streams.send(readyFrame)
  await page.clock.runFor(100)
  await server.streams.send(activityFrame)
  await server.streams.send(activityFrame)
  await page.clock.runFor(149)
  expect(server.detailCalls.length).toBe(details)
  await page.clock.runFor(1)
  await expect.poll(() => server.detailCalls.length).toBe(details + 1)
  await expect.poll(() => counts.feed).toBe(feed + 1)
  await expect.poll(() => counts.nextAction).toBeGreaterThan(nextAction)
  expect(await invalidated()).toEqual([false, false, false, false, true])
  await page.clock.runFor(5_000)
  expect(server.detailCalls.length).toBe(details + 1)

  // The activity frame resets the wait; the reconnect's ready frame refreshes.
  const connections = await server.streams.count()
  await server.streams.close()
  await page.clock.runFor(1_000)
  await expect.poll(() => server.streams.count()).toBe(connections + 1)
  await server.streams.open()
  await server.streams.send(readyFrame)
  await page.clock.runFor(250)
  await expect.poll(() => server.detailCalls.length).toBe(details + 2)
  expect(server.calls).toEqual([])
  expect(server.detailCalls.every((call) => call.includes('view=bounded') && call.includes('workspace_id=demo'))).toBe(
    true,
  )
})

// Timings and transfer sizes, measured around user actions with
// performance.now() over five runs after one warm-up.
for (const name of ['20k', '100k'] as const) {
  test(`detail opens within 2,000 ms and closes or goes back within 1,000 ms over ${name} events`, async ({ page }) => {
    test.setTimeout(120_000)
    const generated = ledger(name)
    const complete = JSON.stringify(generated.item)
    const bounded = JSON.stringify(boundedView(generated.item))
    const technical = JSON.stringify(historyPage(generated.item.events, { taskId: generated.taskId }))
    const human = JSON.stringify(historyPage(generated.item.events, { taskId: generated.taskId, selection: 'human' }))
    await serveLedgers(page, [generated])
    const ready: number[] = []
    const close: number[] = []
    const back: number[] = []
    for (let run = 0; run <= 5; run++) {
      await page.goto('/')
      await expect(page.getByRole('region', { name: 'Task board' })).toBeVisible()
      let start = performance.now()
      await page.getByText(generated.item.task.title).first().click()
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      await expect(sheet.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
      await expect(sheet.getByRole('region', { name: 'What you are approving' })).toBeVisible()
      const opened = performance.now() - start

      await sheet.getByRole('link', { name: 'Open full task page' }).click()
      await expect(page.locator('main').getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
      start = performance.now()
      await page.goBack()
      await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
      await expect(sheet.getByRole('region', { name: 'Execution event timeline' })).toBeVisible()
      const returned = performance.now() - start

      start = performance.now()
      await sheet.getByRole('button', { name: 'Close panel' }).click()
      await expect(page.getByRole('dialog', { name: 'Task detail' })).toHaveCount(0)
      const closed = performance.now() - start
      if (run === 0) continue
      ready.push(Math.round(opened))
      back.push(Math.round(returned))
      close.push(Math.round(closed))
    }
    console.log(
      `PERF: ${JSON.stringify({
        fixture_events: generated.item.events.length,
        reconciled: generated.item.events.filter((event) => event.kind === 'dispatch.reconciled').length,
        complete_activity_bytes: Buffer.byteLength(complete),
        bounded_view_bytes: Buffer.byteLength(bounded),
        first_technical_page_bytes: Buffer.byteLength(technical),
        first_human_page_bytes: Buffer.byteLength(human),
        detail_ready_ms: ready,
        back_ms: back,
        close_ms: close,
        runs: 5,
        warmups: 1,
        note: 'mocked /v1 API on the Vite dev server; browser-side timings only',
      })}`,
    )
    for (const value of ready) expect(value).toBeLessThanOrEqual(2_000)
    for (const value of [...back, ...close]) expect(value).toBeLessThanOrEqual(1_000)
    expect(Buffer.byteLength(bounded)).toBeLessThanOrEqual(Buffer.byteLength(complete) / 10)
  })
}
