import { expect, type Page, type Route, test } from '@playwright/test'

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
      spec: { task_id: 'blueprint-task', version: 1, approved: false },
      events: [],
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
  lineage: [
    {
      id: 1,
      task_id: '',
      kind: 'requirement.version_proposed',
      actor_id: 'planner',
      actor_role: 'system',
      payload: {},
      at: '2026-07-30T10:05:00Z',
    },
  ],
  lineage_graph: {
    roots: [{ type: 'requirement', id: 'req-retries', label: 'Retry behavior' }],
    nodes: [
      { type: 'requirement', id: 'req-retries', label: 'Retry behavior' },
      { type: 'blueprint', id: 'blueprint-task', label: 'Ship bounded retries' },
    ],
    links: [
      {
        workspace: 'demo',
        src_type: 'requirement',
        src_id: 'req-retries',
        dst_type: 'blueprint',
        dst_id: 'blueprint-task',
        kind: 'serves',
        created_by_event_id: 2,
        created_at: '2026-07-30T10:05:00Z',
      },
    ],
    truncated: false,
    omitted_nodes: 0,
    omitted_links: 0,
    budget: { max_depth: 5, max_nodes: 256, max_links: 1024 },
  },
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
      spec_approval: true,
      merge_approval: true,
      require_verification_evidence: false,
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
  if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
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

  await page.goto('/requirements')
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
  await expect(attention.getByRole('link', { name: 'Inspect task' })).toHaveAttribute('href', '/tasks/blueprint-task')
  await expect(attention.getByRole('link', { name: /Open PR/ })).toHaveAttribute('href', 'https://example.test/pull/42')
  await expect(attention.getByRole('button', { name: 'File a task' })).toBeVisible()
  const stalenessItem = attention
    .getByText('Blueprint delivery may have moved past the confirmed intent')
    .locator('xpath=ancestor::li')
  await expect(stalenessItem.getByRole('button', { name: 'Dismiss' })).toBeVisible()
  await expect(attention).toContainText('Version 1 is waiting for you')

  // AC-1.2: a routine factory-reviewed delivery remains visible as neutral
  // activity and is absent from the attention surface.
  const deliveryActivity = page.getByRole('region', { name: 'Delivery activity' })
  await expect(deliveryActivity.getByRole('link', { name: 'Routine delivery' })).toHaveAttribute(
    'href',
    '/tasks/routine-delivery',
  )
  await expect(attention).not.toContainText('Routine delivery')

  // Detailed signal labels and actions remain confined to the canvas; the
  // approved tree affordance carries only a compact aggregate.
  await expect(page.getByText('Needs confirmation')).toHaveCount(0)
  await expect(page.getByText('Revision pending')).toHaveCount(0)
  await expect(page.getByText('Code ahead of intent')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Requirement alignment' })).toHaveCount(0)
  await expect(tree.getByText('confirmation')).toHaveCount(0)
  // AC-2.2: the assistant column is withdrawn from this surface.
  await expect(page.getByRole('complementary', { name: 'Planning assistant' })).toHaveCount(0)
  for (const action of ['Draft', 'Revise', 'Q&A', 'Plan work', 'New requirement']) {
    await expect(page.getByRole('button', { name: action })).toHaveCount(0)
  }

  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute(
    'href',
    '/blueprints/blueprint-task',
  )
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()
  // The canvas offers the explorer rather than an inline graph card: one
  // rendering of lineage, opened on demand.
  await expect(page.getByRole('button', { name: 'Knowledge explorer' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Intent to delivery' })).toHaveCount(0)

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
    lineage: archived
      ? [
          ...requirement.lineage,
          {
            id: 2,
            task_id: '',
            kind: 'requirement.archived',
            actor_id: 'Operator One',
            actor_role: 'human',
            payload: {},
            at: archivedAt,
          },
        ]
      : requirement.lineage,
  })

  await page.route('**/v1/**', async (route) => {
    const handled = shellResponse(route)
    if (handled) return await handled
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/requirements') {
      expect(url.searchParams.get('include_archived')).toBe('true')
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

  await page.goto('/requirements?requirement=req-retries')
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Archive' }).click()
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

test('requirement archive controls are absent without document confirmation capability', async ({ page }) => {
  await initShell(page)
  let archived = false
  const view = () => ({
    ...requirement,
    requirement: {
      ...requirement.requirement,
      archived,
      archived_by: archived ? 'Operator One' : undefined,
      archived_at: archived ? '2026-09-02T18:00:00Z' : undefined,
    },
  })
  await page.route('**/v1/**', async (route) => {
    if (new URL(route.request().url()).pathname === '/v1/me')
      return route.fulfill({ json: { id: 'usr_contributor', role: 'contributor' } })
    const handled = shellResponse(route)
    if (handled) return await handled
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view())] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    return route.fulfill({ json: [] })
  })
  await page.goto('/requirements?requirement=req-retries')
  await expect(page.getByRole('button', { name: 'Archive' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Restore' })).toHaveCount(0)
  archived = true
  await page.reload()
  await expect(page.getByText('This requirement is archived.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Archive' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Restore' })).toHaveCount(0)
})

test('requirement dismissal waits for dialog confirmation and preserves cancel', async ({ page }) => {
  await initShell(page)
  let dismissed = false
  let dismissRequests = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    const view = dismissed ? { ...requirement, pending_versions: [] } : requirement
    if (path === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view)] })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: view })
    if (path === '/v1/requirements/req-retries/versions')
      return route.fulfill({
        json: dismissed ? [{ ...requirement.pending_versions[0], retired: true }] : requirement.pending_versions,
      })
    if (path === '/v1/requirements/req-retries/versions/1/dismiss') {
      dismissRequests++
      dismissed = true
      return route.fulfill({ json: { requirement: requirement.requirement, version: requirement.pending_versions[0] } })
    }
    if (path === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  const versionItem = attention.getByText('Version 1 is waiting for you').locator('xpath=ancestor::li')
  await versionItem.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('dialog', { name: 'Dismiss version 1 of Retry behavior' })).toBeVisible()
  expect(dismissRequests).toBe(0)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(versionItem).toBeVisible()
  expect(dismissRequests).toBe(0)
  await versionItem.getByRole('button', { name: 'Dismiss' }).click()
  await page.getByRole('button', { name: 'Dismiss version 1' }).click()
  await expect.poll(() => dismissRequests).toBe(1)
  await expect(attention.getByText('Version 1 is waiting for you')).toHaveCount(0)
})

test('viewer can read requirement detail without the Attach context control', async ({ page }) => {
  await initShell(page)
  await page.route('**/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_viewer', role: 'viewer' } })
    const shell = shellResponse(route)
    if (shell) return await shell
    if (path === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(requirement)] })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (path === '/v1/requirements/req-retries/versions') return route.fulfill({ json: requirement.pending_versions })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  await expect(page.getByRole('heading', { level: 1, name: 'Requirements' })).toBeVisible()
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await expect(page.getByText('Loading requirement…')).toHaveCount(0)
  await expect(canvas.getByText('Attach context', { exact: true })).toHaveCount(0)
})

