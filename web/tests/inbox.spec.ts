import { expect, type Locator, type Page, type Request, type Route } from '@playwright/test'
import { test } from './helpers/bounded-detail'
import { callerIdentity, capabilitiesForRole } from './helpers/caller-identity'
import { expectNoNextActionOmissions } from './helpers/next-action-omissions'
import { controlWorkspaceChanges } from './helpers/workspace-changes'
import { report as reportFixture } from './reports-fixtures'

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
  // Served reports by ID, for GET /v1/feedback-reports/{id}; a number
  // answers with that status.
  reports?: Record<string, Record<string, unknown> | number>
}

interface Harness {
  fixture: Fixture
  posts: Request[]
  inboxRequests: string[]
  documentRequests: Request[]
  reportReads: string[]
  badgeReads: number
  onPost?: (request: Request, path: string) => Record<string, unknown> | undefined
  // While set, acts and document reads wait for it before they answer.
  holdPosts?: Promise<void>
  holdDocuments?: Promise<void>
}

async function serve(page: Page, fixture: Fixture, onPost?: Harness['onPost']): Promise<Harness> {
  const harness: Harness = {
    fixture,
    posts: [],
    inboxRequests: [],
    documentRequests: [],
    reportReads: [],
    badgeReads: 0,
    onPost,
  }
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
      harness.badgeReads++
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
    const reportRead = path.match(/^\/v1\/feedback-reports\/([^/]+)$/)
    if (reportRead && request.method() === 'GET') {
      const id = decodeURIComponent(reportRead[1])
      harness.reportReads.push(id)
      const served = f.reports?.[id]
      if (typeof served === 'number') return route.fulfill({ status: served, json: { error: 'unavailable' } })
      return served
        ? route.fulfill({ json: served })
        : route.fulfill({ status: 404, json: { error: 'report_not_found', message: 'The report was not found.' } })
    }
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

// Fixtures for the Markdown rendering tests below: a context proposal whose
// documents the preview panel reads, and a parked decision with a triage
// reason.
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

// Inbox titles and prose carry authored or agent-written Markdown
// (req-operator-next-action AC-2.1, AC-2.3, AC-2.8, AC-2.9). A title renders
// phrasing Markdown only: code, emphasis, strong text, and strikethrough, with
// no block and no link of its own inside the row or heading link. Prose renders
// block Markdown. Raw HTML stays inert in both renderers.
const markdownTitle =
  'Serve `/mcp` from `conveyord` with *new* ~~old~~ **routes**, [docs](https://example.test/docs) and <b data-raw>raw</b> HTML'

// No element a phrasing-only title must not add: a block, a link, or raw HTML.
const titleBlocks = 'a, p, div, ul, ol, li, h1, h2, h3, h4, pre, blockquote, b, [data-raw]'

async function expectMarkdownTitle(title: Locator) {
  await expect(title.locator('code')).toHaveText(['/mcp', 'conveyord'])
  await expect(title.locator('em')).toHaveText('new')
  await expect(title.locator('del')).toHaveText('old')
  await expect(title.locator('strong')).toHaveText('routes')
  await expect(title.locator(titleBlocks)).toHaveCount(0)
  await expect(title).toContainText('docs')
  await expect(title).toContainText('routes')
  expect(await title.innerText()).not.toContain('`')
  expect(await title.innerText()).not.toContain('**')
}

function markdownTitlesFixture(): Fixture {
  const fixture = decisionsFixture()
  for (const item of fixture.items) {
    const current = item.task as { id: string; title: string } | undefined
    if (current && ['T-zeta', 'T-wait', 'T-auto'].includes(current.id)) item.task = { ...current, title: markdownTitle }
  }
  fixture.items.push({
    band: 'decisions',
    kind: 'document_proposal',
    label: 'Decision proposal',
    question: 'Confirm or dismiss this proposed decision?',
    if_ignored: 'The proposal stays pending.',
    proposal: {
      id: 'DEC-200',
      title: markdownTitle,
      tier: 'decision',
      origin_type: 'operator',
      proposed_at: proposedAt,
      age_seconds: 600,
    },
  })
  fixture.documents = {
    '/v1/decisions/DEC-200': {
      id: 'DEC-200',
      statement: markdownTitle,
      context: 'The daemon reads `CONVEYOR_PUBLIC_URL`.',
      alternatives_rejected: '',
      status: 'proposed',
    },
  }
  return fixture
}

test('Inbox titles render inline Markdown with no link, block, or raw HTML of their own', async ({ page }) => {
  await serve(page, markdownTitlesFixture())
  await page.goto('/w/demo/inbox')
  const decisions = page.getByRole('region', { name: 'Decisions' })

  // The task decision row: the title stays inside its row link, clamped to
  // two lines, with the source text on its title attribute, and the sibling
  // pull request link stays outside the row link.
  const taskRow = decisions.locator('[data-inbox-row="T-zeta"]')
  const taskTitle = taskRow.locator('[data-row-title]')
  await expectMarkdownTitle(taskTitle)
  await expect(taskTitle).toHaveAttribute('title', markdownTitle)
  await expect(taskTitle).toHaveClass(/line-clamp-2/)
  expect(await taskTitle.evaluate((node) => getComputedStyle(node).webkitLineClamp)).toBe('2')
  await expect(taskRow).toHaveAttribute('href', /selected=T-zeta/)
  const pullRequest = decisions.locator('[data-inbox-row-item="T-zeta"] [data-row-pull-request]')
  await expect(pullRequest).toHaveAttribute('href', 'https://github.com/acme/conveyor/pull/77')
  expect(await page.locator('a a').count()).toBe(0)

  // The decision proposal row carries the same Markdown title.
  const proposalRow = decisions.locator('[data-inbox-row="decision:DEC-200"]')
  await expectMarkdownTitle(proposalRow.locator('[data-row-title]'))
  await expect(proposalRow.locator('[data-row-title]')).toHaveAttribute('title', markdownTitle)

  // The task decision detail: the heading link keeps its target and holds
  // no nested link; the article keeps the source text as its name.
  const detail = decisionDetail(page)
  await expect(detail).toHaveAttribute('aria-label', markdownTitle)
  const heading = detail.locator('[data-detail-title]')
  const headingLink = heading.getByRole('link')
  await expect(headingLink).toHaveCount(1)
  await expect(headingLink).toHaveAttribute('href', '/w/demo/tasks/T-zeta')
  await expectMarkdownTitle(headingLink)
  await expect(heading.locator('p, div, ul, li, [data-raw], b')).toHaveCount(0)
  // The Markdown title adds none of the fields AC-1.9 withholds.
  await expectNoNextActionOmissions(detail, {
    actor: 'operator',
    sentences: ['Does this plan meet the requirement?', 'The task waits at the plan gate.'],
    identifiers: ['wo-zeta-1'],
  })

  // The decision proposal detail renders the heading and keeps the statement.
  await proposalRow.click()
  await expect(page).toHaveURL(/selected=decision%3ADEC-200|selected=decision:DEC-200/)
  const proposal = page.locator('[data-inbox-detail] [data-inbox-item="document_proposal"]')
  await expect(proposal).toHaveAttribute('aria-label', markdownTitle)
  const proposalHeading = proposal.locator('[data-detail-title]')
  await expectMarkdownTitle(proposalHeading)
  const statement = proposal
    .getByRole('region', { name: 'Proposed decision' })
    .locator('div')
    .filter({ has: page.getByText('Statement', { exact: true }) })
    .locator('dd')
  await expect(statement.locator('code')).toHaveText(['/mcp', 'conveyord'])
  await expect(proposal.locator('[data-raw], b')).toHaveCount(0)
  expect(await proposal.innerText()).not.toContain('`')
  await expectNoNextActionOmissions(proposal, {
    sentences: ['Confirm or dismiss this proposed decision?', 'The proposal stays pending.'],
  })
  expect(await page.locator('a a').count()).toBe(0)

  // The blocked and automatic rows keep one truncated line, the source text
  // on the title attribute, and the link to the task.
  await page.setViewportSize({ width: 375, height: 900 })
  for (const [band, taskId] of [
    ['blocked', 'T-wait'],
    ['automatic', 'T-auto'],
  ] as const) {
    await page.goto(`/w/demo/inbox?band=${band}`)
    const link = page.locator(`[data-inbox-item="${taskId}"]`).getByRole('link', { name: /Serve/ })
    await expectMarkdownTitle(link)
    await expect(link).toHaveAttribute('title', markdownTitle)
    await expect(link).toHaveAttribute('href', `/w/demo/tasks/${taskId}`)
    await expect(link).toHaveClass(/truncate/)
    const box = await link.evaluate((node) => ({
      overflow: node.scrollWidth > node.clientWidth,
      ellipsis: getComputedStyle(node).textOverflow,
      lines: node.getBoundingClientRect().height / Number.parseFloat(getComputedStyle(node).lineHeight),
    }))
    expect(box.ellipsis).toBe('ellipsis')
    expect(box.overflow).toBe(true)
    expect(box.lines).toBeLessThan(1.5)
    expect(await page.locator('a a').count()).toBe(0)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    expect(overflow).toBeLessThanOrEqual(0)
  }
})

// One fixture whose decisions carry Markdown in every prose field the detail
// renders: the recommendation, a plan file note, the checkpoint question,
// reviewer feedback and the implementer reply, the latest run, and triage.
function markdownProseFixture(): Fixture {
  const fixture = decisionsFixture()
  const zeta = fixture.items[0]
  zeta.recommendation = 'Approve: the plan runs `make test`.\n\n- *First* check\n- Second <b data-raw>check</b>'
  fixture.items.push({
    band: 'decisions',
    task: task('T-run', 'Failed run decision', 'failed'),
    next_action: {
      kind: 'job_failure',
      label: 'Decide after a failed run',
      actor: 'operator',
      judgment: true,
      summary: 'The run failed.',
    },
  })
  fixture.actions['T-run'] = {
    kind: 'job_failure',
    label: 'Decide after a failed run',
    actor: 'operator',
    judgment: true,
    summary: 'The run failed.',
    choices: [choice('retry', 'Retry', '/v1/tasks/T-run/retry')],
    deadline: null,
    details: {},
  }
  const base = (id: string, title: string) => ({
    task: task(id, title),
    events: [],
    jobs: [],
    work_orders: [],
    interventions: [],
  })
  const job = (taskId: string, id: string, stage: string, hour: number, summary: string) => ({
    event: {
      id: hour,
      task_id: taskId,
      job_id: id,
      kind: 'job.summary',
      at: `2026-10-10T1${hour}:30:00Z`,
      payload: { summary },
    },
    job: { id, task_id: taskId, stage, state: 'succeeded', started_at: `2026-10-10T1${hour}:00:00Z`, summary },
  })
  const review = [
    job('T-mid', 'job-impl', 'implement', 0, 'First implementation.'),
    job('T-mid', 'job-review', 'review', 1, 'The `cache_key` is *wrong*.\n\nUse the task ID.'),
    job('T-mid', 'job-impl-2', 'implement', 2, 'Fixed `cache_key`:\n\n1. **Keyed** by task\n2. Tested'),
  ]
  const run = [job('T-run', 'job-run', 'implement', 3, 'The build failed in `go vet`.\n\n- *Fix* the import')]
  fixture.activity = {
    'T-zeta': {
      ...base('T-zeta', 'Zeta plan everyone waits on'),
      spec: {
        task_id: 'T-zeta',
        version: 2,
        content: [
          '## Approach',
          'Render it.',
          '',
          '## Files touched',
          '- `web/a.tsx` — renders `InlineMarkdown` for *titles*',
          '- `web/b.tsx`',
          '',
          '## Ordering',
          '1. Render.',
          '',
          '## Risks',
          '- None.',
          '',
          '## Done criteria',
          '- Tests pass.',
        ].join('\n'),
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
            decision_request: 'Use the `redis` cache?\n\n1. **Keep** it\n2. Drop <b data-raw>it</b>',
            class: 'authority_conflict',
            citations: [],
          },
        },
      ],
    },
    'T-mid': {
      ...base('T-mid', 'Mid item without a deadline'),
      events: review.map((entry) => entry.event),
      jobs: review.map((entry) => entry.job),
    },
    'T-run': {
      ...base('T-run', 'Failed run decision'),
      events: run.map((entry) => entry.event),
      jobs: run.map((entry) => entry.job),
    },
  }
  return fixture
}

