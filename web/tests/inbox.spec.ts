import { expect, type Page, type Request, type Route } from '@playwright/test'
import { test } from './helpers/bounded-detail'
import { callerIdentity, capabilitiesForRole } from './helpers/caller-identity'
import { expectNoNextActionOmissions } from './helpers/next-action-omissions'

// The operator inbox (feature-operator-next-action NA-6; REQ-2) over stubbed
// GET /v1/next-actions, GET /v1/tasks/{id}/next-action and the retired
// pending-proposals projection that still owns the navigation count.

const proposedAt = '2026-10-10T10:00:00Z'
const soon = new Date(Date.now() + 3 * 3_600_000).toISOString()

function task(id: string, title: string, state = 'awaiting_spec_approval') {
  return {
    id,
    workspace: 'demo',
    source: 'cli',
    title,
    body: '',
    class: 'feature',
    level: 'L1',
    repo: 'conveyor',
    base_branch: 'main',
    branch: `conveyor/${id}`,
    state,
    created_at: proposedAt,
  }
}

function choice(id: string, label: string, path: string, allowed = true, method = 'POST') {
  return { id, label, effect: `${label} effect`, method, path, capability: 'operate_tasks', allowed }
}

interface Fixture {
  items: (Record<string, unknown> & { task?: { id: string }; proposal?: { id: string } })[]
  acts?: Record<string, unknown>[]
  actions: Record<string, Record<string, unknown>>
  activity?: Record<string, Record<string, unknown>>
  proposals?: Record<string, unknown>[]
  operatorDecisions?: number
  role?: string
  capabilities?: string[]
  // Document reads by path; a number answers with that status.
  documents?: Record<string, Record<string, unknown> | number>
  workspaces?: { id: string; name: string }[]
}

interface Harness {
  fixture: Fixture
  posts: Request[]
  inboxRequests: string[]
  documentRequests: Request[]
  onPost?: (request: Request, path: string) => Record<string, unknown> | undefined
  // While set, acts and document reads wait for it before they answer.
  holdPosts?: Promise<void>
  holdDocuments?: Promise<void>
}

async function serve(page: Page, fixture: Fixture, onPost?: Harness['onPost']): Promise<Harness> {
  const harness: Harness = { fixture, posts: [], inboxRequests: [], documentRequests: [], onPost }
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const f = harness.fixture
    if (path === '/v1/workspaces') return route.fulfill({ json: f.workspaces ?? [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') {
      const identity = callerIdentity(
        { id: 'usr_operator', role: f.role ?? 'operator', capabilities: f.capabilities },
        route.request(),
      )
      return route.fulfill({ json: identity })
    }
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/next-actions') {
      harness.inboxRequests.push(url.search)
      const band = url.searchParams.get('band')
      const items = band ? f.items.filter((item) => item.band === band) : f.items
      const offset = Number(url.searchParams.get('offset') ?? 0)
      const limit = Number(url.searchParams.get('limit') ?? items.length)
      return route.fulfill({
        json: { items: items.slice(offset, offset + limit), acts: f.acts ?? [] },
        headers: { 'X-Conveyor-Total': String(items.length) },
      })
    }
    if (path === '/v1/pending-proposals') {
      const items = f.proposals ?? []
      const decisions = f.operatorDecisions ?? 0
      return route.fulfill({
        json: {
          items,
          attention: { task_count: 9, pending_proposal_count: items.length, operator_decisions: decisions, total: 99 },
        },
      })
    }
    const nextAction = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (nextAction) return route.fulfill({ json: f.actions[decodeURIComponent(nextAction[1])] })
    const activity = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (activity) {
      const detail = f.activity?.[decodeURIComponent(activity[1])]
      return detail ? route.fulfill({ json: detail }) : route.fulfill({ status: 404, json: { error: 'not found' } })
    }
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 204, body: '' })
    if (request.method() === 'GET' && f.documents && path in f.documents) {
      harness.documentRequests.push(request)
      if (harness.holdDocuments) await harness.holdDocuments
      // Read at answer time, so a held read answers with the current fixture.
      const document = f.documents[path]
      if (typeof document === 'number')
        return route.fulfill({ status: document, json: { error: 'unavailable' } }).catch(() => {})
      return route.fulfill({ json: document }).catch(() => {})
    }
    if (request.method() !== 'GET') {
      harness.posts.push(request)
      if (harness.holdPosts) await harness.holdPosts
      const body = harness.onPost?.(request, path)
      // A numeric `status` refuses the act with the body's error.
      const status = typeof body?.status === 'number' ? body.status : 200
      return route.fulfill({ status, json: body ?? {} }).catch(() => {})
    }
    return route.fulfill({ json: [] })
  })
  return harness
}

