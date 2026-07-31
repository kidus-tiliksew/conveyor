import { expect, test, type Page, type Route } from '@playwright/test'

// Blueprint presentation surface (spec §21.49). The anchor is an intent
// artifact: it never appears on the board, it reads in blueprint vocabulary,
// and its detail shows none of the affordances that imply a worker will pick
// it up.

const createdAt = '2026-07-30T09:00:00Z'
const anchorId = 'blueprint-anchor'

const childStates = {
  'child-sub-1': 'merged',
  'child-sub-2': 'running',
  'child-sub-3': 'queued',
  'child-sub-4': 'closed',
} as const

const childTitles = {
  'child-sub-1': 'Persist the contract',
  'child-sub-2': 'Run the planning loop',
  'child-sub-3': 'Present the delivery',
  'child-sub-4': 'Retire the feature tree',
} as const

function childRelations() {
  return [
    { id: 'child-sub-1', title: childTitles['child-sub-1'], state: childStates['child-sub-1'], origin_spec_version: 1, origin_sub_id: 'SUB-1' },
    { id: 'child-sub-2', title: childTitles['child-sub-2'], state: childStates['child-sub-2'], origin_spec_version: 1, origin_sub_id: 'SUB-2' },
    { id: 'child-sub-3', title: childTitles['child-sub-3'], state: childStates['child-sub-3'], origin_spec_version: 1, origin_sub_id: 'SUB-3' },
    { id: 'child-sub-4', title: childTitles['child-sub-4'], state: childStates['child-sub-4'], origin_spec_version: 1, origin_sub_id: 'SUB-4' },
  ]
}

function anchorTask() {
  return {
    id: anchorId, workspace: 'demo', source: 'planning', title: 'Deliver the planning flow',
    body: 'Deliver confirmed planning intent through the ordinary spec gate.',
    class: 'feature', level: '', spec_approval: true, merge_approval: true, policy_version: 1,
    setup: 'default', repo: 'conveyor', base_branch: 'main', branch: 'conveyor/task-blueprint-anchor',
    state: 'queued', next_stage: 'implement', children: childRelations(), created_at: createdAt,
  }
}

function childTask(id: keyof typeof childStates) {
  return {
    id, workspace: 'demo', source: `blueprint:${anchorId}@v1#SUB-1`, title: childTitles[id],
    body: '', class: 'feature', level: '', spec_approval: true, merge_approval: true, policy_version: 1,
    setup: 'default', repo: 'conveyor', base_branch: 'main', branch: `conveyor/task-${id}`,
    state: childStates[id], next_stage: 'implement', parent_task_id: anchorId,
    origin_spec_version: 1, origin_sub_id: 'SUB-1', created_at: createdAt,
  }
}

const specContent = [
  '# Blueprint planning surface',
  '',
  '## Intent',
  '',
  'Present blueprint anchors as intent artifacts rather than pipeline work.',
].join('\n')