test('requirements resolves attributed drift inline and refreshes its pending amendment', async ({ page }) => {
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
    staleness: {
      delivery_after_intent: false,
      active_drift: resolved ? [] : [drift],
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

  await page.goto('/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  const form = attention.getByRole('form', { name: 'Resolve drift requirement-drift-1' })
  const outcomes = form.getByLabel('Resolution outcome for requirement-drift-1')
  await expect(outcomes.getByRole('option', { name: 'Design document updated' })).toHaveCount(0)
  await outcomes.selectOption('requirements_amended')
  await expect(form.getByLabel('Confirmed requirement for requirement-drift-1')).toHaveCount(0)
  await form.getByRole('button', { name: 'Resolve' }).click()

  await expect.poll(() => resolution).toEqual({ outcome: 'requirements_amended', requirement_id: 'req-retries' })
  await expect(attention).not.toContainText('Code changed in conveyor without reaching this document')
  await expect(attention).toContainText('Version 2 is waiting for you')
  await expect(attention).toContainText('Written from a delivery change')
})

test('requirement staleness can file one linked follow-up and be dismissed in place', async ({ page }) => {
  await initShell(page)
  let state: 'open' | 'linked' | 'dismissed' = 'open'
  let followUpRequests = 0
  let dismissRequests = 0
  const view = () => {
    const delivery = {
      ...requirement.staleness.deliveries[0],
      needs_attention: state === 'open',
      follow_up:
        state === 'linked'
          ? { task_id: 'follow-up-task', title: 'Investigate retry delivery', state: 'queued' }
          : undefined,
    }
    return {
      ...requirement,
      pending_versions: [],
      staleness: {
        delivery_after_intent: state === 'open',
        partial_evaluation: false,
        deliveries: state === 'dismissed' ? [] : [delivery],
        active_drift: [],
      },
    }
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view())] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/requirements/req-retries/versions') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/staleness/signal-blueprint/follow-up')) {
      followUpRequests++
      state = 'linked'
      return route.fulfill({
        status: followUpRequests === 1 ? 201 : 200,
        json: {
          task: { id: 'follow-up-task', title: 'Investigate retry delivery', state: 'queued' },
          created: followUpRequests === 1,
        },
      })
    }
    if (url.pathname.endsWith('/staleness/signal-blueprint/acknowledge')) {
      dismissRequests++
      state = 'dismissed'
      return route.fulfill({ status: 201, json: { signal_id: 'signal-blueprint' } })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await attention.getByRole('button', { name: 'File a task' }).click()
  await expect.poll(() => followUpRequests).toBe(1)
  await expect(
    page
      .getByRole('region', { name: 'Delivery activity' })
      .getByRole('link', { name: 'Follow-up: Investigate retry delivery' }),
  ).toHaveAttribute('href', '/tasks/follow-up-task')
  await expect(attention).toContainText('Nothing needs your attention')

  state = 'open'
  await page.reload()
  await attention.getByRole('button', { name: 'Dismiss' }).click()
  await expect.poll(() => dismissRequests).toBe(1)
  await expect(attention).toContainText('Nothing needs your attention')
  await expect(page.getByText('Blueprint delivery may have moved past the confirmed intent')).toHaveCount(0)
})

