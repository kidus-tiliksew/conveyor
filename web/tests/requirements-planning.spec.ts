import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

const requirement = {
  requirement: {
    id: 'req-retries',
    slug: 'retry-behavior',
    title: 'Retry behavior',
    statement_high_water_mark: 1,
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
  },
  pending_versions: [
    {
      requirement_id: 'req-retries',
      version: 1,
      content:
        'Keep retries bounded.\n\n```mermaid\nflowchart LR\n  Attempt --> Limit\n```\n\n```mermaid\nthis is deliberately malformed requirement mermaid\n```\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop after a finite limit.\n```',
      statements: [{ id: 'REQ-1', statement: 'Retries stop after a finite limit.' }],
      origin: 'chat',
      origin_session_id: 'session-requirement',
      confirmed: false,
      workspace: 'demo',
      created_at: '2026-07-30T10:05:00Z',
    },
  ],
  serving_blueprints: [
    {
      task: {
        id: 'blueprint-task',
        title: 'Ship bounded retries',
        state: 'awaiting_human',
      },
      spec: { version: 1, approved: false },
    },
  ],
  planning_sessions: [
    {
      id: 'session-requirement',
      title: 'Plan retry behavior',
      status: 'finalized',
      produced_requirement_id: 'req-retries',
      model: 'gpt-plan',
      effort: 'high',
      exploration_output_tokens: 12000,
      primary_repo: 'conveyor',
      pinned_revisions: { conveyor: '0123456789abcdef' },
      workspace: 'demo',
      created_at: '2026-07-30T10:00:00Z',
      updated_at: '2026-07-30T10:05:00Z',
      finalized_at: '2026-07-30T10:05:00Z',
    },
  ],
  artifacts: [],
  staleness: {
    delivery_after_intent: true,
    deliveries: [
      {
        signal_id: 'signal-blueprint',
        task_id: 'blueprint-task',
        delivery_event_id: 42,
        event_kind: 'merge.reconciled',
        label: 'Blueprint delivery',
        url: 'https://example.test/pull/42',
        at: '2026-07-30T10:05:00Z',
        pinned_version: 1,
        current_version: 2,
        needs_attention: true,
        reasons: ['merged outside factory review'],
      },
      {
        signal_id: 'signal-routine',
        task_id: 'routine-delivery',
        delivery_event_id: 41,
        event_kind: 'merge.confirmed',
        label: 'Routine delivery',
        at: '2026-07-30T09:05:00Z',
        needs_attention: false,
        reasons: [],
      },
    ],
    active_drift: [],
  },
  migrated_seed: false,
  confirmation_eligible: true,
}

// Requirement activity comes from the events route, never the detail
// (component-web-document-surfaces invariant 8).
const requirementEvents = [
  {
    id: 1,
    task_id: '',
    kind: 'requirement.version_proposed',
    actor_id: 'planner',
    actor_role: 'system',
    payload: {} as Record<string, unknown>,
    at: '2026-07-30T10:05:00Z',
  },
]

function documentEventPage(
  events: Array<{ id: number }>,
  options: { total?: number; offset?: number; snapshot?: number } = {},
) {
  return {
    events,
    total: options.total ?? events.length,
    limit: 50,
    offset: options.offset ?? 0,
    snapshot_id: options.snapshot ?? Math.max(0, ...events.map((event) => event.id)),
  }
}

function summarizeRequirement(view: {
  requirement: Record<string, unknown>
  current_version?: Record<string, unknown>
  pending_versions?: unknown[]
  serving_tasks?: unknown[]
  staleness?: {
    delivery_after_intent?: boolean
    partial_evaluation?: boolean
    deliveries?: Array<Record<string, unknown> & { needs_attention: boolean }>
    active_drift?: unknown[]
  }
  confirmation_eligible?: boolean
}) {
  return {
    requirement: view.requirement,
    current_version: view.current_version
      ? {
          requirement_id: view.current_version.requirement_id,
          version: view.current_version.version,
          origin: view.current_version.origin,
          confirmed: view.current_version.confirmed,
          confirmed_by: view.current_version.confirmed_by,
          confirmed_at: view.current_version.confirmed_at,
          created_at: view.current_version.created_at,
        }
      : undefined,
    pending_version_count: view.pending_versions?.length ?? 0,
    serving_tasks: view.serving_tasks ?? [],
    staleness: {
      delivery_after_intent: view.staleness?.delivery_after_intent ?? false,
      partial_evaluation: view.staleness?.partial_evaluation ?? false,
      deliveries: view.staleness?.deliveries ?? [],
      active_drift: view.staleness?.active_drift ?? [],
    },
    confirmation_eligible: view.confirmation_eligible,
  }
}

