import { expect, type Locator, type Page, type TestInfo } from '@playwright/test'
import type { NextAction, WorkOrder } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

// Task detail leads with the server's next action and acts only through the
// server's choices (feature-operator-next-action NA-6, NA-4; AC-7.1, AC-1.7).
// Every next action here is a server-shaped projection; the assertions check
// that the card, the human gate and the recovery card render what the server
// stated and derive nothing from events.

const timestamp = '2026-10-12T10:00:00Z'
const taskId = 'card-task'

function activity(state: string, nextAction: NextAction, workOrders: WorkOrder[] = []) {
  return {
    task: {
      id: taskId,
      workspace: 'demo',
      title: 'Card task',
      state,
      repo: 'conveyor',
      branch: 'conveyor/card-task',
      created_at: timestamp,
    },
    jobs: [],
    // An event that once implied a different gate: the client must not read it.
    events: [{ id: '9007199254741001', task_id: taskId, kind: 'job.timeout', at: timestamp, payload: {} }],
    work_orders: workOrders,
    reviews: [],
    interventions: [],
    latest_stage: 'implement',
    last_event_at: timestamp,
    needs_attention: nextAction.actor === 'operator',
    at_merge_gate: false,
    checkout_available: false,
    checkout_guidance: '',
    next_action: {
      label: nextAction.label,
      actor: nextAction.actor,
      kind: nextAction.kind,
      judgment: nextAction.judgment,
      summary: nextAction.summary,
      deadline: nextAction.deadline,
    },
  }
}

const gateAction: NextAction = {
  label: 'Approve the plan',
  actor: 'operator',
  kind: 'plan_approval',
  judgment: true,
  summary: 'The plan for Card task waits for your approval.',
  question: 'Does this plan do what the task asks?',
  if_ignored: 'Implementation never starts.',
  deadline: '2026-10-13T10:00:00Z',
  choices: [
    {
      id: 'approve',
      label: 'Approve plan',
      effect: 'Accepts the gate.',
      method: 'PUT',
      path: `/v1/tasks/${taskId}/review?via=next-action`,
      capability: 'operate_gates',
      allowed: true,
    },
    {
      id: 'reject',
      label: 'Reject plan',
      effect: 'Closes the task.',
      method: 'POST',
      path: `/v1/tasks/${taskId}/review`,
      capability: 'operate_gates',
      allowed: false,
    },
    {
      id: 'teleport',
      label: 'Teleport',
      effect: 'A choice this client cannot build.',
      method: 'POST',
      path: `/v1/tasks/${taskId}/teleport`,
      capability: 'operate_gates',
      allowed: true,
    },
  ],
  current_order: {
    id: 'order-spec-7',
    stage: 'spec',
    state: 'queued',
    queue_deadline: null,
    execution_deadline: null,
    lease_expires_at: null,
  },
  details: { gate_kind: 'plan_approval', reason_code: 'gate.plan' },
}

async function mockAPI(page: Page, detail: () => object, action: () => NextAction, role = 'operator') {
  const posts: { method: string; path: string; body: unknown }[] = []
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/next-action`) return route.fulfill({ json: action() })
    if (path === `/v1/tasks/${taskId}/activity`) return route.fulfill({ json: detail() })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/activity')
      return route.fulfill({
        json: [detail()],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '50', 'X-Conveyor-Offset': '0' },
      })
    if (request.method() !== 'GET') {
      // The workspace scope is the client's own query parameter; everything
      // else must be the server's method and path verbatim.
      url.searchParams.delete('workspace_id')
      posts.push({ method: request.method(), path: `${path}${url.search}`, body: request.postDataJSON() })
      return route.fulfill({ json: {} })
    }
    return route.fulfill({ json: [] })
  })
  return posts
}

for (const surface of [`/w/demo/tasks/${taskId}`, `/w/demo/tasks/${taskId}/full`]) {
  test(`next-action card on ${surface} states the server's action and acts through its choices`, async ({ page }) => {
    const posts = await mockAPI(
      page,
      () => activity('awaiting_human', gateAction),
      () => gateAction,
    )
    await page.goto(surface)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByText('The plan for Card task waits for your approval.')).toBeVisible()
    await expect(card.getByText('Approve the plan')).toBeVisible()
    await expect(card.locator('[data-actor]')).toHaveText('operator')
    // One decision has one surface per view: on the Timeline the card is a
    // one-line pointer, and the gate states everything else beside its choices.
    const gate = page.getByRole('region', { name: 'Human gate' })
    await expect(card.locator('[data-go-to-gate]')).toBeVisible()
    await expect(gate.getByText('Does this plan do what the task asks?')).toBeVisible()
    await expect(gate.getByText('If ignored: Implementation never starts.')).toBeVisible()
    await expect(gate.locator('[data-next-action-deadline] time')).toHaveAttribute('datetime', '2026-10-13T10:00:00Z')
    await expect(card.getByText('Does this plan do what the task asks?')).toHaveCount(0)

    // Only allowed choices this client can build render, and the label is the
    // server's. With the gate open they render once, beside its evidence, and
    // the card does not repeat them.
    await expect(page.locator('[data-next-action-choices]')).toHaveCount(1)
    await expect(card.locator('[data-next-action-choices]')).toHaveCount(0)
    const choices = page
      .getByRole('region', { name: 'Human gate' })
      .locator('[data-next-action-choices]')
      .getByRole('button')
    // A disallowed choice and one this page cannot send render disabled.
    await expect(choices).toHaveText(['Approve plan', 'Reject plan', 'Teleport'])
    await expect(choices.nth(1)).toBeDisabled()
    await expect(choices.nth(2)).toBeDisabled()

    // Show IDs discloses the server's identifiers as a labelled list, never
    // as a JSON object (req-operator-next-action AC-1.8).
    const ids = gate.locator('details')
    await expect(ids.getByText('order-spec-7')).toBeHidden()
    await ids.getByText('Show IDs').click()
    await expect(ids.locator('dt')).toHaveText(['Order ID', 'Stage', 'Gate kind', 'Reason code'])
    await expect(ids.locator('dd')).toHaveText(['order-spec-7', 'spec', 'plan_approval', 'gate.plan'])
    await expect(ids).not.toContainText('{')
    await expect(ids).not.toContainText('"')

    await choices.getByText('Approve plan').click()
    await expect.poll(() => posts.length).toBe(1)
    expect(posts[0].method).toBe('PUT')
    expect(posts[0].path).toBe(`/v1/tasks/${taskId}/review?via=next-action`)
    expect(posts[0].body).toMatchObject({ action: 'approve' })
  })
}

// Nested next-action details print as text under their own labels: a list
// joins its items and an object lists its fields, so no braces or quotes reach
// the disclosure (req-operator-next-action AC-1.8, AC-7.1).
test('Show IDs flattens nested next-action details into labelled text', async ({ page }) => {
  const nested: NextAction = {
    ...gateAction,
    details: {
      ...gateAction.details,
      order_ids: ['order-spec-6', 'order-spec-7'],
      pending_proposals: [
        { tier: 'requirement', id: 'req-cards', version: 3 },
        { tier: 'system_design', id: 'component-cards', version: 8 },
      ],
    },
  }
  await mockAPI(
    page,
    () => activity('awaiting_human', nested),
    () => nested,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const ids = page.getByRole('region', { name: 'Human gate' }).locator('details[data-next-action-ids]')
  await ids.getByText('Show IDs').click()
  await expect(ids.locator('[data-next-action-id="Order IDs"] dd')).toHaveText('order-spec-6, order-spec-7')
  await expect(ids.locator('[data-next-action-id="Pending proposals"] dd')).toHaveText(
    'Tier: requirement, ID: req-cards, Version: 3; Tier: system_design, ID: component-cards, Version: 8',
  )
  await expect(ids.locator('pre')).toHaveCount(0)
  await expect(ids).not.toContainText('{')
  await expect(ids).not.toContainText('"')
})

// A running agent attempt reads as one banner on the Timeline: the server's
// label as a status chip, its summary as the one heading, the current order's
// model logo and elapsed time, and one "More actions" menu for the rare acts.
// It prints no deadline (DEC-61; DEC-129; req-operator-next-action AC-1.7,
// AC-1.8, AC-7.1; component-next-action invariant 33;
// component-web-task-surfaces invariant 32).
const runningOrder = {
  id: 'order-impl-running',
  task_id: taskId,
  job_id: 'job-impl-running',
  stage: 'implement',
  state: 'claimed',
  claimable: false,
  attempt_id: 'attempt-running',
  claimed_by: 'run:operator',
  agent: 'claude-code',
  model: 'claude-opus-5-5',
  created_at: timestamp,
  queue_entered_at: '2026-10-12T09:30:00Z',
  execution_started_at: timestamp,
  execution_deadline: '2026-10-12T14:00:00Z',
  updated_at: timestamp,
  // The server records no activity snapshot (DEC-97(1)); a stale response
  // that still carries one renders nothing from it.
  activity_snapshot: {
    attempt_id: 'attempt-running',
    content: 'running focused tests\nall green so far',
    captured_at: timestamp,
  },
} as unknown as WorkOrder

const workingAction: NextAction = {
  label: 'Working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'An agent is implementing this task.',
  if_ignored: 'The attempt continues.',
  deadline: '2026-10-12T14:00:00Z',
  choices: [],
  current_order: {
    id: 'order-impl-running',
    stage: 'implement',
    state: 'claimed',
    queue_deadline: null,
    execution_deadline: '2026-10-12T14:00:00Z',
    lease_expires_at: '2026-10-12T10:05:00Z',
  },
  details: {},
}

// A queued order that an earlier attempt claimed still carries that attempt's
// model; the queued banner must not print it.
const queuedOrder = {
  ...runningOrder,
  id: 'order-impl-queued',
  state: 'queued',
  claimable: true,
  execution_started_at: undefined,
  execution_deadline: undefined,
  activity_snapshot: undefined,
} as unknown as WorkOrder

const queuedAction: NextAction = {
  label: 'Queued',
  actor: 'agent',
  kind: 'queued',
  judgment: false,
  summary: 'The implementation order is waiting for an agent.',
  if_ignored: 'It stays in the queue.',
  deadline: '2026-10-13T09:30:00Z',
  choices: [],
  current_order: {
    id: 'order-impl-queued',
    stage: 'implement',
    state: 'queued',
    queue_deadline: '2026-10-13T09:30:00Z',
    execution_deadline: null,
    lease_expires_at: null,
  },
  details: {},
}

// One hour into the four-hour execution span.
const oneHourIn = new Date('2026-10-12T11:00:00Z')

const surfaces = [
  { name: 'Board sheet', path: `/w/demo/tasks/${taskId}`, detail: 'dialog', label: 'Task detail' },
  { name: 'full route', path: `/w/demo/tasks/${taskId}/full`, detail: 'region', label: 'Task content' },
] as const

async function verticalCenter(locator: Locator) {
  const box = await locator.boundingBox()
  if (!box) throw new Error('element has no box')
  return box.y + box.height / 2
}

async function noSidewaysScroll(card: Locator) {
  return card.evaluate((element) => ({
    card: element.scrollWidth > element.clientWidth,
    page: document.documentElement.scrollWidth > document.documentElement.clientWidth,
  }))
}

for (const surface of surfaces) {
  for (const width of [375, 1280]) {
    test(`a running attempt on the ${surface.name} renders one banner row at ${width} px`, async ({ page }) => {
      await page.clock.setFixedTime(oneHourIn)
      await page.setViewportSize({ width, height: 900 })
      await mockAPI(
        page,
        () => activity('running', workingAction, [runningOrder]),
        () => workingAction,
      )
      await page.goto(surface.path)
      const card = page.getByRole('region', { name: 'Next action' })
      await expect(card).toHaveCount(1)

      // The label is a chip carrying the actor; its accessible description is
      // the if-ignored sentence, and the working dot pulses.
      const chip = card.getByRole('button', { name: 'Working, agent' })
      await expect(chip).toHaveAttribute('data-actor', 'agent')
      await expect(chip).toHaveAccessibleDescription('If ignored: The attempt continues.')
      await expect(chip.locator('[data-pulse]')).toHaveCount(1)

      // The summary is the one heading, printed once on task detail.
      const heading = card.getByRole('heading', { name: 'An agent is implementing this task.' })
      await expect(heading).toBeVisible()
      const detail = page.getByRole(surface.detail, { name: surface.label })
      await expect(detail.getByText('An agent is implementing this task.', { exact: true })).toHaveCount(1)

      // The current order's model as its logo, named by the model, and the
      // elapsed time. The model text and the deadline do not render (DEC-129).
      const meta = card.locator('[data-banner-meta]')
      const model = meta.locator('[data-banner-model]')
      await expect(model).toHaveAccessibleName('claude-opus-5-5')
      await expect(model.locator('svg')).toHaveCount(1)
      await expect(card.getByText('claude-opus-5-5')).toHaveCount(0)
      await expect(meta.locator('time')).toHaveText('started 1h ago')
      // At 375 px the meta has its own line, so the logo shows.
      if (width < 640) await expect(model).toBeInViewport()
      await expect(meta.locator('time')).toHaveAttribute('datetime', '2026-10-12T10:00:00.000Z')
      await expect(card.locator('[data-next-action-deadline]')).toHaveCount(0)
      await expect(card.getByText(/\bleft\b|Deadline passed/)).toHaveCount(0)
      await expect(card).toHaveAttribute('data-tone', 'neutral')

      // The stage line, the visible if-ignored line, the inline stop button,
      // and the IDs disclosure are gone from the row.
      await expect(card.getByText('Implementation · Progressing', { exact: true })).toHaveCount(0)
      await expect(card.locator('[data-execution-status]')).toHaveCount(0)
      await expect(card.getByText(/^Deadline /)).toHaveCount(0)
      await expect(page.getByRole('button', { name: /Stop attempt/ })).toHaveCount(0)
      await expect(card.locator('details[data-next-action-ids]')).toHaveCount(0)
      await expect(card.getByRole('button', { name: 'More actions' })).toHaveCount(1)
      await expect(page.getByRole('region', { name: 'Client machine activity' })).toHaveCount(0)
      await expect(page.getByText('running focused tests')).toHaveCount(0)

      // The execution state adds nothing the summary already says.
      await expect(page.getByText('Implementation is in progress')).toHaveCount(0)
      await expect(page.locator('#current-execution-title')).toHaveCount(0)
      for (const label of ['Current blocker', 'Retry', 'What to do next']) {
        await expect(page.locator('dt').getByText(label, { exact: true })).toHaveCount(0)
      }

      // From 640 px the chip, summary, meta, and menu share one row. Below
      // 640 px the summary and menu take the first line, and the chip and the
      // meta take the second (DEC-129(3)).
      const menu = card.getByRole('button', { name: 'More actions' })
      const row = await verticalCenter(chip)
      const headingBox = await heading.boundingBox()
      const metaBox = await meta.boundingBox()
      const chipBox = await chip.boundingBox()
      if (!headingBox || !metaBox || !chipBox) throw new Error('banner part has no box')
      if (width < 640) {
        expect(chipBox.y).toBeGreaterThanOrEqual(headingBox.y + headingBox.height - 1)
        expect(Math.abs((await verticalCenter(meta)) - row)).toBeLessThanOrEqual(4)
        expect(metaBox.x).toBeGreaterThan(chipBox.x + chipBox.width)
        expect(Math.abs((await verticalCenter(menu)) - (await verticalCenter(heading)))).toBeLessThanOrEqual(4)
      } else {
        for (const part of [meta, menu]) expect(Math.abs((await verticalCenter(part)) - row)).toBeLessThanOrEqual(4)
        expect(Math.abs(headingBox.y - chipBox.y)).toBeLessThanOrEqual(4)
        expect(metaBox.x).toBeGreaterThan(headingBox.x)
        const cardBox = await card.boundingBox()
        expect(cardBox && cardBox.height).toBeLessThanOrEqual(48)
      }

      // The banner sits directly above the Activity heading.
      const activityHeading = page.getByRole('heading', { name: 'Activity', exact: true })
      const cardBox = await card.boundingBox()
      const activityBox = await activityHeading.boundingBox()
      expect(cardBox && activityBox && cardBox.y + cardBox.height <= activityBox.y).toBe(true)

      expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })
    })
  }
}