test('requirements search and deterministic sorting preserve the selected canvas and show attention counts', async ({
  page,
}) => {
  await initShell(page)
  const items = [
    {
      ...requirement,
      requirement: {
        ...requirement.requirement,
        id: 'req-alpha-a',
        slug: 'alpha-a',
        title: 'Alpha',
        created_at: '2026-07-01T10:00:00Z',
        updated_at: '2026-07-04T10:00:00Z',
      },
      pending_versions: [{ ...requirement.pending_versions[0], requirement_id: 'req-alpha-a' }],
    },
    {
      ...requirement,
      requirement: {
        ...requirement.requirement,
        id: 'req-alpha-b',
        slug: 'alpha-b',
        title: 'alpha',
        created_at: '2026-07-01T10:00:00Z',
        updated_at: 'not-a-date',
      },
      pending_versions: [],
      staleness: { delivery_after_intent: false, partial_evaluation: false, active_drift: [] },
    },
    {
      ...requirement,
      requirement: {
        ...requirement.requirement,
        id: 'req-beta',
        slug: 'beta',
        title: 'Beta',
        created_at: '2026-07-03T10:00:00Z',
        updated_at: '2026-07-02T10:00:00Z',
      },
      pending_versions: [],
      staleness: { delivery_after_intent: false, partial_evaluation: false, active_drift: [] },
    },
    {
      ...requirement,
      requirement: {
        ...requirement.requirement,
        id: 'req-gamma',
        slug: 'gamma',
        title: 'Gamma',
        created_at: 'not-a-date',
        updated_at: '2026-07-03T10:00:00Z',
      },
      pending_versions: [],
      staleness: { delivery_after_intent: false, partial_evaluation: false, active_drift: [] },
    },
  ]
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: items.map(summarizeRequirement) })
    const detail = items.find((item) => path === `/v1/requirements/${item.requirement.id}`)
    if (detail) return route.fulfill({ json: detail })
    if (path.endsWith('/versions')) return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-alpha-a')
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  const group = tree.getByRole('heading', { name: 'Requirements' }).locator('..')
  const rowOrder = () =>
    group
      .getByRole('button')
      .filter({ hasText: /^(Alpha|alpha|Beta|Gamma)/ })
      .allTextContents()

  // Most recently updated first is the default ordering.
  await expect.poll(rowOrder).toEqual(['Alpha2', 'Gamma', 'Beta', 'alpha'])
  await expect(group.getByRole('button', { name: /Alpha/ }).getByLabel('2 attention items')).toBeVisible()

  // The sort control is one menu on the search field: choosing an option
  // sorts by it, and re-choosing the active option flips the direction.
  const sortTrigger = group.getByRole('button', { name: 'Sort requirements by' })
  const chooseSort = async (label: string) => {
    await sortTrigger.click()
    await page.getByRole('menuitemradio', { name: label }).click()
  }

  await chooseSort('Updated')
  await expect.poll(rowOrder).toEqual(['alpha', 'Beta', 'Gamma', 'Alpha2'])

  await chooseSort('Name')
  await expect.poll(rowOrder).toEqual(['Alpha2', 'alpha', 'Beta', 'Gamma'])
  await chooseSort('Name')
  await expect.poll(rowOrder).toEqual(['Gamma', 'Beta', 'Alpha2', 'alpha'])

  await chooseSort('Created')
  await expect.poll(rowOrder).toEqual(['Beta', 'Alpha2', 'alpha', 'Gamma'])

  const search = group.getByRole('searchbox', { name: 'Search requirements' })
  await search.fill('bEt')
  await expect.poll(rowOrder).toEqual(['Beta'])
  await search.fill('missing')
  await expect(group.getByText('No requirements match your search.')).toBeVisible()
  await expect(
    page.getByRole('region', { name: 'Requirement document' }).getByRole('heading', { name: 'Alpha' }),
  ).toBeVisible()
})

test('design drift names its subject, counts once, and clears every rendering from a requirement', async ({ page }) => {
  await initShell(page)
  let resolved = false
  const drift = {
    id: 'shared-design-drift',
    workspace_id: 'demo',
    repository: 'conveyor',
    kind: 'external_pr_merge',
    source_url: 'https://example.test/pr/488',
    task_id: '260812-cc4619',
    system_design_id: 'design-system-architecture',
    system_design_version: 6,
    matching_paths: ['internal/store/storetest/task_operations.go'],
    detected_at: '2026-08-12T08:00:00Z',
  }
  const requirementView = (id: string, title: string) => ({
    ...requirement,
    requirement: { ...requirement.requirement, id, slug: id, title },
    pending_versions: [],
    serving_blueprints: [],
    planning_sessions: [],
    staleness: {
      delivery_after_intent: false,
      partial_evaluation: false,
      deliveries: [],
      active_drift: resolved ? [] : [drift],
    },
  })
  const requirements = () => [
    requirementView('req-deployment', 'Deployment, identity, and multi-user operation'),
    requirementView('req-lifecycle', 'Task lifecycle and the work queue'),
  ]
  const designView = () => ({
    document: {
      id: 'design-system-architecture',
      slug: 'system-architecture',
      title: 'System architecture',
      category: 'Architecture',
      current_version: 6,
      workspace: 'demo',
      created_at: '2026-08-01T08:00:00Z',
      updated_at: '2026-08-12T08:00:00Z',
    },
    current_version: {
      document_id: 'design-system-architecture',
      version: 6,
      content: '# System architecture',
      governs: [],
      origin: 'operator',
      confirmed: true,
      workspace: 'demo',
      created_at: '2026-08-01T08:00:00Z',
    },
    pending_versions: [],
    versions: [],
    lineage: [],
    drift: resolved ? [] : [drift],
  })
  const designSummary = () => {
    const view = designView()
    const { content: _content, governs: _governs, ...currentVersion } = view.current_version
    return {
      document: view.document,
      current_version: currentVersion,
      pending_versions: [],
      pending_version_count: 0,
      drift_count: view.drift.length,
    }
  }

  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: requirements().map(summarizeRequirement) })
    const detail = requirements().find((item) => path === `/v1/requirements/${item.requirement.id}`)
    if (detail) return route.fulfill({ json: detail })
    if (path.startsWith('/v1/requirements/') && path.endsWith('/versions')) return route.fulfill({ json: [] })
    if (path === '/v1/system-designs') return route.fulfill({ json: [designSummary()] })
    if (path === '/v1/system-designs/design-system-architecture') return route.fulfill({ json: designView() })
    if (path === '/v1/decisions') return route.fulfill({ json: [] })
    if (path === '/v1/monitor/drift/shared-design-drift/resolve') {
      resolved = true
      return route.fulfill({ json: drift })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-deployment')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText("Code changed in conveyor and hasn't reached System architecture")
  await expect(attention).not.toContainText('without reaching this document')
  await expect(attention.getByRole('link', { name: 'System architecture' })).toHaveAttribute(
    'href',
    '/system-design?document=design-system-architecture',
  )
  await expect(attention).toContainText('Related attention context')
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  await expect(tree.getByLabel(/attention item/)).toHaveCount(0)

  await page.goto('/system-design?document=design-system-architecture')
  await expect(
    page
      .getByRole('navigation', { name: 'Document tree' })
      .getByRole('button', { name: /System architecture/ })
      .getByLabel('1 attention item'),
  ).toBeVisible()
  await page.goto('/requirements?requirement=req-deployment')

  const form = attention.getByRole('form', { name: 'Resolve drift shared-design-drift' })
  await form.getByLabel('Resolution outcome for shared-design-drift').selectOption('conflict_resolved')
  await form.getByRole('button', { name: 'Resolve' }).click()
  await expect(attention).toContainText('Nothing needs your attention on this document.')

  await tree.getByRole('button', { name: /Task lifecycle and the work queue/ }).click()
  await expect(attention).toContainText('Nothing needs your attention on this document.')
  await page.goto('/system-design?document=design-system-architecture')
  await expect(page.getByRole('region', { name: 'Needs your attention' })).toContainText(
    'Nothing needs your attention on this document.',
  )
})