async function expectNoMarkdownSource(locator: Locator) {
  const text = await locator.innerText()
  expect(text).not.toContain('`')
  expect(text).not.toContain('**')
  await expect(locator.locator('[data-raw], b')).toHaveCount(0)
}

test('Inbox decision prose renders Markdown in the recommendation, plan notes, checkpoint, and runs', async ({
  page,
}) => {
  await serve(page, markdownProseFixture())
  await page.goto('/w/demo/inbox')
  const detail = decisionDetail(page)

  // The recommendation renders code, emphasis, and its list.
  const recommendation = detail.locator('[data-recommendation]')
  await expect(recommendation.locator('code')).toHaveText('make test')
  // Raw HTML stays inert: the source prints as text and adds no element.
  await expect(recommendation.locator('li')).toHaveText(['First check', 'Second <b data-raw>check</b>'])
  await expect(recommendation.locator('em')).toHaveText('First')
  await expectNoMarkdownSource(recommendation)

  // A plan file note renders code and emphasis beside its path; a file with
  // no note shows only its path.
  const files = detail.locator('[data-plan-section="Files touched"] li')
  await expect(files).toHaveCount(2)
  await expect(files.nth(0).locator('code')).toHaveText(['web/a.tsx', 'InlineMarkdown'])
  await expect(files.nth(0).locator('em')).toHaveText('titles')
  await expect(files.nth(1)).toHaveText('web/b.tsx')
  await expect(files.nth(1).locator('.markdown')).toHaveCount(0)
  await expectNoMarkdownSource(detail.locator('[data-plan-section="Files touched"]'))

  // The checkpoint question renders as block Markdown.
  await select(page, 'Alpha agent question')
  const checkpoint = detail.getByRole('region', { name: 'Agent checkpoint' })
  await expect(checkpoint.locator('code')).toHaveText('redis')
  await expect(checkpoint.locator('ol > li')).toHaveText(['Keep it', 'Drop <b data-raw>it</b>'])
  await expect(checkpoint.locator('strong')).toHaveText('Keep')
  await expectNoMarkdownSource(checkpoint)

  // A single review's summary and the implementer reply render Markdown.
  await select(page, 'Mid item without a deadline')
  const feedback = detail.getByRole('region', { name: 'Latest reviewer feedback' })
  await expect(feedback.locator('code')).toHaveText('cache_key')
  await expect(feedback.locator('em')).toHaveText('wrong')
  await expect(feedback.locator('p')).toHaveText(['The cache_key is wrong.', 'Use the task ID.'])
  await expectNoMarkdownSource(feedback)
  const reply = detail.getByRole('region', { name: 'Implementer reply' })
  await expect(reply.locator('code')).toHaveText('cache_key')
  await expect(reply.locator('ol > li')).toHaveText(['Keyed by task', 'Tested'])
  await expectNoMarkdownSource(reply)

  // Any other decision's latest run renders Markdown under its stage.
  await select(page, 'Failed run decision')
  await expect(detail.locator('[data-context-heading]')).toHaveText('Latest run')
  const latest = detail.getByRole('region', { name: 'Latest run' })
  await expect(latest.locator('h4')).toHaveText('Implement')
  await expect(latest.locator('code')).toHaveText('go vet')
  await expect(latest.locator('li')).toHaveText('Fix the import')
  await expectNoMarkdownSource(latest)
})