for (const width of [375, 1280]) {
  test(`a queued order renders the same row with its queue age at ${width} px`, async ({ page }) => {
    await page.clock.setFixedTime(oneHourIn)
    await page.setViewportSize({ width, height: 900 })
    await mockAPI(
      page,
      () => activity('queued', queuedAction, [queuedOrder]),
      () => queuedAction,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    const chip = card.getByRole('button', { name: 'Queued, agent' })
    await expect(chip).toHaveAccessibleDescription('If ignored: It stays in the queue.')
    await expect(chip.locator('[data-pulse]')).toHaveCount(0)
    await expect(card.getByRole('heading', { name: 'The implementation order is waiting for an agent.' })).toBeVisible()
    // The queue age only: the earlier attempt's model is not this order's.
    await expect(card.locator('[data-banner-meta]')).toHaveText('queued 1h 30m ago')
    await expect(card.getByText('claude-opus-5-5')).toHaveCount(0)
    await expect(card.locator('[data-next-action-deadline]')).toHaveCount(0)
    await expect(card.getByText(/\bleft\b|Deadline passed/)).toHaveCount(0)
    await expect(card.locator('[data-deadline-progress]')).toHaveCount(0)
    await expect(card).toHaveAttribute('data-tone', 'neutral')
    expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })
  })
}

// On a 375 px touch screen the banner keeps its visual sizes: the chip stays
// 24 px tall, and touch areas outside the layout box give the chip and the
// model logo 40 px of height without overlapping. The summary sits on the
// first line, and the chip, logo, and elapsed time on the second, with the
// time at the line's right edge (DEC-129(3), DEC-129(4)).
test.describe('the banner on a 375 px touch screen', () => {
  test.use({ viewport: { width: 375, height: 812 }, hasTouch: true, isMobile: true })

  test('keeps the chip 24 px tall and gives the chip and logo separate 40 px touch areas', async ({ page }) => {
    await page.clock.setFixedTime(oneHourIn)
    await mockAPI(
      page,
      () => activity('running', workingAction, [runningOrder]),
      () => workingAction,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    const chip = card.getByRole('button', { name: 'Working, agent' })
    const logo = card.locator('[data-banner-model]')
    const heading = card.getByRole('heading', { name: 'An agent is implementing this task.' })
    const meta = card.locator('[data-banner-meta]')
    const time = meta.locator('time')
    await expect(card.getByRole('button', { name: 'operator' })).toHaveAttribute('data-live', '')
    await expect(card.locator('[data-live-halo]')).toHaveCount(0)

    const chipBox = await chip.boundingBox()
    const logoBox = await logo.boundingBox()
    const headingBox = await heading.boundingBox()
    const metaBox = await meta.boundingBox()
    const timeBox = await time.boundingBox()
    if (!chipBox || !logoBox || !headingBox || !metaBox || !timeBox) throw new Error('banner part has no box')
    expect(Math.round(chipBox.height)).toBe(24)

    // Each touch area answers a hit test across 40 px of height.
    const hits = (x: number, top: number, count: number, selector: string) =>
      page.evaluate(
        ({ x, top, count, selector }) =>
          Array.from({ length: count }, (_, index) =>
            Boolean(document.elementFromPoint(x, top + index)?.closest(selector)),
          ),
        { x, top, count, selector },
      )
    const chipCenter = chipBox.y + chipBox.height / 2
    const logoCenter = logoBox.y + logoBox.height / 2
    expect(await hits(chipBox.x + chipBox.width / 2, chipCenter - 19.5, 40, '[data-status-chip]')).not.toContain(false)
    expect(await hits(logoBox.x + logoBox.width / 2, logoCenter - 19.5, 40, '[data-banner-model]')).not.toContain(false)

    // No point along the second line's center row lies in both touch areas.
    const areas = await page.evaluate(() => {
      const area = (element: Element) => {
        const box = element.getBoundingClientRect()
        const after = getComputedStyle(element, '::after')
        const px = (value: string) => Number.parseFloat(value) || 0
        // A negative inset places the pseudo-element's edge outside the box.
        return {
          left: box.left + px(after.left),
          right: box.right - px(after.right),
          top: box.top + px(after.top),
          bottom: box.bottom - px(after.bottom),
        }
      }
      return {
        chip: area(document.querySelector('[data-status-chip]') as Element),
        logo: area(document.querySelector('[data-banner-model]') as Element),
      }
    })
    expect(areas.chip.bottom - areas.chip.top).toBeGreaterThanOrEqual(40)
    expect(areas.logo.bottom - areas.logo.top).toBeGreaterThanOrEqual(40)
    for (let x = 0; x <= 375; x++) {
      const inChip = x >= areas.chip.left && x <= areas.chip.right
      const inLogo = x >= areas.logo.left && x <= areas.logo.right
      expect(inChip && inLogo, `x=${x}`).toBe(false)
    }
    const row = await page.evaluate(
      ({ y }) =>
        Array.from({ length: 376 }, (_, x) => {
          const hit = document.elementFromPoint(x, y)
          return hit?.closest('[data-status-chip]') ? 'chip' : hit?.closest('[data-banner-model]') ? 'logo' : ''
        }),
      { y: chipCenter },
    )
    expect(row.lastIndexOf('chip')).toBeLessThan(row.indexOf('logo'))

    // The summary is above the chip, and the elapsed time ends at the meta
    // line's right edge.
    expect(headingBox.y + headingBox.height).toBeLessThanOrEqual(chipBox.y)
    expect(Math.abs(timeBox.x + timeBox.width - (metaBox.x + metaBox.width))).toBeLessThanOrEqual(2)
    expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })

    // A tap on the logo opens its tooltip with the model name.
    await logo.tap()
    await expect(card.getByRole('tooltip').filter({ hasText: /^claude-opus-5-5$/ })).toBeVisible()
  })
})

// At 1280 px a long summary wraps instead of truncating, and the meta and the
// menu stay inside the banner (DEC-129(3)). An unbroken token, such as a URL,
// wraps inside the summary's column too.
test('a long summary wraps at 1280 px and the menu stays inside the banner', async ({ page }) => {
  await page.clock.setFixedTime(oneHourIn)
  await page.setViewportSize({ width: 1280, height: 900 })
  const summary =
    'An agent is implementing this task, which rebuilds the passive next-action banner so that a long summary wraps onto a second line and never pushes the elapsed time or the More actions menu outside the banner box on a wide screen.'
  const long: NextAction = { ...workingAction, summary }
  await mockAPI(
    page,
    () => activity('running', long, [runningOrder]),
    () => long,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const heading = card.getByRole('heading', { name: summary })
  await expect(heading).toBeVisible()
  const headingBox = await heading.boundingBox()
  const cardBox = await card.boundingBox()
  const menuBox = await card.getByRole('button', { name: 'More actions' }).boundingBox()
  const metaBox = await card.locator('[data-banner-meta]').boundingBox()
  if (!headingBox || !cardBox || !menuBox || !metaBox) throw new Error('banner part has no box')
  // Two or more 20 px lines, with no ellipsis.
  expect(headingBox.height).toBeGreaterThanOrEqual(2 * 20)
  expect(await heading.evaluate((element) => getComputedStyle(element).textOverflow)).not.toBe('ellipsis')
  for (const box of [menuBox, metaBox]) {
    expect(box.x).toBeGreaterThanOrEqual(cardBox.x)
    expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width)
    expect(box.y).toBeGreaterThanOrEqual(cardBox.y)
    expect(box.y + box.height).toBeLessThanOrEqual(cardBox.y + cardBox.height)
  }
  expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })

  // A summary with one long unbroken token stays left of the meta.
  const token: NextAction = {
    ...workingAction,
    summary: `See https://example.test/${'a'.repeat(160)} for the order.`,
  }
  await page.unroute('**/v1/**')
  await mockAPI(
    page,
    () => activity('running', token, [runningOrder]),
    () => token,
  )
  await page.reload()
  const tokenHeading = card.getByRole('heading', { name: token.summary })
  await expect(tokenHeading).toBeVisible()
  const tokenRight = await tokenHeading.evaluate((element) => {
    const range = document.createRange()
    range.selectNodeContents(element)
    return Math.max(...Array.from(range.getClientRects(), (rect) => rect.right))
  })
  const tokenMetaBox = await card.locator('[data-banner-meta]').boundingBox()
  if (!tokenMetaBox) throw new Error('meta has no box')
  expect(tokenRight).toBeLessThanOrEqual(tokenMetaBox.x)
  expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })
})

// Screenshots of the banner's working, queued, and long-summary states at
// 375 px on a touch screen and at 1280 px, attached to the Playwright report
// for review (AGENTS.md visual check). No screenshot is committed.
const longSummaryAction: NextAction = {
  ...workingAction,
  summary:
    'An agent is implementing this task, which rebuilds the passive next-action banner so that a long summary wraps onto a second line and never pushes the elapsed time or the More actions menu outside the banner box.',
}
const bannerStates = [
  { name: 'working', state: 'running', action: workingAction, order: runningOrder },
  { name: 'queued', state: 'queued', action: queuedAction, order: queuedOrder },
  { name: 'long-summary', state: 'running', action: longSummaryAction, order: runningOrder },
] as const

async function captureBanner(page: Page, testInfo: TestInfo, name: string) {
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByRole('heading')).toBeVisible()
  await card.scrollIntoViewIfNeeded()
  const shots = process.env.CONVEYOR_SCREENSHOT_DIR
  const image = await page.screenshot({ path: shots ? `${shots}/${name}` : undefined, animations: 'disabled' })
  await testInfo.attach(name, { body: image, contentType: 'image/png' })
}

test.describe('capture the banner on a 375 px touch screen', () => {
  test.use({ viewport: { width: 375, height: 812 }, hasTouch: true, isMobile: true })
  for (const banner of bannerStates) {
    test(`capture the ${banner.name} banner at 375 px`, async ({ page }, testInfo) => {
      await page.clock.setFixedTime(oneHourIn)
      await mockAPI(
        page,
        () => activity(banner.state, banner.action, [banner.order]),
        () => banner.action,
      )
      await page.goto(`/w/demo/tasks/${taskId}/full`)
      await captureBanner(page, testInfo, `banner-${banner.name}-375-touch.png`)
    })
  }
})

