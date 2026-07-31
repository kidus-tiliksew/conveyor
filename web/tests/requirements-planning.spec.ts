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
  planning_sessions: [],
  artifacts: [],
  lineage: [{
    id: 1, task_id: '', kind: 'requirement.version_proposed',
    actor_id: 'planner', actor_role: 'system', payload: {},
    at: '2026-07-30T10:05:00Z',
  }],
  stale: true,
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
  await expect(page.getByRole('link', { name: /Ship bounded retries/ })).toHaveAttribute('href', '/tasks/blueprint-task')

  await page.getByRole('button', { name: 'Confirm version 1' }).click()
  await expect.poll(() => confirmed).toBe(true)

  await page.getByRole('button', { name: 'Plan work' }).click()
  await expect(page).toHaveURL(/\/planning$/)
  await expect(page.getByLabel('Planning session title')).toHaveAttribute('placeholder', 'Plan work for Retry behavior')
})

test('planning restores durable messages, tool markers, and streams a new turn', async ({ page }) => {
  await initShell(page)
  let posted = ''
  const session = {
    id: 'session-retries', title: 'Plan retry behavior', status: 'active',
    workspace: 'demo', created_at: '2026-07-30T10:00:00Z', updated_at: '2026-07-30T10:10:00Z',
  }
  const messages = [
    {
      session_id: session.id, seq: 1, role: 'user', content: 'Plan bounded retries.',
      workspace: 'demo', created_at: '2026-07-30T10:01:00Z',
    },
    {
      session_id: session.id, seq: 2, role: 'assistant', content: 'I found the approved queue contract.',
      parts: [{ type: 'tool-output-available', toolName: 'read_approved_spec', state: 'output-available' }],
      workspace: 'demo', created_at: '2026-07-30T10:02:00Z',
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

  await page.getByLabel('Planning message').fill('Draft a requirement with stable statements.')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => JSON.parse(posted).message.content).toBe('Draft a requirement with stable statements.')
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