test('requirement confirmation offers explicit attachment to eligible checkpoint-paused tasks', async ({ page }) => {
  await initShell(page)
  let contextWrites = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(requirement)] })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (path === '/v1/requirements/req-retries/versions') return route.fulfill({ json: requirement.pending_versions })
    if (path === '/v1/requirements/req-retries/versions/1/confirm')
      return route.fulfill({
        json: { requirement: requirement.requirement, version: requirement.pending_versions[0] },
      })
    if (path === '/v1/requirements/req-retries/checkpoint-context-candidates')
      return route.fulfill({ json: [{ id: 'paused-task', title: 'Paused delivery', state: 'running' }] })
    if (path === '/v1/tasks/paused-task/context') {
      contextWrites++
      return route.fulfill({ json: { requirements: [{ id: 'req-retries', title: 'Retry behavior', version: 1 }] } })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  await page.getByRole('button', { name: 'Confirm version 1' }).click()
  const offer = page.getByRole('dialog', { name: 'Attach confirmed requirement' })
  await expect(offer).toContainText('Paused delivery')
  await offer.getByRole('button', { name: 'Dismiss' }).click()
  await expect.poll(() => contextWrites).toBe(0)

  await page.getByRole('button', { name: 'Confirm version 1' }).click()
  await offer.getByRole('checkbox').check()
  await offer.getByRole('button', { name: 'Attach to 1 task' }).click()
  await expect.poll(() => contextWrites).toBe(1)
  await expect(offer).toHaveCount(0)
})

