import { expect, type Page, test } from '@playwright/test'

// Stateful local fixture for the operator grant disclosure
// (component-web-task-surfaces). The browser proves request composition and
// presentation; HTTP and store suites own authorization.

const at = '2026-10-01T10:00:00Z'
const sha = 'a'.repeat(40)
const digest = 'f'.repeat(64)
const networkSubject = { kind: 'ordinary', obligation_id: 'network', contract_digest: digest }
const emptySubject = { kind: 'ordinary', obligation_id: 'ordinary', contract_digest: 'e'.repeat(64) }

type Options = {
  role?: string
  eligibility?: string
  contextOnRefresh?: string
  refuse?: boolean
  existingGrant?: boolean
}

function contextView(id: string) {
  return {
    id,
    work_order_attempt_id: 'attempt-1',
    revisions: [{ repository: 'conveyor', remote_identity: 'https://github.com/org/conveyor', sha }],
    governing_pins: [],
    sealed: false,
    created_at: at,
  }
}

function grantView(id: string, revoked = false) {
  return {
    id,
    request_key: id,
    context_id: 'ctx-1',
    work_order_id: 'verify-1',
    work_order_attempt_id: 'attempt-1',
    subject: networkSubject,
    actions: [{ kind: 'network', binding: 'api', target: 'https://api.example.test:443' }],
    contract_digest: 'c'.repeat(64),
    revisions: [{ repository: 'conveyor', remote_identity: 'https://github.com/org/conveyor', sha }],
    actor: 'user:operator',
    created_at: at,
    ...(revoked
      ? {
          revocation: {
            id: 'rev-1',
            request_key: 'rev',
            reason: 'Wrong origin',
            actor: 'user:operator',
            created_at: at,
          },
        }
      : {}),
  }
}