// Decisions deliberately not alphabetical: the dependent-first item leads and
// the null-deadline item trails, as the server orders them.
function decisionsFixture(): Fixture {
  return {
    operatorDecisions: 3,
    items: [
      {
        band: 'decisions',
        task: {
          ...task('T-zeta', 'Zeta plan everyone waits on'),
          context: {
            requirements: [{ id: 'REQ-2', title: 'Reproducible builds', version: 3, archived: false }],
          },
        },
        next_action: {
          kind: 'plan_approval',
          label: 'Approve the plan',
          actor: 'operator',
          judgment: true,
          summary: 'The plan is ready.',
          deadline: soon,
        },
        pull_request: {
          url: 'https://github.com/acme/conveyor/pull/77',
          number: 77,
          repository: 'acme/conveyor',
        },
        serves: ['REQ-2', 'REQ-6'],
        recommendation: 'Approve: the plan covers every criterion.',
        deadline: soon,
      },
      {
        band: 'decisions',
        task: task('T-alpha', 'Alpha agent question', 'running'),
        next_action: {
          kind: 'agent_question',
          label: 'Answer the agent',
          actor: 'operator',
          judgment: true,
          summary: 'The agent stopped.',
          deadline: soon,
        },
      },
      {
        band: 'decisions',
        task: task('T-mid', 'Mid item without a deadline', 'reviewing'),
        next_action: {
          kind: 'review_bounce_limit',
          label: 'Decide after repeated review bounces',
          actor: 'operator',
          judgment: true,
          summary: 'Review bounced three times.',
          deadline: null,
        },
      },
      {
        band: 'blocked',
        task: task('T-wait', 'Waiting on zeta', 'queued'),
        next_action: { kind: 'blocked', label: 'Blocked', actor: 'dependency', judgment: false, summary: 'Blocked.' },
        blockers: [
          { task_id: 'T-zeta', title: 'Zeta plan everyone waits on', state: 'awaiting_spec_approval' },
          { task_id: 'T-done', title: 'Elsewhere dependency', state: 'running' },
        ],
      },
      {
        band: 'automatic',
        task: task('T-auto', 'Automatic redispatch', 'stalled'),
        next_action: { kind: 'stalled', label: 'Stalled', actor: 'factory', judgment: false, summary: 'Will retry.' },
        no_judgment_needed: true,
      },
    ],
    acts: [{ id: 1, task_id: 'T-old', kind: 'redispatch', at: proposedAt, automatic: true, summary: 'Redispatched' }],
    actions: {
      'T-zeta': {
        kind: 'plan_approval',
        label: 'Approve the plan',
        actor: 'operator',
        judgment: true,
        summary: 'The plan is ready.',
        question: 'Does this plan meet the requirement?',
        choices: [
          choice('approve', 'Approve plan', '/v1/tasks/T-zeta/spec/review'),
          choice('reject', 'Reject plan', '/v1/tasks/T-zeta/spec/review', false),
          choice('summon_dragon', 'Summon dragon', '/v1/tasks/T-zeta/dragon'),
        ],
        if_ignored: 'The task waits at the plan gate.',
        deadline: soon,
        details: { gate_kind: 'spec', order_ids: ['wo-zeta-1'] },
      },
      'T-alpha': {
        kind: 'agent_question',
        label: 'Answer the agent',
        actor: 'operator',
        judgment: true,
        summary: 'The agent stopped.',
        question: 'Which cache should it use?',
        choices: [choice('answer', 'Answer', '/v1/work-orders/wo-alpha/recover')],
        current_order: {
          id: 'wo-alpha',
          stage: 'implement',
          state: 'released',
          queue_deadline: null,
          execution_deadline: null,
          lease_expires_at: null,
        },
        deadline: soon,
        details: { checkpoint: 'authority_conflict' },
      },
      'T-mid': {
        kind: 'review_bounce_limit',
        label: 'Decide after repeated review bounces',
        actor: 'operator',
        judgment: true,
        summary: 'Review bounced three times.',
        choices: [],
        deadline: null,
        details: {},
      },
      'T-auto': {
        kind: 'stalled',
        label: 'Stalled',
        actor: 'factory',
        judgment: false,
        summary: 'Will retry.',
        choices: [choice('redispatch', 'Redispatch now', '/v1/tasks/T-auto/redispatch')],
        deadline: null,
        details: {},
      },
    },
  }
}

// The decision list's rows, in the server's order, and the selected item's
// detail view (req-operator-next-action AC-2.8; DEC-82).
const decisionRows = (page: Page) => page.getByRole('region', { name: 'Decisions' }).locator('[data-inbox-row]')
// The whole row item: the selection link plus the meta line beside it.
const rowItems = (page: Page) => page.getByRole('region', { name: 'Decisions' }).locator('[data-inbox-row-item]')
const decisionDetail = (page: Page) => page.locator('[data-inbox-detail] [data-inbox-item]')