test('planning uses deployment configuration and sends no execution detail', async ({ page }) => {
  await initShell(page)
  let createdWith: Record<string, unknown> = {}
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST') {
      createdWith = JSON.parse(route.request().postData() ?? '{}') as Record<string, unknown>
      return route.fulfill({
        json: {
          id: 'session-new',
          title: 'Planning work…',
          status: 'active',
          goal: createdWith.goal,
          effort: 'high',
          exploration_output_tokens: 12000,
          primary_repo: 'conveyor',
          pinned_revisions: { conveyor: '0123456789abcdef' },
          workspace: 'demo',
          created_at: '2026-07-30T10:00:00Z',
          updated_at: '2026-07-30T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await expect(page.getByLabel('Planning model')).toHaveCount(0)
  await page.getByLabel('Planning goal').selectOption('bundle')
  await page.getByRole('button', { name: 'New session' }).click()
  // The server names and configures the session; the browser sends only intent.
  await expect.poll(() => createdWith).toEqual({ goal: 'bundle' })
})

test('finalize immediately reveals a complete bundle preview, approval, and created tasks', async ({ page }) => {
  await initShell(page)
  let finalized = false
  let approved = false
  let bundleReads = 0
  const session = () => ({
    id: 'session-bundle',
    title: finalized ? 'Ship safe retries' : 'Planning delivery…',
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
    if (url.pathname === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
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
                  spec_approval: true,
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

  await page.goto('/planning')
  await page.getByLabel('Planning message').fill('Finalize this delivery bundle.')
  await page.getByRole('button', { name: 'Send' }).click()
  const preview = page.getByRole('region', { name: 'Planning bundle preview' })
  await expect(preview).toBeVisible()
  await expect(preview.getByText('Retry behavior')).toBeVisible()
  await expect(preview.getByText('Implement bounded retries')).toBeVisible()
  await expect(preview.getByText(/depends on: task-prerequisite/)).toBeVisible()
  await expect(preview.getByText(/req-retries, design-runtime/)).toBeVisible()
  expect(bundleReads).toBeGreaterThan(1)
  await preview.getByRole('button', { name: 'Approve task set' }).click()
  await expect.poll(() => approved).toBe(true)
  await page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'Board' }).click()
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
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_viewer', role: 'viewer' } })
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

  await page.goto('/planning')
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

  await page.goto('/planning')
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

test('a requirement voices drift once and collapses to a quiet line when nothing is outstanding', async ({ page }) => {
  await initShell(page)
  const drifting = {
    ...requirement,
    current_version: { ...requirement.pending_versions[0], confirmed: true },
    pending_versions: [],
    staleness: {
      delivery_after_intent: false,
      active_drift: [
        {
          id: 'drift-9',
          workspace_id: 'demo',
          repository: 'conveyor',
          kind: 'external_pr_merge',
          source_url: 'https://example.test/pr/9',
          task_id: '260807-example',
          matching_paths: ['internal/dispatch/dispatch.go'],
          detected_at: '2026-08-06T10:00:00Z',
        },
      ],
    },
  }
  const settled = {
    ...drifting,
    requirement: { ...requirement.requirement, id: 'req-calm', slug: 'calm-intent', title: 'Calm intent' },
    serving_blueprints: [],
    planning_sessions: [],
    lineage_graph: undefined,
    staleness: { delivery_after_intent: false, active_drift: [] },
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: [drifting, settled].map(summarizeRequirement) })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: drifting })
    if (path === '/v1/requirements/req-calm') return route.fulfill({ json: settled })
    if (path.endsWith('/versions')) return route.fulfill({ json: [drifting.current_version] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  // AC-1.1: the unreconciled change is listed with the action that opens it.
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText('Code changed in conveyor without reaching this document')
  await expect(attention).toContainText('internal/dispatch/dispatch.go')
  await expect(attention.getByRole('link', { name: 'Open the change' })).toHaveAttribute(
    'href',
    'https://example.test/pr/9',
  )
  // Detailed drift copy stays on the canvas; the tree carries only the
  // approved compact aggregate and no second banner.
  await expect(page.getByText('1 active drift')).toHaveCount(0)
  await expect(page.getByText(/unreconciled repository change/)).toHaveCount(0)
  await expect(page.getByText('internal/dispatch/dispatch.go')).toHaveCount(1)

  // AC-2.1 and AC-1.3: the tree switches documents, and a document with
  // nothing outstanding collapses to one quiet line.
  await page
    .getByRole('navigation', { name: 'Document tree' })
    .getByRole('button', { name: /Calm intent/ })
    .click()
  await expect(
    page.getByRole('region', { name: 'Requirement document' }).getByRole('heading', { name: 'Calm intent' }),
  ).toBeVisible()
  await expect(attention).toContainText('Nothing needs your attention on this document.')
  await expect(attention.getByRole('button')).toHaveCount(0)
})

test('partial staleness is voiced and delivery is task-centric with blueprints as history', async ({ page }) => {
  await initShell(page)
  const partial = {
    ...requirement,
    current_version: { ...requirement.pending_versions[0], confirmed: true },
    pending_versions: [],
    serving_tasks: [{ id: '260807-0e2bc1', title: 'Scale task operations queries', state: 'running' }],
    // A truncated lineage walk cannot prove the absence of newer delivery, so
    // delivery_after_intent is a false negative here.
    staleness: { delivery_after_intent: false, active_drift: [], partial_evaluation: true },
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(partial)] })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: partial })
    if (path.endsWith('/versions')) return route.fulfill({ json: [partial.current_version] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  // An incomplete evaluation is never reported as alignment: it is voiced in
  // the one attention surface, like every other machinery signal (AC-1.2).
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText('Staleness could only be partially evaluated')
  await expect(attention).toContainText('bounded delivery lineage was truncated')
  await expect(attention).not.toContainText('Nothing needs your attention')
  await expect(page.getByText('Staleness partially evaluated')).toHaveCount(0)
  await expect(page.getByText('Intent aligned')).toHaveCount(0)

  // Delivery counts the serving tasks; the blueprint list reads as history.
  await expect(page.getByRole('link', { name: /Scale task operations queries/ })).toHaveAttribute(
    'href',
    '/tasks/260807-0e2bc1/full',
  )
  await expect(page.getByRole('heading', { name: 'Delivery before blueprints retired' })).toBeVisible()
  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute(
    'href',
    '/blueprints/blueprint-task',
  )
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

  await page.goto('/requirements?requirement=req-retries')
  await expect(page).toHaveURL(/requirement=req-retries/)
  await page.getByText('Version history').click()
  const history = page.getByRole('region', { name: 'Requirement versions' })
  const version2 = history.getByRole('button').filter({ hasText: /^v2/ })
  await expect(version2).toBeVisible()
  await version2.click()
  await expect(version2).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByText('Written by an operator').first()).toBeVisible()
  await expect(page.getByText('Compared with confirmed v1')).toBeVisible()
  await expect(page.locator('.bg-failure-soft').filter({ hasText: 'Keep retries bounded.' })).toBeVisible()
  await expect(
    page.locator('.bg-positive-soft').filter({ hasText: 'Keep retries bounded and observable.' }),
  ).toBeVisible()
  await expect(
    page.getByRole('region', { name: 'Requirement statements' }).getByRole('link', { name: 'Link to REQ-1' }),
  ).toBeVisible()
  await expect(page.getByText('conveyor:requirements')).toHaveCount(0)
  // AC-1.1: both proposed versions are listed once, each with its own
  // confirmation, in the single attention surface.
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention.getByRole('button', { name: /^Confirm version/ })).toHaveCount(2)
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

  await page.goto('/requirements?requirement=req-retries')
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

test('retired requirement versions stay in history as superseded and leave attention', async ({ page }) => {
  await initShell(page)
  const current = {
    ...requirement.pending_versions[0],
    version: 4,
    confirmed: true,
    confirmed_at: '2026-07-30T10:10:00Z',
  }
  const retired = {
    ...requirement.pending_versions[0],
    version: 2,
    retired: true,
    retired_by: 'operator',
    retired_at: '2026-07-30T10:10:00Z',
    retired_by_version: 4,
  }
  const view = {
    ...requirement,
    requirement: { ...requirement.requirement, current_version: 4 },
    current_version: current,
    pending_versions: [],
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(view)] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view })
    if (url.pathname === '/v1/requirements/req-retries/versions') return route.fulfill({ json: [retired, current] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).not.toContainText('Version 2 is waiting for you')
  const history = page.getByRole('region', { name: 'Requirement versions' })
  const version2 = history.getByRole('button').filter({ hasText: /^v2/ })
  await expect(version2).toContainText('Superseded')
  await version2.click()
  await expect(page.getByText('Superseded', { exact: true }).first()).toBeVisible()
})

test('migrated seeds explain disabled confirmation and requirement switches open the latest version', async ({
  page,
}) => {
  await initShell(page)
  const seedVersion = {
    ...requirement.pending_versions[0],
    origin: 'feature_migration',
    origin_session_id: undefined,
  }
  const migrated = {
    ...requirement,
    requirement: { ...requirement.requirement, title: 'Migrated intent' },
    pending_versions: [seedVersion],
    migrated_seed: true,
    confirmation_eligible: false,
    staleness: { delivery_after_intent: false, active_drift: [] },
  }
  const secondV1 = {
    ...requirement.pending_versions[0],
    requirement_id: 'req-second',
    version: 1,
    content: 'Earlier second document.',
  }
  const secondV2 = {
    ...secondV1,
    version: 2,
    content: 'Latest second document.',
  }
  const second = {
    ...requirement,
    requirement: {
      ...requirement.requirement,
      id: 'req-second',
      slug: 'second-intent',
      title: 'Second intent',
    },
    pending_versions: [secondV1, secondV2],
    migrated_seed: false,
    confirmation_eligible: true,
    staleness: { delivery_after_intent: false, active_drift: [] },
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: [migrated, second].map(summarizeRequirement) })
    if (path === '/v1/requirements/req-retries') return route.fulfill({ json: migrated })
    if (path === '/v1/requirements/req-retries/versions') return route.fulfill({ json: [seedVersion] })
    if (path === '/v1/requirements/req-second') return route.fulfill({ json: second })
    if (path === '/v1/requirements/req-second/versions') return route.fulfill({ json: [secondV1, secondV2] })
    return route.fulfill({ json: [] })
  })
  await page.goto('/requirements?requirement=req-retries')
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  const disabled = attention.getByRole('button', { name: 'Confirm version 1' })
  await expect(disabled).toBeDisabled()
  await expect(disabled).toHaveAttribute('title', /Revise this migrated seed/)
  await expect(attention.getByText(/needs its first deliberate revision/)).toBeVisible()
  await page
    .getByRole('navigation', { name: 'Document tree' })
    .getByRole('button', { name: 'Second intent', exact: false })
    .click()
  await page.getByText('Version history').click()
  await expect(
    page.getByRole('region', { name: 'Requirement versions' }).getByRole('button').filter({ hasText: /^v2/ }),
  ).toHaveAttribute('aria-pressed', 'true')
})