test('Inbox fallbacks stay plain text, and an omitted recommendation adds nothing', async ({ page }) => {
  const fixture = markdownProseFixture()
  delete fixture.items[0].recommendation
  const alpha = fixture.activity?.['T-alpha'] as { work_orders: { checkpoint: { decision_request: string } }[] }
  alpha.work_orders[0].checkpoint.decision_request = ''
  await serve(page, fixture)
  await page.goto('/w/demo/inbox')
  const detail = decisionDetail(page)
  await expect(detail.locator('[data-plan-context]')).toBeVisible()
  await expect(detail.locator('[data-recommendation]')).toHaveCount(0)
  await select(page, 'Alpha agent question')
  const checkpoint = detail.getByRole('region', { name: 'Agent checkpoint' })
  await expect(checkpoint.locator('p')).toHaveText('The agent stopped without a structured question.')
  await expect(checkpoint.locator('.markdown')).toHaveCount(0)
})

test('a parked decision renders its triage summary, questions, and risks as Markdown in an intact list', async ({
  page,
}) => {
  const fixture = parkedFixture()
  const parked = fixture.activity?.['T-parked'] as { events: { payload: Record<string, unknown> }[] }
  parked.events[0].payload.summary = 'The billing service has no `seat_count` field.\n\nIt syncs *nightly*.'
  parked.events[0].payload.brief = {
    questions: ['Is the `seat_floor` per **organization**?', 'Who owns <b data-raw>billing</b>?'],
    risks: ['Syncing early could *lock out* members.', 'A stale `seat_cache` could keep removed members.'],
    affected_areas: [],
  }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox?selected=T-parked')
  const reason = decisionDetail(page).locator('[data-park-reason]')
  const summary = reason.getByRole('region', { name: 'Triage summary' })
  await expect(summary.locator('code')).toHaveText('seat_count')
  await expect(summary.locator('p')).toHaveText(['The billing service has no seat_count field.', 'It syncs nightly.'])
  await expect(summary.locator('em')).toHaveText('nightly')
  const questions = reason.getByRole('region', { name: 'Open questions' })
  await expect(questions.locator('ul')).toHaveCount(1)
  await expect(questions.getByRole('listitem')).toHaveText([
    'Is the seat_floor per organization?',
    'Who owns <b data-raw>billing</b>?',
  ])
  await expect(questions.locator('code')).toHaveText('seat_floor')
  await expect(questions.locator('strong')).toHaveText('organization')
  const risks = reason.getByRole('region', { name: 'Risks' })
  await expect(risks.getByRole('listitem')).toHaveText([
    'Syncing early could lock out members.',
    'A stale seat_cache could keep removed members.',
  ])
  await expect(risks.locator('em')).toHaveText('lock out')
  await expect(risks.locator('code')).toHaveText('seat_cache')
  expect(await risks.innerText()).not.toContain('`')
  // Each entry keeps the bullet list's layout: a disc marker and no paragraph
  // margin around the entry's text.
  for (const list of [questions, risks]) {
    const layout = await list.locator('ul').evaluate((ul) => ({
      style: getComputedStyle(ul).listStyleType,
      margins: Array.from(ul.querySelectorAll(':scope > li > .markdown > p')).map((p) => {
        const style = getComputedStyle(p)
        return `${style.marginTop} ${style.marginBottom}`
      }),
    }))
    expect(layout.style).toBe('disc')
    expect(layout.margins.length).toBeGreaterThan(0)
    for (const margin of layout.margins) expect(margin).toBe('0px 0px')
  }
  await expectNoMarkdownSource(reason)
})

