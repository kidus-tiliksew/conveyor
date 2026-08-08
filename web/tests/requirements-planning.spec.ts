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
        'Keep retries bounded.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop after a finite limit.\n```',
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
    latest_delivery: 'blueprint-task',
    latest_delivery_at: '2026-07-30T10:05:00Z',
    active_drift: [],
  },
  migrated_seed: false,
  confirmation_eligible: true,
}

const planningConfig = {
  version: 1,
  document: {
    workspace: 'demo',
    planning_models: ['gpt-plan', 'gpt-plan-fast'],
    execution_settings: {
      control_plane: {
        triage: { model: 'gpt-triage', timeout: '20m' },
        planning: {
          model: 'gpt-plan',
          effort: 'high',
          timeout: '30m',
          exploration_output_tokens: 12000,
        },
      },
    },
    routing: { stages: { review: {} } },
    review: { seats: [] },
    setups: [],
    default_setup: '',
    execution: {},
    harnesses: [],
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
    sessionStorage.setItem('conveyor-token', 'test-token')
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
  if (path === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
  if (path === '/v1/activity') return route.fulfill({ json: [] })
}

test('requirements renders a document tree, one attention surface, and confirms from the canvas', async ({ page }) => {
  await initShell(page)
  let confirmed = false
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [requirement] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
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

  // AC-2.1: the tree groups the corpus beside the canvas, and the selected
  // document is the hero.
  const tree = page.getByRole('navigation', { name: 'Document tree' })
  await expect(tree.getByRole('heading', { name: 'Product overviews' })).toBeVisible()
  await expect(tree.getByRole('heading', { name: 'Requirements' })).toBeVisible()
  await expect(tree.getByRole('button', { name: /Retry behavior/ })).toHaveAttribute('aria-current', 'true')
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  await expect(page.getByText('Feature tree')).toHaveCount(0)

  // AC-1.1: staleness names its causal delivery and the pending version
  // carries the confirmation, both inside the one attention surface.
  const attention = page.getByRole('region', { name: 'Needs your attention' })
  await expect(attention).toContainText('Code shipped past the confirmed intent')
  await expect(attention.getByText('Latest delivery: blueprint-task.')).toHaveAttribute(
    'title',
    /blueprint-task delivered/,
  )
  await expect(attention.getByRole('link', { name: 'Open the delivery' })).toHaveAttribute(
    'href',
    '/tasks/blueprint-task',
  )
  await expect(attention).toContainText('Version 1 is waiting for you')

  // AC-1.2: the badges and banners that used to repeat those signals.
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
  // rendering of lineage, opened on demand (spec §21.61 change 2, REQ-3).
  await expect(page.getByRole('button', { name: 'Related' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Intent to delivery' })).toHaveCount(0)

  await attention.getByRole('button', { name: 'Confirm version 1' }).click()
  await expect.poll(() => confirmed).toBe(true)
  await expect(page).toHaveURL(/\/requirements/)
})

test('planning starts with an allowlisted model and sends that choice', async ({ page }) => {
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
          model: createdWith.model,
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
  await expect(page.getByLabel('Planning model')).toHaveValue('gpt-plan')
  await expect(page.getByLabel('Planning model').locator('option')).toHaveText(['gpt-plan', 'gpt-plan-fast'])
  await page.getByLabel('Planning goal').selectOption('bundle')
  await page.getByLabel('Planning model').selectOption('gpt-plan-fast')
  await page.getByRole('button', { name: 'New session' }).click()
  // The goal and model are the only creation inputs: no caller title reaches
  // the server, which names the session itself (spec §21.57 change 3).
  await expect
    .poll(() => createdWith)
    .toEqual({
      goal: 'bundle',
      model: 'gpt-plan-fast',
    })
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
    if (url.pathname !== '/v1/workspaces') expect(route.request().headers().authorization).toBe('Bearer test-token')
    if (url.pathname === '/v1/workspaces')
      return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
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
    if (path === '/v1/requirements') return route.fulfill({ json: [drifting, settled] })
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
  // AC-1.2: exactly once — no drift chip in the tree and no second banner.
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
    // delivery_after_intent is a false negative here (spec §21.58 change 6).
    staleness: { delivery_after_intent: false, active_drift: [], partial_evaluation: true },
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/requirements') return route.fulfill({ json: [partial] })
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
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [view] })
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
    if (path === '/v1/requirements') return route.fulfill({ json: [migrated, second] })
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

test('planning explains run conflicts, uses a disabled default model fallback, and surfaces abandon failures', async ({
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
    if (url.pathname === '/v1/workspace/config')
      return route.fulfill({
        json: {
          ...planningConfig,
          document: { ...planningConfig.document, planning_models: [] },
        },
      })
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
  await expect(page.getByLabel('Planning model')).toHaveValue('gpt-plan')
  await expect(page.getByLabel('Planning model')).toBeDisabled()
  await expect(page.getByLabel('Planning model').locator('option')).toHaveCount(1)

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
    sessionStorage.setItem('conveyor-token', 'test-token')
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
  await expect(page.getByRole('link', { name: 'Open spec gate' })).toHaveAttribute('href', '/tasks/blueprint-task')
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
  expect(listAuthorization).toBe('Bearer test-token')
  expect(versionRequests).toBe(0)

  const addInput = tree.locator('label').filter({ hasText: 'Add Markdown' }).locator('input[type=file]')
  await addInput.setInputFiles({ name: 'new.md', mimeType: 'application/octet-stream', buffer: Buffer.from('# New') })
  await expect.poll(() => uploadRequests).toBe(1)
  await addInput.setInputFiles({ name: 'bad.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-1.7') })
  await expect(page.getByText('reference document content is not Markdown')).toBeVisible()

  await overviewItem.click()
  await expect.poll(() => versionRequests).toBe(1)
  expect(versionAuthorization).toBe('Bearer test-token')
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
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [derived] })
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
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [withSession] })
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
  // Freehand editing stays rejected (spec §21.57 change 2), and with the
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
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [other, requirement] })
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
