import { expect, type Page, type Request, type Route, test } from '@playwright/test'
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
}

interface Harness {
  fixture: Fixture
  posts: Request[]
  inboxRequests: string[]
  onPost?: (request: Request, path: string) => Record<string, unknown> | undefined
}

async function serve(page: Page, fixture: Fixture, onPost?: Harness['onPost']): Promise<Harness> {
  const harness: Harness = { fixture, posts: [], inboxRequests: [], onPost }
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const f = harness.fixture
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') {
      const identity = callerIdentity({ id: 'usr_operator', role: f.role ?? 'operator' })
      return route.fulfill({ json: f.capabilities ? { ...identity, capabilities: f.capabilities } : identity })
    }
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/next-actions') {
      harness.inboxRequests.push(url.search)
      const band = url.searchParams.get('band')
      const items = band ? f.items.filter((item) => item.band === band) : f.items
      return route.fulfill({
        json: { items, acts: f.acts ?? [] },
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
    if (request.method() !== 'GET') {
      harness.posts.push(request)
      const body = harness.onPost?.(request, path)
      return route.fulfill({ json: body ?? {} })
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
        task: task('T-zeta', 'Zeta plan everyone waits on'),
        next_action: {
          kind: 'plan_approval',
          label: 'Approve the plan',
          actor: 'operator',
          judgment: true,
          summary: 'The plan is ready.',
          deadline: soon,
        },
        pull_request: {
          task_id: 'T-zeta',
          repository: 'acme/conveyor',
          issue_number: 77,
          issue_url: 'https://github.com/acme/conveyor/pull/77',
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

const decisionCards = (page: Page) => page.getByRole('region', { name: 'Decisions' }).locator('[data-inbox-item]')

test('bands render in server order with the decision card the server states', async ({ page }) => {
  await serve(page, decisionsFixture())
  await page.goto('/inbox')

  await expect(decisionCards(page)).toHaveCount(3)
  await expect(decisionCards(page).locator('h3')).toHaveText([
    'Zeta plan everyone waits on',
    'Alpha agent question',
    'Mid item without a deadline',
  ])
  const bands = page.locator(
    'section[aria-label="Decisions"], section[aria-label="Blocked"], section[aria-label="Automatic"]',
  )
  await expect(bands).toHaveCount(3)
  expect(await bands.evaluateAll((nodes) => nodes.map((node) => node.getAttribute('aria-label')))).toEqual([
    'Decisions',
    'Blocked',
    'Automatic',
  ])

  const card = decisionCards(page).first()
  await expect(card.getByText('Approve the plan', { exact: true })).toBeVisible()
  await expect(card.getByRole('link', { name: 'acme/conveyor#77' })).toHaveAttribute(
    'href',
    'https://github.com/acme/conveyor/pull/77',
  )
  await expect(card.getByText('Serves REQ-2, REQ-6')).toBeVisible()
  await expect(card.getByText('Does this plan meet the requirement?')).toBeVisible()
  await expect(card.getByText('Recommendation: Approve: the plan covers every criterion.')).toBeVisible()
  await expect(card.getByText('If ignored: The task waits at the plan gate.')).toBeVisible()
  await expect(card.locator('[data-next-action-deadline] time')).toHaveAttribute('datetime', soon)
  await card.getByText('Show IDs').click()
  await expect(card.getByText('wo-zeta-1')).toBeVisible()

  // Every server choice renders (DEC-61): a disallowed choice and one this
  // client cannot build are disabled with the reason beside them.
  await expect(card.getByRole('button', { name: 'Approve plan' })).toBeEnabled()
  await expect(card.getByRole('button', { name: 'Reject plan' })).toBeDisabled()
  await expect(card.getByText('Needs the operate tasks capability.')).toBeVisible()
  await expect(card.getByRole('button', { name: 'Summon dragon' })).toBeDisabled()
  await expect(card.getByText('Take this from the CLI or MCP; it needs input this page cannot send yet.')).toBeVisible()
  // The null-deadline item states none.
  await expect(decisionCards(page).nth(2).locator('[data-next-action-deadline]')).toHaveCount(0)
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
  await page.goto('/inbox')
  const inbox = page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: /Inbox/ })
  await expect(inbox.getByText('3', { exact: true })).toBeVisible()
  const before = harness.inboxRequests.length

  await decisionCards(page).first().getByRole('button', { name: 'Approve plan' }).dblclick()
  await expect(decisionCards(page)).toHaveCount(2)
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
  await page.goto('/tasks/T-zeta')
  await expect(page.getByRole('region', { name: 'Next action' })).toContainText(
    'An agent is implementing the approved plan.',
  )
})

test('agent_question opens the direction input and posts the direction', async ({ page }) => {
  const harness = await serve(page, decisionsFixture())
  await page.goto('/inbox')
  const card = decisionCards(page).nth(1)
  await card.getByRole('button', { name: 'Answer' }).click()
  await card.getByLabel('Direction').fill('Use the shared cache.')
  await card.getByRole('button', { name: 'Answer' }).last().click()
  await expect.poll(() => harness.posts.length).toBe(1)
  expect(new URL(harness.posts[0].url()).pathname).toBe('/v1/work-orders/wo-alpha/recover')
  expect(harness.posts[0].postDataJSON()).toMatchObject({ direction: 'Use the shared cache.' })
})

test('View opens the decision context for each kind without leaving the inbox', async ({ page }) => {
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
  await page.goto('/inbox')

  await decisionCards(page).nth(0).getByRole('button', { name: 'View' }).click()
  const sheet = page.getByRole('dialog', { name: 'Decision context' })
  await expect(sheet.getByText('Plan body for zeta')).toBeVisible()
  await sheet.getByRole('button', { name: 'Close' }).click()

  await decisionCards(page).nth(1).getByRole('button', { name: 'View' }).click()
  await expect(sheet.getByRole('region', { name: 'Agent checkpoint' })).toContainText('Which cache should it use?')
  await expect(sheet.getByText(/Cache design \(sd-cache\) cites v2/)).toBeVisible()
  await sheet.getByRole('button', { name: 'Close' }).click()

  await decisionCards(page).nth(2).getByRole('button', { name: 'View' }).click()
  await expect(sheet.getByRole('region', { name: 'Latest reviewer feedback' })).toContainText(
    'Reviewer says the cache key is wrong.',
  )
  await expect(sheet.getByRole('region', { name: 'Implementer reply' })).toContainText(
    'Implementer fixed the cache key.',
  )
  await expect(page).toHaveURL(/\/inbox$/)
})

// The inbox has no stream of its own; a task's event stream moves that task's
// next action, so it refreshes the inbox listing it (lib/use-task-stream.ts).
test('a task stream event refreshes the inbox listing while its context is open', async ({ page }) => {
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
  await page.goto('/inbox')
  await expect(decisionCards(page)).toHaveCount(3)
  await decisionCards(page).nth(0).getByRole('button', { name: 'View' }).click()
  await expect(page.getByRole('dialog', { name: 'Decision context' })).toBeVisible()

  // The plan was decided elsewhere; only the stream can tell this page.
  harness.fixture = { ...fixture, items: fixture.items.filter((item) => item.task?.id !== 'T-zeta') }
  const before = harness.inboxRequests.length
  releaseStream()
  await expect.poll(() => harness.inboxRequests.length).toBeGreaterThan(before)
  await expect(decisionCards(page)).toHaveCount(2)
  await expect(decisionCards(page).getByText('Zeta plan everyone waits on')).toHaveCount(0)
})

test('blocked rows link blockers, the automatic band offers its one choice and a collapsed acts log', async ({
  page,
}) => {
  await serve(page, decisionsFixture())
  await page.goto('/inbox')

  const blocked = page.getByRole('region', { name: 'Blocked' })
  await expect(blocked.getByText('Waits for')).toBeVisible()
  await expect(blocked.getByRole('link', { name: 'Zeta plan everyone waits on' })).toHaveAttribute(
    'href',
    '/inbox?task=T-zeta',
  )
  await expect(blocked.getByRole('link', { name: 'Elsewhere dependency' })).toHaveAttribute('href', '/tasks/T-done')
  await blocked.getByRole('link', { name: 'Zeta plan everyone waits on' }).click()
  await expect(decisionCards(page)).toHaveCount(1)
  await expect(decisionCards(page).locator('h3')).toHaveText('Zeta plan everyone waits on')
  await page.getByRole('link', { name: 'Show all' }).click()

  const automatic = page.getByRole('region', { name: 'Automatic' })
  await expect(automatic.getByText('Automatic redispatch')).toBeVisible()
  await expect(automatic.getByRole('button')).toHaveCount(1)
  await expect(automatic.getByRole('button', { name: 'Redispatch now' })).toBeVisible()
  const log = automatic.locator('[data-acts-log]')
  await expect(log).not.toHaveAttribute('open')
  await expect(log.getByText('Redispatched')).toBeHidden()
  await log.getByText('Factory acts (1)').click()
  await expect(log.getByText('Redispatched')).toBeVisible()
})

function proposalFixture(): Fixture {
  const proposal = (id: string, title: string, tier: string, version: number | undefined, originTask?: string) => ({
    band: 'decisions',
    kind: 'document_proposal',
    proposal: {
      id,
      title,
      tier,
      ...(version == null ? {} : { version }),
      origin_type: originTask ? 'task' : 'operator',
      ...(originTask ? { origin_id: originTask } : {}),
      proposed_at: proposedAt,
      age_seconds: 3600,
    },
  })
  return {
    operatorDecisions: 4,
    items: [
      {
        ...proposal('req-attention', 'Operator attention', 'requirement', 3, 'review-task'),
        sentence: 'Confirming a later one dismisses the earlier ones.',
      },
      proposal('design-dashboard', 'Dashboard architecture', 'system_design', 4, 'other-task'),
      proposal('DEC-16', 'Generate dashboard output from web sources.', 'decision', undefined),
      proposal('req-other', 'Other requirement', 'requirement', 1, 'review-task'),
    ],
    actions: {},
  }
}

test('proposal cards keep the retired queue rules for every tier', async ({ page }) => {
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
  await page.goto('/inbox')
  const cards = page.locator('[data-inbox-item="document_proposal"]')
  await expect(cards).toHaveCount(4)
  await expect(cards.first().getByText('Confirming a later one dismisses the earlier ones.')).toBeVisible()

  // Confirm reads the current version first and confirms against it.
  await cards.filter({ hasText: 'Operator attention' }).getByRole('button', { name: 'Confirm' }).click()
  await expect(cards).toHaveCount(3)
  expect(seen.indexOf('GET /v1/requirements/req-attention')).toBeLessThan(
    seen.indexOf('POST /v1/requirements/req-attention/versions/3/confirm'),
  )

  // System Design Dismiss asks first; Decision Dismiss sends at once.
  const design = cards.filter({ hasText: 'Dashboard architecture' })
  await design.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  const sentBeforeCancel = harness.posts.length
  await page.getByRole('button', { name: 'Cancel' }).click()
  expect(harness.posts.length).toBe(sentBeforeCancel)

  await cards.filter({ hasText: 'Generate dashboard output' }).getByRole('button', { name: 'Dismiss' }).click()
  await expect(cards).toHaveCount(2)
  await expect(page.getByRole('dialog')).toHaveCount(0)
})

test('task, document and tier narrow the decisions band', async ({ page }) => {
  await serve(page, proposalFixture())
  await page.goto('/inbox?task=review-task')
  const cards = page.locator('[data-inbox-item="document_proposal"]')
  await expect(cards).toHaveCount(2)
  await expect(cards.locator('h3')).toHaveText(['Operator attention', 'Other requirement'])

  await page.goto('/inbox?document=req-attention&tier=requirement')
  await expect(cards).toHaveCount(1)
  await expect(cards.locator('h3')).toHaveText('Operator attention')

  // The tier must match too: the same id at another tier is not this document.
  await page.goto('/inbox?document=req-attention&tier=system_design')
  await expect(page.getByText('No decisions are waiting for you.')).toBeVisible()
  await expect(cards).toHaveCount(0)
})

test('a maintainer without confirm_documents reads proposals without acting', async ({ page }) => {
  const fixture = proposalFixture()
  fixture.capabilities = capabilitiesForRole('maintainer').filter((capability) => capability !== 'confirm_documents')
  fixture.role = 'maintainer'
  await serve(page, fixture)
  await page.goto('/inbox')
  const cards = page.locator('[data-inbox-item="document_proposal"]')
  await expect(cards).toHaveCount(4)
  await expect(cards.getByRole('button', { name: 'Confirm' })).toHaveCount(0)
  await expect(cards.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('/pending-proposals lands on the inbox decisions band with its narrowing', async ({ page }) => {
  await serve(page, proposalFixture())
  await page.goto('/pending-proposals?task=T&document=D&tier=requirement')
  await expect(page).toHaveURL((url) => {
    const search = url.searchParams
    return (
      url.pathname === '/inbox' &&
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
  await page.goto('/inbox')
  const primary = page.getByRole('navigation', { name: 'Primary' })
  await expect(primary.getByRole('link', { name: /Inbox/ }).getByText('5', { exact: true })).toBeVisible()
  const labels = await primary
    .locator('a')
    .evaluateAll((links) => links.map((link) => link.textContent?.replace(/\d+$/, '').trim()))
  expect(labels).toEqual(['Board', 'Inbox', 'Workspace', 'Requirements', 'System Design', 'Monitor', 'Settings'])
  await expect(primary.getByRole('link', { name: /Inbox/ }).getByText('5', { exact: true })).toBeVisible()
})

test('the inbox fits a 390px viewport without horizontal overflow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 900 })
  await serve(page, decisionsFixture())
  await page.goto('/inbox')
  await expect(decisionCards(page)).toHaveCount(3)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(0)
  await expect(page.getByRole('link', { name: '3 decisions waiting in the Inbox' })).toBeVisible()
})