// Selects the row whose text matches and waits for its detail view.
async function select(page: Page, text: string | RegExp) {
  await decisionRows(page).filter({ hasText: text }).click()
  await expect(decisionDetail(page)).toContainText(text)
  return decisionDetail(page)
}

test('bands render in server order with the decision detail the server states', async ({ page }) => {
  await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox')

  await expect(decisionRows(page)).toHaveCount(3)
  await expect(decisionRows(page).locator('[data-row-title]')).toHaveText([
    'Zeta plan everyone waits on',
    'Alpha agent question',
    'Mid item without a deadline',
  ])
  // A row carries no choice control (DEC-82(1)); the first item is selected.
  await expect(decisionRows(page).getByRole('button')).toHaveCount(0)
  await expect(decisionRows(page).first()).toHaveAttribute('aria-current', 'true')
  await expect(rowItems(page).nth(1)).not.toContainText('No PR yet')
  // The row names the pull request by number; its tooltip carries the repository.
  await expect(rowItems(page).first().locator('[data-row-pull-request]')).toHaveText('PR #77')
  await expect(rowItems(page).first().locator('[data-row-pull-request]')).toHaveAttribute(
    'title',
    'acme/conveyor PR #77',
  )
  const bands = page.locator(
    'section[aria-label="Decisions"], section[aria-label="Blocked"], section[aria-label="Automatic"]',
  )
  await expect(bands).toHaveCount(3)
  expect(await bands.evaluateAll((nodes) => nodes.map((node) => node.getAttribute('aria-label')))).toEqual([
    'Decisions',
    'Blocked',
    'Automatic',
  ])

  const card = decisionDetail(page)
  await expect(card.getByText('Approve the plan', { exact: true })).toBeVisible()
  await expect(card.getByRole('link', { name: 'acme/conveyor PR #77' })).toHaveAttribute(
    'href',
    'https://github.com/acme/conveyor/pull/77',
  )
  // Served requirements are chips: the context title when the task carries
  // one, the bare ID otherwise.
  await expect(card.locator('[data-serves="REQ-2"]')).toHaveText('REQ-2Reproducible builds')
  await expect(card.locator('[data-serves="REQ-6"]')).toHaveText('REQ-6')
  // The recommendation is a callout, and the label a pill in its kind's tone.
  await expect(card.locator('[data-recommendation]')).toHaveText(
    'Agent recommendsApprove: the plan covers every criterion.',
  )
  await expect(card.locator('[data-decision-label]')).toHaveAttribute('data-kind-tone', 'gate')
  await expect(card.locator('[data-decision-label]')).not.toHaveClass(/attention/)
  // The deadline is the countdown chip in the head line. The detail prints no
  // actor, question, if-ignored sentence, or identifiers (AC-1.9, AC-2.8;
  // component-next-action invariant 41).
  await expect(card.locator('[data-due]')).toHaveText(/^due in /)
  await expect(card.locator('[data-due]')).toHaveAttribute('title', /\d/)
  await expect(card.locator('[data-context-heading]')).toHaveText('Plan')
  await expectNoNextActionOmissions(card, {
    actor: 'operator',
    sentences: ['Does this plan meet the requirement?', 'The task waits at the plan gate.'],
    identifiers: ['wo-zeta-1'],
  })

  // Every server choice renders (DEC-61): a disallowed choice and one this
  // client cannot build are disabled with the reason beside them.
  await expect(card.getByRole('button', { name: 'Approve plan' })).toBeEnabled()
  await expect(card.getByRole('button', { name: 'Reject plan' })).toBeDisabled()
  await expect(card.getByText('Needs the operate tasks capability.')).toBeVisible()
  await expect(card.getByRole('button', { name: 'Summon dragon' })).toBeDisabled()
  await expect(card.getByText('This page cannot send this choice.')).toBeVisible()

  const alpha = await select(page, 'Alpha agent question')
  await expect(alpha.getByText('No PR yet')).toBeVisible()
  await expect(alpha.locator('[data-context-heading]')).toHaveText('Checkpoint')
  // The null-deadline item states none.
  const mid = await select(page, 'Mid item without a deadline')
  await expect(mid.locator('[data-context-heading]')).toHaveText('Latest review')
  await expect(mid.locator('[data-next-action-deadline]')).toHaveCount(0)
})