test('a parked decision with an empty triage summary keeps its plain fallback', async ({ page }) => {
  const fixture = parkedFixture()
  const parked = fixture.activity?.['T-parked'] as { events: { payload: Record<string, unknown> }[] }
  parked.events[0].payload.summary = ''
  parked.events[0].payload.brief = { questions: [], risks: [], affected_areas: [] }
  await serve(page, fixture)
  await page.goto('/w/demo/inbox?selected=T-parked')
  const reason = decisionDetail(page).locator('[data-park-reason]')
  const summary = reason.getByRole('region', { name: 'Triage summary' })
  await expect(summary.locator('p')).toHaveText('Triage stated no summary.')
  await expect(summary.locator('.markdown')).toHaveCount(0)
  await expect(reason.getByRole('region', { name: 'Open questions' })).toHaveCount(0)
  await expect(reason.getByRole('region', { name: 'Risks' })).toHaveCount(0)
})

// A justification renders phrasing Markdown under the two-line clamp and the
// full Markdown once expanded. "Show more" appears only when the clamped
// preview clips text, as measured on the rendered preview.
const markdownJustifications: [string, string] = [
  'Guards the `review` evidence *rules*.',
  [
    `Its Credential handling section and its Verification section apply to \`make test\` **runs**, see [the guide](https://example.test/guide) and <b data-raw>raw</b>. ${'The suite names every gate it guards and the evidence each gate keeps. '.repeat(4)}`,
    '',
    '- First *listed* reason',
    '- Second reason',
  ].join('\n'),
]

async function clampState(justification: Locator) {
  return justification.evaluate((node) => ({
    clamped: node.classList.contains('line-clamp-2'),
    clips: node.scrollHeight > node.clientHeight + 1,
  }))
}

test('a context justification clamps inline Markdown and expands to full Markdown', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  await serve(page, previewFixture(markdownJustifications))
  await page.goto('/w/demo/inbox')
  // Below md the list and the detail show one at a time.
  await decisionRows(page).first().click()
  const rows = decisionDetail(page).locator('[data-context-proposal]')
  await expect(rows).toHaveCount(2)

  // A short justification renders its code and emphasis on its clamped line
  // and offers no control.
  const short = rows.nth(0).locator('[data-context-justification]')
  await expect(short.locator('code')).toHaveText('review')
  await expect(short.locator('em')).toHaveText('rules')
  await expect(short).toHaveClass(/line-clamp-2/)
  expect((await clampState(short)).clips).toBe(false)
  await expect(rows.nth(0).locator('[data-context-expand]')).toHaveCount(0)
  await expectNoMarkdownSource(short)

  // A long one clips at two lines with phrasing Markdown only, and offers
  // Show more.
  const long = rows.nth(1).locator('[data-context-justification]')
  const expand = rows.nth(1).locator('[data-context-expand]')
  await expect(long).toHaveClass(/line-clamp-2/)
  await expect(long.locator('code')).toHaveText('make test')
  await expect(long.locator('strong')).toHaveText('runs')
  await expect(long.locator('a, p, ul, li, [data-raw], b')).toHaveCount(0)
  expect(await clampState(long)).toEqual({ clamped: true, clips: true })
  await expect(expand).toHaveText('Show more')
  await expect(expand).toHaveAttribute('aria-controls', (await long.getAttribute('id')) ?? '')
  await expect(expand).toHaveAttribute('aria-expanded', 'false')

  // Expanded, it renders the full Markdown: paragraphs, the list, and the link.
  await expand.click()
  await expect(expand).toHaveText('Show less')
  await expect(expand).toHaveAttribute('aria-expanded', 'true')
  await expect(long).not.toHaveClass(/line-clamp-2/)
  await expect(long.locator('li')).toHaveText(['First listed reason', 'Second reason'])
  await expect(long.locator('p')).toHaveCount(1)
  await expect(long.getByRole('link', { name: 'the guide' })).toHaveAttribute('href', 'https://example.test/guide')
  await expectNoMarkdownSource(long)

  // Show less restores the clamp and the measured control.
  await expand.click()
  await expect(expand).toHaveText('Show more')
  await expect(long).toHaveClass(/line-clamp-2/)
  await expect(long.locator('li')).toHaveCount(0)
  expect(await clampState(long)).toEqual({ clamped: true, clips: true })

  // After a width change the control follows the rendered preview: it shows
  // exactly when the clamped preview clips text.
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 })
    for (const index of [0, 1]) {
      const row = rows.nth(index)
      await expect
        .poll(async () => {
          const state = await clampState(row.locator('[data-context-justification]'))
          const shown = await row.locator('[data-context-expand]').count()
          return state.clips === (shown === 1)
        })
        .toBe(true)
    }
  }
  await expect(expand).toHaveText('Show more')
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