async function fixture(page: Page, options: Options = {}) {
  const posts: Record<string, unknown>[] = []
  const gets: URL[] = []
  const grants: ReturnType<typeof grantView>[] = options.existingGrant ? [grantView('ctx-1:existing')] : []
  let refreshed = false
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const task = {
    id: 'grant-fixture',
    workspace: 'demo',
    source: 'mcp',
    title: 'Grant fixture',
    body: 'Issue an operator grant.',
    class: 'feature',
    level: '',
    spec_approval: false,
    merge_approval: true,
    policy_version: 1,
    policy_contract: {
      verify_stage: true,
      max_bounces: 10,
      stage_timeouts: { spec: '30m', implement: '4h', verify: '1h', review: '1h' },
      review: { seats: [{}] },
      refresh_review: 'delta',
    },
    repo: 'conveyor',
    base_branch: 'main',
    branch: 'conveyor/task-grant',
    state: 'running',
    next_stage: 'verify',
    reviewed_head_sha: sha,
    created_at: at,
  }
  const item = {
    task,
    jobs: [
      {
        id: 'verify-1',
        task_id: task.id,
        stage: 'verify',
        harness: 'codex',
        model_tier: 'operator-owned',
        runner: 'mcp',
        confinement: 'none',
        tokens_in: 0,
        tokens_out: 0,
        state: 'running',
        started_at: at,
      },
    ],
    events: [],
    interventions: [],
    checkout_available: true,
    checkout_guidance: '',
    needs_attention: false,
    at_merge_gate: false,
    attachments: [],
    verification_evidence: [],
    work_orders: [
      {
        id: 'verify-1',
        task_id: task.id,
        job_id: 'verify-1',
        stage: 'verify',
        state: 'claimed',
        claimed_by: 'worker-verifier',
        created_at: at,
        updated_at: at,
        execution_started_at: at,
      },
    ],
    spec: { task_id: task.id, version: 1, content: 'Grant fixture.', approved: true, created_at: at },
  }
  const view = (contextId: string) => {
    const state = options.eligibility ?? 'eligible'
    return {
      task_id: task.id,
      work_order_id: 'verify-1',
      order_state: 'claimed',
      work_order_attempt_id: 'attempt-1',
      observed_at: at,
      lease_expires_at: '2026-10-01T10:05:00Z',
      execution_deadline: '2026-10-01T11:00:00Z',
      submitted_head: sha,
      context: contextView(contextId),
      eligibility:
        state === 'eligible'
          ? { state }
          : {
              state,
              reason: "The verifier's claim lease or execution deadline has passed.",
              recovery: 'Recover the verify order. The next verifier claim prepares a new context.',
            },
      subjects: [
        {
          subject: emptySubject,
          description: 'Ordinary check with no actions',
          exercise_kind: 'script',
          permissions: [],
          action_requirements: [],
          prerequisites: [],
          inputs: [],
          retry_policy: 'safe_to_replay',
          grant_ids: [],
        },
        {
          subject: networkSubject,
          description: 'Reads the fixture API',
          exercise_kind: 'script',
          permissions: [{ kind: 'network', target_binding: 'api' }],
          action_requirements: [{ kind: 'network', binding: 'api', required: true }],
          prerequisites: [],
          inputs: [{ name: 'case', type: 'string', required: true, sensitive: false }],
          retry_policy: 'safe_to_replay',
          grant_ids: grants.map((g) => g.id),
        },
      ],
      grants,
    }
  }
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'operator', role: options.role ?? 'operator' } })
    if (path === '/v1/activity')
      return route.fulfill({ json: [{ task, latest_stage: 'verify', last_event_at: at, needs_attention: false }] })
    if (path.endsWith('/activity')) return route.fulfill({ json: item })
    if (path.endsWith('/events/stream')) return new Promise(() => {})
    if (path === '/v1/work-orders/verify-1/verification/permissions') {
      if (request.method() === 'POST') {
        const body = request.postDataJSON() as Record<string, unknown>
        posts.push(body)
        if (options.refuse)
          return route.fulfill({
            status: 409,
            json: {
              error: "The verifier's claim lease or execution deadline has passed.",
              reason: 'claim_expired',
              recovery: 'Recover the verify order.',
            },
          })
        if (body.revoke_grant_id) {
          const index = grants.findIndex((g) => g.id === body.revoke_grant_id)
          grants[index] = grantView(grants[index].id, true)
          return route.fulfill({ json: { ID: 'rev-1' } })
        }
        grants.push(grantView(`ctx-1:${body.request_key}`))
        return route.fulfill({ json: { ID: `ctx-1:${body.request_key}` } })
      }
      gets.push(url)
      const grantId = url.searchParams.get('grant_id')
      if (grantId)
        return route.fulfill({
          json: { ...view('ctx-1'), subjects: [], grants: grants.filter((g) => g.id === grantId) },
        })
      const contextId = refreshed && options.contextOnRefresh ? options.contextOnRefresh : 'ctx-1'
      if (gets.length > 1) refreshed = true
      return route.fulfill({ json: view(contextId) })
    }
    if (path.endsWith('/verification'))
      return route.fulfill({
        json: {
          head_sha: sha,
          current_context_id: 'ctx-1',
          contexts: {
            items: [
              {
                id: 'ctx-1',
                context_id: 'ctx-1',
                run_id: '',
                state: 'open',
                at,
                metadata: {
                  source_sha: sha,
                  work_order_id: 'verify-1',
                  claimant: 'worker-verifier',
                  attempt_count: '0',
                },
              },
            ],
          },
          overview: {},
        },
      })
    if (path.includes('/verification/')) return route.fulfill({ json: { items: [] } })
    return route.fulfill({ json: [] })
  })
  return { posts, gets, grants }
}

const entry = (page: Page) => page.getByRole('article', { name: 'Verification', exact: true })

async function openPermissions(page: Page) {
  await page.goto('/tasks/grant-fixture/full')
  const toggle = entry(page).getByRole('button', { name: 'Permissions' })
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await toggle.click()
  await expect(toggle).toHaveAttribute('aria-expanded', 'true')
  return entry(page).getByRole('region', { name: 'Verification permissions' })
}