test('a choice sends one request as stated, refetches the inbox and the badge, and the item leaves', async ({
  page,
}) => {
  const fixture = decisionsFixture()
  const harness = await serve(page, fixture, (_request, path) => {
    if (path === '/v1/tasks/T-zeta/spec/review') {
      fixture.items = fixture.items.filter((item) => item.task?.id !== 'T-zeta')
      fixture.operatorDecisions = 2
      fixture.actions['T-zeta'] = {
        kind: 'implementing',
        label: 'Implementing',
        actor: 'agent',
        judgment: false,
        summary: 'An agent is implementing the approved plan.',
        choices: [],
        deadline: null,
        details: {},
      }
    }
    return undefined
  })
  await page.goto('/w/demo/inbox')
  const inbox = page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: /Inbox/ })
  await expect(inbox.getByText('3', { exact: true })).toBeVisible()
  const before = harness.inboxRequests.length

  await decisionDetail(page).getByRole('button', { name: 'Approve plan' }).dblclick()
  await expect(decisionRows(page)).toHaveCount(2)
  // The selection moves to the next item (AC-2.10).
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
  expect(harness.posts).toHaveLength(1)
  const [post] = harness.posts
  expect(post.method()).toBe('POST')
  expect(new URL(post.url()).pathname).toBe('/v1/tasks/T-zeta/spec/review')
  expect(post.headers()['x-conveyor-csrf']).toBe('1')
  await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(before)
  await expect(inbox.getByText('2', { exact: true })).toBeVisible()

  // The task detail's next-action card reads the same, now-changed state.
  fixture.activity = {
    'T-zeta': {
      task: task('T-zeta', 'Zeta plan everyone waits on', 'running'),
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  await page.goto('/w/demo/tasks/T-zeta')
  await expect(page.getByRole('region', { name: 'Next action' })).toContainText(
    'An agent is implementing the approved plan.',
  )
})

// Task-context proposal choices carry their target in the id
// (confirm:task_context:<kind>:<target-id>), are labeled by the target's title
// and tier word, and post no body to the task-context route; the details state
// each proposal in choice order (feature-operator-next-action NA-3, NA-4,
// NA-6; AC-1.8, AC-2.2).
interface ContextTarget {
  kind: string
  id: string
  title: string
  justification: string
  source: string
}

const tierWords: Record<string, string> = { requirement: 'requirement', system_design: 'System Design' }

const contextTargets: ContextTarget[] = [
  {
    kind: 'requirement',
    id: 'req-review-gates-evidence',
    title: 'Review gates and evidence',
    justification: 'The failing check guards the review evidence rules.',
    source: 'triage',
  },
  {
    kind: 'system_design',
    id: 'component-verification-strategy',
    title: 'Testing strategy',
    justification: 'The failure is in a suite this strategy names.',
    source: 'planning',
  },
  {
    kind: 'system_design',
    id: 'component-web-dashboard',
    title: 'Web dashboard',
    justification: 'The broken test renders the dashboard shell.',
    source: 'operator',
  },
]

const contextLabel = (act: 'confirm' | 'dismiss', target: ContextTarget) =>
  `${act === 'confirm' ? 'Confirm' : 'Dismiss'} ${target.title} (${tierWords[target.kind]})`

function contextChoices(taskId: string, targets: ContextTarget[], allowed = true) {
  return targets.flatMap((target) =>
    (['confirm', 'dismiss'] as const).map((act) => ({
      ...choice(
        `${act}:task_context:${target.kind}:${target.id}`,
        contextLabel(act, target),
        `/v1/tasks/${taskId}/context/proposals/${target.kind}/${target.id}/${act}`,
        allowed,
      ),
      effect: act === 'confirm' ? 'Attaches the proposed context to this task.' : 'Dismisses the proposed context.',
      capability: 'operate_gates',
    })),
  )
}

function contextAction(targets: ContextTarget[], allowed = true, origin?: Record<string, unknown>) {
  // One creation instant for every proposal: the shared age shows only when
  // the times agree, so a per-row clock read that straddles a millisecond
  // would drop it (NA-6).
  const createdAt = new Date(Date.now() - 2 * 3_600_000).toISOString()
  return {
    kind: 'context_proposal',
    label: 'Context proposal',
    actor: 'operator',
    judgment: true,
    summary: 'A context proposal is waiting for a decision.',
    question: 'Confirm or dismiss the proposed context?',
    if_ignored: 'The proposal stays pending.',
    choices: contextChoices('T-ctx', targets, allowed),
    deadline: null,
    details: {
      context_proposals: targets.map((target) => ({
        kind: target.kind,
        target_id: target.id,
        target_title: target.title,
        tier_label: tierWords[target.kind],
        justification: target.justification,
        source: target.source,
        proposed_by: 'system:triage',
        created_at: createdAt,
      })),
      ...(origin ? { monitor_origin: origin } : {}),
    },
  }
}