function blueprintView() {
  return {
    task: anchorTask(),
    spec: {
      task_id: anchorId, version: 3, content: specContent, acceptance_count: 1,
      acceptance: [{ id: 'AC-1', criterion: 'The board excludes the anchor.', verify: 'test' }],
      decomposition: [
        { id: 'SUB-1', repo: 'conveyor', summary: 'Persist the contract', depends_on: [] },
        { id: 'SUB-2', repo: 'conveyor', summary: 'Run the planning loop', depends_on: ['SUB-1'] },
        { id: 'SUB-3', repo: 'conveyor', summary: 'Present the delivery', depends_on: ['SUB-2'] },
        { id: 'SUB-4', repo: 'conveyor', summary: 'Retire the feature tree', depends_on: [] },
      ],
      approved: true, created_at: createdAt, approved_at: createdAt,
    },
    governing_version: 3,
    // Deliberately server-ordered by dependency: SUB-3 declares after SUB-2.
    children: [
      { ...childRelations()[0], repo: 'conveyor', summary: 'Persist the contract', depends_on: [] },
      { ...childRelations()[1], repo: 'conveyor', summary: 'Run the planning loop', depends_on: ['SUB-1'] },
      { ...childRelations()[2], repo: 'conveyor', summary: 'Present the delivery', depends_on: ['SUB-2'] },
      { ...childRelations()[3], repo: 'conveyor', summary: 'Retire the feature tree', depends_on: [] },
    ],
    delivery: { state: 'in_delivery', total: 4, merged: 1, closed: 1, open: 2 },
    serves: [{ id: 'req-planning', slug: 'in-product-planning', title: 'In-product planning' }],
    events: [
      { id: 1, task_id: anchorId, kind: 'blueprint.materialized', actor_id: 'system', actor_role: 'system', payload: { version: 3, children_total: 4 }, at: createdAt },
    ],
    // The planning transcript attaches to the anchor, so it is lineage the
    // blueprint detail has to surface.
    artifacts: [{
      id: 'artifact-transcript', workspace: 'demo', name: 'planning-transcript.json',
      content_type: 'application/json', size_bytes: 2048, role: 'generated_audit',
      task_id: anchorId, download_url: '/v1/artifacts/artifact-transcript', created_at: createdAt,
    }],
    planning_session: {
      id: 'session-blueprint', title: 'Plan the delivery', status: 'finalized',
      model: 'gpt-plan', effort: 'high', exploration_output_tokens: 12000,
      primary_repo: 'conveyor',
      pinned_revisions: {
        conveyor: '0123456789abcdef',
        companion: 'fedcba9876543210',
      },
      produced_task_id: anchorId, workspace: 'demo',
      created_at: createdAt, updated_at: createdAt, finalized_at: createdAt,
    },
  }
}

function taskActivity(taskId: string) {
  if (taskId === anchorId) {
    return {
      task: anchorTask(),
      jobs: [], interventions: [], work_orders: [],
      events: blueprintView().events,
      checkout_available: false, checkout_guidance: 'No checkout for a blueprint anchor.',
      needs_attention: false, spec: blueprintView().spec, attachments: [], verification_evidence: [],
      // A queued anchor with no serviceable worker is exactly the state that
      // used to offer redispatch and a worker alarm for work it never takes.
      worker_status: {
        available: false, required_harnesses: ['claude'],
        reason: 'no worker has claimed this workspace', queue_context: 'never_started',
      },
    }
  }
  return {
    task: childTask(taskId as keyof typeof childStates),
    jobs: [], events: [], interventions: [], work_orders: [],
    checkout_command: `conveyor checkout ${taskId}`, checkout_available: true, checkout_guidance: '',
    needs_attention: false, attachments: [], verification_evidence: [],
  }
}

// The activity feed the server produces: children, never the anchor.
function activityFeed() {
  return Object.keys(childStates).map((id) => ({
    task: childTask(id as keyof typeof childStates),
    latest_stage: 'implement',
    last_event_at: createdAt,
    needs_attention: false,
  }))
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
  if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [{ name: 'conveyor', base: 'main' }] } })
  return undefined
}

async function routeAPI(page: Page, options: { feed?: unknown[]; blueprints?: unknown[] } = {}) {
  await page.route('**/v1/**', async (route) => {
    const shell = shellResponse(route)
    if (shell) return await shell
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/activity') return route.fulfill({ json: options.feed ?? activityFeed() })
    if (path === '/v1/blueprints') return route.fulfill({ json: options.blueprints ?? [blueprintView()] })
    const detail = /^\/v1\/tasks\/([^/]+)\/activity$/.exec(path)
    if (detail) return route.fulfill({ json: taskActivity(decodeURIComponent(detail[1])) })
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: '' })
    return route.fulfill({ json: [] })
  })
}

