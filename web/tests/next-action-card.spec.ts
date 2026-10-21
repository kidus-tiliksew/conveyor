import { expect, type Page } from '@playwright/test'
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

async function mockAPI(page: Page, detail: () => object, action: () => NextAction) {
  const posts: { method: string; path: string; body: unknown }[] = []
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === `/v1/tasks/${taskId}/next-action`) return route.fulfill({ json: action() })
    if (path === `/v1/tasks/${taskId}/activity`) return route.fulfill({ json: detail() })
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/activity' || path === '/v1/task-operations')
      return route.fulfill({
        json: [detail()],
        headers: { 'X-Conveyor-Total': '1', 'X-Conveyor-Limit': '50', 'X-Conveyor-Offset': '0' },
      })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
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

for (const surface of [`/tasks/${taskId}`, `/tasks/${taskId}/full`]) {
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
    await expect(card.getByText('Does this plan do what the task asks?')).toBeVisible()
    await expect(card.getByText('If ignored: Implementation never starts.')).toBeVisible()
    await expect(card.locator('[data-next-action-deadline] time')).toHaveAttribute('datetime', '2026-10-13T10:00:00Z')

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

    // Show IDs discloses the server's identifiers verbatim.
    const ids = card.locator('details')
    await expect(ids.getByText('order-spec-7')).toBeHidden()
    await ids.getByText('Show IDs').click()
    await expect(ids.getByText(/"id": "order-spec-7"/)).toBeVisible()
    await expect(ids.getByText(/"reason_code": "gate.plan"/)).toBeVisible()

    await choices.getByText('Approve plan').click()
    await expect.poll(() => posts.length).toBe(1)
    expect(posts[0].method).toBe('PUT')
    expect(posts[0].path).toBe(`/v1/tasks/${taskId}/review?via=next-action`)
    expect(posts[0].body).toMatchObject({ action: 'approve' })
  })
}

// A running agent attempt reads as one box: the server's statement with the
// current order's execution state folded in, its empty-state rows omitted, and
// the stop and client-activity affordances printed once (DEC-61;
// component-web-task-surfaces, Next-action card).
const runningOrder = {
  id: 'order-impl-running',
  task_id: taskId,
  job_id: 'job-impl-running',
  stage: 'implement',
  state: 'claimed',
  claimable: false,
  attempt_id: 'attempt-running',
  claimed_by: 'run:operator',
  created_at: timestamp,
  queue_entered_at: timestamp,
  updated_at: timestamp,
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
  summary: 'An agent is working on this task.',
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

for (const surface of [
  { name: 'Board sheet', path: `/tasks/${taskId}`, detail: 'dialog', label: 'Task detail' },
  { name: 'full route', path: `/tasks/${taskId}/full`, detail: 'region', label: 'Task content' },
] as const) {
  test(`a running attempt on the ${surface.name} shows one next-action box above Activity at 390 px`, async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await mockAPI(
      page,
      () => activity('running', workingAction, [runningOrder]),
      () => workingAction,
    )
    await page.goto(surface.path)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card).toHaveCount(1)

    // The server's statement: label and actor, the summary once as the box's
    // one headline, what happens if ignored, and the deadline.
    await expect(card.getByText('Working', { exact: true })).toBeVisible()
    await expect(card.locator('[data-actor]')).toHaveText('agent')
    await expect(card.getByRole('heading', { name: 'An agent is working on this task.' })).toBeVisible()
    // The Board behind the sheet keeps the summary in its card tooltip, so the
    // count is taken over task detail.
    const detail = page.getByRole(surface.detail, { name: surface.label })
    await expect(detail.getByText('An agent is working on this task.', { exact: true })).toHaveCount(1)
    await expect(card.getByText('If ignored: The attempt continues.')).toBeVisible()
    await expect(card.locator('[data-next-action-deadline] time')).toHaveAttribute('datetime', '2026-10-12T14:00:00Z')
    await expect(card.getByText('Implementation · Progressing', { exact: true })).toBeVisible()

    // The execution state adds nothing the summary already says: no derived
    // title, no empty-state rows, and no second current-state section.
    await expect(page.getByText('Implementation is in progress')).toHaveCount(0)
    await expect(page.locator('#current-execution-title')).toHaveCount(0)
    await expect(page.locator('section[aria-labelledby="current-execution-title"]')).toHaveCount(0)
    for (const label of ['Current blocker', 'Retry', 'What to do next']) {
      await expect(page.locator('dt').getByText(label, { exact: true })).toHaveCount(0)
    }
    for (const empty of ['No current blocker.', 'No retry is needed.', 'No operator action is needed.']) {
      await expect(page.getByText(empty)).toHaveCount(0)
    }

    // The stop affordance and the client machine activity render once, in the box.
    await expect(page.getByRole('button', { name: 'Stop attempt' })).toHaveCount(1)
    await expect(card.getByRole('button', { name: 'Stop attempt' })).toBeVisible()
    await expect(page.getByRole('region', { name: 'Client machine activity' })).toHaveCount(1)
    await expect(card.getByRole('region', { name: 'Client machine activity' })).toBeVisible()

    // The box sits directly above the Activity heading.
    const activityHeading = page.getByRole('heading', { name: 'Activity', exact: true })
    const cardBox = await card.boundingBox()
    const headingBox = await activityHeading.boundingBox()
    expect(cardBox && headingBox && cardBox.y + cardBox.height <= headingBox.y).toBe(true)

    // Nothing scrolls sideways at 390 px.
    const overflow = await card.evaluate((element) => ({
      card: element.scrollWidth > element.clientWidth,
      page: document.documentElement.scrollWidth > document.documentElement.clientWidth,
    }))
    expect(overflow).toEqual({ card: false, page: false })
  })
}