const queuedAction = {
  kind: 'queued',
  label: 'Queued',
  actor: 'agent',
  judgment: false,
  summary: 'An order is waiting to be claimed.',
  choices: [],
  deadline: null,
  details: {},
}

function contextFixture(
  options: { targets?: ContextTarget[]; allowed?: boolean; origin?: Record<string, unknown>; body?: string } = {},
): Fixture {
  const targets = options.targets ?? contextTargets.slice(0, 2)
  return {
    operatorDecisions: 1,
    items: [
      {
        band: 'decisions',
        task: { ...task('T-ctx', 'Repair post-merge test failures', 'queued'), body: options.body ?? '' },
        next_action: {
          kind: 'context_proposal',
          label: 'Context proposal',
          actor: 'operator',
          judgment: true,
          summary: 'A context proposal is waiting for a decision.',
          deadline: null,
        },
      },
    ],
    actions: { 'T-ctx': contextAction(targets, options.allowed ?? true, options.origin) },
  }
}

// The server's next action after the given targets were decided: the rest
// stay pending, and with none left the task leaves the inbox.
function decide(fixture: Fixture, remaining: ContextTarget[], origin?: Record<string, unknown>) {
  if (remaining.length === 0) {
    fixture.items = []
    fixture.operatorDecisions = 0
    fixture.actions['T-ctx'] = queuedAction
    return
  }
  fixture.actions['T-ctx'] = contextAction(remaining, true, origin)
}

// The card's visible text and tooltips outside the IDs disclosure.
async function textOutsideIDs(card: import('@playwright/test').Locator) {
  return card.evaluate((element) => {
    const clone = element.cloneNode(true) as HTMLElement
    for (const details of clone.querySelectorAll('details')) details.remove()
    const titles = [...clone.querySelectorAll('[title]')].map((node) => node.getAttribute('title') ?? '')
    return `${clone.textContent ?? ''}\n${titles.join('\n')}`
  })
}

// The checklist's Apply button, labelled by the current selection (NA-6).
const applyButton = (card: import('@playwright/test').Locator) => card.locator('[data-context-apply]')
const dismissAllButton = (card: import('@playwright/test').Locator) =>
  card.getByRole('button', { name: 'Dismiss all', exact: true })
const contextPath = (target: ContextTarget, act: 'confirm' | 'dismiss') =>
  `/v1/tasks/T-ctx/context/proposals/${target.kind}/${target.id}/${act}`