// AC-1: the board carries the children and never the anchor, and the anchor
// is not counted in any column — even if the feed hands one over.
test('the board excludes the blueprint anchor from cards and column counts', async ({ page }) => {
  await initShell(page)
  await routeAPI(page, {
    feed: [
      ...activityFeed(),
      { task: anchorTask(), latest_stage: 'implement', last_event_at: createdAt, needs_attention: false },
    ],
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
  await expect(page.getByText(childTitles['child-sub-2'])).toBeVisible()
  await expect(page.getByText('Deliver the planning flow')).toHaveCount(0)

  // Merged and closed children archive under Completed; the two live ones sit
  // in Implementing. Neither count may absorb the anchor.
  const implementing = page.getByRole('region', { name: 'Implementing' })
  await expect(implementing.getByRole('link')).toHaveCount(2)
  await expect(implementing.locator('header span').first()).toHaveText('2')
  const completed = page.getByRole('region', { name: 'Completed' })
  await expect(completed.getByRole('link')).toHaveCount(2)
  await expect(completed.locator('header span').first()).toHaveText('2')
})

// AC-5 (second half): only the ambient presentation moved — a blueprint still
// awaiting its spec gate has no children yet, so it stays in the inbox column
// with its approval card intact.
test('a blueprint awaiting its spec gate stays in the review inbox', async ({ page }) => {
  await initShell(page)
  const gated = {
    ...anchorTask(), id: 'gated-blueprint', title: 'Blueprint awaiting approval',
    state: 'awaiting_human', children: [],
  }
  await routeAPI(page, {
    feed: [{ task: gated, latest_stage: 'spec', last_event_at: createdAt, needs_attention: true }],
    blueprints: [],
  })

  await page.goto('/')
  const inbox = page.getByRole('region', { name: 'Needs operator' })
  await expect(inbox.getByText('Blueprint awaiting approval')).toBeVisible()
  await expect(inbox.getByText('Awaiting review')).toBeVisible()
})

// AC-2: the planning-side list speaks blueprint vocabulary and keeps merged
// and closed apart.
test('the blueprints list reports delivery, governing version, and served requirements', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)

  await page.goto('/blueprints')
  await expect(page.getByRole('heading', { name: 'Blueprints' })).toBeVisible()
  const entry = page.getByRole('link', { name: /Deliver the planning flow/ })
  await expect(entry).toBeVisible()
  await expect(entry).toHaveAttribute('href', `/tasks/${anchorId}/full`)
  await expect(entry.getByText('In delivery — 1 of 4')).toBeVisible()
  await expect(entry.getByText('Blueprint v3')).toBeVisible()
  await expect(entry.getByText('1 merged · 1 closed without merging · 2 in progress')).toBeVisible()
  await expect(entry.getByText('In-product planning')).toBeVisible()
  await expect(entry.getByText('gpt-plan · high')).toBeVisible()
  await expect(entry.getByText('12,000 tokens/call')).toBeVisible()
  await expect(entry.getByText('conveyor@0123456789ab')).toBeVisible()
  await expect(entry.getByText('companion@fedcba987654')).toBeVisible()
  // Pipeline vocabulary never reaches this surface.
  await expect(page.getByText('Queued', { exact: true })).toHaveCount(0)
  await expect(page.getByText('in_delivery')).toHaveCount(0)
})

test('a completed blueprint reads as completed, and a missing serves link is a normal empty state', async ({ page }) => {
  await initShell(page)
  const completed = blueprintView()
  completed.task.state = 'closed'
  completed.delivery = { state: 'completed', total: 4, merged: 3, closed: 1, open: 0 }
  completed.serves = []
  await routeAPI(page, { blueprints: [completed] })

  await page.goto('/blueprints')
  const entry = page.getByRole('link', { name: /Deliver the planning flow/ })
  await expect(entry.getByText('Completed')).toBeVisible()
  await expect(entry.getByText('3 merged · 1 closed without merging')).toBeVisible()
  await expect(entry.getByText('Serves')).toHaveCount(0)

  // The detail renders too, and says plainly that nothing is linked yet.
  await entry.click()
  await expect(page.getByText('No requirement linked yet')).toBeVisible()
})