for (const width of [1366, 390]) {
  test(`operator reviews and issues one exact grant at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const { posts, gets } = await fixture(page)
    const panel = await openPermissions(page)
    await expect(panel.getByText('Grants can be issued')).toBeVisible()
    await expect(panel).toContainText(digest)
    await expect(panel).toContainText('Lease expires')
    await expect(panel).toContainText('Deadline')
    // Opening the disclosure reads the projection only.
    expect(posts).toHaveLength(0)
    const network = panel.locator('div.rounded-md').filter({ hasText: 'ordinary:network' }).first()
    await network.getByRole('button', { name: 'Select' }).click()
    const editor = panel.getByRole('region', { name: 'Grant request' })
    await expect(editor.getByText('Unresolved')).toBeVisible()
    const review = editor.getByRole('button', { name: 'Review request' })
    await expect(review).toBeDisabled()
    await editor.getByLabel('Target').fill('https://api.example.test')
    await review.click()
    const exact = editor.locator('pre')
    await expect(exact).toContainText(digest)
    await expect(exact).toContainText('"target": "https://api.example.test"')
    expect(posts).toHaveLength(0)
    const before = gets.length
    await editor.getByRole('button', { name: 'Issue grant' }).click()
    const receipt = panel.getByRole('status', { name: 'Grant receipt' })
    await expect(receipt).toContainText('Grant issued and read back')
    await expect(receipt).toContainText('https://api.example.test:443')
    expect(posts).toHaveLength(1)
    expect(posts[0]).toMatchObject({
      context_id: 'ctx-1',
      subject: networkSubject,
      actions: [{ kind: 'network', binding: 'api', target: 'https://api.example.test' }],
    })
    expect(Object.keys(posts[0]).sort()).toEqual(['actions', 'context_id', 'request_key', 'subject'])
    // The projection is refreshed before the POST and the receipt is read back by ID.
    const after = gets.slice(before)
    expect(after.some((url) => !url.searchParams.get('grant_id'))).toBe(true)
    expect(after.some((url) => url.searchParams.get('grant_id') === `ctx-1:${posts[0].request_key}`)).toBe(true)
    expect(after.every((url) => url.searchParams.get('workspace_id') === 'demo')).toBe(true)
    const hasOverflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)
    expect(hasOverflow).toBe(false)
  })
}

test('subject without actions grants an explicit empty list', async ({ page }) => {
  const { posts } = await fixture(page)
  const panel = await openPermissions(page)
  await panel
    .locator('div.rounded-md')
    .filter({ hasText: 'ordinary:ordinary' })
    .first()
    .getByRole('button', { name: 'Select' })
    .click()
  const editor = panel.getByRole('region', { name: 'Grant request' })
  await expect(editor).toContainText('explicit empty list')
  await editor.getByRole('button', { name: 'Review request' }).click()
  await editor.getByRole('button', { name: 'Issue grant' }).click()
  await expect.poll(() => posts.length).toBe(1)
  expect(posts[0]).toMatchObject({ subject: emptySubject, actions: [] })
})

test('viewer sees no permissions disclosure', async ({ page }) => {
  const { gets } = await fixture(page, { role: 'viewer' })
  await page.goto('/tasks/grant-fixture/full')
  await expect(entry(page)).toBeVisible()
  await expect(entry(page).getByRole('button', { name: 'Permissions' })).toHaveCount(0)
  expect(gets).toHaveLength(0)
})

test('expired claim disables issuance and shows recovery', async ({ page }) => {
  const { posts } = await fixture(page, { eligibility: 'claim_expired' })
  const panel = await openPermissions(page)
  await expect(panel).toContainText('claim_expired')
  await expect(panel).toContainText('Recover the verify order')
  for (const select of await panel.getByRole('button', { name: 'Select' }).all()) await expect(select).toBeDisabled()
  expect(posts).toHaveLength(0)
})

test('context change before issue sends nothing and clears the selection', async ({ page }) => {
  const { posts } = await fixture(page, { contextOnRefresh: 'ctx-2' })
  const panel = await openPermissions(page)
  await panel
    .locator('div.rounded-md')
    .filter({ hasText: 'ordinary:network' })
    .first()
    .getByRole('button', { name: 'Select' })
    .click()
  const editor = panel.getByRole('region', { name: 'Grant request' })
  await editor.getByLabel('Target').fill('https://api.example.test')
  await editor.getByRole('button', { name: 'Review request' }).click()
  await editor.getByRole('button', { name: 'Issue grant' }).click()
  await expect(panel.getByRole('region', { name: 'Grant request' })).toHaveCount(0)
  await expect(panel).toContainText('ctx-2')
  expect(posts).toHaveLength(0)
})

test('server refusal mid-flow shows its reason and recovery', async ({ page }) => {
  const { posts } = await fixture(page, { refuse: true })
  const panel = await openPermissions(page)
  await panel
    .locator('div.rounded-md')
    .filter({ hasText: 'ordinary:network' })
    .first()
    .getByRole('button', { name: 'Select' })
    .click()
  const editor = panel.getByRole('region', { name: 'Grant request' })
  await editor.getByLabel('Target').fill('https://api.example.test')
  await editor.getByRole('button', { name: 'Review request' }).click()
  await editor.getByRole('button', { name: 'Issue grant' }).click()
  const alert = panel.getByRole('alert')
  await expect(alert).toContainText('claim_expired')
  await expect(alert).toContainText('Recover the verify order.')
  expect(posts).toHaveLength(1)
})

test('revocation requires a reason and shows the retained revocation', async ({ page }) => {
  const { posts } = await fixture(page, { existingGrant: true })
  const panel = await openPermissions(page)
  const grants = panel.getByRole('region', { name: 'Grants' })
  await expect(grants.getByText('Active')).toBeVisible()
  const revoke = grants.getByRole('button', { name: 'Revoke' })
  await revoke.click()
  await expect(revoke).toHaveAttribute('aria-expanded', 'true')
  const confirm = grants.getByRole('button', { name: 'Confirm revocation' })
  await expect(confirm).toBeDisabled()
  await grants.getByLabel('Revocation reason').fill('Wrong origin')
  await confirm.click()
  await expect(grants.locator('span', { hasText: /^Revoked$/ })).toBeVisible()
  await expect(grants).toContainText('Wrong origin')
  expect(posts).toEqual([
    expect.objectContaining({ context_id: 'ctx-1', revoke_grant_id: 'ctx-1:existing', reason: 'Wrong origin' }),
  ])
})

test('workspace change clears the open disclosure and selection', async ({ page }) => {
  await fixture(page)
  const panel = await openPermissions(page)
  await panel
    .locator('div.rounded-md')
    .filter({ hasText: 'ordinary:network' })
    .first()
    .getByRole('button', { name: 'Select' })
    .click()
  await expect(panel.getByRole('region', { name: 'Grant request' })).toBeVisible()
  const rail = page.getByRole('navigation', { name: 'Workspaces' })
  // Switching workspace leaves the task route; the disclosure and its draft
  // request unmount with it.
  await rail.getByRole('button', { name: 'Switch to Other' }).click()
  await expect(page.getByRole('region', { name: 'Verification permissions' })).toHaveCount(0)
  await rail.getByRole('button', { name: 'Switch to Demo' }).click()
  await expect.poll(() => page.evaluate(() => localStorage.getItem('conveyor-workspace'))).toBe('demo')
  await page.goto('/tasks/grant-fixture/full')
  const toggle = entry(page).getByRole('button', { name: 'Permissions' })
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await toggle.click()
  await expect(entry(page).getByRole('region', { name: 'Grant request' })).toHaveCount(0)
})