test('planning keeps partial text through malformed and error stream frames and resolves pending markers', async ({
  page,
}) => {
  await initShell(page)
  let bundleFetches = 0
  const session = {
    id: 'session-stream-error',
    title: 'Stream recovery',
    status: 'active',
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-bundles') {
      bundleFetches++
      return route.fulfill({ json: [] })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}`) return route.fulfill({ json: session })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: [
          'data: not-json',
          '',
          'data: {"type":"tool-input-available","toolCallId":"call-1","toolName":"read_requirement"}',
          '',
          'data: {"type":"text-delta","delta":"I found part of the answer."}',
          '',
          'data: {"type":"error","errorText":"Planning request failed. Please retry."}',
          '',
        ].join('\n'),
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await page.getByLabel('Planning message').fill('Continue the plan.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText('I found part of the answer.')).toBeVisible()
  await expect(page.getByText('Planning request failed. Please retry.')).toBeVisible()
  await expect(page.getByLabel('read_requirement: failed')).toBeVisible()
  await expect(page.getByLabel('Planning message')).toBeEnabled()
  await expect.poll(() => bundleFetches).toBeGreaterThanOrEqual(2)
})

test('planning explains run conflicts and surfaces abandon failures without configuration controls', async ({
  page,
}) => {
  await initShell(page)
  const session = {
    id: 'session-conflict',
    title: 'Conflict recovery',
    status: 'active',
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces')
      return route.fulfill({
        json: [{ id: 'demo', name: 'Demo', config_version: 1 }],
      })
    if (url.pathname === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (url.pathname === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
    if (url.pathname === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', repos: ['conveyor'] },
      })
    if (url.pathname === '/v1/activity' || url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}/abandon`)
      return route.fulfill({
        status: 409,
        body: 'The session was finalized elsewhere.',
      })
    if (url.pathname === `/v1/planning-sessions/${session.id}`) return route.fulfill({ json: session })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages'))
      return route.fulfill({
        status: 409,
        body: 'planning session already has a message in progress',
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await expect(page.getByLabel('Planning model')).toHaveCount(0)

  await page.getByRole('button', { name: 'Abandon' }).click()
  const dialog = page.getByRole('dialog', { name: 'Abandon planning session' })
  await expect(dialog).toBeVisible()
  await dialog.getByLabel('Reason for abandoning').fill('No longer needed')
  await dialog.getByRole('button', { name: 'Abandon session' }).click()
  await expect(dialog.getByText('The session was finalized elsewhere.')).toBeVisible()
  await dialog.getByRole('button', { name: 'Keep session' }).click()

  await page.getByLabel('Planning message').fill('Continue.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText('A reply is already in progress for this session.')).toBeVisible()
})

test('planning sends attachments as file parts without smuggling them into message text', async ({ page }) => {
  await initShell(page)
  const session = {
    id: 'session-attachment',
    title: 'Attachment planning',
    status: 'active',
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
  }
  let posted: {
    message?: { content?: string; parts?: Array<Record<string, unknown>> }
  } = {}
  let uploadBody = ''
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/artifacts') {
      uploadBody = route.request().postData() ?? ''
      return route.fulfill({
        status: 201,
        json: {
          id: 'artifact-1',
          workspace: 'demo',
          name: 'context.txt',
          content_type: 'text/plain',
          size_bytes: 7,
          role: 'task_context',
          planning_session_id: session.id,
          created_at: '2026-07-30T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}`) return route.fulfill({ json: session })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      posted = JSON.parse(route.request().postData() ?? '{}') as typeof posted
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: 'data: [DONE]\n\n',
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await page.locator('input[type=file]').setInputFiles({
    name: 'context.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('context'),
  })
  await expect(page.getByText('context.txt')).toBeVisible()
  expect(uploadBody).toContain('planning_session_id')
  expect(uploadBody).toContain(session.id)
  await page.getByLabel('Planning message').fill('Use this context.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => posted.message?.parts?.length ?? 0).toBe(2)
  expect(posted.message?.content).toBe('Use this context.')
  expect(posted.message?.content).not.toContain('Attached context')
  expect(posted.message?.parts?.[1]).toMatchObject({
    type: 'file',
    artifactId: 'artifact-1',
    filename: 'context.txt',
  })
})

test('planning restores the selected session independently in each workspace', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-planning-session:demo', 'session-demo')
    localStorage.setItem('conveyor-planning-session:beta', 'session-beta')
  })
  const sessions = {
    demo: [
      {
        id: 'session-demo',
        title: 'Demo session',
        status: 'active',
        workspace: 'demo',
        created_at: '2026-07-30T10:00:00Z',
        updated_at: '2026-07-30T10:00:00Z',
      },
    ],
    beta: [
      {
        id: 'session-beta',
        title: 'Beta session',
        status: 'active',
        workspace: 'beta',
        created_at: '2026-07-30T10:00:00Z',
        updated_at: '2026-07-30T10:00:00Z',
      },
    ],
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const workspace = url.searchParams.get('workspace_id') === 'beta' ? 'beta' : 'demo'
    if (url.pathname === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo', config_version: 1 },
          { id: 'beta', name: 'Beta', config_version: 1 },
        ],
      })
    if (url.pathname === '/v1/workspace/config')
      return route.fulfill({
        json: {
          ...planningConfig,
          document: { ...planningConfig.document, workspace },
        },
      })
    if (url.pathname === '/v1/workspace') return route.fulfill({ json: { workspace, repos: ['conveyor'] } })
    if (url.pathname === '/v1/activity' || url.pathname === '/v1/blueprints' || url.pathname === '/v1/requirements')
      return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: sessions[workspace] })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ json: [] })
    if (url.pathname.includes('/planning-sessions/')) return route.fulfill({ json: sessions[workspace][0] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await expect(page.getByRole('button', { name: /Demo session/ })).toHaveAttribute('aria-current', 'true')
  await page.getByRole('button', { name: 'Switch to Beta' }).click()
  // Planning left the sidebar while its presentation is parked (§21.61 change
  // 3), so the route is reached directly — exactly as a deep link does. The
  // reload re-runs the init script above, so the switched workspace is pinned
  // for the new load rather than reset to the seeded one.
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'beta'))
  await page.goto('/planning')
  await expect(page.getByRole('button', { name: /Beta session/ })).toHaveAttribute('aria-current', 'true')
  expect(await page.evaluate(() => localStorage.getItem('conveyor-planning-session:demo'))).toBe('session-demo')
  expect(await page.evaluate(() => localStorage.getItem('conveyor-planning-session:beta'))).toBe('session-beta')
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
  await page.goto('/requirements')
  await expect(page.getByText('Requirement service is unavailable.')).toBeVisible()
  await page.goto('/planning')
  await expect(page.getByText('Planning history is unavailable.')).toBeVisible()
  await expect(page.getByText('Restoring sessions…')).toHaveCount(0)
})