// AC-3 and AC-4: the spec leads, children follow in dependency order with
// labelled states, and every inert execution affordance is gone but Cancel.
test('blueprint detail leads with the spec, orders children, and suppresses inert affordances', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)

  // Direct URL access is preserved.
  await page.goto(`/tasks/${anchorId}/full`)
  await expect(page.getByRole('heading', { name: 'Deliver the planning flow' })).toBeVisible()
  await expect(page.getByText('In delivery — 1 of 4')).toBeVisible()

  const blueprintSection = page.getByRole('region', { name: 'Blueprint' })
  await expect(blueprintSection.getByText('Blueprint planning surface')).toBeVisible()
  const deliveryList = page.getByRole('list', { name: 'Blueprint tasks' })

  // The approved specification renders above the delivery list.
  const specBox = await blueprintSection.getByText('Blueprint planning surface').boundingBox()
  const deliveryBox = await deliveryList.boundingBox()
  expect(specBox!.y).toBeLessThan(deliveryBox!.y)

  // Children keep dependency order and carry labelled states, never raw ones.
  const childLinks = deliveryList.getByRole('link')
  await expect(childLinks).toHaveCount(4)
  await expect(childLinks.nth(0)).toContainText(childTitles['child-sub-1'])
  await expect(childLinks.nth(1)).toContainText(childTitles['child-sub-2'])
  await expect(childLinks.nth(2)).toContainText(childTitles['child-sub-3'])
  await expect(deliveryList.getByText('Merged', { exact: true })).toBeVisible()
  await expect(deliveryList.getByText('Running', { exact: true })).toBeVisible()
  await expect(deliveryList.getByText('Closed', { exact: true })).toBeVisible()
  await expect(page.getByText('awaiting_human')).toHaveCount(0)

  // Inert affordances are suppressed; Cancel — which is lifecycle — remains.
  await expect(page.getByText('Checkout')).toHaveCount(0)
  await expect(page.getByText('conveyor/task-blueprint-anchor')).toHaveCount(0)
  await expect(page.getByText('Assigned branch')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Hold' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Cancel task' })).toBeVisible()

  // An anchor takes no work orders, so execution-recovery affordances are
  // inert too: no redispatch nudge, no worker-serviceability alarm.
  await expect(page.getByRole('button', { name: /Redispatch/ })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Auto worker unavailable' })).toHaveCount(0)

  // The batch timeline and artifact lineage are present.
  await expect(page.getByText('4 tasks created from the blueprint')).toBeVisible()
  await expect(page.getByText('Lineage and artifacts')).toBeVisible()
  await expect(page.getByText('planning-transcript.json')).toBeVisible()
})

// AC-5 (first half): navigation resolves the blueprint detail in the route
// variant it was reached from, in both directions.
test('child and blueprint navigate to each other in the matching route variant', async ({ page }) => {
  await initShell(page)
  await routeAPI(page)

  // Full page: the child's parent reference stays a full-page route.
  await page.goto('/tasks/child-sub-2/full')
  const parentLinkFull = page.getByRole('link', { name: /Deliver the planning flow/ })
  await expect(parentLinkFull).toHaveAttribute('href', `/tasks/${anchorId}/full`)
  await parentLinkFull.click()
  await expect(page).toHaveURL(new RegExp(`/tasks/${anchorId}/full$`))
  await expect(page.getByText('In delivery — 1 of 4')).toBeVisible()

  // Parent to child, still full-page.
  await page.getByRole('list', { name: 'Blueprint tasks' })
    .getByRole('link', { name: childTitles['child-sub-3'] }).click()
  await expect(page).toHaveURL(/\/tasks\/child-sub-3\/full$/)

  // Sheet: the same references stay on the sheet route over the board.
  await page.goto('/tasks/child-sub-2')
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const parentLinkSheet = sheet.getByRole('link', { name: /Deliver the planning flow/ })
  await expect(parentLinkSheet).toHaveAttribute('href', `/tasks/${anchorId}`)
  await parentLinkSheet.click()
  await expect(page).toHaveURL(new RegExp(`/tasks/${anchorId}$`))
  await expect(sheet.getByText('In delivery — 1 of 4')).toBeVisible()
  await expect(
    sheet.getByRole('list', { name: 'Blueprint tasks' }).getByRole('link', { name: childTitles['child-sub-3'] }),
  ).toHaveAttribute('href', '/tasks/child-sub-3')
})
