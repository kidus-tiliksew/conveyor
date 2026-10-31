import { expect, type Page, type Request, type Route } from '@playwright/test'
import { citedSections, headingWords } from '../src/components/inbox/citation-matcher'
import { test } from './helpers/bounded-detail'
import { callerIdentity, capabilitiesForRole } from './helpers/caller-identity'

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

// A promise the test resolves to let held responses through.
function gate() {
  let release = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  return { held, release }
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
  await expect(rowItems(page).nth(1)).toContainText('No PR yet')
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
  await expect(card.getByText('Does this plan meet the requirement?')).toBeVisible()
  // The recommendation is a callout, and the label a pill in its kind's tone.
  await expect(card.locator('[data-recommendation]')).toHaveText(
    'Agent recommendsApprove: the plan covers every criterion.',
  )
  await expect(card.locator('[data-decision-label]')).toHaveAttribute('data-kind-tone', 'gate')
  await expect(card.locator('[data-decision-label]')).not.toHaveClass(/attention/)
  // The deadline is a countdown chip in the head line and a time of day in
  // the footer's consequence line.
  await expect(card.locator('[data-due]')).toHaveText(/^due in /)
  await expect(card.locator('[data-decision-footer] [data-if-ignored]')).toHaveText(
    /^If nobody acts by \d{1,2}:\d{2} [AP]M: The task waits at the plan gate\.$/,
  )
  await expect(card.locator('[data-context-heading]')).toHaveText('Plan')
  await expect(card.locator('[data-next-action-deadline]')).toHaveAttribute('datetime', soon)
  await card.getByText('Show IDs').click()
  await expect(card.getByText('wo-zeta-1')).toBeVisible()

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
  expect(harness.inboxRequests.length).toBeGreaterThan(before)
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
const rowCheck = (card: import('@playwright/test').Locator, target: ContextTarget) =>
  card.locator(`[data-context-check="${target.kind}:${target.id}"]`)
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

  // Raw target IDs and the proposer appear only inside the IDs disclosure.
  const outside = await textOutsideIDs(card)
  for (const target of [requirement, design]) expect(outside).not.toContain(target.id)
  expect(outside).not.toContain('system:triage')
  await expect(card.locator('details').last()).toContainText(requirement.id)

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

test('a mixed selection posts each row’s selected choice in the server order', async ({ page }) => {
  const [requirement, design, dashboard] = contextTargets
  const fixture = contextFixture({ targets: contextTargets })
  const harness = await serve(page, fixture, (_request, path) => {
    if (path.endsWith(`/${dashboard.id}/confirm`)) decide(fixture, [])
    return { state: 'decided' }
  })
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  // Sources differ, so each row states its own.
  await expect(card.locator('[data-context-source]')).toHaveCount(0)
  await expect(card.locator('[data-context-row-meta]').nth(1)).toContainText('Suggested by planning')
  // Equal relative ages still print on each row, with the absolute time on hover.
  for (const meta of await card.locator('[data-context-row-meta]').all()) {
    await expect(meta).toContainText('2 hours ago')
    await expect(meta.locator('time')).toHaveAttribute('title', /\S/)
  }
  await expect(applyButton(card)).toHaveText('Attach all 3')

  await rowCheck(card, design).uncheck()
  await expect(card.getByRole('checkbox', { name: contextLabel('dismiss', design), exact: true })).not.toBeChecked()
  await expect(card.locator('[data-context-proposal]').nth(1)).toHaveAttribute('data-checked', 'false')
  await expect(applyButton(card)).toHaveText('Attach 2 · dismiss 1')
  // c still acts with focus on a checkbox.
  await page.keyboard.press('c')
  await expect.poll(() => harness.posts.length).toBe(3)
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual([
    contextPath(requirement, 'confirm'),
    contextPath(design, 'dismiss'),
    contextPath(dashboard, 'confirm'),
  ])
  await expect(decisionRows(page)).toHaveCount(0)
})

test('one source with distinct creation times that round to the same age keeps each row’s own time', async ({
  page,
}) => {
  await page.clock.setFixedTime(new Date('2026-10-21T12:00:00Z'))
  const fixture = contextFixture()
  const action = fixture.actions['T-ctx'] as { details: { context_proposals: Record<string, unknown>[] } }
  const times = ['2026-10-19T03:00:00Z', '2026-10-19T15:00:00Z']
  action.details.context_proposals.forEach((proposal, index) => {
    proposal.source = 'triage'
    proposal.created_at = times[index]
  })
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  // The source is shared; the age is not, because the times differ.
  await expect(card.locator('[data-context-source]')).toHaveText('Suggested by triage')
  const metas = card.locator('[data-context-row-meta]')
  await expect(metas).toHaveCount(2)
  for (const [index, time] of times.entries()) {
    await expect(metas.nth(index)).toHaveText('2 days ago')
    await expect(metas.nth(index).locator('time')).toHaveAttribute('datetime', time)
    await expect(metas.nth(index).locator('time')).toHaveAttribute('title', /\S/)
  }
  const titles = await metas.locator('time').evaluateAll((nodes) => nodes.map((node) => node.getAttribute('title')))
  expect(new Set(titles).size).toBe(2)
})

test('Apply reads the effect of every selection, and one proposal reads Attach or Dismiss', async ({ page }) => {
  const [requirement, design] = contextTargets
  const fixture = contextFixture()
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.locator('[data-kbd-hint]').filter({ hasText: 'c' })).toBeVisible()
  await rowCheck(card, requirement).uncheck()
  await rowCheck(card, design).uncheck()
  await expect(applyButton(card)).toHaveText('Dismiss all 2')
  await rowCheck(card, design).check()
  await expect(applyButton(card)).toHaveText('Attach 1 · dismiss 1')

  fixture.actions['T-ctx'] = contextAction([requirement])
  await page.reload()
  await expect(applyButton(decisionDetail(page))).toHaveText('Attach')
  await rowCheck(decisionDetail(page), requirement).uncheck()
  await expect(applyButton(decisionDetail(page))).toHaveText('Dismiss')
})

test('Dismiss all posts every dismiss choice once in the server order and the item leaves', async ({ page }) => {
  const fixture = contextFixture({ targets: contextTargets })
  const harness = await serve(page, fixture, (_request, path) => {
    if (path.endsWith(`/${contextTargets[2].id}/dismiss`)) decide(fixture, [])
    return { state: 'dismissed' }
  })
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.locator('[data-context-proposal]')).toHaveCount(3)
  await dismissAllButton(card).dblclick()
  await expect.poll(() => harness.posts.length).toBe(3)
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual(
    contextTargets.map((target) => contextPath(target, 'dismiss')),
  )
  for (const post of harness.posts) expect(post.postData()).toBeNull()
  await expect(decisionRows(page)).toHaveCount(0)
  expect(harness.posts).toHaveLength(3)
})

test('leaving the inbox mid-run sends nothing after the request in flight', async ({ page }) => {
  const fixture = contextFixture({ targets: contextTargets })
  fixture.activity = {
    'T-ctx': {
      task: task('T-ctx', 'Repair post-merge test failures', 'queued'),
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  const harness = await serve(page, fixture, () => ({ state: 'confirmed' }))
  const held = gate()
  harness.holdPosts = held.held
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await applyButton(card).click()
  await expect.poll(() => harness.posts.length).toBe(1)
  // Selection is frozen while the run is in flight.
  await expect(rowCheck(card, contextTargets[1])).toBeDisabled()
  // Opening the task leaves the inbox while the first act is in flight.
  await card.getByRole('link', { name: 'Repair post-merge test failures' }).click()
  await expect(page).toHaveURL(/\/tasks\/T-ctx/)
  held.release()
  await page.waitForTimeout(500)
  expect(harness.posts).toHaveLength(1)
})

test('switching workspace mid-run sends nothing after the request in flight, and nothing to the new workspace', async ({
  page,
}) => {
  const fixture = contextFixture({ targets: contextTargets })
  fixture.workspaces = [
    { id: 'demo', name: 'Demo' },
    { id: 'other', name: 'Other' },
  ]
  const harness = await serve(page, fixture, () => ({ state: 'confirmed' }))
  const held = gate()
  harness.holdPosts = held.held
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await applyButton(card).click()
  await expect.poll(() => harness.posts.length).toBe(1)
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }).click()
  await expect(
    page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }),
  ).toHaveAttribute('aria-current', 'true')
  held.release()
  await page.waitForTimeout(500)
  expect(harness.posts).toHaveLength(1)
  expect(new URL(harness.posts[0].url()).searchParams.get('workspace_id')).toBe('demo')
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

test('task-context choices a caller may not take disable both controls in the inbox with the capability reason', async ({
  page,
}) => {
  const harness = await serve(page, contextFixture({ allowed: false }))
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(applyButton(card)).toBeDisabled()
  await expect(dismissAllButton(card)).toBeDisabled()
  await expect(card.locator('[data-context-control-reason]')).toHaveText('Needs the operate gates capability.')
  await expect(card.locator('[data-context-row-reason]').first()).toHaveText('Needs the operate gates capability.')
  await page.keyboard.press('c')
  await page.waitForTimeout(200)
  expect(harness.posts).toHaveLength(0)
})

test('a disallowed dismiss leaves an all-checked Apply enabled and disables Dismiss all with its reason', async ({
  page,
}) => {
  const [requirement, design] = contextTargets
  const fixture = contextFixture()
  const action = fixture.actions['T-ctx'] as { choices: { id: string; allowed: boolean }[] }
  for (const entry of action.choices) if (entry.id.startsWith('dismiss:')) entry.allowed = false
  const harness = await serve(page, fixture, (_request, path) => {
    if (path.endsWith(`/${design.id}/confirm`)) decide(fixture, [])
    return { state: 'confirmed' }
  })
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(dismissAllButton(card)).toBeDisabled()
  await expect(card.locator('[data-context-control-reason]')).toHaveText(
    'Dismiss all: Needs the operate gates capability.',
  )
  await expect(card.locator('[data-context-row-reason]').first()).toHaveText(
    'Dismiss: Needs the operate gates capability.',
  )
  // Unchecking a row would send a disallowed dismiss, so Apply disables.
  await rowCheck(card, requirement).uncheck()
  await expect(applyButton(card)).toBeDisabled()
  await rowCheck(card, requirement).check()
  await expect(applyButton(card)).toBeEnabled()
  await applyButton(card).click()
  await expect.poll(() => harness.posts.length).toBe(2)
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual([
    contextPath(requirement, 'confirm'),
    contextPath(design, 'confirm'),
  ])
})

test('a monitor-filed task states its origin with the run link, and without one when none was observed', async ({
  page,
}) => {
  const run = 'https://github.com/kidus-tiliksew/conveyor/actions/runs/38879608883'
  const fixture = contextFixture({
    origin: { kind: 'post_merge_failure', summary: 'Filed by the monitor: post-merge failure', source_url: run },
  })
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const origin = decisionDetail(page).locator('[data-monitor-origin]')
  await expect(origin).toContainText('Filed by the monitor: post-merge failure')
  await expect(origin.getByRole('link', { name: 'View the run' })).toHaveAttribute('href', run)

  fixture.actions['T-ctx'] = contextAction(contextTargets.slice(0, 2), true, {
    kind: 'post_merge_failure',
    summary: 'Filed by the monitor: post-merge failure',
  })
  await page.reload()
  const bare = decisionDetail(page).locator('[data-monitor-origin]')
  await expect(bare).toContainText('Filed by the monitor: post-merge failure')
  await expect(bare.getByRole('link')).toHaveCount(0)
})