test('the preview sheet renders the justification as Markdown and keeps its cited sections', async ({ page }) => {
  await serve(page, previewFixture(markdownJustifications))
  await page.goto('/w/demo/inbox')
  const card = decisionDetail(page)
  await card.getByRole('button', { name: contextTargets[1].title, exact: true }).click()
  const panel = previewPanel(page)
  await expect(panel).toHaveAccessibleName(`Preview: ${contextTargets[1].title}`)
  const why = panel.locator('[data-preview-why]')
  await expect(why).toContainText('Why planning suggests it')
  await expect(why.locator('code')).toHaveText('make test')
  await expect(why.locator('strong')).toHaveText('runs')
  await expect(why.locator('li')).toHaveText(['First listed reason', 'Second reason'])
  await expect(why.getByRole('link', { name: 'the guide' })).toHaveAttribute('href', 'https://example.test/guide')
  await expectNoMarkdownSource(why)
  // The source text still decides the cited sections.
  await expect(panel.locator('[data-preview-cited-count]')).toHaveText('2 of 4 sections cited')
  // The preview opens in place over the detail.
  await expect(page).toHaveURL(/\/w\/demo\/inbox/)
  await page.keyboard.press('Escape')
  await expect(previewPanel(page)).toHaveCount(0)
  await expect(card).toBeVisible()
})

test('the compact checklist on task detail shows the justification as Markdown behind Show why', async ({ page }) => {
  await serve(page, previewFixture(markdownJustifications))
  await page.goto('/w/demo/tasks/T-ctx')
  const checklist = page.locator('[data-context-checklist="compact"]')
  await expect(checklist).toBeVisible()
  const row = checklist.locator('[data-context-proposal]').nth(1)
  await expect(row.locator('[data-context-justification]')).toHaveCount(0)
  const why = row.getByRole('button', { name: 'Show why' })
  await why.click()
  const justification = row.locator('[data-context-justification]')
  await expect(row.getByRole('button', { name: 'Hide why' })).toHaveAttribute(
    'aria-controls',
    (await justification.getAttribute('id')) ?? '',
  )
  await expect(justification).not.toHaveClass(/line-clamp-2/)
  await expect(justification.locator('code')).toHaveText('make test')
  await expect(justification.locator('li')).toHaveText(['First listed reason', 'Second reason'])
  await expectNoMarkdownSource(justification)
  await row.getByRole('button', { name: 'Hide why' }).click()
  await expect(row.locator('[data-context-justification]')).toHaveCount(0)
})

// Reports from production in the Inbox (component-feedback-reports v5
// invariants 19, 25, 26; component-next-action invariants 21, 22, 34, 37, 38;
// req-operator-next-action AC-2.8, AC-2.10, AC-2.12;
// feature-production-feedback-listening FR-5). A report item carries the
// served report; the detail renders the shared report card with its acts.
const REPORT_A = 'rpt-261127-a0a0a1'
const REPORT_B = 'rpt-261127-b0b0b2'
const reportRow = (page: Page, id: string) => page.locator(`[data-inbox-row="${id}"]`)
const reportDetail = (page: Page) => page.locator('[data-inbox-detail] [data-inbox-item="feedback_report"]')

function servedReport(id: string, overrides: Record<string, unknown> = {}) {
  return reportFixture(id, overrides) as unknown as Record<string, unknown>
}

function reportItem(served: Record<string, unknown>, at: string) {
  return { kind: 'feedback_report', band: 'decisions', report: served, last_event_at: at }
}

// Server order: T-zeta, T-alpha, report A, T-mid, report B, then the other
// bands.
function reportsFixture(): Fixture {
  const fixture = decisionsFixture()
  const a = servedReport(REPORT_A, {
    summary: 'Checkout request timed out after 30 seconds\nStack: pay.go:214',
    created_at: '2026-11-27T11:00:00Z',
  })
  const b = servedReport(REPORT_B, { kind: 'alert', summary: 'Payment queue depth is above 10,000' })
  const decisions = fixture.items.filter((item) => item.band === 'decisions')
  const rest = fixture.items.filter((item) => item.band !== 'decisions')
  fixture.items = [
    decisions[0],
    decisions[1],
    reportItem(a, '2026-11-27T11:00:00Z'),
    decisions[2],
    reportItem(b, '2026-11-27T11:30:00Z'),
    ...rest,
  ]
  fixture.operatorDecisions = 5
  fixture.reports = { [REPORT_A]: a, [REPORT_B]: b }
  return fixture
}