for (const banner of bannerStates) {
  test(`capture the ${banner.name} banner at 1280 px`, async ({ page }, testInfo) => {
    await page.clock.setFixedTime(oneHourIn)
    await page.setViewportSize({ width: 1280, height: 900 })
    await mockAPI(
      page,
      () => activity(banner.state, banner.action, [banner.order]),
      () => banner.action,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    await captureBanner(page, testInfo, `banner-${banner.name}-1280.png`)
  })
}

test('a banner field the order lacks prints nothing', async ({ page }) => {
  await page.clock.setFixedTime(oneHourIn)
  const bare = { ...runningOrder, model: undefined, execution_started_at: undefined } as unknown as WorkOrder
  const noDeadline: NextAction = { ...workingAction, deadline: null }
  await mockAPI(
    page,
    () => activity('running', noDeadline, [bare]),
    () => noDeadline,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByRole('heading', { name: 'An agent is implementing this task.' })).toBeVisible()
  await expect(card.locator('[data-banner-meta]')).toHaveCount(0)
  await expect(card.locator('[data-next-action-deadline]')).toHaveCount(0)
  await expect(card.getByText(/ago|left|undefined|NaN/)).toHaveCount(0)
})

// The banner prints no deadline, time left, or deadline progress, and takes
// no tone from the deadline, at any point of the execution span (DEC-129(1);
// component-next-action invariant 33).
for (const at of ['2026-10-12T13:30:00Z', '2026-10-12T13:36:00Z', '2026-10-12T14:10:00Z']) {
  test(`at ${at} the banner prints no deadline and keeps the neutral tone`, async ({ page }) => {
    await page.clock.setFixedTime(new Date(at))
    await page.setViewportSize({ width: 1280, height: 900 })
    await mockAPI(
      page,
      () => activity('running', workingAction, [runningOrder]),
      () => workingAction,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByRole('heading', { name: 'An agent is implementing this task.' })).toBeVisible()
    await expect(card.locator('[data-next-action-deadline]')).toHaveCount(0)
    await expect(card.locator('[data-deadline-progress]')).toHaveCount(0)
    await expect(card.getByText(/\bleft\b|Deadline passed|^Deadline /)).toHaveCount(0)
    await expect(card).toHaveAttribute('data-tone', 'neutral')
    const colors = await card.evaluate((element) => {
      const span = document.createElement('span')
      span.className = 'border border-attention/60'
      element.append(span)
      const attentionBorder = getComputedStyle(span).borderTopColor
      span.remove()
      return { border: getComputedStyle(element).borderTopColor, attentionBorder }
    })
    expect(colors.border).not.toBe(colors.attentionBorder)
  })
}

// The model logo and the chip keep their tooltips reachable by keyboard: the
// logo's names the model, and the chip's names the actor and the if-ignored
// sentence (DEC-129(2); component-next-action invariant 33).
test('the model logo and the chip open their tooltips by keyboard', async ({ page }) => {
  await page.clock.setFixedTime(oneHourIn)
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await card.locator('[data-banner-model]').focus()
  await expect(card.getByRole('tooltip').filter({ hasText: /^claude-opus-5-5$/ })).toBeVisible()
  // The chip's tooltip names the actor and the if-ignored sentence on focus.
  await card.getByRole('button', { name: 'Working, agent' }).focus()
  const chipTip = card.getByRole('tooltip').filter({ hasText: 'If ignored: The attempt continues.' })
  await expect(chipTip).toBeVisible()
  await expect(chipTip).toContainText('agent')
})

// A model that names no known provider shows its harness logo and is named
// "<model> via <harness>"; a model with no known logo shows a generic chip
// icon and is named by the model alone (DEC-129(2)).
for (const { name, agent, label, harness } of [
  { name: 'auto', agent: 'cursor', label: 'auto via Cursor', harness: true },
  { name: 'mystery-model-9', agent: 'some-harness', label: 'mystery-model-9', harness: false },
]) {
  test(`the banner names the ${harness ? 'harness' : 'generic'} logo ${label}`, async ({ page }) => {
    await page.clock.setFixedTime(oneHourIn)
    const order = { ...runningOrder, model: name, agent } as unknown as WorkOrder
    await mockAPI(
      page,
      () => activity('running', workingAction, [order]),
      () => workingAction,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    const logo = card.locator('[data-banner-model]')
    await expect(logo).toHaveAccessibleName(label)
    await expect(logo.locator('svg.lucide-cpu')).toHaveCount(harness ? 0 : 1)
    await expect(card.getByText(name, { exact: true })).toHaveCount(0)
    await logo.focus()
    await expect(card.getByRole('tooltip').filter({ hasText: label })).toBeVisible()
  })
}

// The rare acts sit behind one menu: Copy order ID when the statement names an
// order, Show IDs, and Stop attempt… only for recover_work and a claimed order
// (req-operator-next-action AC-1.8, AC-7.1).
for (const { role, stop } of [
  { role: 'operator', stop: true },
  { role: 'contributor', stop: false },
]) {
  test(`the More actions menu for a ${role} ${stop ? 'offers' : 'withholds'} Stop attempt`, async ({ page }) => {
    await mockAPI(
      page,
      () => activity('running', workingAction, [runningOrder]),
      () => workingAction,
      role,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    await card.getByRole('button', { name: 'More actions' }).click()
    const items = page.getByRole('menu', { name: 'More actions' }).getByRole('menuitem')
    await expect(items).toHaveText(
      stop ? ['Copy order ID', 'Show IDs', 'Stop attempt…'] : ['Copy order ID', 'Show IDs'],
    )
    await expect(page.getByRole('menu').getByRole('separator')).toHaveCount(stop ? 1 : 0)
  })
}

test('the More actions menu works by keyboard and Escape returns focus', async ({ page }) => {
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const button = sheet.getByRole('button', { name: 'More actions' })
  await button.focus()
  await page.keyboard.press('Enter')
  const items = page.getByRole('menu', { name: 'More actions' }).getByRole('menuitem')
  await expect(items.nth(0)).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(items.nth(1)).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(items.nth(2)).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(items.nth(0)).toBeFocused()
  await page.keyboard.press('ArrowUp')
  await expect(items.nth(2)).toBeFocused()
  // Escape closes the menu, not the Board sheet, and focus returns.
  await page.keyboard.press('Escape')
  await expect(page.getByRole('menu')).toHaveCount(0)
  await expect(button).toBeFocused()
  await expect(sheet).toBeVisible()
  await page.keyboard.press(' ')
  await expect(items.nth(0)).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(button).toBeFocused()
})

test('Show IDs and Copy order ID read the stated order', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const menu = card.getByRole('button', { name: 'More actions' })
  await menu.click()
  await page.getByRole('menuitem', { name: 'Show IDs' }).click()
  const ids = card.locator('[data-next-action-ids]')
  await expect(ids.locator('[data-next-action-id="Order ID"] dd')).toHaveText('order-impl-running')
  await expect(ids.locator('[data-next-action-id="Stage"] dd')).toHaveText('implement')
  await expect(ids).not.toContainText('{')
  // The list sits under the banner's row.
  const headingBox = await card.getByRole('heading').boundingBox()
  const idsBox = await ids.boundingBox()
  expect(headingBox && idsBox && idsBox.y >= headingBox.y + headingBox.height).toBe(true)
  await menu.click()
  await page.getByRole('menuitem', { name: 'Hide IDs' }).click()
  await expect(ids).toHaveCount(0)

  await menu.click()
  await page.getByRole('menuitem', { name: 'Copy order ID' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe('order-impl-running')
  await expect(card.locator('[aria-live="polite"]')).toHaveText('Order ID copied')
  await expect(card.getByRole('alert')).toHaveCount(0)
})

// Copy order ID treats a missing clipboard API, a write that throws, and a
// refused write as a failed copy. The banner says so, keeps the order ID
// behind Show IDs, and raises no uncaught error or unhandled rejection. The
// confirmation appears only after a write resolves. Each test installs its
// own clipboard before the page's scripts run, so no test depends on browser
// permissions (req-operator-next-action AC-7.1; component-testing-strategy
// invariant 21: these mocks prove presentation only).
type ClipboardMode = 'missing' | 'resolve' | 'throw' | 'reject' | 'pending'

type ClipboardProbe = {
  clipboardMode: ClipboardMode
  copiedText: string[]
  pageFailures: string[]
  resolveCopy?: () => void
}

const copyFailedText = 'Could not copy the order ID. Use Show IDs to view it.'

async function stubClipboard(page: Page, mode: ClipboardMode) {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.addInitScript((initial: ClipboardMode) => {
    const probe = window as unknown as ClipboardProbe
    probe.clipboardMode = initial
    probe.copiedText = []
    probe.pageFailures = []
    window.addEventListener('unhandledrejection', (event) =>
      probe.pageFailures.push(`unhandledrejection: ${String(event.reason)}`),
    )
    window.addEventListener('error', (event) => probe.pageFailures.push(`error: ${event.message}`))
    if (initial === 'missing') {
      // An insecure context has no clipboard API. Deleting the property alone
      // would expose the prototype's API again on localhost.
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined })
      return
    }
    const clipboard = {
      writeText(text: string): Promise<void> {
        switch (probe.clipboardMode) {
          case 'throw':
            throw new TypeError('clipboard write threw')
          case 'reject':
            return Promise.reject(new DOMException('Write permission denied.', 'NotAllowedError'))
          case 'pending':
            return new Promise<void>((resolve) => {
              probe.resolveCopy = () => {
                probe.copiedText.push(text)
                resolve()
              }
            })
          default:
            probe.copiedText.push(text)
            return Promise.resolve()
        }
      },
    }
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: clipboard })
  }, mode)
  return {
    errors,
    setMode: (next: ClipboardMode) =>
      page.evaluate((value) => {
        ;(window as unknown as ClipboardProbe).clipboardMode = value
      }, next),
    copied: () => page.evaluate(() => (window as unknown as ClipboardProbe).copiedText),
    failures: () => page.evaluate(() => (window as unknown as ClipboardProbe).pageFailures),
    resolve: () => page.evaluate(() => (window as unknown as ClipboardProbe).resolveCopy?.()),
  }
}

async function copyOrderID(card: Locator) {
  await card.getByRole('button', { name: 'More actions' }).click()
  await card.page().getByRole('menuitem', { name: 'Copy order ID' }).click()
}

test('Copy order ID confirms only after the clipboard write resolves', async ({ page }) => {
  const clipboard = await stubClipboard(page, 'pending')
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const live = card.locator('[aria-live="polite"]')
  await copyOrderID(card)
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as ClipboardProbe).resolveCopy))
    .toBe('function')
  await expect(live).toHaveText('')
  await expect(card.getByRole('alert')).toHaveCount(0)
  await clipboard.resolve()
  await expect(live).toHaveText('Order ID copied')
  await expect(card.getByRole('alert')).toHaveCount(0)
  expect(await clipboard.copied()).toEqual(['order-impl-running'])
  expect(await clipboard.failures()).toEqual([])
  expect(clipboard.errors).toEqual([])
})

for (const surface of surfaces) {
  for (const { mode, name } of [
    { mode: 'missing', name: 'a missing clipboard API' },
    { mode: 'throw', name: 'a write that throws' },
    { mode: 'reject', name: 'a refused write' },
  ] as const) {
    test(`Copy order ID on the ${surface.name} reports ${name} and keeps Show IDs`, async ({ page }) => {
      const clipboard = await stubClipboard(page, mode)
      await mockAPI(
        page,
        () => activity('running', workingAction, [runningOrder]),
        () => workingAction,
      )
      await page.goto(surface.path)
      const card = page.getByRole('region', { name: 'Next action' })
      await copyOrderID(card)

      const failed = card.getByRole('alert')
      await expect(failed).toHaveText(copyFailedText)
      await expect(failed).toBeVisible()
      await expect(card.locator('[aria-live="polite"]')).toHaveText('')
      // The message sits under the banner's row; the row keeps its summary.
      const headingBox = await card.getByRole('heading').boundingBox()
      const failedBox = await failed.boundingBox()
      expect(headingBox && failedBox && failedBox.y >= headingBox.y + headingBox.height).toBe(true)
      await expect(card.getByRole('heading', { name: 'An agent is implementing this task.' })).toBeVisible()

      // The menu is unchanged, and Show IDs shows the stated order.
      await card.getByRole('button', { name: 'More actions' }).click()
      await expect(page.getByRole('menu', { name: 'More actions' }).getByRole('menuitem')).toHaveText([
        'Copy order ID',
        'Show IDs',
        'Stop attempt…',
      ])
      await page.getByRole('menuitem', { name: 'Show IDs' }).click()
      await expect(card.locator('[data-next-action-ids] [data-next-action-id="Order ID"] dd')).toHaveText(
        'order-impl-running',
      )
      await expect(failed).toHaveText(copyFailedText)

      expect(await clipboard.copied()).toEqual([])
      expect(await clipboard.failures()).toEqual([])
      expect(clipboard.errors).toEqual([])
      expect(await noSidewaysScroll(card)).toEqual({ card: false, page: false })
    })
  }
}

test('a successful retry clears the copy failure', async ({ page }) => {
  const clipboard = await stubClipboard(page, 'reject')
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const live = card.locator('[aria-live="polite"]')
  await copyOrderID(card)
  await expect(card.getByRole('alert')).toHaveText(copyFailedText)

  await clipboard.setMode('resolve')
  await copyOrderID(card)
  await expect(live).toHaveText('Order ID copied')
  await expect(card.getByRole('alert')).toHaveCount(0)
  expect(await clipboard.copied()).toEqual(['order-impl-running'])
  expect(await clipboard.failures()).toEqual([])
  expect(clipboard.errors).toEqual([])
})

test('a failed retry clears the copy confirmation', async ({ page }) => {
  const clipboard = await stubClipboard(page, 'resolve')
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  // A paused clock keeps the confirmation on screen until the retry, so its
  // absence afterwards comes from the retry and not from its timeout.
  await page.clock.install({ time: oneHourIn })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const live = card.locator('[aria-live="polite"]')
  await expect(card).toBeVisible()
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1_000)
  await copyOrderID(card)
  await expect(live).toHaveText('Order ID copied')

  await clipboard.setMode('reject')
  await copyOrderID(card)
  await expect(card.getByRole('alert')).toHaveText(copyFailedText)
  await expect(live).toHaveText('')
  expect(await clipboard.copied()).toEqual(['order-impl-running'])
  expect(await clipboard.failures()).toEqual([])
  expect(clipboard.errors).toEqual([])
})

test('Stop attempt… opens the confirmation under the banner and posts the same preempt request', async ({ page }) => {
  const posts = await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const menu = card.getByRole('button', { name: 'More actions' })
  await menu.click()
  await page.getByRole('menuitem', { name: 'Stop attempt…' }).click()
  const reason = card.getByLabel('Reason for stopping this attempt')
  await expect(reason).toBeFocused()
  await expect(card.getByText(/stops within about a minute/)).toBeVisible()
  const confirmation = card.locator('[data-preempt-confirmation]')
  const headingBox = await card.getByRole('heading').boundingBox()
  const confirmationBox = await confirmation.boundingBox()
  expect(headingBox && confirmationBox && confirmationBox.y >= headingBox.y + headingBox.height).toBe(true)

  // Cancel closes it and returns focus to the menu button.
  await card.getByRole('button', { name: 'Cancel' }).click()
  await expect(confirmation).toHaveCount(0)
  await expect(menu).toBeFocused()

  await menu.click()
  await page.getByRole('menuitem', { name: 'Stop attempt…' }).click()
  const stop = card.getByRole('button', { name: 'Stop the attempt' })
  await expect(stop).toBeDisabled()
  await reason.fill('Switch to the repaired setup')
  await stop.click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].method).toBe('POST')
  expect(posts[0].path).toBe('/v1/work-orders/order-impl-running/preempt')
  const body = posts[0].body as { reason: string; request_id: string }
  expect(body.reason).toBe('Switch to the repaired setup')
  expect(body.request_id).toMatch(/^[0-9a-f-]{36}$/)
  await expect(card.getByRole('button', { name: 'Stopped' })).toBeDisabled()
})

// A claimed order the next action does not name as current, such as a claimed
// refresh-review round, still offers Stop attempt… once, against that claimed
// order; with no stated order there is no meta and no Copy order ID.
test('a claimed order the statement does not name keeps one Stop attempt in the menu', async ({ page }) => {
  const reviewOrder = { ...runningOrder, id: 'order-review-refresh', stage: 'review', activity_snapshot: undefined }
  const unnamed: NextAction = {
    ...workingAction,
    summary: 'An agent is working on this task.',
    current_order: undefined,
  }
  const posts = await mockAPI(
    page,
    () => activity('running', unnamed, [reviewOrder as WorkOrder]),
    () => unnamed,
  )
  for (const path of [`/w/demo/tasks/${taskId}/full`, `/w/demo/tasks/${taskId}`]) {
    await page.goto(path)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByRole('heading', { name: 'An agent is working on this task.' })).toBeVisible()
    await expect(card.locator('[data-execution-status]')).toHaveCount(0)
    await expect(card.locator('[data-banner-meta]')).toHaveCount(0)
    await card.getByRole('button', { name: 'More actions' }).click()
    await expect(page.getByRole('menuitem')).toHaveText(['Show IDs', 'Stop attempt…'])
    await page.keyboard.press('Escape')
  }
  await page.getByRole('region', { name: 'Next action' }).getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Stop attempt…' }).click()
  await page.getByLabel('Reason for stopping this attempt').fill('Stale round')
  await page.getByRole('button', { name: 'Stop the attempt' }).click()
  await expect.poll(() => posts.map((post) => post.path)).toEqual(['/v1/work-orders/order-review-refresh/preempt'])
})

// The Flow view keeps the full card: its execution line, the visible
// if-ignored sentence and deadline, and the non-inline Stop attempt control.
test('the Flow view keeps the full card for a running attempt', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', 'flow'))
  await mockAPI(
    page,
    () => activity('running', workingAction, [runningOrder]),
    () => workingAction,
  )
  await page.route(`**/v1/tasks/${taskId}/flow*`, (route) => route.fulfill({ status: 500, json: {} }))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByRole('heading', { name: 'An agent is implementing this task.' })).toBeVisible()
  await expect(card.locator('[data-passive-banner]')).toHaveCount(0)
  await expect(card.getByText('Implementation · Progressing', { exact: true })).toBeVisible()
  await expect(card.getByText('If ignored: The attempt continues.')).toBeVisible()
  await expect(card.locator('[data-next-action-deadline] time')).toHaveAttribute('datetime', '2026-10-12T14:00:00Z')
  await expect(card.getByRole('button', { name: 'More actions' })).toHaveCount(0)
  await card.getByRole('button', { name: 'Stop attempt' }).click()
  await expect(card.getByLabel('Reason for stopping this attempt')).toBeVisible()
  await expect(card.getByRole('button', { name: 'Cancel' })).toHaveCount(0)
})