test('a historical finalized blueprint session remains readable', async ({ page }) => {
  await initShell(page)
  const session = {
    id: 'session-blueprint',
    title: 'Plan delivery',
    status: 'finalized',
    produced_task_id: 'blueprint-task',
    transcript_artifact_id: 'artifact-1',
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:10:00Z',
    finalized_at: '2026-07-30T10:10:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await expect(page.getByText('Planning artifact finalized')).toBeVisible()
  await expect(page.getByText(/authority remains with the ordinary confirmation or plan gate/)).toBeVisible()
  await expect(page.getByRole('link', { name: 'Open plan gate' })).toHaveAttribute('href', '/tasks/blueprint-task')
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

  await page.goto('/requirements')
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

test('upload then supersede uses the version endpoint and bounds oversized comparison', async ({ page }) => {
  await initShell(page)
  let currentVersion = 0
  let createRequests = 0
  let supersedeRequests = 0
  const priorContent = Array.from({ length: 600 }, (_, index) => `Prior line ${index}`).join('\n')
  const currentContent = Array.from({ length: 600 }, (_, index) => `Current line ${index}`).join('\n')
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'GET') {
      return route.fulfill({
        json: currentVersion
          ? [{ id: 'ref-live', name: 'overview', current_version: currentVersion, workspace: 'demo' }]
          : [],
      })
    }
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'POST') {
      createRequests++
      currentVersion = 1
      return route.fulfill({ status: 201, json: { document: { id: 'ref-live' }, version: { version: 1 } } })
    }
    if (url.pathname === '/v1/reference-documents/ref-live/versions' && route.request().method() === 'POST') {
      supersedeRequests++
      currentVersion = 2
      return route.fulfill({ status: 201, json: { document_id: 'ref-live', version: 2, supersedes_version: 1 } })
    }
    if (url.pathname === '/v1/reference-documents/ref-live/versions') {
      return route.fulfill({
        json: [
          {
            document_id: 'ref-live',
            version: 1,
            filename: 'overview.md',
            content_type: 'text/markdown',
            content: priorContent,
            workspace: 'demo',
          },
          ...(currentVersion > 1
            ? [
                {
                  document_id: 'ref-live',
                  version: 2,
                  filename: 'overview.md',
                  content_type: 'text/markdown',
                  content: currentContent,
                  supersedes_version: 1,
                  workspace: 'demo',
                },
              ]
            : []),
        ],
      })
    }
    if (url.pathname === '/v1/requirements' || url.pathname === '/v1/planning-sessions') {
      return route.fulfill({ json: [] })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  await page
    .locator('label')
    .filter({ hasText: 'Add Markdown' })
    .locator('input[type=file]')
    .setInputFiles({
      name: 'overview.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(priorContent),
    })
  await expect.poll(() => createRequests).toBe(1)
  const item = page.getByRole('navigation', { name: 'Document tree' }).getByRole('button', { name: /overview/ })
  await item.click()
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByText('v1', { exact: true })).toBeVisible()
  await expect(page.getByText('Prior line 0').first()).toBeVisible()

  await page
    .locator('label')
    .filter({ hasText: 'Re-upload' })
    .locator('input[type=file]')
    .setInputFiles({
      name: 'overview.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from(currentContent),
    })
  await expect.poll(() => supersedeRequests).toBe(1)
  await expect(canvas.getByText('v2', { exact: true })).toBeVisible()
  const comparison = page.locator('details').filter({ hasText: 'Compared with v1' })
  await comparison.getByText('Compared with v1').click()
  await expect(comparison.getByText('Diff too large; showing both versions without highlighting.')).toBeVisible()
  await expect(comparison.getByText('Prior line 0', { exact: true })).toBeVisible()
  await expect(comparison.getByText('Current line 0', { exact: true })).toBeVisible()

  await page.getByLabel('Read version').selectOption('1')
  await expect(page.getByText('Prior line 599')).toBeVisible()
})

test('pending derivation links to its pinned source and requirement anchors scroll after rendering', async ({
  page,
}) => {
  await initShell(page)
  let historyRequests = 0
  const derived = {
    ...requirement,
    pending_versions: [
      {
        ...requirement.pending_versions[0],
        derived_from: { document_id: 'ref-overview', version: 1, section_anchor: '#billing-rule', target_id: 'AC-1.1' },
        statements: [
          {
            id: 'REQ-1',
            statement: 'Retries stay bounded.',
            user_story: { as_a: 'billing operator', i_want: 'bounded retries', so_that: 'failures remain visible' },
            acceptance_criteria: [{ id: 'AC-1.1', statement: 'A failed charge retries twice.' }],
          },
        ],
      },
    ],
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/reference-documents')
      return route.fulfill({
        json: [{ id: 'ref-overview', name: 'Product overview', current_version: 2, workspace: 'demo' }],
      })
    if (url.pathname === '/v1/reference-documents/ref-overview/versions') {
      historyRequests++
      return route.fulfill({
        json: [
          {
            document_id: 'ref-overview',
            version: 1,
            filename: 'overview.md',
            content_type: 'text/markdown',
            content: '# Billing rule\n\nRetry twice.',
            workspace: 'demo',
          },
        ],
      })
    }
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(derived)] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: derived })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: derived.pending_versions })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries#ac-1.1')
  await expect(
    page.getByText('As billing operator, I want bounded retries, so that failures remain visible.'),
  ).toBeVisible()
  const criterion = page.locator('#ac-1\\.1')
  await expect(criterion).toBeVisible()
  await expect
    .poll(() => criterion.evaluate((element) => element.getBoundingClientRect().top < window.innerHeight))
    .toBe(true)
  const source = page.getByRole('link', { name: 'Product overview · version 1' })
  await expect(source).toHaveAttribute('href', '#reference-ref-overview-v1')
  expect(historyRequests).toBe(0)
  await source.click()
  await expect.poll(() => historyRequests).toBe(1)
  await expect(page.getByText('Retry twice.')).toBeVisible()
})