const planningConfig = {
  version: 1,
  document: {
    workspace: 'demo',
    max_bounces: 3,
    work_order_queue_timeout: '24h',
    stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
    review: { seats: [{}] },
    execution: {
      default_plan_state: 'needed',
      merge_approval: true,
      implement_concurrency: 1,
      review_concurrency: 1,
      first_activity_timeout: '2m',
    },
    repos: [
      {
        name: 'conveyor',
        url: 'https://github.com/kidus-tiliksew/conveyor',
        github: 'kidus-tiliksew/conveyor',
        base: 'main',
      },
      {
        name: 'companion',
        url: 'https://github.com/example/companion',
        github: 'example/companion',
        base: 'main',
      },
    ],
  },
}

async function initShell(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
}

async function expectStructuralCorrection(page: Page, text: string) {
  const correction = page.getByText(text)
  await expect(correction).toBeVisible()
  const row = correction.locator('xpath=../../..')
  await expect(row).toHaveClass(/justify-center/)
  await expect(row.locator('svg')).toHaveCount(0)
}

function shellResponse(route: Route) {
  const path = new URL(route.request().url()).pathname
  if (path === '/v1/workspaces')
    return route.fulfill({
      json: [{ id: 'demo', name: 'Demo', config_version: 1 }],
    })
  if (path === '/v1/me')
    return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
  if (path === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
  if (path === '/v1/activity') return route.fulfill({ json: [] })
}

test('requirements renders a document tree, one attention surface, and confirms from the canvas', async ({ page }) => {
  await initShell(page)
  let confirmed = false
  let releaseDetail: () => void = () => {}
  const detailReady = new Promise<void>((resolve) => {
    releaseDetail = resolve
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(requirement)] })
    if (url.pathname === '/v1/requirements/req-retries') {
      await detailReady
      return route.fulfill({ json: requirement })
    }
    if (url.pathname === '/v1/requirements/req-retries/events')
      return route.fulfill({ json: documentEventPage(requirementEvents) })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/requirements/req-retries/versions/1/confirm') {
      confirmed = true
      return route.fulfill({
        json: {
          requirement: requirement.requirement,
          version: requirement.pending_versions[0],
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements')
  await expect(page.getByRole('heading', { level: 1, name: 'Requirements' })).toBeVisible()
  await expect(page.getByText('Loading requirement…')).toBeVisible()
  releaseDetail()

  // AC-2.1: the tree groups the corpus beside the canvas, and the selected
  // document is the hero.
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  await expect(tree.getByRole('heading', { name: 'Product overviews' })).toBeVisible()
  await expect(tree.getByRole('heading', { name: 'Requirements' })).toBeVisible()
  await expect(tree.getByRole('button', { name: /Retry behavior/ })).toHaveAttribute('aria-current', 'true')
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await expect(canvas.locator('[data-mermaid] svg')).toBeVisible()
  await expect(canvas.locator('code.language-mermaid')).toContainText(
    'this is deliberately malformed requirement mermaid',
  )
  await expect(page.getByText('Feature tree')).toHaveCount(0)

  // AC-1.3: suspect staleness names its causal delivery and plain-language
  // reason; the pending version remains inside the one attention surface.
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText('Blueprint delivery may have moved past the confirmed intent')
  await expect(attention.getByTitle(/Blueprint delivery delivered/)).toContainText('merged outside factory review')
  await expect(attention).toContainText('Served v1 · current at delivery v2')
  await expect(attention.getByRole('link', { name: 'Inspect task' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/blueprint-task',
  )
  await expect(attention.getByRole('link', { name: /Open PR/ })).toHaveAttribute('href', 'https://example.test/pull/42')
  await expect(attention.getByRole('button', { name: 'File a task' })).toBeVisible()
  const stalenessItem = attention
    .getByText('Blueprint delivery may have moved past the confirmed intent')
    .locator('xpath=ancestor::li')
  await expect(stalenessItem.getByRole('button', { name: 'Dismiss' })).toBeVisible()
  await expect(attention).toContainText('Version 1 is waiting for you')

  // AC-1.2: a routine factory-reviewed delivery remains visible as neutral
  // activity and is absent from the attention surface.
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  const deliveryActivity = page.getByRole('region', { name: 'Delivery activity' })
  await expect(deliveryActivity.getByRole('link', { name: 'Routine delivery' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/routine-delivery',
  )
  await expect(attention).not.toContainText('Routine delivery')

  // Detailed signal labels and actions remain confined to the canvas; the
  // approved tree affordance carries only a compact aggregate.
  await expect(page.getByText('Needs confirmation')).toHaveCount(0)
  await expect(page.getByText('Revision pending')).toHaveCount(0)
  await expect(page.getByText('Code ahead of intent')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Requirement alignment' })).toHaveCount(0)
  await expect(tree.getByText('confirmation')).toHaveCount(0)
  await page.getByRole('button', { name: 'Review changes · v1', exact: true }).click()
  await expect(attention.getByRole('button', { name: 'Revise', exact: true })).toBeVisible()
  // AC-2.2: the assistant column is withdrawn from this surface.
  await expect(page.getByRole('complementary', { name: 'Planning assistant' })).toHaveCount(0)
  for (const action of ['Draft', 'Q&A', 'Plan work', 'New requirement']) {
    await expect(page.getByRole('button', { name: action })).toHaveCount(0)
  }

  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute(
    'href',
    '/w/demo/blueprints/blueprint-task',
  )
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()
  // The canvas offers the explorer rather than an inline graph card: one
  // rendering of lineage, opened on demand.
  await expect(page.getByRole('button', { name: 'Knowledge explorer' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Intent to delivery' })).toHaveCount(0)

  await page.getByRole('button', { name: 'Review changes · v1', exact: true }).click()
  await attention.getByRole('button', { name: 'Confirm version 1' }).click()
  await expect.poll(() => confirmed).toBe(true)
  await expect(page).toHaveURL(/\/requirements/)
})

test('requirements archive and restore from the canvas while preserving archived deep links', async ({ page }) => {
  await initShell(page)
  let archived = false
  let archiveCalls = 0
  let restoreCalls = 0
  const archivedAt = '2026-09-02T18:00:00Z'
  const view = () => ({
    ...requirement,
    requirement: {
      ...requirement.requirement,
      archived,
      archived_by: archived ? 'Operator One' : undefined,
      archived_at: archived ? archivedAt : undefined,
    },
  })

  await page.route('**/v1/**', async (route) => {
    const handled = shellResponse(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/requirements') {
      if (url.searchParams.has('include_archived')) expect(url.searchParams.get('include_archived')).toBe('true')
      return route.fulfill({ json: [summarizeRequirement(view())] })
    }
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/requirements/req-retries/archive' && request.method() === 'POST') {
      archived = true
      archiveCalls++
      return route.fulfill({ json: view().requirement })
    }
    if (url.pathname === '/v1/requirements/req-retries/restore' && request.method() === 'POST') {
      archived = false
      restoreCalls++
      return route.fulfill({ json: view().requirement })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements?requirement=req-retries')
  await page.getByRole('button', { name: 'Archive' }).click()
  await page.getByRole('dialog', { name: 'Archive Retry behavior' }).getByRole('button', { name: 'Archive' }).click()
  await expect.poll(() => archiveCalls).toBe(1)
  await expect(page.getByText('This requirement is archived.')).toBeVisible()
  const archivedGroup = page.getByRole('navigation', { name: 'Document tree' }).locator('details')
  await expect(archivedGroup).not.toHaveAttribute('open', '')
  await archivedGroup.locator('summary').click()
  await expect(archivedGroup.getByRole('button', { name: 'Retry behavior' })).toBeVisible()
  await expect(page.getByText('Archived', { exact: true }).last()).toHaveAttribute(
    'title',
    /Archived by Operator One on/,
  )
  await expect(page).toHaveURL(/requirement=req-retries/)
  await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
  await page.getByRole('button', { name: 'Restore' }).click()
  await expect.poll(() => restoreCalls).toBe(1)
  await expect(page.getByRole('button', { name: 'Archive' })).toBeVisible()
  await expect(page.getByText('This requirement is archived.')).toHaveCount(0)
  await expect(page.getByRole('navigation', { name: 'Document tree' }).locator('details')).toHaveCount(0)
})

test('requirements proposes an attributed drift amendment inline and keeps the drift open', async ({ page }) => {
  await initShell(page)
  const confirmedVersion = {
    ...requirement.pending_versions[0],
    confirmed: true,
    origin: 'operator',
  }
  const drift = {
    id: 'requirement-drift-1',
    workspace_id: 'demo',
    repository: 'conveyor',
    kind: 'external_pr_merge',
    source_url: 'https://example.test/pr/51',
    task_id: '260810-drift',
    requirement_id: 'req-retries',
    matching_paths: ['internal/retry/retry.go'],
    detected_at: '2026-08-10T12:00:00Z',
  }
  const amendment = {
    ...confirmedVersion,
    version: 2,
    content: 'Keep retries bounded and record delivery drift.',
    statements: [{ id: 'REQ-1', statement: 'Retries stop after a finite limit and record delivery drift.' }],
    origin: 'drift_amendment',
    origin_session_id: undefined,
    confirmed: false,
    created_at: '2026-08-10T12:01:00Z',
  }
  let resolved = false
  let resolution: Record<string, string> | undefined
  const view = () => ({
    ...requirement,
    current_version: confirmedVersion,
    pending_versions: resolved ? [amendment] : [],
    // requirements_amended proposes the amendment and leaves the drift open
    // until the operator confirms that version (DEC-46).
    staleness: {
      delivery_after_intent: false,
      active_drift: [drift],
    },
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view())] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: resolved ? [confirmedVersion, amendment] : [confirmedVersion] })
    if (url.pathname === '/v1/monitor/drift/requirement-drift-1/resolve') {
      resolution = request.postDataJSON() as Record<string, string>
      resolved = true
      return route.fulfill({ json: drift })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  const form = attention.getByRole('form', { name: 'Resolve drift requirement-drift-1' })
  const outcomes = form.getByLabel('Resolution outcome for requirement-drift-1')
  await expect(outcomes.getByRole('option', { name: 'Design document updated' })).toHaveCount(0)
  await outcomes.selectOption('requirements_amended')
  await expect(form.getByLabel('Confirmed requirement for requirement-drift-1')).toHaveCount(0)
  await form.getByRole('button', { name: 'Resolve' }).click()

  await expect.poll(() => resolution).toEqual({ outcome: 'requirements_amended', requirement_id: 'req-retries' })
  await expect(attention).toContainText('Version 2 is waiting for you')
  await expect(attention).toContainText('Code changed in conveyor without reaching this document')
  await expect(attention).toContainText('Written from a delivery change')
})

test('finalize immediately reveals a complete bundle preview, approval, and created tasks', async ({ page }) => {
  await initShell(page)
  let finalized = false
  let approved = false
  let bundleReads = 0
  const session = () => ({
    id: 'session-bundle',
    title: finalized ? 'Ship safe retries' : 'Planning delivery · bundle',
    status: finalized ? ('finalized' as const) : ('active' as const),
    goal: 'bundle' as const,
    produced_bundle_id: finalized ? 'bundle-1' : undefined,
    workspace: 'demo',
    created_at: '2026-08-06T10:00:00Z',
    updated_at: '2026-08-06T10:05:00Z',
  })
  const bundle = () => ({
    id: 'bundle-1',
    session_id: 'session-bundle',
    title: 'Ship safe retries',
    status: approved ? ('approved' as const) : ('pending' as const),
    documents: [{ kind: 'requirement', id: 'req-retries', version: 2, title: 'Retry behavior' }],
    tasks: [
      {
        member_id: 'task-a',
        created_task_id: approved ? 'task-created' : '',
        title: 'Implement bounded retries',
        body: 'Keep retries finite and visible.',
        repo: 'conveyor',
        depends_on: ['task-prerequisite'],
        context: { requirement_ids: ['req-retries'], system_design_ids: ['design-runtime'] },
      },
    ],
    workspace: 'demo',
    created_at: '2026-08-06T10:05:00Z',
  })
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    expect(url.searchParams.get('workspace_id') ?? 'demo').toBe('demo')
    if (url.pathname !== '/v1/workspaces') expect(route.request().headers().authorization).toBeUndefined()
    if (url.pathname === '/v1/workspaces')
      return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (url.pathname === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
    if (url.pathname === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
    if (url.pathname === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (url.pathname === '/v1/requirements' || url.pathname === '/v1/blueprints') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session()] })
    if (url.pathname === '/v1/planning-sessions/session-bundle') return route.fulfill({ json: session() })
    if (url.pathname === '/v1/planning-sessions/session-bundle/messages' && route.request().method() === 'GET')
      return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions/session-bundle/messages') {
      finalized = true
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: 'data: {"type":"tool-output-available","toolCallId":"call-1","toolName":"finalize_bundle"}\n\n',
      })
    }
    if (url.pathname === '/v1/planning-bundles') {
      bundleReads++
      return route.fulfill({ json: finalized ? [bundle()] : [] })
    }
    if (url.pathname === '/v1/planning-bundles/bundle-1/approve') {
      approved = true
      return route.fulfill({ json: bundle() })
    }
    if (url.pathname === '/v1/activity')
      return route.fulfill({
        json: approved
          ? [
              {
                task: {
                  id: 'task-created',
                  workspace: 'demo',
                  source: 'planning:session-bundle',
                  title: 'Implement bounded retries',
                  body: 'Keep retries finite and visible.',
                  class: 'feature',
                  repo: 'conveyor',
                  base_branch: 'main',
                  branch: 'conveyor/task-created',
                  state: 'queued',
                  next_stage: 'triage',
                  plan_state: 'needed',
                  merge_approval: false,
                  policy_version: 1,
                  setup: 'default',
                  setup_contract: {},
                  created_at: '2026-08-06T10:06:00Z',
                },
                latest_stage: 'triage',
                last_event_at: '2026-08-06T10:06:00Z',
                needs_attention: false,
              },
            ]
          : [],
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/planning')
  await page.getByLabel('Planning message').fill('Finalize this delivery bundle.')
  await page.getByRole('button', { name: 'Send' }).click()
  const preview = page.getByRole('region', { name: 'Planning bundle preview' })
  await expect(preview).toBeVisible()
  await expect(preview.getByText('Retry behavior')).toBeVisible()
  await expect(preview.getByText('Implement bounded retries')).toBeVisible()
  await expect(preview.getByText(/depends on: task-prerequisite/)).toBeVisible()
  await expect(preview.getByText(/req-retries, design-runtime/)).toBeVisible()
  await expect.poll(() => bundleReads).toBeGreaterThan(1)
  await preview.getByRole('button', { name: 'Approve task set' }).click()
  await expect.poll(() => approved).toBe(true)
  await page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'Tasks', exact: true }).click()
  await expect(page.getByText('Implement bounded retries')).toBeVisible()
})

test('a viewer can read planning and a pending bundle without mutation affordances', async ({ page }) => {
  await initShell(page)
  let mutationRequests = 0
  const readOnlySession = {
    id: 'session-viewer',
    title: 'Review safe retries',
    status: 'active' as const,
    goal: 'bundle' as const,
    produced_bundle_id: 'bundle-viewer',
    workspace: 'demo',
    created_at: '2026-08-06T10:00:00Z',
    updated_at: '2026-08-06T10:05:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.method() !== 'GET') mutationRequests++
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: callerIdentity({ id: 'usr_viewer', role: 'viewer' }, route.request()) })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
    if (path === '/v1/activity') return route.fulfill({ json: [] })
    if (path === '/v1/planning-sessions') return route.fulfill({ json: [readOnlySession] })
    if (path === '/v1/planning-sessions/session-viewer') return route.fulfill({ json: readOnlySession })
    if (path === '/v1/planning-sessions/session-viewer/messages')
      return route.fulfill({
        json: [
          {
            session_id: 'session-viewer',
            seq: 1,
            role: 'assistant',
            content: 'Readable plan',
            workspace: 'demo',
            created_at: '2026-08-06T10:01:00Z',
          },
        ],
      })
    if (path === '/v1/planning-bundles')
      return route.fulfill({
        json: [
          {
            id: 'bundle-viewer',
            session_id: 'session-viewer',
            title: 'Safe retry delivery',
            status: 'pending',
            documents: [{ kind: 'requirement', id: 'req-retries', version: 2, title: 'Retry behavior' }],
            tasks: [
              {
                member_id: 'task-a',
                title: 'Implement retries',
                body: 'Keep retries finite.',
                repo: 'conveyor',
                depends_on: [],
                context: {},
              },
            ],
            workspace: 'demo',
            created_at: '2026-08-06T10:05:00Z',
          },
        ],
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/planning')
  await expect(page.getByRole('heading', { name: 'Planning' })).toBeVisible()
  await expect(page.getByText('Readable plan')).toBeVisible()
  const preview = page.getByRole('region', { name: 'Planning bundle preview' })
  await expect(preview.getByText('Safe retry delivery')).toBeVisible()
  await expect(page.getByRole('button', { name: 'New session' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Abandon' })).toHaveCount(0)
  await expect(page.getByLabel('Planning message')).toHaveCount(0)
  await expect(preview.getByRole('button', { name: 'Reject' })).toHaveCount(0)
  await expect(preview.getByRole('button', { name: 'Approve task set' })).toHaveCount(0)
  expect(mutationRequests).toBe(0)
})

test('planning restores durable messages, tool markers, and streams a new turn', async ({ page }) => {
  await initShell(page)
  await page.addInitScript(() => {
    const originalFetch = window.fetch.bind(window)
    window.fetch = async (input, init) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      if (url.includes('/v1/planning-sessions/session-retries/messages') && init?.method === 'POST') {
        ;(window as Window & { __planningPosted?: string }).__planningPosted = String(init.body ?? '')
        const encoder = new TextEncoder()
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(
              encoder.encode(
                [
                  'data: {"type":"start"}',
                  '',
                  'data: {"type":"text-delta","delta":"Drafting the requirement."}',
                  '',
                  'data: {"type":"system-correction","text":"Live correction row","detail":"live correction detail"}',
                  '',
                  'data: {"type":"text-delta","delta":" Continuing safely."}',
                  '',
                ].join('\n'),
              ),
            )
            window.setTimeout(() => {
              controller.enqueue(encoder.encode('data: {"type":"finish"}\n\ndata: [DONE]\n\n'))
              controller.close()
            }, 1_500)
          },
        })
        return new Response(stream, {
          status: 200,
          headers: {
            'Content-Type': 'text/event-stream',
            'X-Vercel-AI-UI-Message-Stream': 'v1',
          },
        })
      }
      return originalFetch(input, init)
    }
  })
  const session = {
    id: 'session-retries',
    title: 'Plan retry behavior',
    status: 'active',
    model: 'gpt-plan',
    effort: 'high',
    exploration_output_tokens: 12000,
    primary_repo: 'conveyor',
    pinned_revisions: { conveyor: '0123456789abcdef' },
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:10:00Z',
  }
  const messages = [
    {
      session_id: session.id,
      seq: 1,
      role: 'user',
      content: 'Plan bounded retries.',
      workspace: 'demo',
      created_at: '2026-07-30T10:01:00Z',
    },
    {
      session_id: session.id,
      seq: 2,
      role: 'assistant',
      content: 'I found the approved queue contract.',
      parts: [
        {
          type: 'tool-input-available',
          toolName: 'read_approved_spec',
          toolCallId: 'call-1',
          input: { task_id: 'task-1' },
        },
      ],
      workspace: 'demo',
      created_at: '2026-07-30T10:02:00Z',
    },
    {
      session_id: session.id,
      seq: 3,
      role: 'tool',
      content: '',
      parts: [
        {
          type: 'tool-output-available',
          toolCallId: 'call-1',
          output: { title: 'Queue contract' },
        },
      ],
      workspace: 'demo',
      created_at: '2026-07-30T10:02:01Z',
    },
    {
      session_id: session.id,
      seq: 4,
      role: 'system',
      content: 'planning_step parse detail',
      parts: [
        {
          type: 'system-correction',
          text: "The assistant's response needed correction — retrying.",
          detail: 'planning_step parse detail',
        },
      ],
      workspace: 'demo',
      created_at: '2026-07-30T10:02:02Z',
    },
    {
      session_id: session.id,
      seq: 5,
      role: 'assistant',
      content: '',
      parts: [
        {
          type: 'tool-input-available',
          toolName: 'finalize_blueprint',
          toolCallId: 'call-corrected',
        },
        {
          type: 'tool-input-available',
          toolName: 'read_artifact',
          toolCallId: 'call-deferred',
        },
        {
          type: 'tool-input-available',
          toolName: 'read_requirement',
          toolCallId: 'call-failed',
        },
        {
          type: 'tool-input-available',
          toolName: 'read_cancelled',
          toolCallId: 'call-cancelled',
        },
      ],
      workspace: 'demo',
      created_at: '2026-07-30T10:02:03Z',
    },
    {
      session_id: session.id,
      seq: 6,
      role: 'tool',
      content: '',
      parts: [
        {
          type: 'tool-output-error',
          toolCallId: 'call-corrected',
          output: { status: 'invalid' },
        },
        {
          type: 'tool-output-error',
          toolCallId: 'call-deferred',
          output: { status: 'deferred' },
        },
        {
          type: 'tool-output-error',
          toolCallId: 'call-failed',
          output: { status: 'failed' },
        },
        {
          type: 'tool-output-error',
          toolCallId: 'call-cancelled',
          output: { status: 'cancelled' },
        },
      ],
      workspace: 'demo',
      created_at: '2026-07-30T10:02:04Z',
    },
  ]
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}/messages` && route.request().method() === 'GET') {
      return route.fulfill({ json: messages })
    }
    if (url.pathname === `/v1/planning-sessions/${session.id}/messages`) {
      return route.fulfill({
        status: 200,
        headers: {
          'Content-Type': 'text/event-stream',
          'X-Vercel-AI-UI-Message-Stream': 'v1',
        },
        body: [
          'data: {"type":"start"}',
          '',
          'data: {"type":"text-delta","delta":"Drafting the requirement."}',
          '',
          'data: {"type":"system-correction","text":"Live correction row","detail":"live correction detail"}',
          '',
          'data: {"type":"text-delta","delta":" Continuing safely."}',
          '',
          'data: {"type":"finish"}',
          '',
          'data: [DONE]',
          '',
          '',
        ].join('\n'),
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/planning')
  await expect(page.getByText('Plan bounded retries.')).toBeVisible()
  await expect(page.getByText('I found the approved queue contract.')).toBeVisible()
  await expect(page.getByText('read_approved_spec')).toBeVisible()
  await expectStructuralCorrection(page, "The assistant's response needed correction — retrying.")
  await page.getByText('Technical details').click()
  await expect(page.getByText('planning_step parse detail')).toBeVisible()
  await expect(page.getByText('finalize_blueprint corrected')).toBeVisible()
  await expect(page.getByText('read_artifact deferred')).toBeVisible()
  await expect(page.getByText('read_requirement failed')).toBeVisible()
  await expect(page.getByText('read_cancelled cancelled')).toBeVisible()
  await expect(page.getByLabel('read_approved_spec: complete')).toHaveCount(1)
  await expect(page.getByText(/Queue contract/)).toHaveCount(0)
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()

  await page.getByLabel('Planning message').fill('Draft a requirement with stable statements.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expectStructuralCorrection(page, 'Live correction row')
  await expect
    .poll(() =>
      page.evaluate(
        () => JSON.parse((window as Window & { __planningPosted?: string }).__planningPosted ?? '{}').message?.content,
      ),
    )
    .toBe('Draft a requirement with stable statements.')
})

test('requirements deep-link exact versions, render statements, diff pending intent, and guard confirmation', async ({
  page,
}) => {
  await initShell(page)
  const confirmed = {
    ...requirement.pending_versions[0],
    version: 1,
    confirmed: true,
    confirmed_at: '2026-07-30T10:06:00Z',
    content: 'Keep retries bounded.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop.\n```',
    statements: [{ id: 'REQ-1', statement: 'Retries stop.' }],
  }
  const pending2 = {
    ...requirement.pending_versions[0],
    version: 2,
    origin: 'operator',
    origin_session_id: undefined,
    content:
      'Keep retries bounded and observable.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop after a finite limit.\n```',
  }
  const pending3 = {
    ...pending2,
    version: 3,
    origin: 'drift_amendment',
    origin_session_id: undefined,
    origin_drift_id: 'drift-1',
  }
  const view = {
    ...requirement,
    current_version: confirmed,
    pending_versions: [pending2, pending3],
    migrated_seed: false,
    confirmation_eligible: true,
  }
  let ifMatch = ''
  let confirmedVersion = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view)] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: [confirmed, pending2, pending3] })
    if (url.pathname === '/v1/requirements/req-retries/events')
      return route.fulfill({ json: documentEventPage(requirementEvents) })
    const confirmation = /\/versions\/(\d+)\/confirm$/.exec(url.pathname)
    if (confirmation) {
      confirmedVersion = Number(confirmation[1])
      ifMatch = route.request().headers()['if-match'] ?? ''
      return route.fulfill({
        json: { requirement: view.requirement, version: pending2 },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements?requirement=req-retries&tab=changes&target=2')
  await expect(page.getByLabel('Target version', { exact: true })).toHaveValue('2')
  await expect(page.getByLabel('Base version', { exact: true })).toHaveValue('1')
  await expect(page.getByText('Written by an operator').first()).toBeVisible()
  const diff = page.getByRole('region', { name: 'Version comparison', exact: true })
  await expect(diff.locator('ins').filter({ hasText: 'and observable' })).toBeVisible()
  await page.getByRole('tab', { name: 'document', exact: true }).click()
  await expect(
    page.getByRole('region', { name: 'Requirement statements' }).getByRole('link', { name: 'Link to REQ-1' }),
  ).toBeVisible()
  await expect(page.getByText('conveyor:requirements')).toHaveCount(0)
  await page.getByRole('tab', { name: 'history', exact: true }).click()
  await page.getByRole('button', { name: 'Compare version 2', exact: true }).click()
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention.getByRole('button', { name: /^Confirm version/ })).toHaveCount(1)
  await attention.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect.poll(() => confirmedVersion).toBe(2)
  expect(ifMatch).toBe('"1"')
})

test('requirement confirmation explains superseded versions separately from If-Match races', async ({ page }) => {
  await initShell(page)
  const current = {
    ...requirement.pending_versions[0],
    confirmed: true,
    confirmed_at: '2026-07-30T10:06:00Z',
  }
  const pending2 = { ...requirement.pending_versions[0], version: 2 }
  const pending3 = { ...requirement.pending_versions[0], version: 3 }
  const view = {
    ...requirement,
    current_version: current,
    pending_versions: [pending2, pending3],
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view)] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: [current, pending2, pending3] })
    if (url.pathname.endsWith('/versions/2/confirm'))
      return route.fulfill({
        status: 409,
        json: {
          error: 'requirement_version_superseded',
          message: 'requirement req-retries version 2 was superseded by newer confirmed version 4',
        },
      })
    if (url.pathname.endsWith('/versions/3/confirm'))
      return route.fulfill({
        status: 409,
        json: { error: 'requirement_current_version_mismatch', message: 'stale If-Match' },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await attention.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect(attention).toContainText(
    'This requirement version was superseded by a newer confirmed version and can no longer be confirmed.',
  )
  await attention.getByRole('button', { name: 'Confirm version 3' }).click()
  await expect(attention).toContainText(
    'This requirement changed while you were reviewing it. Refresh and choose the version again.',
  )
})

test('requirements and planning render fetch failures instead of indefinite loading', async ({ page }) => {
  await initShell(page)
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements')
      return route.fulfill({
        status: 500,
        body: 'Requirement service is unavailable.',
      })
    if (path === '/v1/planning-sessions')
      return route.fulfill({
        status: 500,
        body: 'Planning history is unavailable.',
      })
    return route.fulfill({ json: [] })
  })
  await page.goto('/w/demo/requirements')
  await expect(page.getByText('Requirement service is unavailable.')).toBeVisible()
  await page.goto('/w/demo/planning')
  await expect(page.getByText('Planning history is unavailable.')).toBeVisible()
  await expect(page.getByText('Restoring sessions…')).toHaveCount(0)
})

test('reference documents upload safely, load history on demand, compare both sides, and confirm deletion', async ({
  page,
}) => {
  await initShell(page)
  let versionRequests = 0
  let listAuthorization = ''
  let versionAuthorization = ''
  let uploadRequests = 0
  let deleteRequests = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'GET') {
      listAuthorization = route.request().headers().authorization ?? ''
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      return route.fulfill({
        json: [{ id: 'ref-overview', name: 'Product overview', current_version: 2, workspace: 'demo' }],
      })
    }
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'POST') {
      uploadRequests++
      if (uploadRequests === 2)
        return route.fulfill({ status: 400, body: 'reference document content is not Markdown' })
      return route.fulfill({ status: 201, json: {} })
    }
    if (url.pathname === '/v1/reference-documents/ref-overview/versions') {
      versionRequests++
      versionAuthorization = route.request().headers().authorization ?? ''
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      return route.fulfill({
        json: [
          {
            document_id: 'ref-overview',
            version: 1,
            filename: 'overview.md',
            content_type: 'text/markdown',
            content: '# Overview\n\nKeep this line.\n\nRemoved section.',
            workspace: 'demo',
          },
          {
            document_id: 'ref-overview',
            version: 2,
            filename: 'overview.md',
            content_type: 'text/plain',
            content: '# Overview\n\nKeep this line.\n\nAdded section.',
            supersedes_version: 1,
            workspace: 'demo',
          },
        ],
      })
    }
    if (url.pathname === '/v1/reference-documents/ref-overview' && route.request().method() === 'DELETE') {
      deleteRequests++
      return route.fulfill({ status: 204 })
    }
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/w/demo/requirements')
  // AC-2.1: product overviews are their own group in the tree, and opening one
  // makes it the canvas — the version history it needs loads only then.
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  const overviewItem = tree.getByRole('button', { name: /Product overview/ })
  await expect(overviewItem).toBeVisible()
  expect(listAuthorization).toBe('')
  expect(versionRequests).toBe(0)

  const addInput = tree.locator('label').filter({ hasText: 'Add Markdown' }).locator('input[type=file]')
  await addInput.setInputFiles({ name: 'new.md', mimeType: 'application/octet-stream', buffer: Buffer.from('# New') })
  await expect.poll(() => uploadRequests).toBe(1)
  await addInput.setInputFiles({ name: 'bad.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-1.7') })
  await expect(page.getByText('reference document content is not Markdown')).toBeVisible()

  await overviewItem.click()
  await expect.poll(() => versionRequests).toBe(1)
  expect(versionAuthorization).toBe('')
  await expect(page.getByRole('heading', { level: 2, name: 'Product overview', exact: true })).toBeVisible()
  await page.getByLabel('Read version').selectOption('1')
  await expect(page.getByText('Removed section.')).toBeVisible()
  await page.getByLabel('Read version').selectOption('2')
  const comparison = page.locator('details').filter({ hasText: 'Compared with v1' })
  await comparison.getByText('Compared with v1').click()
  await expect(comparison.locator('span').filter({ hasText: 'Removed section.' })).toBeVisible()
  await expect(comparison.locator('span').filter({ hasText: 'Added section.' })).toBeVisible()

  page.once('dialog', async (dialog) => {
    expect(dialog.message()).toContain('Delete Product overview?')
    await dialog.accept()
  })
  await page.getByRole('button', { name: 'Delete' }).click()
  await expect.poll(() => deleteRequests).toBe(1)
})