// On the Flow tab the timeline's gate card is not mounted, so the card itself
// carries the server's choices: still usable, and the summary still printed
// once (component-web-task-surfaces, Activity tabs and the task flow view).
for (const surface of [`/w/demo/tasks/${taskId}`, `/w/demo/tasks/${taskId}/full`]) {
  test(`on ${surface} the Flow tab keeps the gate choices usable from the card`, async ({ page }) => {
    const posts = await mockAPI(
      page,
      () => activity('awaiting_human', gateAction),
      () => gateAction,
    )
    await page.route(`**/v1/tasks/${taskId}/flow*`, (route) => route.fulfill({ status: 500, json: {} }))
    await page.goto(surface)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(page.getByRole('tab', { name: 'Timeline', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(card.locator('[data-next-action-choices]')).toHaveCount(0)
    await page.getByRole('tab', { name: 'Flow', exact: true }).click()
    await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
    await expect(page.locator('[data-next-action-choices]')).toHaveCount(1)
    const choices = card.locator('[data-next-action-choices]').getByRole('button')
    await expect(choices).toHaveText(['Approve plan', 'Reject plan', 'Teleport'])
    // The summary prints once on the detail surface.
    const detail = surface.endsWith('/full')
      ? page.getByRole('region', { name: 'Task content' })
      : page.getByRole('dialog', { name: 'Task detail' })
    await expect(detail.getByText('The plan for Card task waits for your approval.')).toHaveCount(1)
    await choices.getByText('Approve plan').click()
    await expect.poll(() => posts.length).toBe(1)
    expect(posts[0].path).toBe(`/v1/tasks/${taskId}/review?via=next-action`)
  })
}

// A disabled choice's reason prints in one block under the row, naming the
// choice, so every button on the row keeps one height and one line
// (feature-operator-next-action NA-6; component-web-task-surfaces).
for (const width of [390, 1280]) {
  test(`disabled choices keep the row aligned and list their reasons under it at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const parked: NextAction = {
      label: 'Decide on a parked task',
      actor: 'operator',
      kind: 'parked',
      judgment: true,
      summary: 'This task is parked.',
      question: 'Put it back, or close it?',
      deadline: null,
      choices: [
        ['recover', 'Recover', 'redispatch', 'recover_work', true],
        ['close', 'Close', 'close', 'operate_gates', false],
        ['start_over', 'Start over', 'restart', 'operate_gates', false],
      ].map(([id, label, route, capability, allowed]) => ({
        id: String(id),
        label: String(label),
        effect: `${label} effect.`,
        method: 'POST',
        path: `/v1/tasks/${taskId}/${route}`,
        capability: String(capability),
        allowed: Boolean(allowed),
      })),
      details: {},
    }
    await mockAPI(
      page,
      () => activity('parked', parked),
      () => parked,
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Next action' })
    const row = card.locator('[data-choice-row]')
    await expect(row).toHaveAttribute('data-choice-row', 'row')
    const buttons = row.getByRole('button')
    await expect(buttons).toHaveText(['Recover', 'Close', 'Start over'])
    const reasons = card.locator('[data-choice-reasons] li')
    await expect(reasons).toHaveText([
      'Close: Needs the operate gates capability.',
      'Start over: Needs the operate gates capability.',
    ])
    for (const [index, choice] of [
      [1, 'close'],
      [2, 'start_over'],
    ] as const) {
      const id = await card.locator(`[data-choice-reason="${choice}"]`).getAttribute('id')
      await expect(buttons.nth(index)).toHaveAttribute('aria-describedby', id ?? '')
    }
    await expect(buttons.nth(0)).not.toHaveAttribute('aria-describedby')
    // No reason sits inside the row, and every button shares one line and height.
    await expect(row.getByText('Needs the operate gates capability.')).toHaveCount(0)
    const boxes = await buttons.evaluateAll((elements) =>
      elements.map((element) => {
        const box = element.getBoundingClientRect()
        return { top: box.top, height: box.height }
      }),
    )
    for (const box of boxes) {
      expect(Math.abs(box.top - boxes[0].top)).toBeLessThanOrEqual(1)
      expect(Math.abs(box.height - boxes[0].height)).toBeLessThanOrEqual(1)
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    expect(overflow).toBeLessThanOrEqual(0)
  })
}

test('the card carries the choices of an operator action outside the human gate', async ({ page }) => {
  const question: NextAction = {
    label: 'Agent question',
    actor: 'operator',
    kind: 'agent_question',
    judgment: true,
    summary: 'The agent stopped for a decision.',
    question: 'Which schema should the migration target?',
    deadline: null,
    choices: [
      {
        id: 'answer',
        label: 'Answer',
        effect: 'Sends your direction to the agent.',
        method: 'POST',
        path: '/v1/work-orders/order-impl-1/answer',
        capability: 'operate_gates',
        allowed: true,
      },
      {
        id: 'close',
        label: 'Close',
        effect: 'Closes the task.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/close`,
        capability: 'operate_gates',
        allowed: false,
      },
    ],
    details: {},
  }
  const posts = await mockAPI(
    page,
    () => activity('running', question),
    () => question,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  const card = page.getByRole('region', { name: 'Next action' })
  // Every server choice renders; one the caller may not take is disabled, its
  // reason prints in the block under the row, and each carries the server's
  // endpoint.
  const choices = card.locator('[data-next-action-choices]').getByRole('button')
  await expect(choices).toHaveText(['Answer', 'Close'])
  await expect(choices.nth(1)).toBeDisabled()
  await expect(card.locator('[data-choice-reason="close"]')).toHaveText('Close: Needs the operate gates capability.')
  await expect(choices.nth(0)).toHaveAttribute('data-endpoint', 'POST /v1/work-orders/order-impl-1/answer')

  // A choice that carries typed input asks for it before sending, and a
  // required input keeps the send disabled while it is empty.
  await choices.getByText('Answer').click()
  const direction = card.getByRole('textbox', { name: 'Direction' })
  await expect(direction).toBeVisible()
  const send = card.locator('[data-next-action-choices]').getByRole('button', { name: 'Answer' }).last()
  await expect(send).toBeDisabled()
  await direction.fill('Target the v2 schema.')
  await send.click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0].method).toBe('POST')
  expect(posts[0].path).toBe('/v1/work-orders/order-impl-1/answer')
  expect(posts[0].body).toMatchObject({ direction: 'Target the v2 schema.' })
  expect((posts[0].body as { request_id: string }).request_id).toBeTruthy()
})

test('a server choice this page cannot send renders disabled with its reason', async ({ page }) => {
  const stalled: NextAction = {
    label: 'Start over',
    actor: 'operator',
    kind: 'dependency_unsatisfiable',
    judgment: true,
    summary: 'A dependency can never be satisfied.',
    deadline: null,
    choices: [
      {
        id: 'remove_dependency',
        label: 'Remove dependency',
        effect: 'Drops the unsatisfiable dependency.',
        method: 'DELETE',
        path: `/v1/tasks/${taskId}/dependencies/{dependency_id}`,
        capability: 'operate_gates',
        allowed: true,
      },
    ],
    details: {},
  }
  await mockAPI(
    page,
    () => activity('queued', stalled),
    () => stalled,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const card = page.getByRole('region', { name: 'Next action' })
  const button = card.getByRole('button', { name: 'Remove dependency' })
  await expect(button).toBeDisabled()
  await expect(button).toHaveAttribute('data-endpoint', `DELETE /v1/tasks/${taskId}/dependencies/{dependency_id}`)
  await expect(card.getByText('This page cannot send this choice.')).toBeVisible()
})

test('a terminal next action renders no card', async ({ page }) => {
  const terminal: NextAction = {
    label: 'Merged',
    actor: 'none',
    kind: 'terminal',
    judgment: false,
    summary: 'Card task is merged.',
    deadline: null,
    choices: [],
    details: {},
  }
  await mockAPI(
    page,
    () => activity('merged', terminal),
    () => terminal,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(page.getByRole('heading', { name: 'Card task' }).first()).toBeVisible()
  await expect(page.getByText('Card task is merged.')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Next action' })).toHaveCount(0)
})

test('the human gate takes its statement and buttons from the server, not the event log', async ({ page }) => {
  // A job.timeout event once made the client offer Approve. The gate now
  // offers exactly the choices the server states, whatever the event log says.
  const jobFailure: NextAction = {
    label: 'Decide the failed stage',
    actor: 'operator',
    kind: 'job_failure',
    judgment: true,
    summary: 'The implementation stage failed and waits for your decision.',
    question: 'Send it back with guidance, or close it?',
    deadline: null,
    choices: [
      {
        id: 'redirect',
        label: 'Redirect',
        effect: 'Sends the task back with guidance.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/review`,
        capability: 'operate_gates',
        allowed: true,
      },
      {
        id: 'reject',
        label: 'Reject',
        effect: 'Closes the task.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/review`,
        capability: 'operate_gates',
        allowed: true,
      },
    ],
    details: { gate_kind: 'job_failure' },
  }
  const posts = await mockAPI(
    page,
    () => activity('awaiting_human', jobFailure),
    () => jobFailure,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  // The gate heads with the server label; the summary stays on the card (DEC-61).
  await expect(gate.getByRole('heading', { name: jobFailure.label })).toBeVisible()
  await expect(gate).not.toContainText(jobFailure.summary)
  await expect(gate.getByText('Send it back with guidance, or close it?')).toBeVisible()
  await expect(gate.locator('[data-next-action-choices]').first().getByRole('button')).toHaveText([
    'Redirect',
    'Reject',
  ])
  await expect(gate.getByRole('button', { name: 'Approve' })).toHaveCount(0)

  await gate.getByRole('button', { name: 'Redirect' }).click()
  await gate.getByLabel('Guidance').fill('Split the migration first.')
  await gate.getByRole('button', { name: 'Redirect' }).last().click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0]).toMatchObject({
    method: 'POST',
    path: `/v1/tasks/${taskId}/review`,
    body: { action: 'redirect', comment: 'Split the migration first.' },
  })
})

test('an approved task with the merge gate off shows the server merging summary and no button', async ({ page }) => {
  const merging: NextAction = {
    label: 'Merging',
    actor: 'factory',
    kind: 'merging',
    judgment: false,
    summary: 'Conveyor is merging',
    deadline: null,
    choices: [],
    details: {},
  }
  await mockAPI(
    page,
    () => activity('approved', merging),
    () => merging,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByText('Conveyor is merging')).toBeVisible()
  await expect(card.getByRole('button')).toHaveCount(0)
  // Not an operator gate: no gate card and no merge button anywhere.
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /merge/i })).toHaveCount(0)
})

test('a check hold names each check with its GitHub link and offers the merge act', async ({ page }) => {
  const held: NextAction = {
    label: 'Checks hold the merge',
    actor: 'operator',
    kind: 'check_hold',
    judgment: true,
    summary: 'A failed check holds the merge of the approved head: e2e (failed).',
    question:
      'Rerun e2e (failed), legacy-status (pending) on GitHub, merge the approved head anyway, request changes, or close?',
    deadline: null,
    choices: [
      {
        id: 'merge',
        label: 'Merge',
        effect: 'Merges the approved head without reading its checks.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/merge`,
        capability: 'operate_gates',
        allowed: true,
      },
    ],
    details: {
      check_hold_reason: 'failed',
      checks: [
        { name: 'e2e', state: 'failed', url: 'https://github.com/acme/app/runs/2' },
        { name: 'legacy-status', state: 'pending' },
      ],
    },
  }
  await mockAPI(
    page,
    () => activity('approved', held),
    () => held,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.getByRole('link', { name: 'e2e' })).toHaveAttribute('href', 'https://github.com/acme/app/runs/2')
  await expect(gate.getByText('legacy-status', { exact: true })).toBeVisible()
  await expect(gate.getByRole('link', { name: 'legacy-status' })).toHaveCount(0)
  // Merging past the warning above it never wears the safe primary look.
  const merge = gate.getByRole('button', { name: 'Merge' })
  await expect(merge).toBeVisible()
  await expect(merge).not.toHaveClass(/bg-primary/)
})

// The merge at check_hold and merge_execution lands the approved head, so it
// opens a confirm before it posts (component-web-task-surfaces invariant 45).
// The choices are the server's, verbatim from internal/nextaction/choices.go;
// the task detail of an approved task carries merge_readiness, the head and
// pull request the confirm names (internal/httpapi/server.go).
const approvedHead = '5eedc0ffee1234567890'
const heldMergePath = `/v1/tasks/${taskId}/merge`

function heldMergeAction(kind: 'check_hold' | 'merge_execution'): NextAction {
  const held = kind === 'check_hold'
  return {
    label: held ? 'Checks hold the merge' : 'Merge',
    actor: 'operator',
    kind,
    judgment: true,
    summary: held
      ? 'A failed check holds the merge of the approved head: e2e (failed).'
      : 'The merge gate is on and the branch is ready to merge.',
    question: held
      ? 'Rerun e2e (failed), deploy (stuck), legacy-status (pending) on GitHub, merge the approved head anyway, request changes, or close?'
      : 'Merge this branch?',
    deadline: null,
    choices: [
      {
        id: 'merge',
        label: 'Merge',
        effect: held
          ? 'Merges the approved head without reading its checks, and records you and the checks that held it.'
          : 'Merges the approved head.',
        method: 'POST',
        path: heldMergePath,
        capability: 'operate_gates',
        allowed: true,
      },
      {
        id: 'request_changes',
        label: 'Request changes…',
        effect: 'Sends the branch back.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/request-changes`,
        capability: 'request_changes',
        allowed: true,
      },
      {
        id: 'close',
        label: 'Close…',
        effect: 'Closes the task.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/close`,
        capability: 'operate_gates',
        allowed: true,
      },
    ],
    details: held
      ? {
          check_hold_reason: 'failed',
          checks: [
            { name: 'e2e', state: 'failed', url: 'https://github.com/acme/app/runs/2' },
            { name: 'deploy', state: 'stuck', url: 'https://github.com/acme/app/runs/3' },
            { name: 'legacy-status', state: 'pending' },
          ],
        }
      : {},
  }
}

function heldMergeActivity(kind: 'check_hold' | 'merge_execution') {
  return {
    ...activity('approved', heldMergeAction(kind)),
    merge_readiness: { state: 'MERGEABLE', head_sha: approvedHead, number: 77, url: 'https://github.test/pr/77' },
  }
}

