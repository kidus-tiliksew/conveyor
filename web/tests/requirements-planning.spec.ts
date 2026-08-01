import { expect, test, type Page, type Route } from '@playwright/test'

const requirement = {
  requirement: {
    id: 'req-retries', slug: 'retry-behavior', title: 'Retry behavior',
    statement_high_water_mark: 1, workspace: 'demo',
    created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z',
  },
  pending_versions: [{
    requirement_id: 'req-retries', version: 1,
    content: 'Keep retries bounded.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop after a finite limit.\n```',
    statements: [{ id: 'REQ-1', statement: 'Retries stop after a finite limit.' }],
    origin: 'chat', origin_session_id: 'session-requirement', confirmed: false,
    workspace: 'demo', created_at: '2026-07-30T10:05:00Z',
  }],
  serving_blueprints: [{
    task: { id: 'blueprint-task', title: 'Ship bounded retries', state: 'awaiting_human' },
    spec: { task_id: 'blueprint-task', version: 1, approved: false },
    events: [],
  }],
  planning_sessions: [{
    id: 'session-requirement', title: 'Plan retry behavior', status: 'finalized',
    produced_requirement_id: 'req-retries',
    model: 'gpt-plan', effort: 'high', exploration_output_tokens: 12000,
    primary_repo: 'conveyor', pinned_revisions: { conveyor: '0123456789abcdef' },
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:05:00Z', finalized_at: '2026-07-30T10:05:00Z',
  }],
  artifacts: [],
  lineage: [{
    id: 1, task_id: '', kind: 'requirement.version_proposed',
    actor_id: 'planner', actor_role: 'system', payload: {},
    at: '2026-07-30T10:05:00Z',
  }],
  shipped_past_intent: 'blueprint-task',
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
        planning: { model: 'gpt-plan', effort: 'high', timeout: '30m', exploration_output_tokens: 12000 },
      },
    },
    routing: { stages: { review: {} } },
    review: { seats: [] },
    setups: [],
    default_setup: '',
    execution: {},
    harnesses: [],
    repos: [
      { name: 'conveyor', url: 'https://github.com/kidus-tiliksew/conveyor', github: 'kidus-tiliksew/conveyor', base: 'main' },
      { name: 'companion', url: 'https://github.com/example/companion', github: 'example/companion', base: 'main' },
    ],
  },
}

async function initShell(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
}

function shellResponse(route: Route) {
  const path = new URL(route.request().url()).pathname
  if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
  if (path === '/v1/workspace/config') return route.fulfill({ json: planningConfig })
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
  if (path === '/v1/activity') return route.fulfill({ json: [] })
}

test('requirements renders living intent, confirms a revision, and opens planning in context', async ({ page }) => {
  await initShell(page)
  let confirmed = false
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [requirement] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: requirement })
    if (url.pathname === '/v1/requirements/req-retries/versions') return route.fulfill({ json: requirement.pending_versions })
    if (url.pathname === '/v1/requirements/req-retries/versions/1/confirm') {
      confirmed = true
      return route.fulfill({ json: { requirement: requirement.requirement, version: requirement.pending_versions[0] } })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements')
  await expect(page.getByRole('heading', { name: 'Requirements' })).toBeVisible()
  await expect(page.getByText('Retry behavior', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('Needs confirmation')).toBeVisible()
  await expect(page.getByText('Feature tree')).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute('href', '/blueprints/blueprint-task')
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()

  await page.getByRole('button', { name: 'Confirm version 1' }).click()
  await expect.poll(() => confirmed).toBe(true)

  await page.getByRole('button', { name: 'Plan work' }).click()
  await expect(page).toHaveURL(/\/planning$/)
  await expect(page.getByLabel('Planning session title')).toHaveAttribute('placeholder', 'Plan work for Retry behavior')
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
          id: 'session-new', title: createdWith.title, status: 'active',
          model: createdWith.model, effort: 'high', exploration_output_tokens: 12000,
          primary_repo: 'conveyor', pinned_revisions: { conveyor: '0123456789abcdef' },
          workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z',
        },
      })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [] })
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await expect(page.getByLabel('Planning model')).toHaveValue('gpt-plan')
  await expect(page.getByLabel('Planning model').locator('option')).toHaveText(['gpt-plan', 'gpt-plan-fast'])
  await page.getByLabel('Planning session title').fill('Explore companion changes')
  await page.getByLabel('Planning model').selectOption('gpt-plan-fast')
  await page.getByRole('button', { name: 'New session' }).click()
  await expect.poll(() => createdWith).toMatchObject({
    title: 'Explore companion changes',
    model: 'gpt-plan-fast',
  })
})