test('the context checklist lists each proposed document checked, and Apply attaches them all until the item leaves', async ({
  page,
}) => {
  const [requirement, design] = contextTargets
  const fixture = contextFixture({ targets: [requirement, { ...design, source: 'triage' }] })
  const harness = await serve(page, fixture, (_request, path) => {
    if (path.endsWith(`/system_design/${design.id}/confirm`)) decide(fixture, [])
    return { state: 'confirmed' }
  })
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  const rows = card.locator('[data-context-proposal]')
  await expect(rows).toHaveCount(2)

  // One shared source line; each row: checkbox, tier word, title that opens
  // the preview panel, Preview, justification. No row carries a Confirm or
  // Dismiss button.
  await expect(card.locator('[data-context-source]')).toHaveText('Suggested by triage · 2 hours ago')
  await expect(card.locator('[data-context-row-meta]')).toHaveCount(0)
  const first = rows.nth(0)
  await expect(first).toContainText('requirement')
  await expect(first.getByRole('button', { name: requirement.title, exact: true })).toBeVisible()
  await expect(first.getByRole('button', { name: `Preview ${requirement.title}` })).toBeVisible()
  await expect(first.getByRole('link')).toHaveCount(0)
  await expect(first).toContainText(requirement.justification)
  await expect(rows.nth(1).getByRole('button', { name: design.title, exact: true })).toBeVisible()
  await expect(rows.nth(1)).toContainText('System Design')
  await expect(
    card.locator('[data-context-proposals]').getByRole('button', { name: /^(Confirm|Dismiss) / }),
  ).toHaveCount(0)
  // Each checkbox is named by the server label of the choice its state selects.
  await expect(card.getByRole('checkbox', { name: contextLabel('confirm', requirement), exact: true })).toBeChecked()
  await expect(card.getByRole('checkbox', { name: contextLabel('confirm', design), exact: true })).toBeChecked()
  await expect(applyButton(card)).toHaveText('Attach all 2')
  await expect(dismissAllButton(card)).toBeEnabled()
  // Short justifications fit their two lines, so no row offers Show more.
  await expect(card.locator('[data-context-expand]')).toHaveCount(0)
  await expect(card).not.toContainText('cannot send')

  // Raw target IDs and the proposer print nowhere: the detail has no IDs
  // list (AC-1.9). The checklist itself stays.
  const outside = await textOutsideIDs(card)
  for (const target of [requirement, design]) expect(outside).not.toContain(target.id)
  expect(outside).not.toContain('system:triage')
  await expectNoNextActionOmissions(card, {
    actor: 'operator',
    sentences: ['Confirm or dismiss the proposed context?'],
    identifiers: [requirement.id, design.id, 'system:triage'],
  })

  // A double click still runs once: one body-less POST per row, in order.
  await applyButton(card).dblclick()
  await expect.poll(() => harness.posts.length).toBe(2)
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual([
    contextPath(requirement, 'confirm'),
    contextPath(design, 'confirm'),
  ])
  for (const post of harness.posts) {
    expect(post.method()).toBe('POST')
    expect(post.postData()).toBeNull()
    expect(post.headers()['x-conveyor-csrf']).toBe('1')
    expect(new URL(post.url()).searchParams.get('workspace_id')).toBe('demo')
  }
  await expect(decisionRows(page)).toHaveCount(0)
  await expect(page.locator('[data-no-decisions]')).toContainText('No decisions are waiting.')
  expect(harness.posts).toHaveLength(2)

  // The task detail shows the confirmed attachment under Attached context; the
  // timeline renders no note for the recorded decision.
  fixture.activity = {
    'T-ctx': {
      task: {
        ...task('T-ctx', 'Repair post-merge test failures', 'queued'),
        context: {
          requirements: [{ id: requirement.id, title: requirement.title, version: 4, archived: false }],
          designs: [],
          proposals: [],
        },
      },
      events: [
        {
          id: '901',
          task_id: 'T-ctx',
          kind: 'task.context_proposal_confirmed',
          actor_id: 'user:usr_operator',
          actor_role: 'operator',
          at: '2026-10-16T18:10:00.000000Z',
          payload: { target_kind: 'requirement', target_id: requirement.id, requirement_id: requirement.id },
        },
      ],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  await page.goto('/w/demo/tasks/T-ctx')
  await expect(
    page.getByRole('region', { name: 'Attached context' }).getByLabel(`${requirement.title} (${requirement.id})`),
  ).toBeVisible()
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline).toBeVisible()
  await expect(timeline).not.toContainText('Context confirmed')
})

test('a refused send stops the run, states how far it got, and keeps the acts already recorded', async ({ page }) => {
  const [requirement, design, dashboard] = contextTargets
  const fixture = contextFixture({ targets: contextTargets })
  const harness = await serve(page, fixture, (_request, path) => {
    if (path.endsWith(`/${requirement.id}/dismiss`)) {
      decide(fixture, [design, dashboard])
      return { state: 'dismissed' }
    }
    if (path.endsWith(`/${design.id}/dismiss`)) return { status: 409, error: 'The proposal was already decided.' }
    return { state: 'dismissed' }
  })
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await dismissAllButton(card).click()
  const refusal = card.locator('[data-context-refusal]')
  await expect(refusal).toContainText('1 of 3 recorded.')
  await expect(refusal).toContainText('already decided')
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual([
    contextPath(requirement, 'dismiss'),
    contextPath(design, 'dismiss'),
  ])
  // The refreshed card keeps the refusal and the two proposals still pending.
  await expect(card.locator('[data-context-proposal]')).toHaveCount(2)
  await expect(refusal).toBeVisible()
  await page.waitForTimeout(300)
  expect(harness.posts).toHaveLength(2)
})

test('the inbox offers the one-act merge only behind the confirm naming the reviewed head and PR', async ({ page }) => {
  const head = 'feedface1234567'
  const fixture: Fixture = {
    items: [
      {
        band: 'decisions',
        task: task('T-merge', 'Merge me', 'awaiting_human'),
        next_action: {
          kind: 'merge_approval',
          label: 'Merge approval',
          actor: 'operator',
          judgment: true,
          summary: 'Waiting for merge approval.',
          deadline: null,
        },
      },
    ],
    actions: {
      'T-merge': {
        kind: 'merge_approval',
        label: 'Merge approval',
        actor: 'operator',
        judgment: true,
        summary: 'Waiting for merge approval.',
        choices: [
          choice('approve', 'Approve only', '/v1/tasks/T-merge/review'),
          choice('merge', 'Merge', '/v1/tasks/T-merge/merge'),
        ],
        deadline: null,
        details: { gate_kind: 'merge_approval' },
      },
    },
    activity: {
      'T-merge': {
        task: task('T-merge', 'Merge me', 'awaiting_human'),
        events: [],
        jobs: [],
        work_orders: [],
        interventions: [],
        merge_readiness: { state: 'MERGEABLE', head_sha: head, number: 9 },
      },
    },
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.locator('[data-choice="approve"]')).toBeVisible()
  // The merge choice is never a one-click button.
  await expect(card.locator('[data-choice="merge"]')).toHaveCount(0)
  await card.locator('[data-merge-at-gate]').click()
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  await expect(dialog).toContainText('#9')
  await expect(dialog.locator('code')).toHaveText(head.slice(0, 7))
  expect(harness.posts).toHaveLength(0)
  await dialog.locator('[data-confirm-merge]').click()
  await expect
    .poll(() => harness.posts.map((post) => new URL(post.url()).pathname))
    .toEqual(['/v1/tasks/T-merge/merge'])
})

const longDecision =
  'All FunnelFlux repositories are registered in the demo workspace, each with its own verification kit, its own review seats, and a merge gate that the operator decides for every pull request.'

// Document proposals as the server states them (feature-operator-next-action
// NA-4, NA-5): the stored fields, a read-time age, and the label, question and
// if-ignored copy. `age` overrides the age: null omits it.
function proposalFixture(): Fixture {
  const copy: Record<string, { label: string; question: string }> = {
    requirement: { label: 'Requirement proposal', question: 'Confirm or dismiss this proposed version?' },
    system_design: { label: 'System Design proposal', question: 'Confirm or dismiss this proposed version?' },
    decision: { label: 'Decision proposal', question: 'Confirm or dismiss this proposed decision?' },
  }
  const proposal = (
    id: string,
    title: string,
    tier: string,
    version: number | undefined,
    originTask?: string,
    age: number | null = 3600,
  ) => ({
    band: 'decisions',
    kind: 'document_proposal',
    label: copy[tier]?.label ?? 'Document proposal',
    question: copy[tier]?.question ?? 'Confirm or dismiss the proposal?',
    if_ignored: 'The proposal stays pending.',
    proposal: {
      id,
      title,
      tier,
      ...(version == null ? {} : { version }),
      origin_type: originTask ? 'task' : 'operator',
      ...(originTask ? { origin_id: originTask } : {}),
      proposed_at: proposedAt,
      ...(age == null ? {} : { age_seconds: age }),
    },
  })
  return {
    operatorDecisions: 5,
    items: [
      {
        ...proposal('req-attention', 'Operator attention', 'requirement', 3, 'review-task'),
        sentence: 'Confirming a later one dismisses the earlier ones.',
      },
      // An older server sends no age: the card reads proposed_at instead.
      proposal('design-dashboard', 'Dashboard architecture', 'system_design', 4, 'other-task', null),
      // An unusable age falls back to proposed_at too.
      proposal('DEC-16', longDecision, 'decision', undefined, undefined, -5),
      proposal('req-other', 'Other requirement', 'requirement', 1, 'review-task'),
      // A tier this client does not know is listed read-only.
      proposal('charter-x', 'Unknown tier proposal', 'charter', 1, 'review-task'),
    ],
    actions: {},
  }
}

// The Inbox keeps deciding pending proposals of every tier, with a dialog
// before a version dismissal, and says nothing about a task waiting on one
// (req-260810-23b69f REQ-1, REQ-3, AC-4.3; req-operator-next-action AC-2.6).
test('every proposal tier still confirms and dismisses from the inbox without task wait copy', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.documents = {
    '/v1/requirements/req-attention': versionView('requirement', 'req-attention', 2, 3),
    '/v1/requirements/req-other': versionView('requirement', 'req-other', null, 1),
    '/v1/system-designs/design-dashboard': versionView('system_design', 'design-dashboard', 3, 4),
    '/v1/decisions/DEC-16': {
      id: 'DEC-16',
      statement: longDecision,
      context: 'Each repository ships its own kit.',
      alternatives_rejected: 'One shared kit was rejected: repositories diverge.',
      status: 'proposed',
    },
  }
  const posted: string[] = []
  const harness = await serve(page, fixture, (request, path) => {
    posted.push(`${request.url().includes('dismiss') ? 'dismiss' : 'confirm'} ${path}`)
    const id = path.split('/')[3]
    fixture.items = fixture.items.filter((item) => item.proposal?.id !== id)
    return undefined
  })
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(5)
  const footer = () => decisionDetail(page).locator('[data-decision-footer]')

  const requirement = await select(page, 'Operator attention')
  await expect(requirement.locator('[data-proposal-version]')).toHaveText('v3')
  await expect(requirement.locator('[data-proposal-age]')).toHaveText(/ago$/)
  await expect(requirement.getByRole('link', { name: 'Task' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/review-task/full',
  )
  await expect(requirement.getByRole('link', { name: 'View changes →' })).toBeVisible()
  await expect(requirement.getByRole('region', { name: 'Proposed changes' })).toBeVisible()
  await expect(page.locator('body')).not.toContainText(/withheld|withholding|cannot be claimed|waiting on a/i)

  // Canceling a version dismissal sends nothing.
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('Operator attention')
  await expect(dialog).toContainText('Dismiss version 3?')
  await expect(dialog).toContainText('stay in version history, but it cannot be confirmed later')
  const before = harness.posts.length
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  expect(harness.posts.length).toBe(before)
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(decisionRows(page)).toHaveCount(4)

  await select(page, 'Dashboard architecture')
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(decisionRows(page)).toHaveCount(3)

  const decision = await select(page, 'All FunnelFlux repositories')
  await expect(decision.getByRole('region', { name: 'Proposed decision' })).toContainText(
    'One shared kit was rejected: repositories diverge.',
  )
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(decisionRows(page)).toHaveCount(2)

  await select(page, 'Other requirement')
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /Dismiss/ })
    .click()
  await expect(decisionRows(page)).toHaveCount(1)
  expect(posted).toEqual([
    'confirm /v1/requirements/req-attention/versions/3/confirm',
    'confirm /v1/system-designs/design-dashboard/versions/4/confirm',
    'confirm /v1/decisions/DEC-16/confirm',
    'dismiss /v1/requirements/req-other/versions/1/dismiss',
  ])
})