// Serves a report in place of the fixture's, in the Inbox item and on its
// own read.
function replaceReport(fixture: Fixture, served: Record<string, unknown>) {
  const id = served.id as string
  fixture.items = fixture.items.map((item) =>
    (item.report as { id?: string } | undefined)?.id === id ? { ...item, report: served } : item,
  )
  fixture.reports = { ...fixture.reports, [id]: served }
}

// Removes a report from the Inbox the server serves, as an act or another
// person's act would, and answers its served report.
function leaveInbox(fixture: Fixture, id: string, change: Record<string, unknown>) {
  fixture.items = fixture.items.filter((item) => (item.report as { id?: string } | undefined)?.id !== id)
  fixture.operatorDecisions = (fixture.operatorDecisions ?? 1) - 1
  const served = { ...(fixture.reports?.[id] as Record<string, unknown>), in_inbox: false, ...change }
  fixture.reports = { ...fixture.reports, [id]: served }
  return served
}

function closedChange(reason: string) {
  return { state: 'closed', closed: { at: '2026-11-27T12:00:00Z', by: 'usr_operator', reason } }
}

async function closeSelectedReport(page: Page, reason = 'Fixed by the last deploy.') {
  const detail = reportDetail(page)
  await detail.getByRole('button', { name: 'Close report' }).click()
  await detail.getByLabel('Reason to close the report').fill(reason)
  await detail.locator('form').getByRole('button', { name: 'Close report' }).click()
}

const actsSent = (harness: Harness) =>
  harness.posts.filter((request) => new URL(request.url()).pathname.startsWith('/v1/feedback-reports/'))