// AC-6: session lists carry human-readable goals with goal- or
// artifact-derived titles, and the requirements authoring surface offers no
// freehand editor — the sidebar is the only authoring path (change 2).
test('session lists label goals and the requirements surface has no freehand editor', async ({ page }) => {
  await initShell(page)
  const sessions = [
    {
      id: 'session-drafting',
      title: 'Drafting requirement…',
      status: 'active',
      goal: 'requirement',
      workspace: 'demo',
      created_at: '2026-07-30T10:00:00Z',
      updated_at: '2026-07-30T10:02:00Z',
    },
    {
      id: 'session-shipped',
      title: 'Ship bounded retries',
      status: 'finalized',
      goal: 'blueprint',
      produced_task_id: 'blueprint-task',
      workspace: 'demo',
      created_at: '2026-07-30T09:00:00Z',
      updated_at: '2026-07-30T09:30:00Z',
    },
    {
      id: 'session-legacy',
      title: 'Exploring…',
      status: 'active',
      workspace: 'demo',
      created_at: '2026-07-30T08:00:00Z',
      updated_at: '2026-07-30T08:30:00Z',
    },
  ]
  const withSession = {
    ...requirement,
    planning_sessions: [{ ...requirement.planning_sessions[0], goal: 'requirement' }],
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [summarizeRequirement(withSession)] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: withSession })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: sessions })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ json: [] })
    if (url.pathname.startsWith('/v1/planning-sessions/')) return route.fulfill({ json: sessions[0] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  // Goal- and artifact-derived titles, never a wall of "New requirement".
  await expect(page.getByRole('button', { name: /Drafting requirement…/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Ship bounded retries/ })).toBeVisible()
  await expect(page.getByText('New requirement', { exact: true })).toHaveCount(0)
  // Human-readable goals, never the raw enum. Pre-goal rows read as open.
  await expect(
    page.getByRole('button', { name: /Drafting requirement…/ }).getByText('Requirement', { exact: true }),
  ).toBeVisible()
  await expect(
    page.getByRole('button', { name: /Ship bounded retries/ }).getByText('Blueprint', { exact: true }),
  ).toBeVisible()
  await expect(
    page.getByRole('button', { name: /Exploring…/ }).getByText('Open exploration', { exact: true }),
  ).toBeVisible()
  // The standalone surface keeps only the goals with no document context.
  await expect(page.getByLabel('Planning goal').locator('option')).toHaveText(['Open exploration', 'Delivery bundle'])
  // Requirement drafting moved beside the document, so it is not offered here.
  await expect(page.getByLabel('Planning goal').locator('option')).toHaveCount(2)

  await page.goto('/requirements?requirement=req-retries')
  await expect(page.getByRole('region', { name: 'Requirement document' })).toBeVisible()
  // Freehand editing stays rejected, and with the
  // assistant column parked (§21.61 change 3) there is no composer either, so
  // this surface carries no editable field at all.
  await expect(page.locator('[contenteditable]')).toHaveCount(0)
  await expect(page.locator('textarea')).toHaveCount(0)
})

// A deep link names the document to open: the fall-back-to-the-first-document
// effect must not steal the canvas from the document the URL asked for.
test('a deep link opens the document the URL asked for, not the first in the corpus', async ({ page }) => {
  await initShell(page)
  const other = {
    ...requirement,
    requirement: {
      ...requirement.requirement,
      id: 'req-other',
      slug: 'other-intent',
      title: 'Other intent',
    },
    serving_blueprints: [],
    planning_sessions: [],
    lineage_graph: undefined,
  }
  const finished = {
    id: 'session-done',
    title: 'Retry behavior',
    status: 'finalized',
    goal: 'requirement',
    produced_requirement_id: 'req-retries',
    transcript_artifact_id: 'artifact-1',
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:10:00Z',
    finalized_at: '2026-07-30T10:10:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements')
      return route.fulfill({ json: [other, requirement].map(summarizeRequirement) })
    if (url.pathname === '/v1/requirements/req-other') return route.fulfill({ json: other })
    if (url.pathname === '/v1/requirements/req-other/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [finished] })
    if (url.pathname === '/v1/planning-sessions/session-done') return route.fulfill({ json: finished })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-other&session=session-done')
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Other intent' })).toBeVisible()
  await expect(page).toHaveURL(/requirement=req-other/)
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toHaveCount(0)
  // The parked assistant does not reappear for a session named in the URL.
  await expect(page.getByRole('complementary', { name: 'Planning assistant' })).toHaveCount(0)
})