test('planning restores durable messages, tool markers, and streams a new turn', async ({ page }) => {
  await initShell(page)
  let posted = ''
  const session = {
    id: 'session-retries', title: 'Plan retry behavior', status: 'active',
    model: 'gpt-plan', effort: 'high', exploration_output_tokens: 12000,
    primary_repo: 'conveyor', pinned_revisions: { conveyor: '0123456789abcdef' },
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:10:00Z',
  }
  const messages = [
    {
      session_id: session.id, seq: 1, role: 'user', content: 'Plan bounded retries.',
      workspace: 'demo', created_at: '2026-07-30T10:01:00Z',
    },
    {
      session_id: session.id, seq: 2, role: 'assistant', content: 'I found the approved queue contract.',
      parts: [{ type: 'tool-input-available', toolName: 'read_approved_spec', toolCallId: 'call-1', input: { task_id: 'task-1' } }],
      workspace: 'demo', created_at: '2026-07-30T10:02:00Z',
    },
    {
      session_id: session.id, seq: 3, role: 'tool', content: '',
      parts: [{ type: 'tool-output-available', toolCallId: 'call-1', output: { title: 'Queue contract' } }],
      workspace: 'demo', created_at: '2026-07-30T10:02:01Z',
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
      posted = route.request().postData() ?? ''
      return route.fulfill({
        status: 200,
        headers: { 'Content-Type': 'text/event-stream', 'X-Vercel-AI-UI-Message-Stream': 'v1' },
        body: [
          'data: {"type":"start"}',
          '',
          'data: {"type":"text-delta","delta":"Drafting the requirement."}',
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
  await expect(page.getByLabel('read_approved_spec: complete')).toHaveCount(1)
  await expect(page.getByText(/Queue contract/)).toHaveCount(0)
  await expect(page.getByText('gpt-plan · high')).toBeVisible()
  await expect(page.getByText('12,000 tokens/call')).toBeVisible()
  await expect(page.getByText('conveyor@0123456789ab')).toBeVisible()

  await page.getByLabel('Planning message').fill('Draft a requirement with stable statements.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => JSON.parse(posted).message.content).toBe('Draft a requirement with stable statements.')
})

test('requirements deep-link exact versions, render statements, diff pending intent, and guard confirmation', async ({ page }) => {
  await initShell(page)
  const confirmed = {
    ...requirement.pending_versions[0], version: 1, confirmed: true,
    confirmed_at: '2026-07-30T10:06:00Z', content: 'Keep retries bounded.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop.\n```',
    statements: [{ id: 'REQ-1', statement: 'Retries stop.' }],
  }
  const pending2 = {
    ...requirement.pending_versions[0], version: 2,
    content: 'Keep retries bounded and observable.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Retries stop after a finite limit.\n```',
  }
  const pending3 = { ...pending2, version: 3, origin: 'drift_amendment', origin_session_id: undefined, origin_drift_id: 'drift-1' }
  const view = { ...requirement, current_version: confirmed, pending_versions: [pending2, pending3], migrated_seed: false, confirmation_eligible: true }
  let ifMatch = ''
  let confirmedVersion = 0
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [view] })
    if (url.pathname === '/v1/requirements/req-retries') return route.fulfill({ json: view })
    if (url.pathname === '/v1/requirements/req-retries/versions') return route.fulfill({ json: [confirmed, pending2, pending3] })
    const confirmation = /\/versions\/(\d+)\/confirm$/.exec(url.pathname)
    if (confirmation) {
      confirmedVersion = Number(confirmation[1])
      ifMatch = route.request().headers()['if-match'] ?? ''
      return route.fulfill({ json: { requirement: view.requirement, version: pending2 } })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/requirements?requirement=req-retries')
  await expect(page).toHaveURL(/requirement=req-retries/)
  await expect(page.getByRole('button', { name: /Version 2/ })).toBeVisible()
  await page.getByRole('button', { name: /Version 2/ }).click()
  await expect(page.getByText('Compared with confirmed v1')).toBeVisible()
	await expect(page.locator('.bg-failure-soft').filter({ hasText: 'Keep retries bounded.' })).toBeVisible()
	await expect(page.locator('.bg-positive-soft').filter({ hasText: 'Keep retries bounded and observable.' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Requirement statements' }).getByText('REQ-1')).toBeVisible()
  await expect(page.getByText('conveyor:requirements')).toHaveCount(0)
  await page.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect.poll(() => confirmedVersion).toBe(2)
  expect(ifMatch).toBe('"1"')
})

test('migrated seeds explain disabled confirmation and requirement switches open the latest version', async ({ page }) => {
	await initShell(page)
	const seedVersion = { ...requirement.pending_versions[0], origin: 'feature_migration', origin_session_id: undefined }
	const migrated = { ...requirement, requirement: { ...requirement.requirement, title: 'Migrated intent' }, pending_versions: [seedVersion], migrated_seed: true, confirmation_eligible: false, shipped_past_intent: undefined }
	const secondV1 = { ...requirement.pending_versions[0], requirement_id: 'req-second', version: 1, content: 'Earlier second document.' }
	const secondV2 = { ...secondV1, version: 2, content: 'Latest second document.' }
	const second = {
	  ...requirement,
	  requirement: { ...requirement.requirement, id: 'req-second', slug: 'second-intent', title: 'Second intent' },
	  pending_versions: [secondV1, secondV2], migrated_seed: false, confirmation_eligible: true, shipped_past_intent: undefined,
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

test('planning keeps partial text through malformed and error stream frames and resolves pending markers', async ({ page }) => {
  await initShell(page)
  const session = {
    id: 'session-stream-error', title: 'Stream recovery', status: 'active',
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z',
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
          'data: not-json', '',
          'data: {"type":"tool-input-available","toolCallId":"call-1","toolName":"read_requirement"}', '',
          'data: {"type":"text-delta","delta":"I found part of the answer."}', '',
          'data: {"type":"error","errorText":"Planning request failed. Please retry."}', '',
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

test('planning explains run conflicts, uses a disabled default model fallback, and surfaces abandon failures', async ({ page }) => {
  await initShell(page)
  const session = {
    id: 'session-conflict', title: 'Conflict recovery', status: 'active',
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z',
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] })
    if (url.pathname === '/v1/workspace/config') return route.fulfill({ json: { ...planningConfig, document: { ...planningConfig.document, planning_models: [] } } })
    if (url.pathname === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (url.pathname === '/v1/activity' || url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}/abandon`) return route.fulfill({ status: 409, body: 'The session was finalized elsewhere.' })
    if (url.pathname === `/v1/planning-sessions/${session.id}`) return route.fulfill({ json: session })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) return route.fulfill({ status: 409, body: 'planning session already has a message in progress' })
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
    id: 'session-attachment', title: 'Attachment planning', status: 'active',
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z',
  }
  let posted: { message?: { content?: string; parts?: Array<Record<string, unknown>> } } = {}
  let uploadBody = ''
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
    if (url.pathname === '/v1/artifacts') {
      uploadBody = route.request().postData() ?? ''
      return route.fulfill({ status: 201, json: { id: 'artifact-1', workspace: 'demo', name: 'context.txt', content_type: 'text/plain', size_bytes: 7, role: 'task_context', planning_session_id: session.id, created_at: '2026-07-30T10:00:00Z' } })
    }
    if (url.pathname === '/v1/planning-sessions') return route.fulfill({ json: [session] })
    if (url.pathname === `/v1/planning-sessions/${session.id}`) return route.fulfill({ json: session })
    if (url.pathname.endsWith('/messages') && route.request().method() === 'GET') return route.fulfill({ json: [] })
    if (url.pathname.endsWith('/messages')) {
      posted = JSON.parse(route.request().postData() ?? '{}') as typeof posted
      return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: 'data: [DONE]\n\n' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/planning')
  await page.locator('input[type=file]').setInputFiles({ name: 'context.txt', mimeType: 'text/plain', buffer: Buffer.from('context') })
  await expect(page.getByText('context.txt')).toBeVisible()
  expect(uploadBody).toContain('planning_session_id')
  expect(uploadBody).toContain(session.id)
  await page.getByLabel('Planning message').fill('Use this context.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => posted.message?.parts?.length ?? 0).toBe(2)
  expect(posted.message?.content).toBe('Use this context.')
  expect(posted.message?.content).not.toContain('Attached context')
  expect(posted.message?.parts?.[1]).toMatchObject({ type: 'file', artifactId: 'artifact-1', filename: 'context.txt' })
})

test('planning restores the selected session independently in each workspace', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-planning-session:demo', 'session-demo')
    localStorage.setItem('conveyor-planning-session:beta', 'session-beta')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  const sessions = {
    demo: [{ id: 'session-demo', title: 'Demo session', status: 'active', workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z' }],
    beta: [{ id: 'session-beta', title: 'Beta session', status: 'active', workspace: 'beta', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:00:00Z' }],
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const workspace = url.searchParams.get('workspace_id') === 'beta' ? 'beta' : 'demo'
    if (url.pathname === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }, { id: 'beta', name: 'Beta', config_version: 1 }] })
    if (url.pathname === '/v1/workspace/config') return route.fulfill({ json: { ...planningConfig, document: { ...planningConfig.document, workspace } } })
    if (url.pathname === '/v1/workspace') return route.fulfill({ json: { workspace, repos: ['conveyor'] } })
    if (url.pathname === '/v1/activity' || url.pathname === '/v1/blueprints' || url.pathname === '/v1/requirements') return route.fulfill({ json: [] })
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
    if (path === '/v1/requirements') return route.fulfill({ status: 500, body: 'Requirement service is unavailable.' })
    if (path === '/v1/planning-sessions') return route.fulfill({ status: 500, body: 'Planning history is unavailable.' })
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
    id: 'session-blueprint', title: 'Plan delivery', status: 'finalized',
    produced_task_id: 'blueprint-task', transcript_artifact_id: 'artifact-1',
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z',
    updated_at: '2026-07-30T10:10:00Z', finalized_at: '2026-07-30T10:10:00Z',
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