test.describe('reports from production in the Inbox', () => {
  test('a report is one decision row with its kind, plain first line, and age, and its card in the detail', async ({
    page,
  }) => {
    await serve(page, reportsFixture())
    await page.goto('/w/demo/inbox')
    await expect(decisionRows(page)).toHaveCount(5)
    const keys = await decisionRows(page).evaluateAll((rows) => rows.map((row) => row.getAttribute('data-inbox-row')))
    expect(keys).toEqual(['T-zeta', 'T-alpha', REPORT_A, 'T-mid', REPORT_B])
    const row = reportRow(page, REPORT_A)
    await expect(row.locator('[data-row-kind]')).toHaveText('Production error')
    await expect(reportRow(page, REPORT_B).locator('[data-row-kind]')).toHaveText('Production alert')
    // The title is the first line of the summary, as plain text.
    await expect(row.locator('[data-row-title]')).toHaveText('Checkout request timed out after 30 seconds')
    await expect(row.locator('[data-row-age]')).toHaveAttribute('datetime', '2026-11-27T11:00:00Z')
    await expect(row.locator('[data-row-age]')).not.toHaveText('')
    // A report is not urgent by itself and has no pull request.
    await expect(page.locator(`[data-inbox-row-item="${REPORT_A}"] [data-row-urgent]`)).toHaveCount(0)
    await expect(page.locator(`[data-inbox-row-item="${REPORT_A}"] [data-row-pull-request]`)).toHaveCount(0)

    await row.click()
    await expect(page).toHaveURL(new RegExp(`selected=${REPORT_A}`))
    const detail = reportDetail(page)
    await expect(detail.locator('[data-decision-label]')).toHaveText('Production error')
    await expect(detail.locator('[data-detail-title]')).toHaveText('Checkout request timed out after 30 seconds')
    const card = detail.locator(`[data-report-card="${REPORT_A}"]`)
    await expect(card.locator('[data-report-summary]')).toHaveText(
      'Checkout request timed out after 30 seconds\nStack: pay.go:214',
    )
    await expect(card.locator('[data-report-environment]')).toHaveText('production')
    for (const name of ['Link a task or crate', 'Close report']) {
      await expect(card.getByRole('button', { name })).toBeEnabled()
    }
    await expect(card.locator('[data-report-start-fix-task]')).toBeVisible()
  })

  test('the address selects a report by its ID, also after a reload', async ({ page }) => {
    await serve(page, reportsFixture())
    await page.goto(`/w/demo/inbox?selected=${REPORT_B}`)
    await expect(reportDetail(page).locator(`[data-report-card="${REPORT_B}"]`)).toBeVisible()
    await expect(reportRow(page, REPORT_B)).toHaveAttribute('aria-current', /.+/)
    await page.reload()
    await expect(reportDetail(page).locator(`[data-report-card="${REPORT_B}"]`)).toBeVisible()
  })

  test('closing the selected report records one act, reads the Inbox and the badge again, and selects the next item', async ({
    page,
  }) => {
    const fixture = reportsFixture()
    const harness = await serve(page, fixture, (_request, path) =>
      path === `/v1/feedback-reports/${REPORT_A}/close`
        ? leaveInbox(fixture, REPORT_A, closedChange('Fixed by the last deploy.'))
        : undefined,
    )
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    const badge = page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: /Inbox/ })
    await expect(badge.getByText('5', { exact: true })).toBeVisible()
    const inboxBefore = harness.inboxRequests.length
    const badgeBefore = harness.badgeReads
    await closeSelectedReport(page)
    // The next item in the server's order (AC-2.10).
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
    await expect(page).toHaveURL(/selected=T-mid/)
    await expect(reportRow(page, REPORT_A)).toHaveCount(0)
    expect(actsSent(harness).map((request) => `${request.method()} ${new URL(request.url()).pathname}`)).toEqual([
      `POST /v1/feedback-reports/${REPORT_A}/close`,
    ])
    expect(actsSent(harness)[0].postDataJSON()).toEqual({ reason: 'Fixed by the last deploy.' })
    await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(inboxBefore)
    await expect.poll(() => harness.badgeReads).toBeGreaterThan(badgeBefore)
    await expect(badge.getByText('4', { exact: true })).toBeVisible()
  })

  test('an act on the last item selects the one before it', async ({ page }) => {
    const fixture = reportsFixture()
    await serve(page, fixture, (_request, path) =>
      path === `/v1/feedback-reports/${REPORT_B}/close`
        ? leaveInbox(fixture, REPORT_B, closedChange('Done.'))
        : undefined,
    )
    await page.goto(`/w/demo/inbox?selected=${REPORT_B}`)
    await closeSelectedReport(page, 'Done.')
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
    await expect(page).toHaveURL(/selected=T-mid/)
  })

  test('when the last decision goes, the Inbox shows the empty state', async ({ page }) => {
    const fixture = reportsFixture()
    fixture.items = fixture.items.filter(
      (item) => item.kind === 'feedback_report' && (item.report as { id: string }).id === REPORT_A,
    )
    fixture.operatorDecisions = 1
    await serve(page, fixture, (_request, path) =>
      path === `/v1/feedback-reports/${REPORT_A}/close`
        ? leaveInbox(fixture, REPORT_A, closedChange('Done.'))
        : undefined,
    )
    await page.goto('/w/demo/inbox')
    await closeSelectedReport(page, 'Done.')
    await expect(page.locator('[data-no-decisions]')).toContainText("You're clear")
    await expect(page.locator('[data-inbox-summary]')).toHaveText('Nothing needs you right now.')
  })

  test('a refused act resolves nothing: a report someone else closed falls back to the first item', async ({
    page,
  }) => {
    const fixture = reportsFixture()
    const harness = await serve(page, fixture, (_request, path) => {
      if (path !== `/v1/feedback-reports/${REPORT_A}/close`) return undefined
      leaveInbox(fixture, REPORT_A, closedChange('Someone else closed it.'))
      return { status: 409, error: 'report_closed', message: 'The report is closed.' }
    })
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    const inboxBefore = harness.inboxRequests.length
    await closeSelectedReport(page)
    await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(inboxBefore)
    await expect(reportRow(page, REPORT_A)).toHaveCount(0)
    // Not the neighbour: an item that left without this page's recorded act
    // is a stale selection, and the wide layout shows the first item.
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
    expect(actsSent(harness)).toHaveLength(1)
  })

  test('an unknown answer reads the report again, never sends the act again, and resolves nothing', async ({
    page,
  }) => {
    const fixture = reportsFixture()
    const harness = await serve(page, fixture, (_request, path) =>
      path === `/v1/feedback-reports/${REPORT_A}/close` ? { status: 500, error: 'internal' } : undefined,
    )
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    const readsBefore = harness.reportReads.length
    await closeSelectedReport(page)
    const detail = reportDetail(page)
    await expect(detail.locator('[data-report-act-error]')).toContainText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again.',
    )
    expect(harness.reportReads.length).toBeGreaterThan(readsBefore)
    expect(harness.reportReads.slice(readsBefore).every((id) => id === REPORT_A)).toBe(true)
    await expect(detail.locator(`[data-report-card="${REPORT_A}"]`)).toBeVisible()
    await expect(page).toHaveURL(new RegExp(`selected=${REPORT_A}`))
    expect(actsSent(harness)).toHaveLength(1)
  })

  test('a report acted on in another tab leaves on a stream frame and the selection falls back to the first item', async ({
    page,
  }) => {
    const stream = await controlWorkspaceChanges(page)
    const fixture = reportsFixture()
    const harness = await serve(page, fixture)
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    await expect(reportDetail(page)).toBeVisible()
    await stream.ready()
    const before = harness.inboxRequests.length
    leaveInbox(fixture, REPORT_A, { links: [] })
    // A `workspace` notice is a frame that names no task.
    await stream.activity()
    await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(before)
    await expect(reportRow(page, REPORT_A)).toHaveCount(0)
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Zeta plan everyone waits on')
  })

  test('an act answer that arrives after the selection moved changes only its own report', async ({ page }) => {
    const fixture = reportsFixture()
    const harness = await serve(page, fixture, (_request, path) =>
      path === `/v1/feedback-reports/${REPORT_A}/close`
        ? leaveInbox(fixture, REPORT_A, closedChange('Late.'))
        : undefined,
    )
    let release = () => {}
    harness.holdPosts = new Promise<void>((resolve) => {
      release = resolve
    })
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    await closeSelectedReport(page, 'Late.')
    await expect.poll(() => actsSent(harness).length).toBe(1)
    await select(page, 'Alpha agent question')
    release()
    await expect(reportRow(page, REPORT_A)).toHaveCount(0)
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Alpha agent question')
    await expect(page).toHaveURL(/selected=T-alpha/)
  })

  test('the band and the narrowings treat a report as a decision of no task', async ({ page }) => {
    await serve(page, reportsFixture())
    await page.goto('/w/demo/inbox?band=blocked')
    await expect(page.getByRole('region', { name: 'Decisions' })).toHaveCount(0)
    await page.goto('/w/demo/inbox?task=T-zeta')
    await expect(reportRow(page, REPORT_A)).toBeVisible()
    await expect(reportRow(page, REPORT_B)).toBeVisible()
    await page.goto('/w/demo/inbox?document=component-x&tier=system_design')
    await expect(reportRow(page, REPORT_A)).toHaveCount(0)
  })

  test('no Inbox key records an act on a selected report', async ({ page }) => {
    const fixture = reportsFixture()
    for (const id of [REPORT_A, REPORT_B]) {
      // A report with a possible link shows every control of an open report.
      replaceReport(
        fixture,
        servedReport(id, {
          links: [
            { id: `lnk-${id}`, state: 'possible', task: { id: 'T-cause', title: 'Cause', crate: null }, evidence: [] },
          ],
        }),
      )
    }
    const harness = await serve(page, fixture)
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    const detail = reportDetail(page)
    await expect(detail.getByRole('button', { name: /Confirm the link/ })).toBeVisible()
    for (const key of ['c', 'r', 'd', 'Enter', 'Space', 'Delete', 'Backspace', 'x', 'a', 'm']) {
      await page.keyboard.press(key)
    }
    await expect(detail.locator('[data-shortcut]')).toHaveCount(0)
    // j and k still move the selection.
    await page.keyboard.press('j')
    await expect(decisionDetail(page).locator('[data-detail-title]')).toHaveText('Mid item without a deadline')
    await page.keyboard.press('k')
    await expect(reportDetail(page)).toBeVisible()
    expect(harness.posts).toHaveLength(0)
  })

  test('every report text prints as plain text in the row and the detail', async ({ page }) => {
    const fixture = reportsFixture()
    const hostile = servedReport(REPORT_A, {
      summary:
        '**bold** <img src=x onerror="window.__xss=1"> `code` ``````‮evil\tTab\nsecond <script>window.__xss=2</script>',
      key: '<b>key</b>‮',
      service: '<script>window.__xss=3</script>',
      environment: {
        id: 'env-x',
        name: '_env_ <i>name</i> [link](javascript:alert(1))',
        listening: true,
        last_signal_at: null,
      },
      file: '<u>file</u>.go',
      line: 3,
    })
    fixture.items = fixture.items.map((item) =>
      (item.report as { id?: string } | undefined)?.id === REPORT_A
        ? reportItem(hostile, '2026-11-27T11:00:00Z')
        : item,
    )
    fixture.reports = { ...fixture.reports, [REPORT_A]: hostile }
    await serve(page, fixture)
    await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
    const row = reportRow(page, REPORT_A)
    await expect(row.locator('[data-row-title]')).toHaveText(
      '**bold** <img src=x onerror="window.__xss=1"> `code` ``````‮evil\tTab',
    )
    const detail = reportDetail(page)
    await expect(detail.locator('[data-report-summary]')).toHaveText(hostile.summary as string)
    await expect(detail.locator('[data-report-key]')).toHaveText('<b>key</b>‮')
    await expect(detail.locator('[data-report-service]')).toHaveText('<script>window.__xss=3</script>')
    await expect(detail.locator('[data-report-environment]')).toHaveText(
      '_env_ <i>name</i> [link](javascript:alert(1))',
    )
    for (const root of [page.locator(`[data-inbox-row-item="${REPORT_A}"]`), detail]) {
      await expect(root.locator('img, script, strong, em, b, i, u, code, pre, a[href^="javascript"]')).toHaveCount(0)
    }
    expect(await page.evaluate(() => (window as unknown as { __xss?: number }).__xss)).toBeUndefined()
  })
})