for (const kind of ['check_hold', 'merge_execution'] as const) {
  for (const surface of [`/w/demo/tasks/${taskId}`, `/w/demo/tasks/${taskId}/full`]) {
    test(`Merge at ${kind} on ${surface} posts only after a confirm that names the approved head`, async ({ page }) => {
      const posts = await mockAPI(
        page,
        () => heldMergeActivity(kind),
        () => heldMergeAction(kind),
      )
      // The forge answers only after both confirm clicks land, so the act is in
      // flight across the second click.
      const forge = Promise.withResolvers<void>()
      const merges: string[] = []
      await page.route(`**${heldMergePath}**`, async (route) => {
        merges.push(route.request().method())
        await forge.promise
        return route.fulfill({ json: {} })
      })
      await page.goto(surface)
      const gate = page.getByRole('region', { name: 'Human gate' })
      // The merge choice is never a one-click button.
      await expect(gate.locator('[data-choice="merge"]')).toHaveCount(0)
      const merge = gate.locator('[data-merge-at-gate]')
      await expect(merge).toHaveText('Merge')
      if (kind === 'check_hold') await expect(merge).not.toHaveClass(/bg-primary/)
      // The server's order stays: Merge, then Request changes and Close.
      await expect(gate.locator('[data-choice-row]').getByRole('button')).toHaveText([
        'Merge',
        'Request changes…',
        'Close…',
      ])

      // Opening the confirm sends nothing, and Cancel sends nothing.
      await merge.click()
      const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
      await expect(dialog.getByRole('heading')).toHaveText('Merge #77?')
      await expect(dialog.locator('[data-confirm-head]')).toHaveText(
        `This merges #77 at the approved head ${approvedHead.slice(0, 7)} under your name. Open #77`,
      )
      await expect(dialog.locator('code')).toHaveAttribute('title', approvedHead)
      await expect(dialog.locator('[data-confirm-merge]')).toHaveAttribute('data-endpoint', `POST ${heldMergePath}`)
      const checks = dialog.locator('[data-confirm-checks]')
      if (kind === 'check_hold') {
        // Each failed, stuck, or pending check the hold names, with its state.
        await expect(checks.getByRole('listitem')).toHaveText([
          /^e2e\s*failed$/,
          /^deploy\s*stuck$/,
          /^legacy-status\s*pending$/,
        ])
        await expect(checks.getByRole('link', { name: 'e2e' })).toHaveAttribute(
          'href',
          'https://github.com/acme/app/runs/2',
        )
        await expect(checks).toContainText('Conveyor records you and these checks with it.')
      } else {
        await expect(checks).toHaveCount(0)
      }
      await dialog.getByRole('button', { name: 'Cancel' }).click()
      await expect(dialog).toHaveCount(0)
      expect(merges).toHaveLength(0)
      expect(posts).toHaveLength(0)

      // Confirming posts the server's merge choice once, even on a double click.
      await merge.click()
      await dialog.locator('[data-confirm-merge]').dblclick()
      await expect.poll(() => merges.length).toBe(1)
      await expect(dialog.locator('[data-confirm-merge]')).toHaveText('Merging…')
      forge.resolve()
      await expect(dialog).toHaveCount(0)
      expect(merges).toEqual(['POST'])
      expect(posts).toHaveLength(0)
    })
  }
}

test('a 409 on Merge at check_hold refetches, keeps the refusal, and never retries', async ({ page }) => {
  await mockAPI(
    page,
    () => heldMergeActivity('check_hold'),
    () => heldMergeAction('check_hold'),
  )
  let merges = 0
  let actionReads = 0
  const refetch = Promise.withResolvers<void>()
  let refused = false
  await page.route(`**/v1/tasks/${taskId}/next-action**`, async (route) => {
    actionReads++
    // The refetch after the refusal waits, so the act must stay pending until it lands.
    if (refused) await refetch.promise
    return route.fallback()
  })
  await page.route(`**${heldMergePath}**`, (route) => {
    merges++
    refused = true
    return route.fulfill({ status: 409, json: { error: 'task is not approved' } })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect.poll(() => actionReads).toBeGreaterThan(0)
  const readsBefore = actionReads
  await gate.locator('[data-merge-at-gate]').click()
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  await dialog.locator('[data-confirm-merge]').click()
  await expect.poll(() => actionReads).toBeGreaterThan(readsBefore)
  await expect(dialog.locator('[data-confirm-merge]')).toBeDisabled()
  refetch.resolve()
  await expect(dialog.getByText('task is not approved')).toBeVisible()
  await expect(dialog.locator('[data-confirm-merge]')).toBeEnabled()
  expect(merges).toBe(1)
})

// The one-act merge at the merge_approval gate (DEC-64). Merge is the server's
// `merge` choice (DEC-61): offered at MERGEABLE, otherwise refused with the
// server's reason. The browser adds only the confirm, which names the reviewed
// head and the PR.
const reviewedHead = 'abc1234def5678'
const mergePath = `/v1/tasks/${taskId}/merge`

function mergeApproval(state: string): NextAction {
  const mergeable = state === 'MERGEABLE'
  const refusal = `Merge is offered only when the pull request is MERGEABLE; it is ${state}.`
  return {
    label: 'Merge approval',
    actor: 'operator',
    kind: 'merge_approval',
    judgment: true,
    summary: 'The reviewed branch is waiting for merge approval.',
    question: 'Approve the merge?',
    deadline: null,
    choices: [
      {
        id: 'approve',
        label: 'Approve',
        effect: 'Approves the merge.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/review`,
        capability: 'operate_gates',
        allowed: true,
      },
      ...(mergeable
        ? [
            {
              id: 'merge',
              label: 'Merge',
              effect: 'Approves and merges the reviewed head.',
              method: 'POST',
              path: mergePath,
              capability: 'operate_gates',
              allowed: true,
            },
          ]
        : []),
    ],
    details: {
      gate_kind: 'merge_approval',
      merge_readiness: state,
      ...(mergeable ? {} : { refusals: [{ choice: 'merge', text: refusal }] }),
    },
  }
}

function mergeGateActivity(state: string) {
  return {
    ...activity('awaiting_human', mergeApproval(state)),
    at_merge_gate: true,
    merge_readiness: { state, head_sha: reviewedHead, number: 42, url: 'https://github.test/pr/42' },
  }
}

// The merge readiness in plain words above the merge-approval choices
// (component-web-task-surfaces, Next-action card).
const mergeStatusCopy: Record<string, string> = {
  UNKNOWN: "Merge not available yet. GitHub hasn't reported whether #42 can merge. Open #42",
  CONFLICTING: "Can't merge. #42 conflicts with its base branch. Open #42",
  STALE: "Can't merge. #42 moved since review. Open #42",
  // Any other refusal prints the server's text verbatim.
  MERGED_HEAD_MISMATCH:
    'Merge is offered only when the pull request is MERGEABLE; it is MERGED_HEAD_MISMATCH. Open #42',
}

for (const [state, copy] of Object.entries(mergeStatusCopy)) {
  test(`the merge_approval gate refuses Merge at ${state} and states why above the choices`, async ({ page }) => {
    await mockAPI(
      page,
      () => mergeGateActivity(state),
      () => mergeApproval(state),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    const approve = gate.locator('[data-choice="approve"]')
    await expect(approve).toBeEnabled()
    // With Merge refused, the first enabled choice is the one primary.
    await expect(approve).toHaveClass(/bg-primary/)
    const merge = gate.locator('[data-merge-at-gate]')
    await expect(merge).toBeDisabled()
    const status = gate.locator('[data-merge-status]')
    await expect(status).toHaveAttribute('data-merge-status', state)
    await expect(status).toHaveText(copy)
    await expect(merge).toHaveAttribute('aria-describedby', (await status.getAttribute('id')) ?? '')
    // The status line carries the refusal, so no reason line repeats it.
    await expect(gate.locator('[data-choice-reasons]')).toHaveCount(0)
  })
}

test('a merge refusal without a readiness read reads as not available yet', async ({ page }) => {
  await mockAPI(
    page,
    () => ({ ...activity('awaiting_human', mergeApproval('UNKNOWN')), at_merge_gate: true }),
    () => mergeApproval('UNKNOWN'),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const status = page.getByRole('region', { name: 'Human gate' }).locator('[data-merge-status]')
  await expect(status).toHaveAttribute('data-merge-status', 'UNKNOWN')
  await expect(status).toHaveText("Merge not available yet. GitHub hasn't reported whether the pull request can merge.")
})

for (const width of [390, 1280]) {
  test(`the merge_approval choices stack on a phone and share one row otherwise, at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    for (const state of ['MERGEABLE', 'UNKNOWN']) {
      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockAPI(
        page,
        () => mergeGateActivity(state),
        () => mergeApproval(state),
      )
      await page.goto(`/w/demo/tasks/${taskId}/full`)
      const gate = page.getByRole('region', { name: 'Human gate' })
      const row = gate.locator('[data-choice-row]')
      await expect(row).toHaveAttribute('data-choice-row', 'grid')
      const merge = gate.locator('[data-merge-at-gate]')
      const approve = gate.locator('[data-choice="approve"]')
      await expect(merge).toBeVisible()
      await expect(row.locator('button[class*="bg-primary"]')).toHaveCount(1)
      await expect(state === 'MERGEABLE' ? merge : approve).toHaveClass(/bg-primary/)
      const rowBox = await row.boundingBox()
      const mergeBox = await merge.boundingBox()
      const approveBox = await approve.boundingBox()
      if (!rowBox || !mergeBox || !approveBox) throw new Error('missing layout box')
      if (width < 640) {
        // Merge spans the grid; Approve sits on the next line in a half-width cell.
        expect(Math.abs(mergeBox.width - rowBox.width)).toBeLessThanOrEqual(1)
        expect(approveBox.y).toBeGreaterThanOrEqual(mergeBox.y + mergeBox.height)
        expect(approveBox.width).toBeLessThan(rowBox.width * 0.6)
      } else {
        // The same row of natural-width buttons as every other gate.
        expect(Math.abs(approveBox.y - mergeBox.y)).toBeLessThanOrEqual(1)
        expect(mergeBox.width).toBeLessThan(rowBox.width * 0.4)
      }
      expect(Math.abs(approveBox.height - mergeBox.height)).toBeLessThanOrEqual(1)
      // The status line and the grid fill the gate's choices container: no
      // width cap leaves them in a narrow left column (component-web-task-surfaces).
      const choicesBox = await gate.locator('[data-next-action-choices]').boundingBox()
      const statusBox = await gate.locator('[data-merge-status]').boundingBox()
      if (!choicesBox || !statusBox) throw new Error('missing layout box')
      expect(Math.abs(rowBox.width - choicesBox.width)).toBeLessThanOrEqual(1)
      expect(Math.abs(statusBox.width - choicesBox.width)).toBeLessThanOrEqual(1)
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
      expect(overflow).toBeLessThanOrEqual(0)
    }
  })
}

test('Merge at MERGEABLE confirms the reviewed head and PR, then posts the choice once', async ({ page }) => {
  const posts = await mockAPI(
    page,
    () => mergeGateActivity('MERGEABLE'),
    () => mergeApproval('MERGEABLE'),
  )
  const merges: string[] = []
  // The forge answers only after both clicks land, so the act is in flight
  // across the second click.
  const forge = Promise.withResolvers<void>()
  await page.route(`**${mergePath}**`, async (route) => {
    merges.push(route.request().method())
    await forge.promise
    return route.fulfill({ json: {} })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  // Approve stays beside Merge, and the merge choice is not also a one-click button.
  await expect(gate.locator('[data-choice="approve"]')).toBeEnabled()
  await expect(gate.locator('[data-choice="merge"]')).toHaveCount(0)
  await expect(gate.locator('[data-merge-status]')).toHaveText(
    `Ready to merge at reviewed head ${reviewedHead.slice(0, 7)}. Open #42`,
  )
  await expect(gate.locator('[data-merge-at-gate]')).toHaveClass(/bg-primary/)
  await gate.locator('[data-merge-at-gate]').click()
  // Opening the confirm sends nothing.
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  await expect(dialog).toContainText('#42')
  await expect(dialog.locator('code')).toHaveText(reviewedHead.slice(0, 7))
  await expect(dialog.locator('code')).toHaveAttribute('title', reviewedHead)
  await expect(dialog.locator('[data-confirm-merge]')).toHaveAttribute('data-endpoint', `POST ${mergePath}`)
  expect(merges).toHaveLength(0)
  await dialog.locator('[data-confirm-merge]').dblclick()
  await expect.poll(() => merges.length).toBe(1)
  await expect(dialog.locator('[data-confirm-merge]')).toHaveText('Merging…')
  forge.resolve()
  await expect(dialog).toHaveCount(0)
  expect(merges).toEqual(['POST'])
  expect(posts).toHaveLength(0)
})

test('a 409 refusal shows the readiness the server names', async ({ page }) => {
  await mockAPI(
    page,
    () => mergeGateActivity('MERGEABLE'),
    () => mergeApproval('MERGEABLE'),
  )
  await page.route(`**${mergePath}**`, (route) =>
    route.fulfill({ status: 409, json: { error: 'merge gate not ready: CONFLICTING', readiness: 'CONFLICTING' } }),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await page.getByRole('region', { name: 'Human gate' }).locator('[data-merge-at-gate]').click()
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  await dialog.locator('[data-confirm-merge]').click()
  await expect(dialog.getByText('Not merged: the pull request has merge conflicts.')).toBeVisible()
})

test('the recovery card follows the order the server names as current', async ({ page }) => {
  const orders = [
    {
      id: 'order-impl-old',
      task_id: taskId,
      stage: 'implement',
      state: 'timed_out',
      created_at: '2026-10-12T09:00:00Z',
      queue_entered_at: '2026-10-12T09:00:00Z',
      updated_at: '2026-10-12T09:59:00Z',
    },
    {
      id: 'order-verify-current',
      task_id: taskId,
      stage: 'verify',
      state: 'stale',
      created_at: '2026-10-12T08:00:00Z',
      queue_entered_at: '2026-10-12T08:00:00Z',
      updated_at: '2026-10-12T08:00:00Z',
    },
  ] as unknown as WorkOrder[]
  const stalled: NextAction = {
    label: 'Recover the stalled order',
    actor: 'operator',
    kind: 'stalled',
    judgment: false,
    summary: 'Verification went stale in the queue.',
    deadline: null,
    choices: [],
    current_order: {
      id: 'order-verify-current',
      stage: 'verify',
      state: 'stale',
      queue_deadline: null,
      execution_deadline: null,
      lease_expires_at: null,
    },
    details: { stall_reason: 'stale' },
  }
  await mockAPI(
    page,
    () => activity('queued', stalled, orders),
    () => stalled,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  // The newest order would win a client-side pick; the server named the verify order.
  await expect(page.getByText('Verification paused — recovery needed').first()).toBeVisible()
  await expect(page.getByText('The order was not claimed before its queue deadline.').first()).toBeVisible()
  await expect(page.getByText('Implementation paused — recovery needed')).toHaveCount(0)
  // An operator act that is not a gate decision opens no human gate.
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
})

// Three pending task-context proposals: the next-action card's compact
// checklist is the one place task detail decides them (component-web-task-
// surfaces, Next-action card; feature-operator-next-action NA-6).
const contextTargets = [
  { kind: 'requirement', id: 'req-task-centric-operations-view', title: 'Task-centric operations view' },
  { kind: 'system_design', id: 'component-verification-strategy', title: 'Testing strategy' },
  {
    kind: 'system_design',
    id: 'component-web-task-surfaces',
    title: 'Web task surfaces: board, tasks, and task detail',
  },
] as const

const tierWords: Record<string, string> = { requirement: 'requirement', system_design: 'System Design' }

function contextProposalAction(allowed = true, extra: NextAction['choices'] = []): NextAction {
  return {
    label: 'Context proposal',
    actor: 'operator',
    kind: 'context_proposal',
    judgment: true,
    summary: 'A context proposal is waiting for a decision.',
    question: 'Confirm or dismiss the proposed context?',
    if_ignored: 'The proposal stays pending.',
    deadline: null,
    choices: [
      ...contextTargets.flatMap((target) =>
        (['confirm', 'dismiss'] as const).map((action) => ({
          id: `${action}:task_context:${target.kind}:${target.id}`,
          label: `${action === 'confirm' ? 'Confirm' : 'Dismiss'} ${target.title} (${tierWords[target.kind]})`,
          effect:
            action === 'confirm' ? 'Attaches the proposed context to this task.' : 'Dismisses the proposed context.',
          method: 'POST',
          path: `/v1/tasks/${taskId}/context/proposals/${target.kind}/${target.id}/${action}`,
          capability: 'operate_gates',
          allowed,
        })),
      ),
      ...extra,
    ],
    details: {
      context_proposals: contextTargets.map((target) => ({
        kind: target.kind,
        target_id: target.id,
        target_title: target.title,
        tier_label: tierWords[target.kind],
        justification: `Triage suggests ${target.title} because the change touches its scope.`,
        source: 'triage',
        proposed_by: 'queue:job/dispatch_task:task-1@1',
        created_at: timestamp,
      })),
    },
  }
}

function contextProposalDetail(action: NextAction) {
  const detail = activity('queued', action)
  return {
    ...detail,
    task: {
      ...detail.task,
      context: {
        requirements: [],
        designs: [],
        proposals: contextTargets.map((target) => ({
          task_id: taskId,
          target_kind: target.kind,
          target_id: target.id,
          target_title: target.title,
          state: 'proposed',
          source: 'triage',
          justification: `Triage suggests ${target.title}.`,
          created_by_event_id: 1,
          proposed_by: 'queue',
          workspace: 'demo',
          created_at: timestamp,
          updated_at: timestamp,
        })),
      },
    },
  }
}

const contextPath = (target: (typeof contextTargets)[number], act: 'confirm' | 'dismiss') =>
  `/v1/tasks/${taskId}/context/proposals/${target.kind}/${target.id}/${act}`

test('the next-action card decides task-context proposals through its compact checklist on the sheet', async ({
  page,
}) => {
  const malformed = {
    id: 'confirm:task_context:widget:',
    label: 'Confirm context widget',
    effect: 'A malformed task-context choice.',
    method: 'POST',
    path: `/v1/tasks/${taskId}/context/proposals/widget/x/confirm`,
    capability: 'operate_gates',
    allowed: true,
  }
  const action = contextProposalAction(true, [malformed])
  const posts = await mockAPI(
    page,
    () => contextProposalDetail(action),
    () => action,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const card = sheet.getByRole('region', { name: 'Next action' })
  await expect(card.getByText('A context proposal is waiting for a decision.')).toBeVisible()

  // Suggested context only informs: no Attach or Dismiss of its own.
  const suggestions = sheet.getByRole('region', { name: 'Suggested context' })
  await expect(suggestions.getByRole('listitem')).toHaveCount(3)
  await expect(suggestions.getByRole('button')).toHaveCount(0)

  // The compact checklist replaces the flat choice row: no Confirm or Dismiss
  // button, one checked row per proposal, the justification behind a chevron.
  await expect(card.locator('[data-next-action-choices]')).toHaveCount(0)
  await expect(card.locator('[data-context-checklist="compact"]')).toBeVisible()
  await expect(card.getByRole('button', { name: /^(Confirm|Dismiss) .*\(/ })).toHaveCount(0)
  const rows = card.locator('[data-context-proposal]')
  await expect(rows).toHaveCount(3)
  for (const target of contextTargets)
    await expect(
      card.getByRole('checkbox', { name: `Confirm ${target.title} (${tierWords[target.kind]})` }),
    ).toBeChecked()
  await expect(rows.nth(0).getByRole('button', { name: contextTargets[0].title, exact: true })).toBeVisible()
  await expect(rows.nth(0).getByRole('button', { name: `Preview ${contextTargets[0].title}` })).toBeVisible()
  await expect(rows.nth(0)).not.toContainText('Triage suggests')
  await rows.nth(0).getByRole('button', { name: 'Show why' }).click()
  await expect(rows.nth(0)).toContainText('Triage suggests Task-centric operations view')
  await expect(card.locator('[data-context-source]')).toContainText('Suggested by triage')
  // The proposer appears only inside the IDs disclosure.
  const outside = await card.evaluate((element) => {
    const clone = element.cloneNode(true) as HTMLElement
    for (const details of clone.querySelectorAll('details')) details.remove()
    return clone.textContent ?? ''
  })
  expect(outside).not.toContain('queue:job/dispatch_task')
  await expect(card.getByRole('link', { name: 'Review in Inbox →' })).toHaveAttribute(
    'href',
    `/w/demo/inbox?selected=${taskId}`,
  )
  // A choice naming no listed proposal stays visible with its reason.
  await expect(card.locator('[data-context-unmatched]')).toContainText(
    'Confirm context widget: This page cannot send this choice.',
  )

  // Apply posts each row's selected choice once, body-less, in server order.
  const apply = card.locator('[data-context-apply]')
  await expect(apply).toHaveText('Attach all 3')
  await card.locator(`[data-context-check="system_design:${contextTargets[1].id}"]`).uncheck()
  await expect(apply).toHaveText('Attach 2 · dismiss 1')
  await apply.dblclick()
  await expect.poll(() => posts.length).toBe(3)
  expect(posts).toEqual([
    { method: 'POST', path: contextPath(contextTargets[0], 'confirm'), body: null },
    { method: 'POST', path: contextPath(contextTargets[1], 'dismiss'), body: null },
    { method: 'POST', path: contextPath(contextTargets[2], 'confirm'), body: null },
  ])

  // On the Flow tab the same checklist stays usable from the card.
  await sheet.getByRole('tab', { name: 'Flow' }).click()
  await expect(sheet.getByRole('tab', { name: 'Flow' })).toHaveAttribute('aria-selected', 'true')
  await card.getByRole('button', { name: 'Dismiss all', exact: true }).click()
  await expect.poll(() => posts.length).toBe(6)
  expect(posts.slice(3).map((post) => post.path)).toEqual(
    contextTargets.map((target) => contextPath(target, 'dismiss')),
  )
})

test('task-context choices a caller may not take disable Apply and Dismiss all with the capability reason', async ({
  page,
}) => {
  const action = contextProposalAction(false)
  const posts = await mockAPI(
    page,
    () => contextProposalDetail(action),
    () => action,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const card = page.getByRole('dialog', { name: 'Task detail' }).getByRole('region', { name: 'Next action' })
  await expect(card.locator('[data-context-apply]')).toBeDisabled()
  await expect(card.getByRole('button', { name: 'Dismiss all', exact: true })).toBeDisabled()
  await expect(card.locator('[data-context-control-reason]')).toHaveText('Needs the operate gates capability.')
  expect(posts).toEqual([])
})

// Long titles truncate on their one line and the checklist stays inside the
// card and the viewport on both detail surfaces. The sheet clips overflow, so
// the document width alone would not catch this.
for (const surface of [`/w/demo/tasks/${taskId}`, `/w/demo/tasks/${taskId}/full`]) {
  test(`the compact context checklist fits the next-action card at 390 px on ${surface}`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    const action = contextProposalAction()
    await mockAPI(
      page,
      () => contextProposalDetail(action),
      () => action,
    )
    await page.goto(surface)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.locator('[data-context-proposal]')).toHaveCount(3)
    if (surface === `/w/demo/tasks/${taskId}`) {
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      await expect.poll(() => sheet.evaluate((node) => node.getAnimations().length)).toBe(0)
    }
    const cardBox = (await card.boundingBox())!
    expect(cardBox.x + cardBox.width).toBeLessThanOrEqual(390)
    for (const row of await card.locator('[data-context-proposal]').all()) {
      const box = (await row.boundingBox())!
      expect(box.x).toBeGreaterThanOrEqual(cardBox.x - 0.5)
      expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width + 0.5)
    }
    for (const control of [
      card.locator('[data-context-apply]'),
      card.getByRole('link', { name: 'Review in Inbox →' }),
    ]) {
      const box = (await control.boundingBox())!
      expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width + 0.5)
    }
    // The longest title stays on one line, truncated.
    const longest = card.getByRole('button', { name: contextTargets[2].title, exact: true })
    const lines = await longest.evaluate(
      (node) => node.getBoundingClientRect().height / Number.parseFloat(getComputedStyle(node).lineHeight),
    )
    expect(lines).toBeLessThan(1.5)
  })
}

// Two cached context_proposal tasks on the full route: the reused page keys
// the checklist by workspace and task, so a held send never continues under
// the next task and a return starts all-checked (feature-operator-next-action
// NA-6).
test('switching between cached context-proposal tasks isolates the checklist and its sends', async ({ page }) => {
  const otherId = 'card-task-b'
  const forTask = (id: string): NextAction => {
    const action = contextProposalAction()
    return {
      ...action,
      choices: action.choices.map((entry) => ({ ...entry, path: entry.path.replace(taskId, id) })),
    }
  }
  const detailFor = (id: string) => {
    const detail = contextProposalDetail(forTask(id))
    return { ...detail, task: { ...detail.task, id, title: id === taskId ? 'Card task' : 'Second card task' } }
  }
  const posts: string[] = []
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, request),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    const read = path.match(/^\/v1\/tasks\/([^/]+)\/(next-action|activity)$/)
    if (read) return route.fulfill({ json: read[2] === 'next-action' ? forTask(read[1]) : detailFor(read[1]) })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (request.method() !== 'GET') {
      posts.push(path)
      await held
      return route.fulfill({ json: {} }).catch(() => {})
    }
    return route.fulfill({ json: [] })
  })
  // Client-side navigation keeps both tasks' reads cached and the page mounted.
  const open = async (id: string, title: string) => {
    await page.evaluate((path) => {
      window.history.pushState(window.history.state, '', path)
      window.dispatchEvent(new PopStateEvent('popstate', { state: window.history.state }))
    }, `/w/demo/tasks/${id}/full`)
    await expect(page.getByRole('heading', { name: title }).first()).toBeVisible()
  }
  const card = page.getByRole('region', { name: 'Next action' })
  const apply = card.locator('[data-context-apply]')
  const strategy = card.locator(`[data-context-check="system_design:${contextTargets[1].id}"]`)

  await page.goto(`/w/demo/tasks/${otherId}/full`)
  await expect(apply).toHaveText('Attach all 3')
  await open(taskId, 'Card task')
  await expect(apply).toHaveText('Attach all 3')

  // A -> B -> A without touching B: A starts all-checked again.
  await strategy.uncheck()
  await expect(apply).toHaveText('Attach 2 · dismiss 1')
  await open(otherId, 'Second card task')
  await expect(apply).toHaveText('Attach all 3')
  await open(taskId, 'Card task')
  await expect(apply).toHaveText('Attach all 3')

  // A held send, then a switch: the next task starts idle and all-checked,
  // and the first task's remainder is never sent.
  await apply.click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0]).toBe(`/v1/tasks/${taskId}/context/proposals/requirement/${contextTargets[0].id}/confirm`)
  await open(otherId, 'Second card task')
  await expect(apply).toHaveText('Attach all 3')
  await expect(apply).toBeEnabled()
  await expect(strategy).toBeEnabled()
  await expect(card.locator('[data-context-refusal]')).toHaveCount(0)
  release()
  await page.waitForTimeout(500)
  expect(posts).toHaveLength(1)
})