test('a maintainer without confirm_documents reads proposals without acting', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.capabilities = capabilitiesForRole('maintainer').filter((capability) => capability !== 'confirm_documents')
  fixture.role = 'maintainer'
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(5)
  for (const title of ['Operator attention', 'Dashboard architecture', 'All FunnelFlux repositories']) {
    const detail = await select(page, title)
    await expect(detail.locator('[data-decision-footer]').getByRole('button')).toHaveCount(0)
  }
})

// The contract layer decodes a refusal body once: a JSON envelope reaches the
// page as its sentence, and a body that is no sentence, such as a proxy's HTML
// page, yields the response's status text instead (component-web-dashboard
// invariant 13).
test('a failed inbox read shows the JSON body as its sentence and never the JSON', async ({ page }) => {
  await serve(page, decisionsFixture())
  let body: { status: number; contentType: string; body: string } = {
    status: 503,
    contentType: 'application/json',
    body: JSON.stringify({ error: 'inbox_unavailable', message: 'the inbox read is unavailable right now' }),
  }
  await page.route('**/v1/next-actions*', (route) => route.fulfill(body))

  await page.goto('/w/demo/inbox')
  const failure = page.getByRole('alert').filter({ hasText: 'Could not load the inbox' })
  await expect(failure).toContainText('The inbox read is unavailable right now.')
  await expect(failure).not.toContainText('{')
  await expect(failure).not.toContainText('inbox_unavailable')

  body = { status: 502, contentType: 'text/html', body: '<html><body><h1>502 Bad Gateway</h1></body></html>' }
  await page.reload()
  // The page reads the HTTP status text the contract layer falls back to.
  await expect(failure).toContainText('Bad Gateway.')
  await expect(failure).not.toContainText('<')
  await expect(failure).not.toContainText('502 Bad Gateway')
})