// A typical report and the longest values the design allows, at 375 px and
// at a desktop width: nothing overflows the page, and the clamped row title
// and heading are read in full in the card. CONVEYOR_SCREENSHOT_DIR keeps a
// local copy, and the Playwright report carries each attachment.
const screenshotReports: Record<string, Record<string, unknown>> = {
  typical: servedReport(REPORT_A, {
    summary: 'Checkout request timed out after 30 seconds\nThe payment provider did not answer.',
    key: 'checkout.timeout',
    file: 'internal/checkout/pay.go',
    line: 214,
    links: [
      {
        id: 'lnk-1',
        state: 'possible',
        task: { id: '261120-abc123', title: 'Retry payment calls once', crate: { id: 'crate-1', name: 'Payments' } },
        evidence: [
          {
            kind: 'line',
            file: 'internal/checkout/pay.go',
            line: 214,
            commit: '3f9c2ab1d4e5f60718293a4b5c6d7e8f90a1b2c3',
          },
        ],
      },
    ],
  }),
  longest: servedReport(REPORT_A, {
    summary: `${'Checkout '.repeat(20)}${'W'.repeat(400)}\n${'Second line. '.repeat(30)}`.slice(0, 1000),
    key: `checkout.${'k'.repeat(191)}`,
    service: 's'.repeat(100),
    environment: {
      id: 'env-long',
      name: `production-${'e'.repeat(60)}`,
      listening: false,
      last_signal_at: '2026-11-26T18:00:00Z',
    },
    file: `internal/${'d'.repeat(480)}/pay.go`.slice(0, 500),
    line: 1000000,
    count: 1000000000,
    links_pending: true,
  }),
}

for (const [variant, served] of Object.entries(screenshotReports)) {
  for (const width of [375, 1280]) {
    test(`a selected ${variant} report in the Inbox screenshot at ${width} px`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 })
      const fixture = reportsFixture()
      replaceReport(fixture, served)
      await serve(page, fixture)
      await page.goto(`/w/demo/inbox?selected=${REPORT_A}`)
      const detail = reportDetail(page)
      await expect(detail.locator('[data-report-summary]')).toHaveText(served.summary as string)
      await page.evaluate(() => document.fonts.ready)
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      )
      expect(overflow).toBeLessThanOrEqual(0)
      for (const locator of [detail, detail.locator('[data-report-card]'), detail.locator('[data-detail-title]')]) {
        const box = await locator.boundingBox()
        expect(box && box.x + box.width).toBeLessThanOrEqual(width)
      }
      if (width >= 768) {
        const box = await page.locator(`[data-inbox-row-item="${REPORT_A}"]`).boundingBox()
        expect(box && box.x + box.width).toBeLessThanOrEqual(width)
      }
      const shots = process.env.CONVEYOR_SCREENSHOT_DIR
      const file = `inbox-report-${variant}-${width}.png`
      const image = await page.screenshot({ path: shots ? `${shots}/${file}` : undefined })
      await testInfo.attach(file, { body: image, contentType: 'image/png' })
    })
  }
}