// The task page shares the inbox's one-row choices: one primary button, and a
// start_over choice opens the restart dialog, whose success here opens the
// successor task (component-web-task-surfaces; task 261021-cd3bf8).
test('a parked next action shows one choice row and Start over opens the successor', async ({ page }) => {
  const parked: NextAction = {
    label: 'Decide on a parked task',
    actor: 'operator',
    kind: 'parked',
    judgment: true,
    summary: 'This task is parked.',
    question: 'Put it back, or close it?',
    if_ignored: 'It stays parked.',
    deadline: null,
    choices: [
      ['recover', 'Recover', 'redispatch', 'recover_work'],
      ['close', 'Close', 'close', 'operate_gates'],
      ['start_over', 'Start over', 'restart', 'operate_gates'],
    ].map(([id, label, route, capability]) => ({
      id,
      label,
      effect: `${label} effect.`,
      method: 'POST',
      path: `/v1/tasks/${taskId}/${route}`,
      capability,
      allowed: true,
    })),
    details: {},
  }
  await mockAPI(
    page,
    () => activity('parked', parked),
    () => parked,
  )
  let restart: Record<string, unknown> | undefined
  await page.route(`**/v1/tasks/${taskId}/restart*`, (route) => {
    restart = route.request().postDataJSON()
    return route.fulfill({ json: { successor: { id: 'T-successor' }, cancelled_task: { id: taskId } } })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const row = card.locator('[data-next-action-choices] > div').first()
  await expect(row.getByRole('button')).toHaveText(['Recover', 'Close', 'Start over'])
  await expect(row.locator('button[class*="bg-primary"]')).toHaveCount(1)
  await expect(row.getByRole('button', { name: 'Recover' })).toHaveClass(/bg-primary/)

  await row.getByRole('button', { name: 'Start over', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Start over', exact: true })
  await dialog.getByLabel(/Reason/).fill('Scope changed')
  await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
  await expect(page).toHaveURL(/\/tasks\/T-successor\/full/)
  expect(restart).toMatchObject({ reason: 'Scope changed' })
})

// The compact rows open the shared document preview panel over the task
// sheet; Escape closes only the panel and returns focus to the opener
// (feature-operator-next-action NA-6; component-web-task-surfaces).
test('a compact row opens the document preview over the task sheet, and Escape closes only the panel', async ({
  page,
}) => {
  const action = contextProposalAction()
  const posts = await mockAPI(
    page,
    () => contextProposalDetail(action),
    () => action,
  )
  await page.route(`**/v1/system-designs/${contextTargets[1].id}*`, (route) =>
    route.fulfill({
      json: {
        document: { id: contextTargets[1].id, title: contextTargets[1].title },
        current_version: {
          version: 33,
          content: '# Testing strategy\n\n## Scope\n\nScope text.\n\n## Evidence\n\nEvidence text.',
          confirmed_at: timestamp,
        },
        pending_versions: [],
        versions: [],
      },
    }),
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const card = sheet.getByRole('region', { name: 'Next action' })
  const opener = card.getByRole('button', { name: `Preview ${contextTargets[1].title}` })
  await opener.click()
  const panel = page.getByRole('dialog', { name: `Preview: ${contextTargets[1].title}` })
  await expect(panel.locator('[data-preview-version]')).toContainText('v33')
  await expect(panel.locator('[data-outline-entry]')).toHaveText(['Testing strategy', 'Scope', 'Evidence'])
  await expect(panel.locator('[data-preview-position]')).toHaveText('2 of 3 proposed')
  await expect(card.locator('[data-context-proposal]').nth(1)).toHaveAttribute('data-current', 'true')
  await panel.locator('[data-preview-attach]').uncheck()
  await expect(card.locator('[data-context-apply]')).toHaveText('Attach 2 · dismiss 1')

  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
  await expect(sheet).toBeVisible()
  await expect(opener).toBeFocused()

  // The compact title opens the same panel.
  await card.getByRole('button', { name: contextTargets[1].title, exact: true }).click()
  await expect(page.getByRole('dialog', { name: `Preview: ${contextTargets[1].title}` })).toBeVisible()
  expect(posts).toEqual([])
})

// A parked task's next-action card says why triage parked it, from the bounded
// detail's summaries.park_reason, and its server Recover choice is the task
// detail's one button for the redispatch route: the timeline adds no second
// card for the same act (DEC-61; req-operator-next-action AC-1.4, AC-1.7;
// component-web-task-surfaces invariant 25; task 261108-1ae968).
function parkedWithRecover(): NextAction {
  return {
    label: 'Decide on a parked task',
    actor: 'operator',
    kind: 'parked',
    judgment: true,
    summary: 'This task is parked.',
    question: 'Put it back, or close it?',
    if_ignored: 'It stays parked.',
    deadline: null,
    choices: [
      ['recover', 'Recover', 'redispatch', 'recover_work'],
      ['close', 'Close', 'close', 'operate_gates'],
      ['start_over', 'Start over', 'restart', 'operate_gates'],
    ].map(([id, label, route, capability]) => ({
      id,
      label,
      effect: `${label} effect.`,
      method: 'POST',
      path: `/v1/tasks/${taskId}/${route}`,
      capability,
      allowed: true,
    })),
    details: {},
  }
}

// What triage recorded when it parked the task; the fake server folds it into
// summaries.park_reason. The event page itself carries no park facts.
const parkedTriage = {
  id: '9007199254741003',
  task_id: taskId,
  kind: 'triage.completed',
  job_id: `${taskId}-triage`,
  at: '2026-10-12T09:30:00Z',
  payload: {
    route: 'parked',
    summary: 'The task names no repository surface to change.',
    brief: {
      questions: ['Which dashboard page shows the duplicate?', '  '],
      risks: ['Guessing the surface could edit an unrelated page.'],
    },
  },
}

for (const surface of [
  { name: 'Board sheet', url: `/w/demo/tasks/${taskId}` },
  { name: 'full task page', url: `/w/demo/tasks/${taskId}/full` },
]) {
  test(`a parked task on the ${surface.name} shows the park reason and one redispatch button`, async ({ page }) => {
    const action = parkedWithRecover()
    const posts = await mockAPI(
      page,
      () => {
        const item = activity('parked', action)
        return { ...item, events: [...item.events, parkedTriage] }
      },
      () => action,
    )
    await page.goto(surface.url)
    const card = page.getByRole('region', { name: 'Next action' })
    const reason = card.getByRole('region', { name: 'Why it is parked' })
    await expect(reason).toContainText('Triage summary')
    await expect(reason).toContainText('The task names no repository surface to change.')
    await expect(reason.getByRole('region', { name: 'Open questions' }).getByRole('listitem')).toHaveText([
      'Which dashboard page shows the duplicate?',
    ])
    await expect(reason.getByRole('region', { name: 'Risks' }).getByRole('listitem')).toHaveText([
      'Guessing the surface could edit an unrelated page.',
    ])
    // The catalogue question still follows the recorded facts.
    await expect(card.getByText('Put it back, or close it?')).toBeVisible()

    // One button posts the redispatch route: the server's Recover choice.
    await expect(page.getByRole('button', { name: /^(Recover|Redispatch|Resume task)$/ })).toHaveCount(1)
    await expect(
      page.getByRole('region', { name: 'Execution event timeline' }).getByRole('button', { name: /Redispatch|Resume/ }),
    ).toHaveCount(0)
    await card.getByRole('button', { name: 'Recover', exact: true }).click()
    await expect
      .poll(() => posts.map((post) => `${post.method} ${post.path}`))
      .toEqual([`POST /v1/tasks/${taskId}/redispatch`])
  })
}

test('a parked task without a recorded park reason says triage recorded none', async ({ page }) => {
  const action = parkedWithRecover()
  await mockAPI(
    page,
    () => activity('parked', action),
    () => action,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.locator('[data-park-reason]')).toHaveText('Triage recorded no reason.')
  await expect(card.getByRole('region', { name: 'Why it is parked' })).toHaveCount(0)
})

test('a stalled task offers redispatch only as the server choice', async ({ page }) => {
  const action: NextAction = {
    label: 'Stalled',
    actor: 'operator',
    kind: 'stalled',
    judgment: false,
    summary: 'No agent claimed this task in time.',
    deadline: null,
    choices: [
      {
        id: 'redispatch',
        label: 'Redispatch',
        effect: 'Puts the task back in the queue.',
        method: 'POST',
        path: `/v1/tasks/${taskId}/redispatch`,
        capability: 'recover_work',
        allowed: true,
      },
    ],
    details: { stall_reason: 'unclaimable' },
  }
  await mockAPI(
    page,
    () =>
      activity('queued', action, [
        {
          id: 'order-implement-1',
          task_id: taskId,
          job_id: 'order-implement-1',
          stage: 'implement',
          state: 'queued',
          claimable: false,
          queue_entered_at: timestamp,
          created_at: timestamp,
          updated_at: timestamp,
        } as WorkOrder,
      ]),
    () => action,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: /^(Recover|Redispatch|Resume task)$/ })).toHaveCount(1)
  await expect(page.locator('[data-park-reason]')).toHaveCount(0)
})

// An enforcing task short of its required human approvals (req-risk-based-review
// AC-8.2, AC-8.7, AC-11.1, AC-11.2; feature-risk-based-review RR-7). The
// server labels the merge choice Approve while more than one approval remains;
// the card states the count beside it, and the confirm says the act records
// the person's approval and merges nothing.
function approvalAction(label: 'Approve' | 'Merge'): NextAction {
  const action = heldMergeAction('merge_execution')
  return {
    ...action,
    summary:
      label === 'Approve'
        ? "The reviewed head has 0 of 2 required human approvals. 2 more approvals are needed, including a trusted author's approval."
        : "The reviewed head has 1 of 2 required human approvals. 1 more approval is needed. A trusted author's approval is not among them.",
    choices: action.choices.map((choice) =>
      choice.id === 'merge'
        ? {
            ...choice,
            label,
            effect:
              label === 'Approve'
                ? "Records your approval. It does not merge, because the merge still needs 2 more approvals, including a trusted author's approval."
                : 'Records your approval and merges the reviewed head when your approval completes the required approvals.',
          }
        : choice,
    ),
  }
}

function approvalActivity(label: 'Approve' | 'Merge') {
  return {
    ...heldMergeActivity('merge_execution'),
    risk: {
      mode: 'enforce' as const,
      tier: 'high' as const,
      source: 'reviewer' as const,
      reason: 'The merge gate gains counted approvals on every store.',
      head: approvedHead,
      required_agent_reviews: 2,
      required_human_approvals: 2,
      calculated_agent_reviews: 2,
      calculated_human_approvals: 2,
      recorded_approvals: label === 'Approve' ? 0 : 1,
      author_approval_needed: label === 'Approve',
      remaining_approvals: label === 'Approve' ? 2 : 1,
      non_author_approval_needed: false,
      untrusted_author: false,
      approval_head: approvedHead,
      approvals_satisfied: false,
      ratings: [],
    },
  }
}

test('an enforcing task short of its approvals offers Approve and states the count', async ({ page }) => {
  await mockAPI(
    page,
    () => approvalActivity('Approve'),
    () => approvalAction('Approve'),
  )
  const posts: string[] = []
  await page.route(`**${heldMergePath}**`, (route) => {
    posts.push(route.request().method())
    return route.fulfill({ status: 202, json: { error: 'approvals_required', recorded: true, merged: false } })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.locator('[data-approval-count]')).toHaveText(
    "0 of 2 human approvals recorded. 2 more needed, including a trusted author's.",
  )
  // The task's tier chip holds the same count behind it.
  const riskChip = page.locator('[data-task-risk]').first().getByRole('button')
  await riskChip.focus()
  await expect(page.locator('[data-task-risk-approvals]').first()).toHaveText(
    "0 of 2 human approvals recorded. 2 more needed, including a trusted author's.",
  )
  await riskChip.blur()
  const approve = gate.locator('[data-merge-at-gate]')
  await expect(approve).toHaveText('Approve')
  await approve.click()
  const dialog = page.getByRole('dialog', { name: 'Confirm approval' })
  await expect(dialog.getByRole('heading')).toHaveText('Approve #77?')
  await expect(dialog.locator('[data-confirm-head]')).toContainText(
    `This records your approval of #77 at the approved head ${approvedHead.slice(0, 7)} under your name. It does not merge, because the merge still needs 2 more approvals, including a trusted author's approval.`,
  )
  await expect(dialog.locator('[data-confirm-merge]')).toHaveText('Approve #77')
  expect(posts).toEqual([])
  await dialog.locator('[data-confirm-merge]').click()
  await expect(dialog).toHaveCount(0)
  expect(posts).toEqual(['POST'])
})

test('the last approval keeps the merge label and its one-act confirm', async ({ page }) => {
  await mockAPI(
    page,
    () => approvalActivity('Merge'),
    () => approvalAction('Merge'),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.locator('[data-approval-count]')).toHaveText('1 of 2 human approvals recorded. 1 more needed.')
  await expect(gate.locator('[data-merge-at-gate]')).toHaveText('Merge')
  await gate.locator('[data-merge-at-gate]').click()
  await expect(page.getByRole('dialog', { name: 'Confirm merge' }).getByRole('heading')).toHaveText('Merge #77?')
})

// Screenshots of the approval count and the Approve control, attached to the
// Playwright report that CI uploads, and written to CONVEYOR_SCREENSHOT_DIR when
// it is set.
for (const viewport of [
  { name: 'mobile', width: 375, height: 1400 },
  { name: 'desktop', width: 1280, height: 1000 },
]) {
  test(`capture the approval count at ${viewport.width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    await mockAPI(
      page,
      () => approvalActivity('Approve'),
      () => approvalAction('Approve'),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    await expect(gate.locator('[data-approval-count]')).toBeVisible()
    await gate.scrollIntoViewIfNeeded()
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    const name = `merge-gate-approvals-${viewport.name}.png`
    const image = await page.screenshot({ path: shots ? `${shots}/${name}` : undefined, fullPage: true })
    await testInfo.attach(name, { body: image, contentType: 'image/png' })
    await gate.locator('[data-merge-at-gate]').click()
    await expect(page.getByRole('dialog', { name: 'Confirm approval' })).toBeVisible()
    const confirmName = `merge-gate-approve-confirm-${viewport.name}.png`
    // The dialog animates in; a finished animation shows it as a person sees it.
    const confirm = await page.screenshot({
      path: shots ? `${shots}/${confirmName}` : undefined,
      animations: 'disabled',
    })
    await testInfo.attach(confirmName, { body: confirm, contentType: 'image/png' })
  })
}

// The merge control offers only an act the caller can take
// (req-risk-based-review AC-8.7, AC-11.1, REQ-9). When the caller's approval
// cannot count or cannot complete the merge, the server withholds the merge
// choice with code approval_not_possible and the card states its reason in
// place of the control, so no merge confirmation opens. When the caller's
// approval counts but leaves the merge short, the control is Approve and its
// confirmation states what is still needed.
function approvalState(risk: Partial<NonNullable<ReturnType<typeof approvalActivity>['risk']>>) {
  const item = approvalActivity('Merge')
  return { ...item, risk: { ...item.risk, ...risk } }
}

function withheldAction(summary: string, refusal: string): NextAction {
  const action = heldMergeAction('merge_execution')
  return {
    ...action,
    summary,
    choices: action.choices.filter((choice) => choice.id !== 'merge'),
    details: { ...action.details, refusals: [{ choice: 'merge', text: refusal, code: 'approval_not_possible' }] },
  }
}

const waiverOnly = {
  risk: {
    required_human_approvals: 1,
    recorded_approvals: 1,
    remaining_approvals: 0,
    author_approval_needed: false,
    non_author_approval_needed: false,
    untrusted_author: true,
    author_waiver_needed: true,
  },
  count: "1 of 1 human approval recorded. A person who is not an author must waive the trusted author's approval.",
  summary:
    "The reviewed head has 1 of 1 required human approvals. No author's approval counts while an author is not trusted, so a person who is not an author must waive the trusted author's approval.",
  refusal:
    "No approval can complete the merge now. A person who is not an author must first waive the trusted author's approval.",
}

const oneShortTrustedAuthor = {
  risk: {
    required_human_approvals: 2,
    recorded_approvals: 1,
    remaining_approvals: 1,
    author_approval_needed: true,
    non_author_approval_needed: false,
    author_waiver_needed: false,
  },
  count: "1 of 2 human approvals recorded. 1 more needed, including a trusted author's.",
  summary:
    "The reviewed head has 1 of 2 required human approvals. 1 more approval is needed, including a trusted author's approval.",
}

const states = [
  {
    name: 'waiver-only',
    detail: () => approvalState(waiverOnly.risk),
    action: () => withheldAction(waiverOnly.summary, waiverOnly.refusal),
    count: waiverOnly.count,
    refusal: waiverOnly.refusal,
  },
  {
    name: 'already-counted',
    detail: () => approvalState(oneShortTrustedAuthor.risk),
    action: () =>
      withheldAction(
        oneShortTrustedAuthor.summary,
        "Your approval of this head is already recorded. The merge still needs 1 more approval, including a trusted author's approval.",
      ),
    count: oneShortTrustedAuthor.count,
    refusal:
      "Your approval of this head is already recorded. The merge still needs 1 more approval, including a trusted author's approval.",
  },
  {
    name: 'coauthor-beside-untrusted',
    detail: () =>
      approvalState({
        ...oneShortTrustedAuthor.risk,
        required_human_approvals: 1,
        recorded_approvals: 0,
        author_approval_needed: false,
        non_author_approval_needed: true,
        untrusted_author: true,
        author_waiver_needed: true,
      }),
    action: () =>
      withheldAction(
        'The reviewed head has 0 of 1 required human approvals. 1 more approval is needed, including an approval from a person who is not an author.',
        "Your approval does not count, because you are an author and another author is not trusted. The merge still needs 1 more approval, including an approval from a person who is not an author, and a waiver of the trusted author's approval by a person who is not an author.",
      ),
    count:
      "0 of 1 human approval recorded. 1 more needed, including one from a person who is not an author. A person who is not an author must waive the trusted author's approval.",
    refusal:
      "Your approval does not count, because you are an author and another author is not trusted. The merge still needs 1 more approval, including an approval from a person who is not an author, and a waiver of the trusted author's approval by a person who is not an author.",
  },
]

for (const state of states) {
  test(`a withheld merge states why and opens no confirmation: ${state.name}`, async ({ page }) => {
    const posts = await mockAPI(page, state.detail, state.action)
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    await expect(gate.locator('[data-approval-refusal]')).toHaveText(state.refusal)
    await expect(gate.locator('[data-approval-count]')).toHaveText(state.count)
    await expect(gate.locator('[data-merge-at-gate]')).toHaveCount(0)
    await expect(gate.getByRole('button', { name: /^(Merge|Approve)$/ })).toHaveCount(0)
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(posts).toEqual([])
  })
}

// A non-author whose approval counts while the trusted author's approval is
// still needed gets Approve, and the confirmation says it does not merge.
function wrongAuthorAction(): NextAction {
  const action = heldMergeAction('merge_execution')
  return {
    ...action,
    summary: oneShortTrustedAuthor.summary,
    choices: action.choices.map((choice) =>
      choice.id === 'merge'
        ? {
            ...choice,
            label: 'Approve',
            effect:
              "Records your approval. It does not merge, because the merge still needs 1 more approval, including a trusted author's approval.",
          }
        : choice,
    ),
  }
}

test('a non-author short of the trusted author gets Approve with the reason', async ({ page }) => {
  await mockAPI(
    page,
    () => approvalState(oneShortTrustedAuthor.risk),
    () => wrongAuthorAction(),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.locator('[data-merge-at-gate]')).toHaveText('Approve')
  await gate.locator('[data-merge-at-gate]').click()
  const dialog = page.getByRole('dialog', { name: 'Confirm approval' })
  await expect(dialog.locator('[data-confirm-head]')).toContainText(
    "under your name. It does not merge, because the merge still needs 1 more approval, including a trusted author's approval.",
  )
  await expect(dialog.locator('[data-confirm-merge]')).toHaveText('Approve #77')
})

// Screenshots of the changed states at 375 px and 1280 px, attached to the
// Playwright report and written to CONVEYOR_SCREENSHOT_DIR when it is set.
for (const viewport of [
  { name: 'mobile', width: 375, height: 1400 },
  { name: 'desktop', width: 1280, height: 1000 },
]) {
  test(`capture the withheld and approve-only merge states at ${viewport.width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    const shoot = async (name: string, full = true) => {
      const image = await page.screenshot({
        path: shots ? `${shots}/${name}` : undefined,
        fullPage: full,
        animations: 'disabled',
      })
      await testInfo.attach(name, { body: image, contentType: 'image/png' })
    }
    for (const state of states.slice(0, 2)) {
      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockAPI(page, state.detail, state.action)
      await page.goto(`/w/demo/tasks/${taskId}/full`)
      await expect(page.getByRole('region', { name: 'Human gate' }).locator('[data-approval-refusal]')).toBeVisible()
      await shoot(`merge-gate-${state.name}-${viewport.name}.png`)
    }
    await page.unrouteAll({ behavior: 'ignoreErrors' })
    await mockAPI(
      page,
      () => approvalState(oneShortTrustedAuthor.risk),
      () => wrongAuthorAction(),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    await gate.locator('[data-merge-at-gate]').click()
    await expect(page.getByRole('dialog', { name: 'Confirm approval' })).toBeVisible()
    await shoot(`merge-gate-non-author-approve-${viewport.name}.png`, false)
  })
}

// At the merge_approval gate the same refusal replaces the one-act merge and
// its readiness line, and neither Merge nor Approve only is offered.
test('the merge_approval gate withholds the merge when only a waiver can complete it', async ({ page }) => {
  const action = mergeApproval('MERGEABLE')
  const withheld: NextAction = {
    ...action,
    summary: waiverOnly.summary,
    choices: action.choices.filter((choice) => choice.id !== 'merge' && choice.id !== 'approve'),
    details: {
      ...action.details,
      refusals: [
        { choice: 'merge', text: waiverOnly.refusal, code: 'approval_not_possible' },
        { choice: 'approve', text: waiverOnly.refusal, code: 'approval_not_possible' },
      ],
    },
  }
  await mockAPI(
    page,
    () => ({ ...mergeGateActivity('MERGEABLE'), risk: approvalState(waiverOnly.risk).risk }),
    () => withheld,
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.locator('[data-approval-refusal]')).toHaveText(waiverOnly.refusal)
  await expect(gate.locator('[data-merge-at-gate]')).toHaveCount(0)
  await expect(gate.locator('[data-merge-status]')).toHaveCount(0)
  await expect(gate.locator('[data-choice="approve"]')).toHaveCount(0)
})

// Row 11 states the order's disposition set, and the card lists every entry
// with an Applied and a Not applied control, preselects none, takes one
// reason, and sends one request (component-next-action invariant 15;
// component-verification-service invariant 45; DEC-133(1)).
const dispositionSet = [
  {
    context_id: 'ctx-a',
    run_id: 'run-login-1',
    operation_ids: [],
    input_digest: 'sha256:login',
    subject: 'kit:staging-fixtures/login',
    state: 'blocked',
  },
  {
    context_id: 'ctx-b',
    run_id: 'run-checkout-2',
    operation_ids: ['op-charge'],
    input_digest: 'sha256:checkout',
    subject: 'ordinary:checkout-charges-card',
    state: 'running',
  },
]

function dispositionAction(
  dispositions: typeof dispositionSet | null = dispositionSet,
  allowed = true,
  refusal = '',
): NextAction {
  return {
    label: 'Confirm an outcome',
    actor: 'operator',
    kind: 'verification_disposition',
    judgment: true,
    summary: 'Verification needs a disposition.',
    question: 'Did the change apply? The verify agent could not observe the result.',
    if_ignored: 'The verify order stays at the checkpoint.',
    deadline: null,
    choices: [
      {
        id: 'record_dispositions',
        label: 'Record dispositions…',
        effect: 'Records whether each change applied, then recovers the order.',
        method: 'POST',
        path: '/v1/work-orders/card-task-verify-1/recover',
        capability: 'operate_gates',
        allowed,
      },
    ],
    current_order: {
      id: 'card-task-verify-1',
      stage: 'verify',
      state: 'queued',
      queue_deadline: null,
      execution_deadline: null,
      lease_expires_at: null,
    },
    details: {
      ...(dispositions ? { dispositions } : {}),
      ...(refusal ? { refusals: [{ choice: 'record_dispositions', text: refusal }] } : {}),
    },
  }
}

function dispositionCard(page: Page) {
  const card = page.getByRole('region', { name: 'Next action' })
  return {
    card,
    open: () => card.getByRole('button', { name: 'Record dispositions…' }),
    send: () =>
      card.locator('[data-next-action-choices]').getByRole('button', { name: 'Record dispositions', exact: true }),
    entry: (subject: string) => card.getByRole('group', { name: subject }),
    reason: () => card.getByRole('textbox', { name: 'Reason' }),
  }
}

test('the disposition card lists every attempt without a preselected answer and sends one request', async ({
  page,
}) => {
  const posts = await mockAPI(
    page,
    () => activity('running', dispositionAction()),
    () => dispositionAction(),
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const d = dispositionCard(page)
  await d.open().click()
  const entries = d.card.locator('[data-disposition-entry]')
  await expect(entries).toHaveCount(2)
  await expect(entries.nth(0)).toContainText('kit:staging-fixtures/login')
  await expect(entries.nth(0)).toContainText('blocked · no unresolved operations · run run-login-1')
  await expect(entries.nth(1)).toContainText('ordinary:checkout-charges-card')
  await expect(entries.nth(1)).toContainText('running · 1 unresolved operation · run run-checkout-2')
  // No answer is preselected, so Send waits for an answer to each attempt
  // and for the reason.
  await expect(d.card.getByRole('radio', { checked: true })).toHaveCount(0)
  await expect(d.send()).toBeDisabled()
  await d.entry('kit:staging-fixtures/login').getByRole('radio', { name: 'Not applied' }).check()
  await d.reason().fill('Fixture account restored; the charge never reached the provider.')
  await expect(d.card.getByText('1 of 2 answered.')).toBeVisible()
  await expect(d.send()).toBeDisabled()
  await d.entry('ordinary:checkout-charges-card').getByRole('radio', { name: 'Applied', exact: true }).check()
  await expect(d.card.getByText('2 of 2 answered.')).toBeVisible()
  // A double click sends once.
  await d.send().dblclick()
  await expect.poll(() => posts.length).toBe(1)
  await page.waitForTimeout(200)
  expect(posts).toHaveLength(1)
  expect(posts[0].method).toBe('POST')
  expect(posts[0].path).toBe('/v1/work-orders/card-task-verify-1/recover')
  const body = posts[0].body as { request_id: string; verification: unknown }
  expect(body.request_id).toBeTruthy()
  expect(body.verification).toEqual({
    reason: 'Fixture account restored; the charge never reached the provider.',
    dispositions: [
      {
        context_id: 'ctx-a',
        run_id: 'run-login-1',
        operation_ids: [],
        input_digest: 'sha256:login',
        disposition: 'not_applied',
      },
      {
        context_id: 'ctx-b',
        run_id: 'run-checkout-2',
        operation_ids: ['op-charge'],
        input_digest: 'sha256:checkout',
        disposition: 'applied',
      },
    ],
  })
})

test('closing the disposition composer or a new set clears every answer', async ({ page }) => {
  let action = dispositionAction()
  const posts = await mockAPI(
    page,
    () => activity('running', action),
    () => action,
  )
  // The task moved under the operator: the server refuses the first send, and
  // the refetched statement names another attempt.
  let refused = 0
  await page.route('**/v1/work-orders/card-task-verify-1/recover**', async (route) => {
    refused++
    action = dispositionAction([
      dispositionSet[0],
      { ...dispositionSet[1], run_id: 'run-checkout-3', state: 'blocked', operation_ids: [] },
    ])
    await route.fulfill({ status: 409, body: 'run run-checkout-2 is not in the disposition set' })
  })
  await page.goto(`/w/demo/tasks/${taskId}`)
  const d = dispositionCard(page)
  await d.open().click()
  await d.entry('kit:staging-fixtures/login').getByRole('radio', { name: 'Applied', exact: true }).check()
  // Cancel discards the answers; opening again starts with none.
  await d.card.getByRole('button', { name: 'Cancel' }).click()
  await d.open().click()
  await expect(d.card.getByRole('radio', { checked: true })).toHaveCount(0)

  await d.entry('kit:staging-fixtures/login').getByRole('radio', { name: 'Applied', exact: true }).check()
  await d.entry('ordinary:checkout-charges-card').getByRole('radio', { name: 'Applied', exact: true }).check()
  await d.reason().fill('Both changes applied.')
  await d.send().click()
  await expect.poll(() => refused).toBe(1)
  await expect(d.card.getByText('run run-checkout-2 is not in the disposition set')).toBeVisible()
  await expect(d.card.locator('[data-disposition-entry="run-checkout-3"]')).toBeVisible()
  await expect(d.card.getByRole('radio', { checked: true })).toHaveCount(0)
  await expect(d.send()).toBeDisabled()
  expect(posts).toHaveLength(0)
})

test('the disposition choice needs operate gates and a stated set', async ({ page }) => {
  let action = dispositionAction(dispositionSet, false)
  const posts = await mockAPI(
    page,
    () => activity('running', action),
    () => action,
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const d = dispositionCard(page)
  await expect(d.open()).toBeDisabled()
  await expect(d.card.locator('[data-choice-reason="record_dispositions"]')).toHaveText(
    'Record dispositions…: Needs the operate gates capability.',
  )

  action = dispositionAction(
    null,
    true,
    'verify order card-task-verify-1 has no unresolved operations and no retry-gated attempts to dispose',
  )
  await page.reload()
  await expect(d.open()).toBeDisabled()
  await expect(d.card.locator('[data-choice-reason="record_dispositions"]')).toHaveText(
    'Record dispositions…: verify order card-task-verify-1 has no unresolved operations and no retry-gated attempts to dispose',
  )
  expect(posts).toHaveLength(0)
})

for (const viewport of [
  { name: 'mobile', width: 375, height: 1200 },
  { name: 'desktop', width: 1280, height: 1000 },
]) {
  test(`capture the disposition card with two entries at ${viewport.width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    await mockAPI(
      page,
      () => activity('running', dispositionAction()),
      () => dispositionAction(),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const d = dispositionCard(page)
    await d.open().click()
    await d.entry('kit:staging-fixtures/login').getByRole('radio', { name: 'Not applied' }).check()
    const entries = d.card.locator('[data-disposition-entry]')
    await expect(entries).toHaveCount(2)
    // Every entry and its controls fit the card at this width.
    const cardBox = await d.card.boundingBox()
    for (const box of await Promise.all((await entries.all()).map((entry) => entry.boundingBox()))) {
      expect(box && cardBox && box.x + box.width <= cardBox.x + cardBox.width + 1).toBeTruthy()
    }
    await d.card.scrollIntoViewIfNeeded()
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    const name = `disposition-card-two-entries-${viewport.name}.png`
    const image = await d.card.screenshot({ path: shots ? `${shots}/${name}` : undefined, animations: 'disabled' })
    await testInfo.attach(name, { body: image, contentType: 'image/png' })
  })
}

// A verify order released at its sealed verification checkpoint states
// verification_disposition with the decision request in details.checkpoint.
// The card prints that request above the disposition entries, and the form
// sends the same one request (component-next-action invariant 15; DEC-133(1)).
const sealedCheckpointRequest =
  'Two exercises need an operator\nRequired operator action: Restore the fixtures and authorize one retry of each'

function sealedCheckpointAction(): NextAction {
  const action = dispositionAction()
  return { ...action, details: { ...action.details, checkpoint: sealedCheckpointRequest } }
}

for (const viewport of [
  { name: 'mobile', width: 375, height: 1400 },
  { name: 'desktop', width: 1280, height: 1100 },
]) {
  test(`the sealed checkpoint card shows the decision request above two entries at ${viewport.width} px`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    const posts = await mockAPI(
      page,
      () => activity('running', sealedCheckpointAction()),
      () => sealedCheckpointAction(),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const d = dispositionCard(page)
    const request = d.card.getByRole('region', { name: 'Verification checkpoint request' })
    await expect(request).toBeVisible()
    await expect(request).toContainText('Two exercises need an operator')
    await expect(request).toContainText(
      'Required operator action: Restore the fixtures and authorize one retry of each',
    )
    await d.open().click()
    const entries = d.card.locator('[data-disposition-entry]')
    await expect(entries).toHaveCount(2)
    await expect(d.card.getByRole('radio', { checked: true })).toHaveCount(0)
    // The request sits above the first entry, and every entry fits the card.
    const requestBox = await request.boundingBox()
    const firstBox = await entries.first().boundingBox()
    expect(requestBox && firstBox && requestBox.y + requestBox.height <= firstBox.y).toBeTruthy()
    const cardBox = await d.card.boundingBox()
    for (const box of [requestBox, ...(await Promise.all((await entries.all()).map((entry) => entry.boundingBox())))]) {
      expect(box && cardBox && box.x + box.width <= cardBox.x + cardBox.width + 1).toBeTruthy()
    }
    await d.card.scrollIntoViewIfNeeded()
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    const name = `sealed-checkpoint-card-${viewport.name}.png`
    const image = await d.card.screenshot({ path: shots ? `${shots}/${name}` : undefined, animations: 'disabled' })
    await testInfo.attach(name, { body: image, contentType: 'image/png' })

    await d.entry('kit:staging-fixtures/login').getByRole('radio', { name: 'Not applied' }).check()
    await d.entry('ordinary:checkout-charges-card').getByRole('radio', { name: 'Applied', exact: true }).check()
    await d.reason().fill('Fixtures restored.')
    await d.send().click()
    await expect.poll(() => posts.length).toBe(1)
    expect(posts[0].path).toBe('/v1/work-orders/card-task-verify-1/recover')
    const body = posts[0].body as { verification: { reason: string; dispositions: { run_id: string }[] } }
    expect(body.verification.reason).toBe('Fixtures restored.')
    expect(body.verification.dispositions.map((entry) => entry.run_id)).toEqual(['run-login-1', 'run-checkout-2'])
  })
}

test('the disposition card without a decision request shows no request box', async ({ page }) => {
  await mockAPI(
    page,
    () => activity('running', dispositionAction()),
    () => dispositionAction(),
  )
  await page.goto(`/w/demo/tasks/${taskId}`)
  const d = dispositionCard(page)
  await expect(d.open()).toBeVisible()
  await expect(d.card.locator('[data-checkpoint-request]')).toHaveCount(0)
})
