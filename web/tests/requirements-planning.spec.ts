import { expect, test, type Page, type Route } from '@playwright/test'

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

test('requirements renders living intent as the canvas, confirms a revision, and docks the assistant', async ({
  page,
}) => {
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
  await expect(page.getByRole('heading', { name: 'Requirements' })).toBeVisible()
  await expect(page.getByText('Retry behavior', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('Needs confirmation')).toBeVisible()
  await expect(page.getByText('Feature tree')).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute(
    'href',
    '/blueprints/blueprint-task',
  )
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Intent to delivery' })).toBeVisible()
  await expect(page.getByText('Latest delivery: blueprint-task.')).toHaveAttribute('title', /blueprint-task delivered/)
  await page.getByText('Trace planning to delivery evidence').click()
  await expect(page.getByText('serves', { exact: true })).toBeVisible()
  await expect(page.getByText('Retry behavior', { exact: true }).last()).toBeVisible()
  await expect(page.getByText('Ship bounded retries', { exact: true }).last()).toBeVisible()

  await page.getByRole('button', { name: 'Confirm version 1' }).click()
  await expect.poll(() => confirmed).toBe(true)

  // The document is the canvas and the assistant is docked beside it, scoped
  // to that document — no detached chat page (spec §21.57 change 1).
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  const assistant = page.getByRole('complementary', {
    name: 'Planning assistant',
  })
  await expect(assistant.getByText('Retry behavior')).toBeVisible()
  for (const action of ['Draft', 'Revise', 'Q&A', 'Plan work']) {
    await expect(assistant.getByRole('button', { name: action })).toBeVisible()
  }
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
  await page.getByLabel('Planning goal').selectOption('blueprint')
  await page.getByLabel('Planning model').selectOption('gpt-plan-fast')
  await page.getByRole('button', { name: 'New session' }).click()
  // The goal and model are the only creation inputs: no caller title reaches
  // the server, which names the session itself (spec §21.57 change 3).
  await expect
    .poll(() => createdWith)
    .toEqual({
      goal: 'blueprint',
      model: 'gpt-plan-fast',
    })
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
  await expect(page.getByRole('button', { name: /Version 2/ })).toBeVisible()
  await page.getByRole('button', { name: /Version 2/ }).click()
  await expect(page.getByText('Compared with confirmed v1')).toBeVisible()
  await expect(page.locator('.bg-failure-soft').filter({ hasText: 'Keep retries bounded.' })).toBeVisible()
  await expect(
    page.locator('.bg-positive-soft').filter({ hasText: 'Keep retries bounded and observable.' }),
  ).toBeVisible()
  await expect(page.getByRole('region', { name: 'Requirement statements' }).getByText('REQ-1')).toBeVisible()
  await expect(page.getByText('conveyor:requirements')).toHaveCount(0)
  await page.getByRole('button', { name: 'Confirm version 2' }).click()
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
  const disabled = page.getByRole('button', { name: 'Confirm version 1' })
  await expect(disabled).toBeDisabled()
  await expect(disabled).toHaveAttribute('title', /Revise this migrated seed/)
  await expect(page.getByText(/needs its first deliberate revision/)).toBeVisible()
  await page.getByRole('button', { name: 'Second intent', exact: false }).click()
  await expect(page.getByRole('button', { name: /Version 2/ })).toHaveAttribute('aria-pressed', 'true')
})

test('planning keeps partial text through malformed and error stream frames and resolves pending markers', async ({
  page,
}) => {
  await initShell(page)
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
  await page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'Planning' }).click()
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

test('a finalized blueprint session hands off to the ordinary spec gate', async ({ page }) => {
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

// AC-4: every guided action starts a sidebar session with its declared goal,
// and none of them leaves the Requirements view (spec §21.57 change 1).
test('guided actions start goal-declared sidebar sessions without leaving requirements', async ({ page }) => {
  await initShell(page)
  const created: Record<string, unknown>[] = []
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST') {
      const body = JSON.parse(route.request().postData() ?? '{}') as Record<string, unknown>
      created.push(body)
      return route.fulfill({
        json: {
          id: `session-${created.length}`,
          title: 'Drafting requirement…',
          status: 'active',
          goal: body.goal,
          requirement_context_id: body.requirement_context_id,
          workspace: 'demo',
          created_at: '2026-07-30T10:00:00Z',
          updated_at: '2026-07-30T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [requirement] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname.startsWith('/v1/planning-sessions/') && url.pathname.endsWith('/messages'))
      return route.fulfill({ json: [] })
    if (url.pathname.startsWith('/v1/planning-sessions/')) {
      const id = url.pathname.split('/')[3]
      const body = created[Number(id.split('-')[1]) - 1] ?? {}
      return route.fulfill({
        json: {
          id,
          title: 'Drafting requirement…',
          status: 'active',
          goal: body.goal,
          requirement_context_id: body.requirement_context_id,
          workspace: 'demo',
          created_at: '2026-07-30T10:00:00Z',
          updated_at: '2026-07-30T10:00:00Z',
        },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  const assistant = page.getByRole('complementary', {
    name: 'Planning assistant',
  })
  const expected = [
    { action: 'Revise', goal: 'requirement', context: 'req-retries' },
    { action: 'Q&A', goal: 'open', context: 'req-retries' },
    { action: 'Plan work', goal: 'blueprint', context: 'req-retries' },
    { action: 'Draft', goal: 'requirement', context: undefined },
  ]
  for (const [index, step] of expected.entries()) {
    await assistant.getByRole('button', { name: step.action }).click()
    await expect.poll(() => created.length).toBe(index + 1)
    expect(created[index].goal).toBe(step.goal)
    expect(created[index].requirement_context_id).toBe(step.context)
    // The conversation opens in the sidebar; the canvas never navigates away.
    await expect(assistant.getByRole('log', { name: 'Planning conversation' })).toBeVisible()
    await expect(assistant.getByRole('heading', { name: 'Drafting requirement…' })).toBeVisible()
    await expect(page).toHaveURL(/\/requirements\?/)
    await expect(page).toHaveURL(/requirement=req-retries/)
  }
  // The sidebar session is deep-linkable, so a reload restores the assistant.
  await expect(page).toHaveURL(/session=session-4/)
})

test('promotion selects immutable provenance for new REQ and existing nested AC sessions', async ({ page }) => {
  await initShell(page)
  let existing = false
  const created: Record<string, unknown>[] = []
  const existingRequirement = {
    ...requirement,
    requirement: { ...requirement.requirement, current_version: 1 },
    current_version: {
      ...requirement.pending_versions[0],
      confirmed: true,
      statements: [
        {
          id: 'REQ-1',
          statement: 'Retries stay bounded.',
          acceptance_criteria: [{ id: 'AC-1.1', statement: 'A failed charge retries twice.' }],
        },
      ],
    },
    pending_versions: [],
  }
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/reference-documents')
      return route.fulfill({
        json: [{ id: 'ref-overview', name: 'Product overview', current_version: 2, workspace: 'demo' }],
      })
    if (url.pathname === '/v1/reference-documents/ref-overview/versions')
      return route.fulfill({
        json: [
          {
            document_id: 'ref-overview',
            version: 2,
            filename: 'overview.md',
            content_type: 'text/markdown',
            content:
              '# Billing rule\n\nRetry failed charges twice.\n\n```md\n# Hidden instruction\n```\n\n# Billing rule\n\nDuplicate section.\n\n    # Four-space code\n\n\t# Tab-indented\n\n   ## Three spaces',
            workspace: 'demo',
          },
        ],
      })
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST') {
      const body = JSON.parse(route.request().postData() ?? '{}') as Record<string, unknown>
      created.push(body)
      return route.fulfill({
        json: {
          id: `session-promotion-${created.length}`,
          title: 'Drafting requirement…',
          status: 'active',
          goal: 'requirement',
          ...body,
          workspace: 'demo',
        },
      })
    }
    if (url.pathname.startsWith('/v1/planning-sessions/') && url.pathname.endsWith('/messages'))
      return route.fulfill({ json: [] })
    if (url.pathname.startsWith('/v1/planning-sessions/')) {
      const body = created.at(-1) ?? {}
      return route.fulfill({
        json: {
          id: url.pathname.split('/')[3],
          title: 'Drafting requirement…',
          status: 'active',
          goal: 'requirement',
          ...body,
          workspace: 'demo',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: existing ? [existingRequirement] : [] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: existingRequirement })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: [existingRequirement.current_version] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  await page.getByRole('button', { name: 'Promote overview' }).click()
  await expect(page.getByRole('dialog', { name: 'Promote product overview' })).toBeVisible()
  await expect(page.getByLabel('Section').getByRole('option', { name: 'Hidden instruction' })).toHaveCount(0)
  await expect(page.getByLabel('Section').getByRole('option', { name: 'Four-space code' })).toHaveCount(0)
  await expect(page.getByLabel('Section').getByRole('option', { name: 'Tab-indented' })).toHaveCount(0)
  await expect(page.getByLabel('Section').getByRole('option', { name: 'Three spaces' })).toHaveCount(1)
  await page.getByRole('button', { name: 'Start promotion' }).click()
  await expect.poll(() => created.length).toBe(1)
  expect(created[0]).toMatchObject({
    goal: 'requirement',
    promotion: { document_id: 'ref-overview', version: 2, section_anchor: '#billing-rule', target_id: 'REQ-1' },
  })
  expect(created[0].requirement_context_id).toBeUndefined()

  existing = true
  await page.goto('/requirements?requirement=req-retries')
  await page.getByRole('button', { name: 'Promote overview' }).click()
  await page.getByLabel('Section').selectOption('#billing-rule-1')
  await page.getByLabel('Promotion target').fill('AC-1.2')
  await page.getByRole('button', { name: 'Start promotion' }).click()
  await expect.poll(() => created.length).toBe(2)
  expect(created[1]).toMatchObject({
    goal: 'requirement',
    requirement_context_id: 'req-retries',
    promotion: { document_id: 'ref-overview', version: 2, section_anchor: '#billing-rule-1', target_id: 'AC-1.2' },
  })
})

test('reference documents upload safely, load history on demand, compare both sides, and confirm deletion', async ({
  page,
}) => {
  await initShell(page)
  let versionRequests = 0
  let uploadRequests = 0
  let deleteRequests = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'GET')
      return route.fulfill({
        json: [{ id: 'ref-overview', name: 'Product overview', current_version: 2, workspace: 'demo' }],
      })
    if (url.pathname === '/v1/reference-documents' && route.request().method() === 'POST') {
      uploadRequests++
      if (uploadRequests === 2)
        return route.fulfill({ status: 400, body: 'reference document content is not Markdown' })
      return route.fulfill({ status: 201, json: {} })
    }
    if (url.pathname === '/v1/reference-documents/ref-overview/versions') {
      versionRequests++
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
  const overviewSummary = page.locator('summary').filter({ hasText: 'Product overview' })
  await expect(overviewSummary).toBeVisible()
  expect(versionRequests).toBe(0)

  const addInput = page.locator('label').filter({ hasText: 'Add Markdown' }).locator('input[type=file]')
  await addInput.setInputFiles({ name: 'new.md', mimeType: 'application/octet-stream', buffer: Buffer.from('# New') })
  await expect.poll(() => uploadRequests).toBe(1)
  await addInput.setInputFiles({ name: 'bad.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-1.7') })
  await expect(page.getByText('reference document content is not Markdown')).toBeVisible()

  await overviewSummary.click()
  await expect.poll(() => versionRequests).toBe(1)
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

// AC-5: finalizing in the sidebar refreshes the canvas in place — a produced
// requirement becomes the open document with its pending version and the
// unchanged confirmation affordance; a produced blueprint refreshes the
// proposed serves link on the document it serves.
test('finalizing in the sidebar refreshes the canvas without leaving the view', async ({ page }) => {
  await initShell(page)
  const drafted = {
    requirement: {
      id: 'req-drafted',
      slug: 'drafted-intent',
      title: 'Drafted intent',
      statement_high_water_mark: 1,
      workspace: 'demo',
      created_at: '2026-07-30T11:00:00Z',
      updated_at: '2026-07-30T11:00:00Z',
    },
    pending_versions: [
      {
        requirement_id: 'req-drafted',
        version: 1,
        content:
          'The drafted intent.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Drafted statements are stable.\n```',
        statements: [{ id: 'REQ-1', statement: 'Drafted statements are stable.' }],
        origin: 'chat',
        origin_session_id: 'session-draft',
        confirmed: false,
        workspace: 'demo',
        created_at: '2026-07-30T11:05:00Z',
      },
    ],
    serving_blueprints: [],
    planning_sessions: [],
    artifacts: [],
    lineage: [],
    migrated_seed: false,
    confirmation_eligible: true,
  }
  let finalized = false
  const session = () => ({
    id: 'session-draft',
    title: finalized ? 'Drafted intent' : 'Drafting requirement…',
    status: finalized ? 'finalized' : 'active',
    goal: 'requirement',
    produced_requirement_id: finalized ? 'req-drafted' : undefined,
    workspace: 'demo',
    created_at: '2026-07-30T11:00:00Z',
    updated_at: '2026-07-30T11:05:00Z',
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: finalized ? [drafted] : [] })
    if (url.pathname === '/v1/requirements/req-drafted') return route.fulfill({ json: drafted })
    if (url.pathname === '/v1/requirements/req-drafted/versions')
      return route.fulfill({ json: drafted.pending_versions })
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST')
      return route.fulfill({ json: session() })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session()] })
    if (url.pathname === '/v1/planning-sessions/session-draft') return route.fulfill({ json: session() })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      finalized = true
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: [
          'data: {"type":"tool-input-available","toolCallId":"call-1","toolName":"finalize_requirement"}',
          '',
          'data: {"type":"tool-output-available","toolCallId":"call-1","toolName":"finalize_requirement"}',
          '',
          'data: {"type":"finish","finishReason":"tool-calls"}',
          '',
        ].join('\n'),
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  const assistant = page.getByRole('complementary', {
    name: 'Planning assistant',
  })
  await assistant.getByRole('button', { name: 'Draft' }).click()
  await assistant.getByLabel('Planning message').fill('Capture the drafted intent.')
  await assistant.getByRole('button', { name: 'Send' }).click()

  // The produced document arrives on the canvas, pending, with the ordinary
  // confirmation affordance and no navigation away.
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Drafted intent' })).toBeVisible()
  await expect(canvas.getByText('A revision is pending operator confirmation.')).toBeVisible()
  await expect(canvas.getByRole('button', { name: 'Confirm version 1' })).toBeEnabled()
  await expect(page).toHaveURL(/requirement=req-drafted/)
  await expect(page).toHaveURL(/\/requirements\?/)
})

test('a produced requirement that never reaches the corpus releases the adoption latch with an actionable error', async ({
  page,
}) => {
  await initShell(page)
  await page.clock.install()
  let finalized = false
  const session = () => ({
    id: 'session-missing-draft',
    title: finalized ? 'Missing drafted intent' : 'Drafting requirement…',
    status: finalized ? 'finalized' : 'active',
    goal: 'requirement',
    produced_requirement_id: finalized ? 'req-missing' : undefined,
    workspace: 'demo',
    created_at: '2026-07-30T11:00:00Z',
    updated_at: '2026-07-30T11:05:00Z',
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST')
      return route.fulfill({ json: session() })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session()] })
    if (url.pathname === '/v1/planning-sessions/session-missing-draft') return route.fulfill({ json: session() })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      finalized = true
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: [
          'data: {"type":"tool-output-available","toolCallId":"call-1","toolName":"finalize_requirement"}',
          '',
          'data: {"type":"finish","finishReason":"tool-calls"}',
          '',
        ].join('\n'),
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  const assistant = page.getByRole('complementary', { name: 'Planning assistant' })
  await assistant.getByRole('button', { name: 'Draft' }).click()
  await assistant.getByLabel('Planning message').fill('Capture missing intent.')
  await assistant.getByRole('button', { name: 'Send' }).click()
  await expect(page).toHaveURL(/requirement=req-missing/)
  await page.clock.fastForward(10_001)
  await expect(page.getByText(/The new requirement req-missing did not appear in the corpus/)).toBeVisible()
})

// AC-5 (blueprint half): contextual Plan work refreshes the visible proposed
// serves link on the requirement it serves.
test('finalizing contextual plan work refreshes the proposed serves link', async ({ page }) => {
  await initShell(page)
  let served = false
  const view = () => ({
    ...requirement,
    serving_blueprints: served ? requirement.serving_blueprints : [],
    planning_sessions: [],
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [view()] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view() })
    if (url.pathname === '/v1/requirements/req-retries/versions')
      return route.fulfill({ json: requirement.pending_versions })
    const planned = {
      id: 'session-plan',
      title: served ? 'Ship bounded retries' : 'Planning work…',
      status: served ? 'finalized' : 'active',
      goal: 'blueprint',
      requirement_context_id: 'req-retries',
      produced_task_id: served ? 'blueprint-task' : undefined,
      workspace: 'demo',
      created_at: '2026-07-30T10:00:00Z',
      updated_at: '2026-07-30T10:00:00Z',
    }
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST')
      return route.fulfill({ json: planned })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [planned] })
    if (url.pathname === '/v1/planning-sessions/session-plan') return route.fulfill({ json: planned })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      served = true
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: 'data: {"type":"tool-output-available","toolCallId":"call-1","toolName":"finalize_blueprint"}\n\n',
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByText('No blueprint has been planned in this requirement’s context yet.')).toBeVisible()
  const assistant = page.getByRole('complementary', {
    name: 'Planning assistant',
  })
  await assistant.getByRole('button', { name: 'Plan work' }).click()
  await assistant.getByLabel('Planning message').fill('Plan the delivery.')
  await assistant.getByRole('button', { name: 'Send' }).click()
  await expect(canvas.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute(
    'href',
    '/blueprints/blueprint-task',
  )
  await expect(page).toHaveURL(/requirement=req-retries/)
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
  await expect(page.getByLabel('Planning goal').locator('option')).toHaveText(['Open exploration', 'Blueprint'])
  // Requirement drafting moved beside the document, so it is not offered here.
  await expect(page.getByLabel('Planning goal').locator('option')).toHaveCount(2)

  await page.goto('/requirements?requirement=req-retries')
  await expect(page.getByRole('region', { name: 'Requirement document' })).toBeVisible()
  // No contentEditable and no Markdown textarea: the only textarea on this
  // surface is the assistant's message composer (spec §21.57 change 2).
  await expect(page.locator('[contenteditable]')).toHaveCount(0)
  await expect(page.locator('textarea:not([aria-label="Planning message"])')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Requirement document' }).locator('textarea')).toHaveCount(0)
  // The assistant sidebar is that only authoring path.
  await expect(
    page.getByRole('complementary', { name: 'Planning assistant' }).getByRole('button', { name: 'Revise' }),
  ).toBeVisible()
})

// AC-5 regression: with a non-empty corpus, the "fall back to the first
// document" effect used to race the post-finalize navigation and strand the
// operator on an unrelated document, and the canvas kept displaying the older
// version because the newly proposed one was never selected.
test('a sidebar revision opens its new version on the canvas over an existing corpus', async ({ page }) => {
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
  const confirmedV1 = {
    ...requirement.pending_versions[0],
    version: 1,
    confirmed: true,
    content: 'Keep retries bounded.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop.\n```',
    statements: [{ id: 'REQ-1', statement: 'Retries stop.' }],
  }
  const pendingV2 = {
    ...requirement.pending_versions[0],
    version: 2,
    content:
      'Keep retries bounded and observable.\n\n```conveyor:requirements\n- id: REQ-2\n  statement: Retries are explainable.\n```',
    statements: [{ id: 'REQ-2', statement: 'Retries are explainable.' }],
  }
  let revised = false
  const retries = () => ({
    ...requirement,
    current_version: confirmedV1,
    pending_versions: revised ? [pendingV2] : [],
    planning_sessions: [],
    confirmation_eligible: true,
  })
  const session = () => ({
    id: 'session-revise',
    title: revised ? 'Retry behavior' : 'Drafting requirement…',
    status: revised ? 'finalized' : 'active',
    goal: 'requirement',
    requirement_context_id: 'req-retries',
    produced_requirement_id: revised ? 'req-retries' : undefined,
    workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:00:00Z',
  })
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    // "Other intent" sorts first, so a bounce lands there and is visible.
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [other, retries()] })
    if (url.pathname === '/v1/requirements/req-other') return route.fulfill({ json: other })
    if (url.pathname === '/v1/requirements/req-other/versions') return route.fulfill({ json: [confirmedV1] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: retries() })
    if (url.pathname === '/v1/requirements/req-retries/versions') {
      return route.fulfill({
        json: revised ? [confirmedV1, pendingV2] : [confirmedV1],
      })
    }
    if (url.pathname === '/v1/planning-sessions' && route.request().method() === 'POST')
      return route.fulfill({ json: session() })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session()] })
    if (url.pathname === '/v1/planning-sessions/session-revise') return route.fulfill({ json: session() })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      revised = true
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
        body: 'data: {"type":"tool-output-available","toolCallId":"call-1","toolName":"finalize_requirement"}\n\n',
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  const canvas = page.getByRole('region', { name: 'Requirement document' })
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toBeVisible()
  const assistant = page.getByRole('complementary', {
    name: 'Planning assistant',
  })
  await assistant.getByRole('button', { name: 'Revise' }).click()
  await assistant.getByLabel('Planning message').fill('Add the observability statement.')
  await assistant.getByRole('button', { name: 'Send' }).click()

  // The canvas stays on the revised document and advances to its new version,
  // with the diff and the unchanged confirmation affordance.
  await expect(canvas.getByRole('button', { name: 'Confirm version 2' })).toBeEnabled()
  await expect(canvas.getByText('Compared with confirmed v1')).toBeVisible()
  await expect(canvas.getByRole('button', { name: /Version 2/ })).toHaveAttribute('aria-current', 'true')
  await expect(page).toHaveURL(/requirement=req-retries/)
  await expect(canvas.getByRole('heading', { name: 'Other intent' })).toHaveCount(0)
})

// A deep link names the document to open; restoring an earlier finalized
// session beside it must not navigate the canvas somewhere else.
test('a deep link to a finalized session keeps the document the URL asked for', async ({ page }) => {
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
  await expect(
    page.getByRole('complementary', { name: 'Planning assistant' }).getByText('Planning artifact finalized'),
  ).toBeVisible()
  // The already-finalized session is history, not news: the canvas stays put.
  await expect(page).toHaveURL(/requirement=req-other/)
  await expect(canvas.getByRole('heading', { name: 'Retry behavior' })).toHaveCount(0)
})