// A claimed order the next action does not name as current, such as a claimed
// refresh-review round, maps no execution state; its stop affordance still
// renders once, in the box.
test('a claimed order without a mapped execution state keeps one Stop attempt in the box', async ({ page }) => {
  const reviewOrder = { ...runningOrder, id: 'order-review-refresh', stage: 'review', activity_snapshot: undefined }
  const unnamed: NextAction = { ...workingAction, current_order: undefined }
  await mockAPI(
    page,
    () => activity('running', unnamed, [reviewOrder as WorkOrder]),
    () => unnamed,
  )
  for (const path of [`/tasks/${taskId}/full`, `/tasks/${taskId}`]) {
    await page.goto(path)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByRole('heading', { name: 'An agent is working on this task.' })).toBeVisible()
    await expect(card.locator('[data-execution-status]')).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Stop attempt' })).toHaveCount(1)
    await expect(card.getByRole('button', { name: 'Stop attempt' })).toBeVisible()
  }
})

// On the Flow tab the timeline's gate card is not mounted, so the card itself
// carries the server's choices: still usable, and the summary still printed
// once (component-web-task-surfaces, Activity tabs and the task flow view).
for (const surface of [`/tasks/${taskId}`, `/tasks/${taskId}/full`]) {
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
  await page.goto(`/tasks/${taskId}`)
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  const card = page.getByRole('region', { name: 'Next action' })
  // Every server choice renders; one the caller may not take is disabled with
  // the reason beside it, and each carries the server's endpoint.
  const choices = card.locator('[data-next-action-choices]').getByRole('button')
  await expect(choices).toHaveText(['Answer', 'Close'])
  await expect(choices.nth(1)).toBeDisabled()
  await expect(card.getByText('Needs the operate gates capability.')).toBeVisible()
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
  await page.goto(`/tasks/${taskId}`)
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
  await page.goto(`/tasks/${taskId}/full`)
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
  await page.goto(`/tasks/${taskId}/full`)
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
  await page.goto(`/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  await expect(card.getByText('Conveyor is merging')).toBeVisible()
  await expect(card.getByRole('button')).toHaveCount(0)
  // Not an operator gate: no gate card and no merge button anywhere.
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /merge/i })).toHaveCount(0)
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

for (const state of ['CONFLICTING', 'STALE', 'UNKNOWN']) {
  test(`the merge_approval gate refuses Merge at ${state} with the server's reason`, async ({ page }) => {
    await mockAPI(
      page,
      () => mergeGateActivity(state),
      () => mergeApproval(state),
    )
    await page.goto(`/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    await expect(gate.locator('[data-choice="approve"]')).toBeEnabled()
    await expect(gate.locator('[data-merge-at-gate]')).toBeDisabled()
    await expect(gate.getByText(`it is ${state}.`)).toBeVisible()
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
  await page.goto(`/tasks/${taskId}/full`)
  const gate = page.getByRole('region', { name: 'Human gate' })
  // Approve stays beside Merge, and the merge choice is not also a one-click button.
  await expect(gate.locator('[data-choice="approve"]')).toBeEnabled()
  await expect(gate.locator('[data-choice="merge"]')).toHaveCount(0)
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
  await page.goto(`/tasks/${taskId}/full`)
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
  await page.goto(`/tasks/${taskId}/full`)
  // The newest order would win a client-side pick; the server named the verify order.
  await expect(page.getByText('Verification paused — recovery needed').first()).toBeVisible()
  await expect(page.getByText('The order was not claimed before its queue deadline.').first()).toBeVisible()
  await expect(page.getByText('Implementation paused — recovery needed')).toHaveCount(0)
  // An operator act that is not a gate decision opens no human gate.
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
})

// Three pending task-context proposals: the next-action card's server choices
// are the one place each is decided (component-web-task-surfaces, Next-action
// card; feature-operator-next-action, Choices row context_proposal).
const contextTargets = [
  { kind: 'requirement', id: 'req-task-centric-operations-view', title: 'Task-centric operations view' },
  { kind: 'system_design', id: 'component-verification-strategy', title: 'Testing strategy' },
  {
    kind: 'system_design',
    id: 'component-web-task-surfaces',
    title: 'Web task surfaces: board, tasks, and task detail',
  },
] as const

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
          label: `${action === 'confirm' ? 'Confirm' : 'Dismiss'} context ${target.kind} ${target.id}`,
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
    details: {},
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

const decisionLabels = contextTargets.flatMap((target) => [
  `Confirm context ${target.kind} ${target.id}`,
  `Dismiss context ${target.kind} ${target.id}`,
])

test('three task-context proposals are decided once each from the next-action card on the sheet', async ({ page }) => {
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
  await page.goto(`/tasks/${taskId}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const card = sheet.getByRole('region', { name: 'Next action' })
  await expect(card.getByText('A context proposal is waiting for a decision.')).toBeVisible()

  // Suggested context only informs: no Attach or Dismiss of its own.
  const suggestions = sheet.getByRole('region', { name: 'Suggested context' })
  await expect(suggestions.getByRole('listitem')).toHaveCount(3)
  await expect(suggestions.getByRole('button')).toHaveCount(0)

  // Exactly six usable decisions, each rendered once on the page.
  const choices = card.locator('[data-next-action-choices]').getByRole('button')
  await expect(choices).toHaveCount(7)
  for (const label of decisionLabels) {
    await expect(page.getByRole('button', { name: label, exact: true })).toHaveCount(1)
    await expect(card.getByRole('button', { name: label, exact: true })).toBeEnabled()
  }
  // A malformed task-context id resolves to no body and stays disabled.
  await expect(card.getByRole('button', { name: 'Confirm context widget', exact: true })).toBeDisabled()
  await expect(card).toContainText('This page cannot send this choice.')

  // Both acts post an empty body to the server-stated path.
  await card.getByRole('button', { name: decisionLabels[0], exact: true }).click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0]).toEqual({
    method: 'POST',
    path: `/v1/tasks/${taskId}/context/proposals/requirement/req-task-centric-operations-view/confirm`,
    body: null,
  })
  await card.getByRole('button', { name: decisionLabels[3], exact: true }).click()
  await expect.poll(() => posts.length).toBe(2)
  expect(posts[1]).toEqual({
    method: 'POST',
    path: `/v1/tasks/${taskId}/context/proposals/system_design/component-verification-strategy/dismiss`,
    body: null,
  })

  // On the Flow tab the same choices stay usable from the card.
  await sheet.getByRole('tab', { name: 'Flow' }).click()
  await expect(sheet.getByRole('tab', { name: 'Flow' })).toHaveAttribute('aria-selected', 'true')
  for (const label of decisionLabels) await expect(card.getByRole('button', { name: label, exact: true })).toBeEnabled()
  await card.getByRole('button', { name: decisionLabels[5], exact: true }).click()
  await expect.poll(() => posts.length).toBe(3)
  expect(posts[2]).toMatchObject({
    path: `/v1/tasks/${taskId}/context/proposals/system_design/component-web-task-surfaces/dismiss`,
    body: null,
  })
})