test('the context section folds the task body and reads no proposed document', async ({ page }) => {
  const body = '## Problem\n\nThe post-merge run failed on **main**.\n\nThe flaky suite needs a fix.'
  const fixture = contextFixture({ body })
  fixture.activity = {
    'T-ctx': {
      task: { ...task('T-ctx', 'Repair post-merge test failures', 'queued'), body },
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  fixture.documents = {
    [`/v1/requirements/${contextTargets[0].id}`]: { requirement: { id: contextTargets[0].id } },
    [`/v1/system-designs/${contextTargets[1].id}`]: { document: { id: contextTargets[1].id } },
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.locator('[data-context-heading]')).toHaveText('Proposed context')
  const folded = card.locator('[data-task-body]')
  await expect(folded.locator('summary')).toHaveText('The task·The post-merge run failed on main.')
  await expect(folded.getByText('The flaky suite needs a fix.')).toBeHidden()
  await folded.locator('summary').click()
  await expect(folded.getByText('The flaky suite needs a fix.')).toBeVisible()
  await expect(folded.getByRole('link', { name: 'Full task →' })).toHaveAttribute('href', '/w/demo/tasks/T-ctx/full')
  await page.waitForTimeout(300)
  expect(harness.documentRequests).toHaveLength(0)
  expect(harness.posts).toHaveLength(0)
})

test('a task without a body says so in the context section', async ({ page }) => {
  const fixture = contextFixture()
  fixture.activity = {
    'T-ctx': {
      task: task('T-ctx', 'Repair post-merge test failures', 'queued'),
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  await expect(decisionDetail(page).getByRole('region', { name: 'Decision context' })).toContainText(
    'The task has no description.',
  )
})

// The document preview panel (feature-operator-next-action NA-6; AC-2.3): a
// row's title or Preview opens the confirmed document in place, the
// sections the justification cites stay at full strength and the rest dim,
// and the footer checkbox mirrors the row.
const previewRequirementPath = `/v1/requirements/${contextTargets[0].id}`
const previewDesignPath = `/v1/system-designs/${contextTargets[1].id}`

function previewRequirement(version = 4) {
  return {
    requirement: { id: contextTargets[0].id, title: contextTargets[0].title },
    current_version: {
      version,
      content: '# Review gates\n\nEvery gate keeps its evidence.',
      confirmed_at: '2026-10-09T10:00:00Z',
      statements: [
        {
          id: 'REQ-1',
          statement: 'Each gate records who decided.',
          acceptance_criteria: [{ id: 'AC-1.1', statement: 'The decider is named.' }],
        },
        {
          id: 'REQ-7',
          statement: 'Evidence stays attached to the gate.',
          acceptance_criteria: [
            { id: 'AC-7.1', statement: 'The evidence link survives a refresh.' },
            { id: 'AC-7.10', statement: 'Archived evidence stays readable.' },
          ],
        },
      ],
    },
    pending_versions: [{ version: version + 1, content: 'A PROPOSED REVISION', statements: [] }],
  }
}

function previewDesign(version = 32, scope = 'Scope text.') {
  return {
    document: { id: contextTargets[1].id, title: contextTargets[1].title },
    current_version: {
      version,
      content: `# Testing strategy\n\nPreamble.\n\n## Scope\n\n${scope}\n\n## Credential handling\n\nCredential text.\n\n## 3. Verification\n\nVerification text.`,
      confirmed_at: '2026-10-18T10:00:00Z',
    },
    pending_versions: [{ version: version + 1, content: 'A PROPOSED REVISION' }],
    versions: [],
  }
}

function previewFixture(
  justifications: [string, string] = [
    'AC-7.1 requires the evidence link to survive.',
    'Its Credential handling section and its Verification section apply.',
  ],
) {
  const targets = contextTargets
    .slice(0, 2)
    .map((target, index) => ({ ...target, justification: justifications[index] }))
  const fixture = contextFixture({ targets })
  fixture.activity = {
    'T-ctx': {
      task: task('T-ctx', 'Repair post-merge test failures', 'queued'),
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  fixture.documents = { [previewRequirementPath]: previewRequirement(), [previewDesignPath]: previewDesign() }
  return fixture
}

const previewPanel = (page: Page) => page.getByRole('dialog', { name: /^Preview: / })
const opacityOf = (locator: import('@playwright/test').Locator) =>
  locator.evaluate((node) => Number.parseFloat(getComputedStyle(node).opacity))

test('a row title opens the preview panel with the confirmed document, its header, and the cited sections', async ({
  page,
}) => {
  const harness = await serve(page, previewFixture())
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  const title = card.getByRole('button', { name: contextTargets[1].title, exact: true })
  await title.click()
  const panel = previewPanel(page)
  await expect(panel).toHaveAccessibleName(`Preview: ${contextTargets[1].title}`)
  await expect(panel.locator('[data-preview-version]')).toContainText('v32 · confirmed')
  await expect(panel.locator('[data-preview-version] time')).toHaveAttribute('title', /\S/)
  await expect(panel.locator('[data-preview-why]')).toContainText('Why planning suggests it')
  await expect(panel.locator('[data-preview-why]')).toContainText('Its Credential handling section')
  await expect(panel.getByRole('link', { name: 'Open full page ↗' })).toHaveAttribute(
    'href',
    `/w/demo/system-design?document=${contextTargets[1].id}`,
  )
  // The outline lists every section; cited entries read in the foreground.
  const outline = panel.getByRole('navigation', { name: 'Outline' }).locator('[data-outline-entry]')
  await expect(outline).toHaveText(['Testing strategy', 'Scope', 'Credential handling', '3. Verification'])
  await expect(outline.nth(2)).toHaveClass(/text-foreground/)
  await expect(outline.nth(1)).toHaveClass(/text-faint/)
  await expect(panel.locator('[data-preview-cited-count]')).toHaveText('2 of 4 sections cited')
  // Cited sections render like ordinary content; the rest dim until focused.
  await page.mouse.move(0, 0)
  const credential = panel.locator('[data-preview-section][data-cited="true"]').first()
  const scope = panel.locator('[data-preview-section]').nth(1)
  await expect(credential).toContainText('Credential text.')
  await expect.poll(() => opacityOf(credential)).toBe(1)
  await expect.poll(() => opacityOf(scope)).toBeCloseTo(0.4, 1)
  await scope.focus()
  await expect.poll(() => opacityOf(scope)).toBe(1)
  // Only the confirmed version renders, never a proposed one.
  await expect(panel).not.toContainText('A PROPOSED REVISION')
  // The panel never uses the attention tone.
  expect(await panel.evaluate((node) => node.querySelectorAll('[class*="attention"]').length)).toBe(0)
  // Outline selection scrolls to the section inside the panel.
  await outline.nth(3).click()
  await expect(panel.locator('[data-preview-section]').nth(3)).toBeInViewport()

  // Escape closes the panel, returns focus to the title, and keeps the detail.
  await page.keyboard.press('Escape')
  await expect(previewPanel(page)).toHaveCount(0)
  await expect(title).toBeFocused()
  await expect(card).toBeVisible()
  for (const request of harness.documentRequests)
    expect(new URL(request.url()).searchParams.get('workspace_id')).toBe('demo')
  expect(harness.posts).toHaveLength(0)
})

test('Preview steps through the proposed documents and its checkbox mirrors the row without posting', async ({
  page,
}) => {
  const harness = await serve(page, previewFixture())
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  const rows = card.locator('[data-context-proposal]')
  await card.getByRole('button', { name: `Preview ${contextTargets[0].title}` }).click()
  const panel = previewPanel(page)
  await expect(panel).toHaveAccessibleName(`Preview: ${contextTargets[0].title}`)
  await expect(panel.locator('[data-preview-position]')).toHaveText('1 of 2 proposed')
  await expect(rows.nth(0)).toHaveAttribute('data-current', 'true')
  await expect(panel.getByRole('button', { name: 'Previous document' })).toBeDisabled()

  // A requirement's sections are its prose headings plus each REQ and AC; the
  // AC-7.1 citation also cites REQ-7, and AC-7.10 stays uncited.
  await expect(panel.locator('[data-outline-entry]')).toHaveText([
    'Review gates',
    'REQ-1',
    'AC-1.1',
    'REQ-7',
    'AC-7.1',
    'AC-7.10',
  ])
  await expect(panel.locator('[data-preview-cited-count]')).toHaveText('2 of 6 sections cited')
  await expect(panel.locator('[data-preview-section="REQ-7"]')).toHaveAttribute('data-cited', 'true')
  await expect(panel.locator('[data-preview-section="AC-7.1"]')).toHaveAttribute('data-cited', 'true')
  await expect(panel.locator('[data-preview-section="AC-7.10"]')).toHaveAttribute('data-cited', 'false')

  // The footer checkbox and the row share one state; neither posts.
  await panel.locator('[data-preview-attach]').uncheck()
  await expect(card.locator(`[data-context-check="requirement:${contextTargets[0].id}"]`)).not.toBeChecked()
  await expect(card.locator('[data-context-apply]')).toHaveText('Attach 1 · dismiss 1')

  // Inbox keys do not act while the panel is open.
  await page.keyboard.press('c')
  await page.keyboard.press('j')

  // ArrowDown moves to the next document, bounded at the end.
  await page.keyboard.press('ArrowDown')
  await expect(panel).toHaveAccessibleName(`Preview: ${contextTargets[1].title}`)
  await expect(panel.locator('[data-preview-position]')).toHaveText('2 of 2 proposed')
  await expect(rows.nth(1)).toHaveAttribute('data-current', 'true')
  await expect(rows.nth(0)).not.toHaveAttribute('data-current', 'true')
  await expect(panel.locator('[data-preview-attach]')).toBeChecked()
  await page.keyboard.press('ArrowDown')
  await expect(panel.locator('[data-preview-position]')).toHaveText('2 of 2 proposed')
  await expect(panel.getByRole('button', { name: 'Next document' })).toBeDisabled()
  await panel.getByRole('button', { name: 'Previous document' }).click()
  await expect(panel.locator('[data-preview-position]')).toHaveText('1 of 2 proposed')
  await expect(panel.locator('[data-preview-attach]')).not.toBeChecked()
  await page.keyboard.press('Escape')
  await expect(card.getByRole('button', { name: `Preview ${contextTargets[0].title}` })).toBeFocused()
  await page.waitForTimeout(200)
  expect(harness.posts).toHaveLength(0)
})

test('a Setext heading and a repeated heading are cited by the text their author wrote', async ({ page }) => {
  const fixture = previewFixture(['Unrelated.', 'Its Verification section applies to this change.'])
  fixture.documents = {
    ...fixture.documents,
    [previewDesignPath]: {
      document: { id: contextTargets[1].id, title: contextTargets[1].title },
      current_version: {
        version: 32,
        confirmed_at: '2026-10-18T10:00:00Z',
        content:
          '# Strategy\n\nIntro.\n\nVerification\n------------\n\nFirst verification.\n\n## Scope\n\nScope text.\n\n## Verification\n\nSecond verification.',
      },
      pending_versions: [],
      versions: [],
    },
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  await decisionDetail(page).getByRole('button', { name: contextTargets[1].title, exact: true }).click()
  const panel = previewPanel(page)
  await expect(panel.locator('[data-outline-entry]')).toHaveText([
    'Strategy',
    'Verification',
    'Scope',
    'Verification (2)',
  ])
  await expect(panel.locator('[data-preview-cited-count]')).toHaveText('2 of 4 sections cited')
  const sections = panel.locator('[data-preview-section]')
  await expect(sections.nth(1)).toHaveAttribute('data-cited', 'true')
  await expect(sections.nth(3)).toHaveAttribute('data-cited', 'true')
  await page.mouse.move(0, 0)
  await expect.poll(() => opacityOf(sections.nth(1))).toBe(1)
  await expect.poll(() => opacityOf(sections.nth(3))).toBe(1)
  await expect.poll(() => opacityOf(sections.nth(2))).toBeCloseTo(0.4, 1)
})

test('a justification that names no section dims nothing and states no count', async ({ page }) => {
  await serve(page, previewFixture(['Unrelated prose about the dashboard.', 'Nothing here names a section.']))
  await page.goto('/w/demo/inbox')
  await decisionDetail(page).getByRole('button', { name: contextTargets[1].title, exact: true }).click()
  const panel = previewPanel(page)
  await expect(panel.locator('[data-preview-section]')).toHaveCount(4)
  await expect(panel.locator('[data-preview-cited-count]')).toHaveCount(0)
  await page.mouse.move(0, 0)
  for (const section of await panel.locator('[data-preview-section]').all())
    await expect.poll(() => opacityOf(section)).toBe(1)
  await expect(panel.locator('[data-outline-entry].text-faint')).toHaveCount(0)
})

test('a reopened preview never shows cached content while its fresh read fails, until Retry succeeds', async ({
  page,
}) => {
  const fixture = previewFixture()
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  const title = card.getByRole('button', { name: contextTargets[1].title, exact: true })
  await title.click()
  await expect(previewPanel(page)).toContainText('Scope text.')
  await page.keyboard.press('Escape')

  // The document became unavailable: the cached v32 must not stand in for it.
  fixture.documents = { ...fixture.documents, [previewDesignPath]: 503 }
  await title.click()
  const panel = previewPanel(page)
  await expect(panel.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(panel).not.toContainText('Scope text.')
  await expect(panel).not.toContainText('v32')
  // The failed read is stated in neutral text, never the failure tone.
  await expect(panel.locator('[data-preview-error]')).toHaveClass(/text-muted/)
  expect(await panel.evaluate((node) => node.querySelectorAll('[class*="failure"], [class*="attention"]').length)).toBe(
    0,
  )

  fixture.documents = { ...fixture.documents, [previewDesignPath]: previewDesign(33, 'Fresh scope.') }
  await panel.getByRole('button', { name: 'Retry' }).click()
  await expect(panel.locator('[data-preview-version]')).toContainText('v33')
  await expect(panel).toContainText('Fresh scope.')
})

test('a read left in flight by a closed preview never shows when it reopens', async ({ page }) => {
  const fixture = previewFixture()
  fixture.documents = { [previewDesignPath]: previewDesign(32, 'Late content from the closed panel.') }
  const harness = await serve(page, fixture)
  const held = gate()
  harness.holdDocuments = held.held
  await page.goto('/w/demo/inbox')
  const title = decisionDetail(page).getByRole('button', { name: contextTargets[1].title, exact: true })
  await title.click()
  await expect.poll(() => harness.documentRequests.length).toBeGreaterThanOrEqual(1)
  await page.keyboard.press('Escape')
  await expect(previewPanel(page)).toHaveCount(0)
  // The read answers after the panel closed; the document then fails.
  held.release()
  harness.holdDocuments = undefined
  await page.waitForTimeout(300)
  fixture.documents = { [previewDesignPath]: 503 }
  await title.click()
  const panel = previewPanel(page)
  await expect(panel.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(panel).not.toContainText('Late content from the closed panel.')
})

test('the citation matcher marks whole-token IDs, numbered headings, and parent requirements', () => {
  const sections = [
    { id: 'heading:Overview:1', title: 'Overview', heading: false },
    { id: 'heading:Credential handling:1', title: 'Credential handling', heading: true },
    { id: 'heading:3.2 Verification:1', title: '3.2 Verification', heading: true },
    { id: 'heading:Factory verification:1', title: '`Factory` verification', heading: true },
    { id: 'heading:Known gaps:1', title: 'Known gaps', heading: true },
    { id: 'REQ-1', title: 'REQ-1', heading: false },
    { id: 'REQ-10', title: 'REQ-10', heading: false },
    { id: 'REQ-7', title: 'REQ-7', heading: false },
    { id: 'AC-7.1', title: 'AC-7.1', heading: false },
    { id: 'AC-7.10', title: 'AC-7.10', heading: false },
  ]
  const cites = (text: string) => [...citedSections(sections, text)].sort()
  // Recorded triage justifications.
  expect(
    cites(
      'Its Credential handling section says Auth API accepts Clerk sessions only through the /v1/auth/validate exchange; its Verification section lists token-signature and cutoff tests this task extends.',
    ),
  ).toEqual(['heading:3.2 Verification:1', 'heading:Credential handling:1'])
  expect(
    cites(
      'REQ-7 and AC-7.1 require exactly one sign-in event per session at the exchange, and REQ-1 requires identity to stay unchanged.',
    ),
  ).toEqual(['AC-7.1', 'REQ-1', 'REQ-7'])
  // An AC cites its parent REQ; a REQ cites no AC.
  expect(cites('Only AC-7.1 applies.')).toEqual(['AC-7.1', 'REQ-7'])
  // Whole tokens: REQ-1 never matches REQ-10, AC-7.1 never matches AC-7.10.
  expect(cites('REQ-10 and AC-7.10 apply.')).toEqual(['AC-7.10', 'REQ-10', 'REQ-7'])
  expect(cites('PREREQ-1 and XAC-7.1 do not count.')).toEqual([])
  // No identifier continuation after an ID either; sentence punctuation still counts.
  expect(cites('REQ-1extra, REQ-1-other, REQ-1_x, AC-7.1suffix, and AC-7.1.b do not count.')).toEqual([])
  expect(cites('See REQ-1. Then (AC-7.1), and REQ-10: done.')).toEqual(['AC-7.1', 'REQ-1', 'REQ-10', 'REQ-7'])
  // Case-insensitive whole words, inline marks ignored; no partial words.
  expect(cites('the FACTORY VERIFICATION rules')).toEqual([
    'heading:3.2 Verification:1',
    'heading:Factory verification:1',
  ])
  expect(cites('Credential handlingness is not a match.')).toEqual([])
  // The synthetic Overview title never matches; empty text matches nothing.
  expect(cites('An overview of the design.')).toEqual([])
  expect(cites('   ')).toEqual([])
  expect(headingWords('3. Verification')).toEqual(['Verification'])
})

test('the context decision fits a 390px viewport with long titles and justifications', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  const long = contextTargets.map((target) => ({
    ...target,
    title: `${target.title} with an unusually long document title that must wrap inside the row`,
    justification: `${target.justification} ${'Long-justification-without-breaks-'.repeat(4)}`,
  }))
  await serve(
    page,
    contextFixture({
      targets: long,
      origin: {
        kind: 'post_merge_failure',
        summary: 'Filed by the monitor: post-merge failure',
        source_url: 'https://github.com/kidus-tiliksew/conveyor/actions/runs/38879608883',
      },
    }),
  )
  await page.goto('/w/demo/inbox')
  await decisionRows(page).first().click()
  await expect(decisionDetail(page).locator('[data-context-proposal]')).toHaveCount(3)
  // A justification longer than its two lines expands and collapses.
  const expand = decisionDetail(page).locator('[data-context-expand]').first()
  await expect(expand).toHaveText('Show more')
  await expand.click()
  await expect(expand).toHaveText('Show less')
  await expect(decisionDetail(page).locator('[data-context-justification]').first()).not.toHaveClass(/line-clamp-2/)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

test('agent_question opens the direction input and posts the direction', async ({ page }) => {
  const harness = await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox')
  const card = await select(page, 'Alpha agent question')
  await card.getByRole('button', { name: 'Answer' }).click()
  await card.getByLabel('Direction').fill('Use the shared cache.')
  await card.getByRole('button', { name: 'Answer' }).last().click()
  await expect.poll(() => harness.posts.length).toBe(1)
  expect(new URL(harness.posts[0].url()).pathname).toBe('/v1/work-orders/wo-alpha/recover')
  expect(harness.posts[0].postDataJSON()).toMatchObject({ direction: 'Use the shared cache.' })
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

test('selecting each decision shows its context in place without leaving the inbox', async ({ page }) => {
  const fixture = decisionsFixture()
  const base = (id: string, title: string) => ({
    task: task(id, title),
    events: [],
    jobs: [],
    work_orders: [],
    interventions: [],
  })
  fixture.activity = {
    'T-zeta': {
      ...base('T-zeta', 'Zeta plan everyone waits on'),
      spec: {
        task_id: 'T-zeta',
        version: 1,
        content: '# Plan body for zeta\n\nShip it.',
        acceptance_criteria: [],
        created_at: proposedAt,
      },
    },
    'T-alpha': {
      ...base('T-alpha', 'Alpha agent question'),
      work_orders: [
        {
          id: 'wo-alpha',
          task_id: 'T-alpha',
          job_id: 'job-alpha',
          stage: 'implement',
          state: 'released',
          checkpoint: {
            decision_request: 'Which cache should it use?',
            class: 'authority_conflict',
            citations: [
              {
                document_id: 'sd-cache',
                document_kind: 'system_design',
                document_title: 'Cache design',
                cited_version: 2,
                current_confirmed_version: 3,
                newer_confirmed: true,
                pending_proposals: [],
              },
            ],
          },
        },
      ],
    },
    'T-mid': {
      ...base('T-mid', 'Mid item without a deadline'),
      events: [
        ['job-impl', 'First implementation.'],
        ['job-review', 'Reviewer says the cache key is wrong.'],
        ['job-impl-2', 'Implementer fixed the cache key.'],
      ].map(([jobId, summary], index) => ({
        id: index + 1,
        task_id: 'T-mid',
        job_id: jobId,
        kind: 'job.summary',
        at: `2026-10-10T1${index}:30:00Z`,
        payload: { summary },
      })),
      jobs: [
        {
          id: 'job-impl',
          task_id: 'T-mid',
          stage: 'implement',
          state: 'succeeded',
          started_at: '2026-10-10T10:00:00Z',
          summary: 'First implementation.',
        },
        {
          id: 'job-review',
          task_id: 'T-mid',
          stage: 'review',
          state: 'succeeded',
          started_at: '2026-10-10T11:00:00Z',
          summary: 'Reviewer says the cache key is wrong.',
        },
        {
          id: 'job-impl-2',
          task_id: 'T-mid',
          stage: 'implement',
          state: 'succeeded',
          started_at: '2026-10-10T12:00:00Z',
          summary: 'Implementer fixed the cache key.',
        },
      ],
    },
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')

  const context = () => decisionDetail(page).getByRole('region', { name: 'Decision context' })
  await expect(context().getByText('Plan body for zeta')).toBeVisible()

  await select(page, 'Alpha agent question')
  await expect(context().getByRole('region', { name: 'Agent checkpoint' })).toContainText('Which cache should it use?')
  await expect(context().getByText(/Cache design \(sd-cache\) cites v2/)).toBeVisible()

  await select(page, 'Mid item without a deadline')
  await expect(context().getByRole('region', { name: 'Latest reviewer feedback' })).toContainText(
    'Reviewer says the cache key is wrong.',
  )
  await expect(context().getByRole('region', { name: 'Implementer reply' })).toContainText(
    'Implementer fixed the cache key.',
  )
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page).toHaveURL(/\/inbox\?selected=T-mid$/)
})

// The inbox has no stream of its own; a task's event stream moves that task's
// next action, so it refreshes the inbox listing it (lib/use-task-stream.ts).
test('a task stream event refreshes the inbox listing while its decision is selected', async ({ page }) => {
  const fixture = decisionsFixture()
  fixture.activity = {
    'T-zeta': {
      task: task('T-zeta', 'Zeta plan everyone waits on'),
      events: [],
      jobs: [],
      work_orders: [],
      interventions: [],
    },
  }
  const harness = await serve(page, fixture)
  let releaseStream = () => {}
  const streamReady = new Promise<void>((resolve) => {
    releaseStream = resolve
  })
  await page.route('**/v1/tasks/T-zeta/events/stream*', async (route) => {
    await streamReady
    await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
  })
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(3)
  await expect(decisionDetail(page).getByRole('region', { name: 'Decision context' })).toBeVisible()

  // The plan was decided elsewhere; only the stream can tell this page.
  harness.fixture = { ...fixture, items: fixture.items.filter((item) => item.task?.id !== 'T-zeta') }
  const before = harness.inboxRequests.length
  releaseStream()
  await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(before)
  await expect(decisionRows(page)).toHaveCount(2)
  await expect(decisionRows(page).getByText('Zeta plan everyone waits on')).toHaveCount(0)
  // A selection that left without an act from this page falls back to the first item.
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
})

test('blocked rows link blockers, the automatic band offers its one choice and a collapsed acts log', async ({
  page,
}) => {
  await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox')

  const blocked = page.getByRole('region', { name: 'Blocked' })
  await expect(blocked.getByText('Waits for')).toBeVisible()
  // A blocker that is itself a decision is marked "needs you" and selects it.
  const zeta = blocked.getByRole('link', { name: /Zeta plan everyone waits on/ })
  await expect(zeta).toHaveAttribute('href', '/w/demo/inbox?selected=T-zeta')
  await expect(zeta).toContainText('needs you')
  await expect(zeta).toHaveClass(/text-attention/)
  const elsewhere = blocked.getByRole('link', { name: 'Elsewhere dependency' })
  await expect(elsewhere).toHaveAttribute('href', '/w/demo/tasks/T-done')
  await expect(elsewhere).not.toContainText('needs you')
  await expect(elsewhere).not.toHaveClass(/text-attention/)
  await select(page, 'Alpha agent question')
  await zeta.click()
  await expect(page).toHaveURL(/[?&]selected=T-zeta/)
  await expect(decisionRows(page)).toHaveCount(3)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')

  const automatic = page.getByRole('region', { name: 'Automatic' })
  await expect(automatic.getByText('Automatic redispatch')).toBeVisible()
  await expect(automatic.getByRole('button')).toHaveCount(1)
  await expect(automatic.getByRole('button', { name: 'Redispatch now' })).toBeVisible()
  const log = automatic.locator('[data-acts-log]')
  await expect(log).not.toHaveAttribute('open')
  await expect(log.getByText('Redispatched')).toBeHidden()
  await log.getByText('Recent factory acts · 1').click()
  await expect(log.getByText('Redispatched')).toBeVisible()
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
        if_ignored: 'The claim stays withheld.',
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

test('proposal details keep the retired queue rules for every tier', async ({ page }) => {
  const fixture = proposalFixture()
  const seen: string[] = []
  const harness = await serve(page, fixture, (request, path) => {
    seen.push(`${request.method()} ${path}`)
    if (path === '/v1/requirements/req-attention/versions/3/confirm') {
      expect(request.headers()['if-match']).toBe('"1"')
      fixture.items = fixture.items.filter((item) => item.proposal?.id !== 'req-attention')
    }
    if (path === '/v1/decisions/DEC-16/dismiss')
      fixture.items = fixture.items.filter((item) => item.proposal?.id !== 'DEC-16')
    return undefined
  })
  await page.route('**/v1/requirements/req-attention?**', (route) => {
    seen.push('GET /v1/requirements/req-attention')
    return route.fulfill({
      json: { requirement: { id: 'req-attention', title: 'Operator attention', current_version: 1 } },
    })
  })
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(5)
  const footer = () => decisionDetail(page).locator('[data-decision-footer]')
  await expect(footer().getByText('Confirming a later one dismisses the earlier ones.')).toBeVisible()
  const unknown = await select(page, 'Unknown tier proposal')
  await expect(unknown.locator('[data-decision-footer]').getByRole('button')).toHaveCount(0)

  // Confirm reads the current version first and confirms against it.
  await select(page, 'Operator attention')
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(decisionRows(page)).toHaveCount(4)
  expect(seen.indexOf('GET /v1/requirements/req-attention')).toBeLessThan(
    seen.indexOf('POST /v1/requirements/req-attention/versions/3/confirm'),
  )

  // System Design Dismiss asks first; Decision Dismiss sends at once.
  await select(page, 'Dashboard architecture')
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  const sentBeforeCancel = harness.posts.length
  await page.getByRole('button', { name: 'Cancel' }).click()
  expect(harness.posts.length).toBe(sentBeforeCancel)

  await select(page, 'All FunnelFlux repositories')
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await expect(decisionRows(page)).toHaveCount(3)
  await expect(page.getByRole('dialog')).toHaveCount(0)
})

test('task, document and tier narrow the decisions band', async ({ page }) => {
  await serve(page, proposalFixture())
  await page.goto('/w/demo/inbox?task=review-task')
  const titles = decisionRows(page).locator('[data-row-title]')
  await expect(titles).toHaveText(['Operator attention', 'Other requirement', 'Unknown tier proposal'])

  await page.goto('/w/demo/inbox?document=req-attention&tier=requirement')
  await expect(titles).toHaveText(['Operator attention'])
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Operator attention')

  // The tier must match too: the same id at another tier is not this document.
  await page.goto('/w/demo/inbox?document=req-attention&tier=system_design')
  await expect(page.locator('[data-no-decisions]')).toContainText("You're clear")
  await expect(decisionRows(page)).toHaveCount(0)
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

// The document-proposal detail reads like a task decision: the server's
// label, question and if-ignored text, a footer band of choices, and raw IDs
// only in the IDs disclosure (feature-operator-next-action NA-6;
// req-operator-next-action AC-1.8, AC-2.6).
test('proposal details use the decision layout with server copy and a safe age', async ({ page }) => {
  await serve(page, proposalFixture())
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(5)
  await expect(page.locator('body')).not.toContainText('NaN')

  const requirement = await select(page, 'Operator attention')
  await expect(requirement.locator('[data-decision-label]')).toHaveText('Requirement proposal')
  await expect(requirement.locator('[data-decision-label]')).toHaveAttribute('data-kind-tone', 'document')
  await expect(requirement.locator('[data-decision-label]')).not.toHaveClass(/attention/)
  await expect(requirement.locator('[data-proposal-version]')).toHaveText('v3')
  await expect(requirement.locator('[data-proposal-age]')).toHaveText(/ago$/)
  await expect(requirement.locator('[data-proposal-age]')).toHaveAttribute('title', /\d/)
  await expect(requirement.getByText('Confirm or dismiss this proposed version?')).toBeVisible()
  await expect(requirement.getByRole('link', { name: 'Task' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/review-task/full',
  )
  await expect(requirement.getByRole('link', { name: 'View changes →' })).toBeVisible()

  // Footer band: primary Confirm, Revise, quiet Dismiss, then the consequence,
  // the multi-version sentence and the IDs disclosure.
  const footer = requirement.locator('[data-decision-footer]')
  await expect(footer.getByRole('button')).toHaveText(['Confirm', 'Revise', 'Dismiss'])
  await expect(footer.getByRole('button', { name: 'Confirm' })).toHaveClass(/bg-primary/)
  await expect(footer.getByRole('button', { name: 'Dismiss' })).not.toHaveClass(/bg-failure/)
  await expect(footer).toContainText('If you do nothing: The claim stays withheld.')
  await expect(footer).toContainText('Confirming a later one dismisses the earlier ones.')

  // Raw document and origin IDs appear only inside the IDs disclosure.
  const visibleText = await requirement.evaluate((card) => {
    const clone = card.cloneNode(true) as HTMLElement
    for (const details of clone.querySelectorAll('details')) details.remove()
    return clone.textContent ?? ''
  })
  expect(visibleText).not.toContain('req-attention')
  expect(visibleText).not.toContain('review-task')
  await requirement.getByText('Show IDs').click()
  await expect(requirement.locator('details pre')).toContainText('"document": "req-attention"')
  await expect(requirement.locator('details pre')).toContainText('"origin_id": "review-task"')

  // A missing or unusable age falls back to proposed_at.
  for (const title of ['Dashboard architecture', 'All FunnelFlux repositories']) {
    const age = (await select(page, title)).locator('[data-proposal-age]')
    await expect(age).toHaveText(/ago$/)
    await expect(age).toHaveAttribute('datetime', '2026-10-10T10:00:00.000Z')
  }

  // A decision reads its own question and context link and its full title;
  // its list row clamps a long title to two lines and keeps the full text in
  // its tooltip.
  const decision = decisionDetail(page)
  await expect(decision.locator('[data-decision-label]')).toHaveText('Decision proposal')
  await expect(decision.locator('[data-proposal-version]')).toHaveCount(0)
  await expect(decision.getByText('Confirm or dismiss this proposed decision?')).toBeVisible()
  // The context link opens the decision's own page (req-document-operating-surfaces AC-8.9).
  await expect(decision.getByRole('link', { name: 'View decision →' })).toHaveAttribute(
    'href',
    '/w/demo/decisions/DEC-16',
  )
  await expect(decision.getByRole('button', { name: 'Revise' })).toHaveCount(0)
  await expect(decision.locator('[data-detail-title]')).toHaveText(longDecision)
  await page.goto('/w/demo/inbox')
  const title = decisionRows(page).filter({ hasText: 'All FunnelFlux repositories' }).locator('[data-row-title]')
  await expect(title).toHaveAttribute('title', longDecision)
  await expect(title).toHaveClass(/line-clamp-2/)
  await page.setViewportSize({ width: 390, height: 900 })
  const lines = await title.evaluate((node) => {
    const style = getComputedStyle(node)
    return Math.round(node.getBoundingClientRect().height / Number.parseFloat(style.lineHeight))
  })
  expect(lines).toBe(2)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

test('proposal cards omit the age when neither age nor proposed time is usable', async ({ page }) => {
  const fixture = proposalFixture()
  const first = fixture.items[0] as { proposal: Record<string, unknown> }
  first.proposal = { ...first.proposal, proposed_at: 'not-a-time' }
  delete first.proposal.age_seconds
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const card = await select(page, 'Operator attention')
  await expect(card.locator('[data-decision-label]')).toHaveText('Requirement proposal')
  await expect(card.locator('[data-proposal-age]')).toHaveCount(0)
  await expect(page.locator('body')).not.toContainText('NaN')
})

test('/pending-proposals lands on the inbox decisions band with its narrowing', async ({ page }) => {
  await serve(page, proposalFixture())
  await page.goto('/w/demo/pending-proposals?task=T&document=D&tier=requirement')
  await expect(page).toHaveURL((url) => {
    const search = url.searchParams
    return (
      url.pathname === '/w/demo/inbox' &&
      search.get('band') === 'decisions' &&
      search.get('task') === 'T' &&
      search.get('document') === 'D' &&
      search.get('tier') === 'requirement'
    )
  })
})

test('navigation is exactly the accepted surfaces and the Inbox badge is the operator decision count', async ({
  page,
}) => {
  await serve(page, { ...decisionsFixture(), operatorDecisions: 5 })
  await page.goto('/w/demo/inbox')
  const primary = page.getByRole('navigation', { name: 'Primary' })
  await expect(primary.getByRole('link', { name: /Inbox/ }).getByText('5', { exact: true })).toBeVisible()
  const labels = await primary
    .locator('a')
    .evaluateAll((links) => links.map((link) => link.textContent?.replace(/\d+$/, '').trim()))
  expect(labels).toEqual([
    'Board',
    'Inbox',
    'Workspace',
    'Requirements',
    'System Design',
    'Decisions',
    'Monitor',
    'Settings',
  ])
  await expect(primary.getByRole('link', { name: /Inbox/ }).getByText('5', { exact: true })).toBeVisible()
})

// A pending requirement proposal is one of the Inbox decisions, so no other
// entry repeats it as a badge (req-operator-next-action AC-2.7;
// req-document-operating-surfaces AC-1.2).
test('Inbox is the only primary navigation entry with a badge while requirement proposals are pending', async ({
  page,
}) => {
  await serve(page, {
    ...decisionsFixture(),
    operatorDecisions: 2,
    proposals: [
      { id: 'req-attention', tier: 'requirement', version: 3, title: 'Operator attention' },
      { id: 'req-other', tier: 'requirement', version: 1, title: 'Other requirement' },
      { id: 'component-web-dashboard', tier: 'system_design', version: 42, title: 'Web dashboard' },
    ],
  })
  await page.goto('/w/demo/inbox')
  const primary = page.getByRole('navigation', { name: 'Primary' })
  const inbox = primary.getByRole('link', { name: /Inbox/ })
  await expect(inbox.getByText('2', { exact: true })).toBeVisible()
  await expect(inbox).toHaveText('Inbox2')
  for (const label of ['Board', 'Workspace', 'Requirements', 'System Design', 'Decisions', 'Monitor', 'Settings']) {
    const link = primary.getByRole('link', { name: label, exact: true })
    await expect(link).toHaveText(label)
    // The label is the entry's only span; a badge would add a second.
    await expect(link.locator('span')).toHaveCount(1)
  }
})

// Tone by kind, attention for urgency only (feature-operator-next-action
// NA-6). Deadlines are set relative to now so the three-hour threshold is
// never borderline.
function toneFixture(): Fixture {
  const fixture = decisionsFixture()
  const deadlines: Record<string, string> = {
    'T-zeta': new Date(Date.now() + 26 * 3_600_000).toISOString(),
    'T-alpha': new Date(Date.now() + 30 * 60_000).toISOString(),
    'T-mid': new Date(Date.now() - 10 * 60_000).toISOString(),
  }
  for (const item of fixture.items) {
    const id = item.task?.id ?? ''
    if (!(id in deadlines)) continue
    item.deadline = deadlines[id]
    Object.assign(item.next_action as Record<string, unknown>, { deadline: deadlines[id] })
    Object.assign(fixture.actions[id], { deadline: deadlines[id], if_ignored: 'Nothing moves.' })
  }
  return fixture
}

test('each kind takes its tone, and attention marks only what unblocks others or is due soon', async ({ page }) => {
  await serve(page, toneFixture())
  await page.goto('/w/demo/inbox')

  await expect(decisionRows(page)).toHaveCount(3)
  await expect(rowItems(page).locator('[data-kind-tone]')).toHaveCount(3)
  expect(
    await rowItems(page)
      .locator('[data-kind-tone]')
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-kind-tone'))),
  ).toEqual(['gate', 'question', 'broken'])
  for (const kind of await rowItems(page).locator('[data-row-kind]').all()) {
    await expect(kind).not.toHaveClass(/attention/)
  }

  // The countdown chip: neutral beyond three hours, attention within them,
  // failure once overdue.
  const later = rowItems(page).nth(0).locator('[data-due]')
  await expect(later).toHaveAttribute('data-due', 'later')
  await expect(later).toHaveText('due in 1d')
  await expect(later).not.toHaveClass(/attention|failure/)
  const soonChip = rowItems(page).nth(1).locator('[data-due]')
  await expect(soonChip).toHaveAttribute('data-due', 'soon')
  await expect(soonChip).toHaveText(/^due in (29|30)m$/)
  await expect(soonChip).toHaveClass(/text-attention/)
  const overdue = rowItems(page).nth(2).locator('[data-due]')
  await expect(overdue).toHaveAttribute('data-due', 'overdue')
  await expect(overdue).toHaveText(/^(10|11)m overdue$/)
  await expect(overdue).toHaveClass(/text-failure/)

  // Urgent counts the decisions due within three hours or overdue.
  await expect(page.locator('[data-inbox-summary]')).toHaveText(
    '3 calls need you · 2 urgent · 1 waiting on other tasks · 1 handled automatically',
  )

  // The detail repeats the chip and its label pill takes the kind's tone.
  const alpha = await select(page, 'Alpha agent question')
  await expect(alpha.locator('[data-decision-label]')).toHaveAttribute('data-kind-tone', 'question')
  await expect(alpha.locator('[data-due]')).toHaveAttribute('data-due', 'soon')
  await expect(alpha.locator('[data-if-ignored]')).toHaveText(
    /^If nobody acts by \d{1,2}:\d{2} [AP]M: Nothing moves\.$/,
  )
  const mid = await select(page, 'Mid item without a deadline')
  await expect(mid.locator('[data-decision-label]')).toHaveAttribute('data-kind-tone', 'broken')
  await expect(mid.locator('[data-due]')).toHaveAttribute('data-due', 'overdue')
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
  await expect(page.getByRole('region', { name: 'Automatic' })).toContainText('Handled automatically')
})

test('the inbox fits a 390px viewport without horizontal overflow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page)).toHaveCount(3)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
  await expect(page.getByRole('link', { name: '3 decisions waiting in the Inbox' })).toBeVisible()
  await decisionRows(page).first().click()
  await expect(decisionDetail(page)).toBeVisible()
  const detailOverflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(detailOverflow).toBeLessThanOrEqual(0)
})

test('band chips count every band from one unfiltered read and filter in the browser', async ({ page }) => {
  const harness = await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox')
  const chips = page.getByRole('navigation', { name: 'Inbox bands' })
  await expect(chips.locator('[data-band-chip]')).toHaveText(['Everything', 'Decisions3', 'Waiting1', 'Automatic1'])
  await expect(page.locator('[data-inbox-order]')).toHaveText('Blockers first, then nearest deadline')
  await expect(chips.locator('[data-band-chip="all"]')).toHaveAttribute('aria-current', 'page')

  await chips.locator('[data-band-chip="blocked"]').click()
  await expect(page).toHaveURL(/[?&]band=blocked/)
  await expect(page.getByRole('region', { name: 'Blocked' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Decisions' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Automatic' })).toHaveCount(0)
  await expect(chips.locator('[data-band-chip="blocked"]')).toHaveAttribute('aria-current', 'page')
  // Counts stay whole while one band shows.
  await expect(chips.locator('[data-band-chip="decisions"]')).toHaveText('Decisions3')

  await chips.locator('[data-band-chip="all"]').click()
  await expect(decisionRows(page)).toHaveCount(3)
  // Every read of the inbox omitted the band.
  expect(harness.inboxRequests.length).toBeGreaterThan(0)
  expect(harness.inboxRequests.every((search) => !new URLSearchParams(search).has('band'))).toBe(true)
})

test('a decision that blocks other tasks names them on hover and keyboard focus', async ({ page }) => {
  const fixture = decisionsFixture()
  const blocks = [
    { task_id: 'T-wait', title: 'Waiting on zeta', state: 'queued' },
    { task_id: 'T-later', title: 'Generate the typed client', state: 'queued' },
  ]
  Object.assign(fixture.items[0].next_action as Record<string, unknown>, { blocks })
  Object.assign(fixture.actions['T-zeta'], { blocks })
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')

  await expect(rowItems(page).first().locator('[data-row-blocks]')).toHaveText('Unblocks 2')
  await expect(rowItems(page).first().locator('[data-row-blocks]')).toHaveClass(/text-attention/)
  await expect(rowItems(page).nth(1).locator('[data-row-blocks]')).toHaveCount(0)

  const card = decisionDetail(page)
  const chip = card.getByRole('button', { name: 'Unblocks 2 tasks' })
  await expect(chip).toBeVisible()
  await chip.hover()
  const list = card.locator('[data-blocks-list]')
  await expect(list.locator('li')).toHaveText(['Waiting on zeta', 'Generate the typed client'])
  await page.mouse.move(0, 0)
  await expect(list).toHaveCount(0)
  await chip.focus()
  await expect(list.locator('li')).toHaveText(['Waiting on zeta', 'Generate the typed client'])

  // A decision without dependents shows no chip.
  const alpha = await select(page, 'Alpha agent question')
  await expect(alpha.getByRole('button', { name: /^Unblocks / })).toHaveCount(0)
})

test('the plan context splits the plan at its headings beside the choices that decide it', async ({ page }) => {
  const fixture = decisionsFixture()
  fixture.activity = {
    'T-zeta': {
      task: {
        ...task('T-zeta', 'Zeta plan everyone waits on'),
        context: { requirements: [{ id: 'REQ-2', title: 'Reproducible builds', version: 3, archived: false }] },
      },
      events: [],
      jobs: [],
      interventions: [],
      work_orders: [
        {
          id: 'T-zeta-spec-1',
          task_id: 'T-zeta',
          job_id: 'T-zeta-spec-1',
          stage: 'spec',
          state: 'completed',
          agent: 'opencode',
          model: 'claude-opus-5-5',
          queue_entered_at: '2026-10-10T09:00:00Z',
        },
      ],
      spec: {
        task_id: 'T-zeta',
        version: 2,
        content: [
          '## Approach',
          'Pin protoc in one tools module.',
          '',
          '## Files touched',
          '- `tools/tools.go` — new module pinning the generators',
          '- scripts/gen-proto.sh rewrite',
          '',
          '## Ordering',
          '1. Add the tools module.',
          '',
          '## Risks',
          '- Other branches need a rebase.',
          '',
          '## Done criteria',
          '- CI fails on drift.',
        ].join('\n'),
        acceptance_criteria: [],
        created_at: proposedAt,
      },
    },
  }
  const harness = await serve(page, fixture, (_request, path) => {
    if (path === '/v1/tasks/T-zeta/spec/review') {
      fixture.items = fixture.items.filter((item) => item.task?.id !== 'T-zeta')
    }
    return undefined
  })
  await page.goto('/w/demo/inbox')

  const sheet = decisionDetail(page)
  const plan = sheet.locator('[data-plan-context]')
  await expect(plan.getByRole('heading', { name: 'Plan v2' })).toBeVisible()
  await expect(plan).toContainText('Reproducible builds')
  await expect(plan.locator('[data-plan-submitted]')).toContainText('by the plan agent (opencode · claude-opus-5-5)')
  await expect(plan.locator('[data-plan-section]')).toHaveCount(5)
  expect(
    await plan
      .locator('[data-plan-section]')
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute('aria-label'))),
  ).toEqual(['Approach', 'Files touched', 'Ordering', 'Risks', 'Done criteria'])
  const files = plan.locator('[data-plan-section="Files touched"] code')
  await expect(files).toHaveText(['tools/tools.go', 'scripts/gen-proto.sh'])
  await expect(plan.locator('[data-plan-section="Files touched"]')).toContainText('new module pinning the generators')
  await expect(plan.locator('[data-plan-section="Approach"]')).toContainText('Pin protoc in one tools module.')

  // The footer under the plan holds the choices; deciding there posts once
  // and the selection moves on.
  const footer = sheet.locator('[data-decision-footer]')
  await footer.getByRole('button', { name: 'Approve plan' }).click()
  await expect(decisionRows(page)).toHaveCount(2)
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual(['/v1/tasks/T-zeta/spec/review'])
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
})

function dispositionFixture(binding: boolean): Fixture {
  const fixture = decisionsFixture()
  fixture.items = [
    {
      band: 'decisions',
      task: task('T-verify', 'Rotate staging credentials', 'verifying'),
      next_action: {
        kind: 'verification_disposition',
        label: 'Confirm an outcome',
        actor: 'operator',
        judgment: true,
        summary: 'Verification needs a disposition.',
        deadline: null,
      },
    },
  ]
  const choices = [
    { ...choice('applied', 'Yes, it applied…', '/v1/work-orders/wo-verify/recover'), capability: 'operate_gates' },
    { ...choice('not_applied', 'No, it didn’t…', '/v1/work-orders/wo-verify/recover'), capability: 'operate_gates' },
  ]
  fixture.actions = {
    'T-verify': {
      kind: 'verification_disposition',
      label: 'Confirm an outcome',
      actor: 'operator',
      judgment: true,
      summary: 'Verification needs a disposition.',
      question: 'Did the change apply?',
      choices,
      deadline: null,
      details: binding
        ? {
            disposition: {
              context_id: 'ctx-1',
              run_id: 'run-1',
              operation_ids: ['op-1', 'op-2'],
              input_digest: 'sha256:abc',
            },
          }
        : { refusals: [{ choice: 'applied', text: 'No unresolved operation to dispose of.' }] },
    },
  }
  return fixture
}

test('a verification disposition posts the server binding with the operator reason', async ({ page }) => {
  const harness = await serve(page, dispositionFixture(true))
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.locator('[data-context-heading]')).toHaveText('Operation log')
  await card.getByRole('button', { name: 'Yes, it applied…' }).click()
  const send = card.locator('[data-next-action-choices]').getByRole('button', { name: 'Yes, it applied…' }).last()
  await expect(send).toBeDisabled()
  await card.getByRole('textbox', { name: 'Reason' }).fill('New secret version printed; health check unreachable.')
  await send.click()
  await expect.poll(() => harness.posts.length).toBe(1)
  const [post] = harness.posts
  expect(new URL(post.url()).pathname).toBe('/v1/work-orders/wo-verify/recover')
  const body = post.postDataJSON()
  expect(body.verification).toEqual({
    context_id: 'ctx-1',
    run_id: 'run-1',
    operation_ids: ['op-1', 'op-2'],
    input_digest: 'sha256:abc',
    disposition: 'applied',
    reason: 'New secret version printed; health check unreachable.',
  })
  expect(post.headers()['x-idempotency-key']).toBe(body.request_id)
})

test('without a binding the disposition choices stay disabled with the server refusal', async ({ page }) => {
  const harness = await serve(page, dispositionFixture(false))
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await expect(card.getByRole('button', { name: 'Yes, it applied…' })).toBeDisabled()
  // The reasons sit beside the choices; the IDs disclosure repeats the raw details.
  const choices = card.locator('[data-next-action-choices]')
  await expect(choices.getByText('No unresolved operation to dispose of.')).toBeVisible()
  await expect(card.getByRole('button', { name: 'No, it didn’t…' })).toBeDisabled()
  await expect(choices.getByText('Take this from the task page.')).toBeVisible()
  expect(harness.posts).toHaveLength(0)
})

test('band counts cover every page of the inbox', async ({ page }) => {
  const fixture = decisionsFixture()
  for (let index = 0; index < 120; index++) {
    fixture.items.push({
      band: 'automatic',
      task: task(`T-run-${index}`, `Running task ${index}`, 'running'),
      next_action: { kind: 'working', label: 'Working', actor: 'agent', judgment: false, summary: 'Working.' },
    })
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const chips = page.getByRole('navigation', { name: 'Inbox bands' })
  await expect(chips.locator('[data-band-chip="automatic"]')).toHaveText('Automatic121')
  await expect(chips.locator('[data-band-chip="decisions"]')).toHaveText('Decisions3')
  const offsets = harness.inboxRequests.map((search) => new URLSearchParams(search).get('offset'))
  expect(offsets).toContain('0')
  expect(offsets).toContain('100')
})

test('the plan context keeps a preamble and the Files touched intro beside the five sections', async ({ page }) => {
  const fixture = decisionsFixture()
  fixture.activity = {
    'T-zeta': {
      task: task('T-zeta', 'Zeta plan everyone waits on'),
      events: [],
      jobs: [],
      interventions: [],
      work_orders: [],
      spec: {
        task_id: 'T-zeta',
        version: 3,
        content: [
          'Constraint: keep the public API frozen until the migration lands.',
          '',
          '## Approach',
          'Pin the generators.',
          '',
          '## Files touched',
          'Only change the following files; preserve existing data.',
          '- `tools/tools.go` — new module',
          '  - nested detail stays with its file',
          '',
          '## Ordering',
          '1. Pin, then regenerate.',
          '',
          '## Risks',
          '- None beyond a rebase.',
          '',
          '## Done criteria',
          '- CI fails on drift.',
        ].join('\n'),
        acceptance_criteria: [],
        created_at: proposedAt,
      },
    },
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const sheet = decisionDetail(page)
  const plan = sheet.locator('[data-plan-context]')
  await expect(plan.locator('[data-plan-section]')).toHaveCount(5)
  await expect(plan.locator('[data-plan-preamble]')).toHaveText(
    'Constraint: keep the public API frozen until the migration lands.',
  )
  const files = plan.locator('[data-plan-section="Files touched"]')
  await expect(files.locator('[data-plan-files-intro]')).toHaveText(
    'Only change the following files; preserve existing data.',
  )
  await expect(files.locator('code')).toHaveText(['tools/tools.go'])
  await expect(files).toContainText('nested detail stays with its file')
  await expect(sheet.locator('[data-decision-footer]').getByRole('button', { name: 'Approve plan' })).toBeVisible()
})

test('the inbox reads past 2,000 items to the server total', async ({ page }) => {
  const fixture = decisionsFixture()
  for (let index = 0; index < 2001; index++) {
    fixture.items.push({
      band: 'blocked',
      task: task(`T-held-${index}`, `Held task ${index}`, 'queued'),
      next_action: { kind: 'blocked', label: 'Blocked', actor: 'dependency', judgment: false, summary: 'Blocked.' },
      blockers: [{ task_id: 'T-zeta', title: 'Zeta plan everyone waits on', state: 'awaiting_spec_approval' }],
    })
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox?band=blocked')
  const chips = page.getByRole('navigation', { name: 'Inbox bands' })
  await expect(chips.locator('[data-band-chip="blocked"]')).toHaveText('Waiting2002')
  await expect(chips.locator('[data-band-chip="decisions"]')).toHaveText('Decisions3')
  await expect(
    page.getByRole('region', { name: 'Blocked' }).getByText('Held task 2000', { exact: true }),
  ).toBeAttached()
  const offsets = harness.inboxRequests.map((search) => new URLSearchParams(search).get('offset'))
  expect(offsets).toContain('2000')
})

// The review pane (req-operator-next-action AC-2.8 to AC-2.11; DEC-82).

test('a selected row shows its detail and the selection round-trips through the URL', async ({ page }) => {
  await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox?band=decisions')
  // With no selection the first item is selected without writing the URL.
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
  await expect(page).toHaveURL(/\/inbox\?band=decisions$/)

  await decisionRows(page).nth(1).click()
  await expect(page).toHaveURL(/[?&]selected=T-alpha/)
  await expect(page).toHaveURL(/[?&]band=decisions/)
  // The router marks the linked selection as the current page.
  await expect(decisionRows(page).nth(1)).toHaveAttribute('aria-current', /^(true|page)$/)
  await expect(decisionRows(page).first()).not.toHaveAttribute('aria-current', /.+/)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')

  await page.reload()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
  await decisionRows(page).nth(2).click()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
  await page.goBack()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')

  // A stale selection falls back to the first item.
  await page.goto('/w/demo/inbox?selected=T-gone')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
  await expect(decisionRows(page).first()).toHaveAttribute('aria-current', 'true')
})

test('task decisions and document proposals keep the server order in one list', async ({ page }) => {
  const fixture = decisionsFixture()
  const proposals = proposalFixture().items
  // Interleaved, not grouped by kind or tier (DEC-82(2)).
  fixture.items = [
    proposals[2],
    fixture.items[0],
    proposals[0],
    fixture.items[1],
    proposals[1],
    ...fixture.items.slice(2),
  ]
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  await expect(decisionRows(page).locator('[data-row-title]')).toHaveText([
    longDecision,
    'Zeta plan everyone waits on',
    'Operator attention',
    'Alpha agent question',
    'Dashboard architecture',
    'Mid item without a deadline',
  ])
  await expect(decisionRows(page).locator('[data-row-kind]')).toHaveText([
    'Decision proposal',
    'Approve the plan',
    'Requirement proposal',
    'Answer the agent',
    'System Design proposal',
    'Decide after repeated review bounces',
  ])
  // Each row carries one key: decision proposals by ID, versions by tier, ID and version.
  expect(await decisionRows(page).evaluateAll((rows) => rows.map((row) => row.getAttribute('data-inbox-row')))).toEqual(
    [
      'decision:DEC-16',
      'T-zeta',
      'requirement:req-attention:3',
      'T-alpha',
      'system_design:design-dashboard:4',
      'T-mid',
    ],
  )
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

test('a version proposal shows its changes in place and a decision proposal shows its full text', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.documents = {
    '/v1/requirements/req-attention': versionView('requirement', 'req-attention', 2, 3),
    '/v1/system-designs/design-dashboard': 503,
    '/v1/requirements/req-other': versionView('requirement', 'req-other', null, 1),
    '/v1/decisions/DEC-16': {
      id: 'DEC-16',
      statement: longDecision,
      context: 'Each repository ships its own kit.',
      alternatives_rejected: 'One shared kit was rejected: repositories diverge.',
      status: 'proposed',
    },
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox')

  const changes = () => decisionDetail(page).getByRole('region', { name: 'Proposed changes' })
  await expect(changes().locator('[data-comparison-base]')).toHaveText('Confirmed v2 compared with proposed v3.')
  const comparison = changes().getByRole('region', { name: 'Version comparison' })
  await expect(comparison).toContainText('Proposed rule text.')
  await expect(comparison).toContainText('1 changed sections')
  await expect(decisionDetail(page).getByRole('link', { name: 'View changes →' })).toHaveAttribute(
    'href',
    /tab=changes/,
  )

  // A failed read offers Retry and then shows the comparison.
  await select(page, 'Dashboard architecture')
  await expect(changes().getByRole('button', { name: 'Retry' })).toBeVisible()
  fixture.documents['/v1/system-designs/design-dashboard'] = versionView('system_design', 'design-dashboard', 3, 4)
  await changes().getByRole('button', { name: 'Retry' }).click()
  await expect(changes().locator('[data-comparison-base]')).toHaveText('Confirmed v3 compared with proposed v4.')

  // A first proposal compares against an empty base, so it shows whole.
  await select(page, 'Other requirement')
  await expect(changes().locator('[data-comparison-base]')).toContainText('No version is confirmed yet')
  await expect(changes()).toContainText('Shared paragraph.')
  await expect(changes()).toContainText('Proposed rule text.')

  const decision = await select(page, 'All FunnelFlux repositories')
  const text = decision.getByRole('region', { name: 'Proposed decision' })
  await expect(text).toContainText(longDecision)
  await expect(text).toContainText('Each repository ships its own kit.')
  await expect(text).toContainText('One shared kit was rejected: repositories diverge.')
  await expect(decision.getByRole('link', { name: 'View decision →' })).toBeVisible()

  for (const request of harness.documentRequests)
    expect(new URL(request.url()).searchParams.get('workspace_id')).toBe('demo')
  expect(harness.posts).toHaveLength(0)
})

test('a late read from a left selection never shows under the new one', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.documents = {
    '/v1/requirements/req-attention': versionView('requirement', 'req-attention', 2, 3),
    '/v1/system-designs/design-dashboard': versionView('system_design', 'design-dashboard', 3, 4),
  }
  const harness = await serve(page, fixture)
  const held = gate()
  harness.holdDocuments = held.held
  await page.goto('/w/demo/inbox')
  await expect.poll(() => harness.documentRequests.length).toBeGreaterThanOrEqual(1)
  await decisionRows(page).filter({ hasText: 'Dashboard architecture' }).click()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')
  held.release()
  harness.holdDocuments = undefined
  const changes = decisionDetail(page).getByRole('region', { name: 'Proposed changes' })
  await expect(changes.locator('[data-comparison-base]')).toHaveText('Confirmed v3 compared with proposed v4.')
  await expect(decisionDetail(page)).not.toContainText('Confirmed v2 compared with proposed v3.')
})

test('resolving the selected item selects the next one, or the previous when it was last', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.items = fixture.items.filter((item) => item.proposal?.id !== 'charter-x')
  fixture.documents = {
    '/v1/requirements/req-attention': versionView('requirement', 'req-attention', 2, 3),
    '/v1/requirements/req-other': versionView('requirement', 'req-other', null, 1),
    '/v1/system-designs/design-dashboard': versionView('system_design', 'design-dashboard', 3, 4),
  }
  let refuse = true
  const harness = await serve(page, fixture, (_request, path) => {
    if (path === '/v1/system-designs/design-dashboard/versions/4/confirm' && refuse)
      return { status: 422, error: 'The document moved.' }
    const leaving = path.match(/^\/v1\/(?:requirements|system-designs|decisions)\/([^/]+)\//)?.[1]
    if (leaving) fixture.items = fixture.items.filter((item) => item.proposal?.id !== leaving)
    return undefined
  })
  await page.goto('/w/demo/inbox?selected=system_design:design-dashboard:4')
  const footer = () => decisionDetail(page).locator('[data-decision-footer]')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')

  // A refused act leaves the item and the selection where they are.
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(footer()).toContainText('The document moved.')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')
  // Cancelling a dialog is no act either.
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')

  refuse = false
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(decisionRows(page)).toHaveCount(3)
  await expect(page).toHaveURL(/selected=decision%3ADEC-16|selected=decision:DEC-16/)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText(longDecision)

  // The last item resolves to its previous neighbour.
  await select(page, 'Other requirement')
  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: /Dismiss/ })
    .click()
  await expect(decisionRows(page)).toHaveCount(2)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText(longDecision)

  await footer().getByRole('button', { name: 'Dismiss' }).click()
  await expect(decisionRows(page)).toHaveCount(1)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Operator attention')
  await footer().getByRole('button', { name: 'Confirm' }).click()
  await expect(page.locator('[data-no-decisions]')).toContainText('No decisions are waiting.')
  expect(harness.posts.length).toBeGreaterThanOrEqual(5)
})

test('keys move the selection and act through the selected item’s own controls', async ({ page }) => {
  const fixture = decisionsFixture()
  const harness = await serve(page, fixture, (_request, path) => {
    if (path === '/v1/tasks/T-zeta/spec/review')
      fixture.items = fixture.items.filter((item) => item.task?.id !== 'T-zeta')
    return undefined
  })
  await page.goto('/w/demo/inbox')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
  // The hint sits beside the control it activates, and the move keys by the list.
  const approve = decisionDetail(page).locator('[data-shortcut="c"]')
  await expect(approve).toHaveText('Approve plan')
  await expect(approve.locator('xpath=following-sibling::*[@data-kbd-hint]')).toHaveText('c')
  await expect(page.locator('[data-move-hint]')).toBeVisible()

  await page.keyboard.press('j')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
  await expect(page).toHaveURL(/selected=T-alpha/)
  await page.keyboard.press('ArrowDown')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
  // Movement stops at the ends.
  await page.keyboard.press('j')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
  // A held modifier is not a shortcut.
  await page.keyboard.press('Control+k')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
  await page.keyboard.press('k')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')

  // c on a choice that needs input opens its composer and sends nothing; keys
  // typed into it stay text.
  await page.keyboard.press('c')
  const direction = decisionDetail(page).getByLabel('Direction')
  await expect(direction).toBeFocused()
  await page.keyboard.type('jkcd')
  await expect(direction).toHaveValue('jkcd')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
  // While the composer is open no key moves the selection or acts, whether
  // focus sits on a composer button or elsewhere on the page, so the unsent
  // draft survives.
  const url = page.url()
  const composerCancel = decisionDetail(page).getByRole('button', { name: 'Cancel', exact: true })
  for (const focus of ['composer button', 'page'] as const) {
    if (focus === 'composer button') await composerCancel.focus()
    else await page.locator('h1').click()
    for (const key of ['j', 'k', 'ArrowDown', 'ArrowUp', 'c']) {
      await page.keyboard.press(key)
      await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
      await expect(direction).toHaveValue('jkcd')
      expect(page.url()).toBe(url)
    }
  }
  expect(harness.posts).toHaveLength(0)

  // Once the composer is cancelled, movement works again.
  await composerCancel.click()
  await expect(direction).toHaveCount(0)

  // c on the plan approval records the same act as its button, once.
  await page.keyboard.press('k')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
  await page.keyboard.down('c')
  await page.keyboard.press('c')
  await page.keyboard.up('c')
  await expect.poll(() => harness.posts.length).toBe(1)
  expect(new URL(harness.posts[0].url()).pathname).toBe('/v1/tasks/T-zeta/spec/review')
  expect(harness.posts[0].postDataJSON()).toMatchObject({ action: 'approve' })
  await expect(decisionRows(page)).toHaveCount(2)
})

test('document keys open the same dialogs as their buttons, and no key acts while a dialog is open', async ({
  page,
}) => {
  const fixture = proposalFixture()
  fixture.documents = {
    '/v1/system-designs/design-dashboard': versionView('system_design', 'design-dashboard', 3, 4),
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox?selected=system_design:design-dashboard:4')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')
  for (const [key, label] of [
    ['c', 'Confirm'],
    ['r', 'Revise'],
    ['d', 'Dismiss'],
  ])
    await expect(decisionDetail(page).locator(`[data-shortcut="${key}"]`)).toHaveText(label)

  await page.keyboard.press('d')
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await page.keyboard.press('j')
  await page.keyboard.press('c')
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Dashboard architecture')
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toHaveCount(0)

  await page.keyboard.press('r')
  await expect(page.getByRole('dialog', { name: /Revise version 4 of Dashboard architecture/ })).toBeVisible()
  await page.keyboard.press('Escape')
  expect(harness.posts).toHaveLength(0)
})

test('c on a merge approval opens the merge confirmation and never merges by itself', async ({ page }) => {
  const fixture: Fixture = {
    items: [
      {
        band: 'decisions',
        task: task('T-merge', 'Merge me', 'awaiting_human'),
        next_action: { kind: 'merge_approval', label: 'Approve merge', actor: 'operator', judgment: true, summary: '' },
      },
    ],
    actions: {
      'T-merge': {
        kind: 'merge_approval',
        label: 'Approve merge',
        actor: 'operator',
        judgment: true,
        summary: 'Waiting for merge approval.',
        choices: [
          choice('merge', 'Merge', '/v1/tasks/T-merge/merge'),
          choice('approve', 'Approve only', '/v1/tasks/T-merge/review'),
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
        merge_readiness: { state: 'MERGEABLE', head_sha: 'feedface1234567', number: 9 },
      },
    },
  }
  const harness = await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  // The merge button carries the c hint beside it, like every bound control.
  const merge = decisionDetail(page).locator('[data-merge-at-gate]')
  await expect(merge).toHaveAttribute('data-shortcut', 'c')
  await expect(merge.locator('xpath=..').locator('[data-kbd-hint]')).toHaveText('c')
  await expect(decisionDetail(page).locator('[data-merge-hint]')).toHaveCount(0)
  await expect(decisionDetail(page).locator('[data-shortcut="c"]')).toHaveCount(1)
  await page.keyboard.press('c')
  await expect(page.getByRole('dialog', { name: 'Confirm merge' })).toBeVisible()
  expect(harness.posts).toHaveLength(0)
})

test('below md the list and the detail show one at a time, and Back keeps the other filters', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  await serve(page, decisionsFixture())
  await page.goto('/w/demo/inbox?band=decisions')
  await expect(decisionRows(page)).toHaveCount(3)
  await expect(page.locator('[data-inbox-detail]')).toBeHidden()
  await expect(page.locator('[data-move-hint]')).toBeHidden()

  await decisionRows(page).nth(1).click()
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
  await expect(decisionRows(page).first()).toBeHidden()
  await expect(decisionDetail(page).locator('[data-kbd-hint]').first()).toBeHidden()

  await page.locator('[data-inbox-back]').click()
  await expect(page).toHaveURL(/\/inbox\?band=decisions$/)
  await expect(decisionRows(page).first()).toBeVisible()
  await expect(page.locator('[data-inbox-detail]')).toBeHidden()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

// A parked decision, a dispatch-exhausted decision and an ordinary plan gate,
// for the choice-row, context and Start over cases (task 261021-cd3bf8).
function parkedFixture(): Fixture {
  const parkedChoices = [
    { ...choice('recover', 'Recover', '/v1/tasks/T-parked/redispatch'), capability: 'recover_work' },
    { ...choice('close', 'Close', '/v1/tasks/T-parked/close'), capability: 'operate_gates' },
    { ...choice('start_over', 'Start over', '/v1/tasks/T-parked/restart'), capability: 'operate_gates' },
  ]
  const compact = (kind: string, label: string) => ({ kind, label, actor: 'operator', judgment: true, summary: '' })
  return {
    items: [
      {
        band: 'decisions',
        task: task('T-parked', 'Parked billing task', 'parked'),
        next_action: compact('parked', 'Decide on a parked task'),
      },
      {
        band: 'decisions',
        task: task('T-exhausted', 'Exhausted dispatch task', 'parked'),
        next_action: compact('dispatch_exhausted', 'Dispatch exhausted'),
      },
      {
        band: 'decisions',
        task: task('T-plan', 'Plan after the parked task'),
        next_action: compact('plan_approval', 'Approve plan'),
      },
    ],
    actions: {
      'T-parked': {
        kind: 'parked',
        label: 'Decide on a parked task',
        actor: 'operator',
        judgment: true,
        summary: 'This task is parked.',
        question: 'Put it back, or close it?',
        if_ignored: 'It stays parked.',
        choices: parkedChoices,
        deadline: null,
        details: {},
      },
      'T-exhausted': {
        kind: 'dispatch_exhausted',
        label: 'Dispatch exhausted',
        actor: 'operator',
        judgment: true,
        summary: 'Dispatch stopped after repeated failures.',
        if_ignored: 'It stays parked until someone recovers it.',
        choices: [
          { ...choice('redispatch', 'Redispatch', '/v1/tasks/T-exhausted/redispatch'), capability: 'recover_work' },
          { ...choice('start_over', 'Start over', '/v1/tasks/T-exhausted/restart'), capability: 'operate_gates' },
        ],
        deadline: null,
        details: {},
      },
      'T-plan': {
        kind: 'plan_approval',
        label: 'Approve plan',
        actor: 'operator',
        judgment: true,
        summary: 'The plan is ready.',
        choices: [choice('approve', 'Approve plan', '/v1/tasks/T-plan/review')],
        deadline: null,
        details: {},
      },
    },
    activity: {
      'T-parked': {
        task: task('T-parked', 'Parked billing task', 'parked'),
        events: [
          {
            id: 1,
            kind: 'triage.completed',
            job_id: 'T-parked-triage-1',
            at: '2026-10-18T09:00:00Z',
            payload: {
              class: 'feature',
              route: 'parked',
              summary: 'The billing service has no seat-count field to sync from.',
              brief: {
                questions: ['Is the seat floor per organization?', ''],
                risks: ['Syncing early could lock out members.'],
                affected_areas: [],
              },
            },
          },
        ],
        jobs: [],
        work_orders: [],
        interventions: [],
      },
      'T-exhausted': {
        task: task('T-exhausted', 'Exhausted dispatch task', 'parked'),
        events: [
          {
            id: 1,
            kind: 'dispatch.failed',
            at: '2026-10-18T09:00:00Z',
            payload: { attempt: 5, max_attempts: 5, error: 'git fetch: authentication failed' },
          },
        ],
        jobs: [],
        work_orders: [],
        interventions: [],
      },
      'T-plan': {
        task: task('T-plan', 'Plan after the parked task'),
        events: [],
        jobs: [],
        work_orders: [],
        interventions: [],
      },
    },
  }
}

test('a parked decision states why it parked and offers Recover first on the c key', async ({ page }) => {
  const harness = await serve(page, parkedFixture())
  await page.goto('/w/demo/inbox?selected=T-parked')
  const card = decisionDetail(page)
  await expect(card.locator('[data-context-heading]')).toHaveText('Why it is parked')
  const reason = card.locator('[data-park-reason]')
  await expect(reason.getByRole('region', { name: 'Triage summary' })).toContainText(
    'The billing service has no seat-count field to sync from.',
  )
  await expect(reason.getByRole('region', { name: 'Open questions' }).getByRole('listitem')).toHaveText([
    'Is the seat floor per organization?',
  ])
  await expect(reason.getByRole('region', { name: 'Risks' }).getByRole('listitem')).toHaveText([
    'Syncing early could lock out members.',
  ])
  await expect(reason).toContainText('Recorded by triage')

  // One row, one primary: Recover leads and carries c; Close and Start over
  // are secondary and enabled.
  const footer = card.locator('[data-decision-footer]')
  const row = footer.locator('[data-next-action-choices] > div').first()
  await expect(row.getByRole('button')).toHaveText(['Recover', 'Close', 'Start over'])
  await expect(row.getByRole('button', { name: 'Recover' })).toHaveClass(/bg-primary/)
  for (const name of ['Close', 'Start over']) {
    await expect(row.getByRole('button', { name, exact: true })).toBeEnabled()
    await expect(row.getByRole('button', { name, exact: true })).not.toHaveClass(/bg-primary/)
  }
  await expect(footer.locator('[data-shortcut="c"]')).toHaveText('Recover')
  await expect(footer).not.toContainText('This page cannot send this choice.')

  await page.keyboard.press('c')
  await expect
    .poll(() => harness.posts.map((post) => new URL(post.url()).pathname))
    .toEqual(['/v1/tasks/T-parked/redispatch'])
})

test('a parked decision without a recorded reason says so', async ({ page }) => {
  const fixture = parkedFixture()
  const activity = fixture.activity?.['T-parked'] as { events: unknown[] }
  activity.events = []
  await serve(page, fixture)
  await page.goto('/w/demo/inbox?selected=T-parked')
  await expect(decisionDetail(page).locator('[data-decision-context]')).toHaveText('Triage recorded no reason.')
})

test('a dispatch-exhausted decision states the failure that exhausted dispatch', async ({ page }) => {
  await serve(page, parkedFixture())
  await page.goto('/w/demo/inbox?selected=T-exhausted')
  const card = decisionDetail(page)
  await expect(card.locator('[data-context-heading]')).toHaveText('Why dispatch stopped')
  const failure = card.locator('[data-dispatch-failure]')
  await expect(failure).toContainText('Attempt 5 of 5')
  await expect(failure).toContainText('git fetch: authentication failed')
})

test('Start over opens the restart dialog in place and the inbox moves on after it', async ({ page }) => {
  const fixture = parkedFixture()
  let restart: Record<string, unknown> | undefined
  const harness = await serve(page, fixture, (request, path) => {
    if (path !== '/v1/tasks/T-parked/restart') return undefined
    restart = request.postDataJSON()
    fixture.items = fixture.items.filter((item) => item.task?.id !== 'T-parked')
    return { successor: { id: 'T-successor' }, cancelled_task: { id: 'T-parked' } }
  })
  await page.goto('/w/demo/inbox?selected=T-parked')
  const footer = decisionDetail(page).locator('[data-decision-footer]')
  await footer.getByRole('button', { name: 'Start over', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Start over', exact: true })
  await expect(dialog).toContainText('This task will be cancelled.')
  const submit = dialog.getByRole('button', { name: 'Start over', exact: true })
  await expect(submit).toBeDisabled()
  await dialog.getByLabel(/Reason/).fill('Triage parked it on stale scope')
  await submit.click()
  await expect(dialog).toBeHidden()
  expect(restart).toMatchObject({ reason: 'Triage parked it on stale scope' })
  expect(typeof restart?.request_id).toBe('string')
  expect(harness.posts.map((post) => new URL(post.url()).pathname)).toEqual(['/v1/tasks/T-parked/restart'])
  // The inbox stays put, refetches, and selects the next decision.
  await expect(page).toHaveURL(/\/inbox/)
  await expect(decisionRows(page)).toHaveCount(2)
  await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Exhausted dispatch task')
})

test('a merge decision names the recorded pull request, keeps the issue apart, and puts every choice in one row', async ({
  page,
}) => {
  const runtimeTask = { ...task('T-merge', 'Enforce fail-closed auth', 'awaiting_human') }
  const withIssue = {
    ...runtimeTask,
    github: {
      task_id: 'T-merge',
      repository: 'acme/runtime',
      issue_number: 1671,
      issue_url: 'https://github.com/acme/runtime/issues/1671',
    },
  }
  const fixture: Fixture = {
    items: [
      {
        band: 'decisions',
        task: withIssue,
        next_action: { kind: 'merge_approval', label: 'Approve merge', actor: 'operator', judgment: true, summary: '' },
        pull_request: { url: 'https://github.com/acme/runtime/pull/1709', number: 1709, repository: 'acme/runtime' },
      },
    ],
    actions: {
      'T-merge': {
        kind: 'merge_approval',
        label: 'Approve merge',
        actor: 'operator',
        judgment: true,
        summary: 'Waiting for merge approval.',
        question: 'Review approved pull request #1709. Merge it?',
        choices: [
          choice('merge', 'Merge', '/v1/tasks/T-merge/merge'),
          choice('approve', 'Approve only', '/v1/tasks/T-merge/review'),
          choice('request_changes', 'Request changes…', '/v1/tasks/T-merge/request-changes'),
        ],
        deadline: null,
        details: { gate_kind: 'merge_approval' },
      },
    },
    activity: {
      'T-merge': {
        task: withIssue,
        events: [],
        jobs: [],
        work_orders: [],
        interventions: [],
        merge_readiness: { state: 'MERGEABLE', head_sha: 'feedface1234567', number: 1709 },
      },
    },
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  // The row links the recorded pull request above its selection link, so the
  // link opens the pull request and never selects the row or the issue.
  const rowLink = rowItems(page).first().locator('[data-row-pull-request]')
  await expect(rowLink).toHaveText('PR #1709')
  await expect(rowLink).toHaveAttribute('title', 'acme/runtime PR #1709')
  await expect(rowLink).toHaveAttribute('href', 'https://github.com/acme/runtime/pull/1709')
  await expect(rowItems(page).first()).not.toContainText('1671')
  await expect(decisionRows(page).first().locator('a')).toHaveCount(0)
  await page.context().route('https://github.com/**', (route) => route.fulfill({ body: 'pull request' }))
  const inboxURL = page.url()
  const [opened] = await Promise.all([page.waitForEvent('popup'), rowLink.click()])
  expect(opened.url()).toBe('https://github.com/acme/runtime/pull/1709')
  await opened.close()
  expect(page.url()).toBe(inboxURL)
  const card = decisionDetail(page)
  await expect(card.locator('[data-pull-request]')).toHaveText('acme/runtime PR #1709')
  await expect(card.locator('[data-pull-request]')).toHaveAttribute('href', 'https://github.com/acme/runtime/pull/1709')
  await expect(card.locator('[data-issue]')).toHaveText('Issue #1671')
  await expect(card.locator('[data-issue]')).toHaveAttribute('href', 'https://github.com/acme/runtime/issues/1671')

  const row = card.locator('[data-decision-footer] [data-next-action-choices] > div').first()
  await expect(row.getByRole('button')).toHaveText(['Merge', 'Approve only', 'Request changes…'])
  await expect(row.locator('button.bg-primary')).toHaveCount(1)
  await expect(row.getByRole('button', { name: 'Merge' })).toHaveClass(/bg-primary/)
})