test('with no decisions the header says nothing needs you and the band shows the empty state', async ({ page }) => {
  const fixture = decisionsFixture()
  fixture.items = fixture.items.filter((item) => item.band !== 'decisions')
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')

  await expect(page.locator('[data-inbox-summary]')).toHaveText('Nothing needs you right now.')
  const empty = page.locator('[data-no-decisions]')
  await expect(empty).toContainText("You're clear")
  await expect(empty).toContainText('No decisions are waiting. Conveyor will put the next one here.')
  await expect(page.getByRole('region', { name: 'Blocked' })).toContainText('Waiting on other tasks')
  await expect(page.getByRole('region', { name: 'Automatic' })).toContainText('Automatic work')
})

function versionView(kind: 'requirement' | 'system_design', id: string, current: number | null, pending: number) {
  const version = (number: number, text: string) => ({
    version: number,
    content: `# ${id}\n\n## Scope\n\nShared paragraph.\n\n## Rules\n\n${text}`,
    statements: [],
    confirmed: number === current,
  })
  const head = kind === 'requirement' ? { requirement: { id, title: id } } : { document: { id, title: id } }
  return {
    ...head,
    ...(current == null ? {} : { current_version: version(current, 'Confirmed rule text.') }),
    pending_versions: [version(pending, 'Proposed rule text.')],
    versions: [],
  }
}