test('task-context choices a caller may not take stay disabled with the capability reason', async ({ page }) => {
  const action = contextProposalAction(false)
  const posts = await mockAPI(
    page,
    () => contextProposalDetail(action),
    () => action,
  )
  await page.goto(`/tasks/${taskId}`)
  const card = page.getByRole('dialog', { name: 'Task detail' }).getByRole('region', { name: 'Next action' })
  for (const label of decisionLabels)
    await expect(card.getByRole('button', { name: label, exact: true })).toBeDisabled()
  await expect(card.getByText('Needs the operate gates capability.').first()).toBeVisible()
  expect(posts).toEqual([])
})

// The longest server labels wrap inside their buttons at phone width: every
// button stays inside the card and the viewport on both detail surfaces. The
// sheet clips overflow, so the document width alone would not catch this.
for (const surface of [`/tasks/${taskId}`, `/tasks/${taskId}/full`]) {
  test(`task-context choices wrap inside the next-action card at 390 px on ${surface}`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    const action = contextProposalAction()
    await mockAPI(
      page,
      () => contextProposalDetail(action),
      () => action,
    )
    await page.goto(surface)
    const card = page.getByRole('region', { name: 'Next action' })
    await expect(card.getByRole('button', { name: decisionLabels[0], exact: true })).toBeVisible()
    if (surface === `/tasks/${taskId}`) {
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      await expect.poll(() => sheet.evaluate((node) => node.getAnimations().length)).toBe(0)
    }
    const cardBox = (await card.boundingBox())!
    for (const label of decisionLabels) {
      const box = (await card.getByRole('button', { name: label, exact: true }).boundingBox())!
      expect(box.x).toBeGreaterThanOrEqual(cardBox.x - 0.5)
      expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width + 0.5)
      expect(box.x + box.width).toBeLessThanOrEqual(390)
    }
    expect(cardBox.x + cardBox.width).toBeLessThanOrEqual(390)
    // Wrapping keeps the whole label: the longest one takes more than a line.
    const longest = card.getByRole('button', { name: decisionLabels[5], exact: true })
    await expect(longest).toHaveText(decisionLabels[5])
    const lines = await longest.evaluate(
      (node) => node.getBoundingClientRect().height / Number.parseFloat(getComputedStyle(node).lineHeight),
    )
    expect(lines).toBeGreaterThan(1.5)
  })
}

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
  await page.goto(`/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Next action' })
  const row = card.locator('[data-next-action-choices] > div').first()
  await expect(row.getByRole('button')).toHaveText(['Recover', 'Close', 'Start over'])
  await expect(row.locator('button.bg-primary')).toHaveCount(1)
  await expect(row.getByRole('button', { name: 'Recover' })).toHaveClass(/bg-primary/)

  await row.getByRole('button', { name: 'Start over', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Start over', exact: true })
  await dialog.getByLabel(/Reason/).fill('Scope changed')
  await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
  await expect(page).toHaveURL(/\/tasks\/T-successor\/full/)
  expect(restart).toMatchObject({ reason: 'Scope changed' })
})
