import { expect, type Locator, type Page } from '@playwright/test'
import type {
  NextActionCompact,
  RequirementInsightDocuments,
  RequirementInsightProgress,
  RequirementInsightSummary,
  RequirementInsightTask,
  TaskMapItem,
  TaskMapResponse,
} from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

// Requirement insights on the Board map (req-requirement-insights v1 REQ-1 to
// REQ-4; feature-requirement-insights v2 RI-7). The four insight routes are
// mocked in the shapes internal/httpapi/requirement_insight.go and
// internal/insight/types.go serve, so these tests prove the panel's requests
// and rendering only; HTTP authorization, generation, and storage are proven
// by the Go route and store tests. The map read is the task-map shape
// web/tests/board-map.spec.ts mocks.

const at = '2026-10-21T09:00:00Z'

function action(label: string, actor: NextActionCompact['actor'], kind: string, judgment = false): NextActionCompact {
  return { label, actor, kind, judgment, summary: `Server summary: ${label}.`, deadline: null }
}

// Labels deliberately differ from the dashboard's own wording, so a verbatim
// rendering is visible (DEC-61).
const planGate = action('Plan waits for you', 'operator', 'plan_approval', true)
const working = action('An agent is on it', 'agent', 'working')
const queued = action('Waiting for a free agent', 'agent', 'queued')
const blocked = action('Held by its dependency', 'dependency', 'blocked')
const merging = action('Factory is merging', 'factory', 'merging')

const titles: Record<string, string> = {
  'req-alpha': 'Alpha delivery',
  'req-beta': 'Beta review',
  'req-tall': 'Tall chain',
}

function task(
  id: string,
  title: string,
  next: NextActionCompact,
  { depends = [], blocking = [], reqs = [] }: { depends?: string[]; blocking?: string[]; reqs?: string[] } = {},
): TaskMapItem {
  return {
    id,
    title,
    repo: 'conveyor',
    // Lifecycle states disagree with the stated actions on purpose.
    state: 'running',
    last_event_at: at,
    next_action: next,
    depends_on: depends,
    blocking_task_ids: blocking,
    unsatisfiable_task_ids: [],
    requirements: reqs.map((req) => ({ id: req, title: titles[req] ?? req })),
  }
}

const alphaTasks = [
  task('a1', 'Approve the alpha plan', planGate, { reqs: ['req-alpha'] }),
  task('a2', 'Build the alpha store', working, { depends: ['a4'], reqs: ['req-alpha'] }),
  task('a3', 'Wire the alpha form', blocked, { depends: ['a2'], blocking: ['a2'], reqs: ['req-alpha'] }),
  task('a4', 'Lay the alpha schema', queued, { reqs: ['req-alpha'] }),
  task('a5', 'Land the alpha copy', merging, { reqs: ['req-alpha'] }),
]
const betaTasks = [
  task('b1', 'Review the beta flow', queued, { reqs: ['req-beta'] }),
  task('b2', 'Plan the beta export', planGate, { reqs: ['req-beta'] }),
]
const looseTask = task('n1', 'Tidy the web build', queued)

function mapRead(tasks: TaskMapItem[] = [...alphaTasks, ...betaTasks, looseTask]): TaskMapResponse {
  const ids = [...new Set(tasks.flatMap((item) => item.requirements.map((req) => req.id)))]
  return {
    tasks,
    requirements: ids.map((id) => {
      const open = tasks.filter((item) => item.requirements.some((req) => req.id === id)).length
      return { id, title: titles[id] ?? id, open_task_count: open, merged_task_count: 2, total_task_count: open + 2 }
    }),
  }
}

const ref = (item: TaskMapItem): RequirementInsightTask => ({
  id: item.id,
  title: item.title,
  next_action: item.next_action,
})
const [a1, a2, a3, a4, a5] = alphaTasks.map(ref)

function summary(overrides: Partial<RequirementInsightSummary> = {}): RequirementInsightSummary {
  return {
    headline: {
      text: 'Alpha is two of seven delivered and waits on one plan approval.',
      model: 'gpt-test-mini',
      generated_at: at,
      generated_by: 'usr_operator',
      current: true,
    },
    fingerprint: 'f'.repeat(64),
    can_generate: true,
    groups: [
      { actor: 'operator', count: 1, tasks: [a1] },
      { actor: 'agent', count: 2, tasks: [a2, a4] },
      { actor: 'factory', count: 1, tasks: [a5] },
      { actor: 'dependency', count: 1, tasks: [a3] },
      { actor: 'none', count: 0, tasks: [] },
    ],
    critical_path: { tasks: [a4, a2, a3], length: 3 },
    forecast: { day: '2026-11-04T00:00:00Z', rate_per_day: 2 / 14 },
    ...overrides,
  }
}

const days = Array.from({ length: 10 }, (_, index) => `2026-10-${String(12 + index).padStart(2, '0')}T00:00:00Z`)
function progress(overrides: Partial<RequirementInsightProgress> = {}): RequirementInsightProgress {
  const scope = [4, 4, 5, 5, 6, 6, 7, 7, 7, 7]
  const delivered = [0, 0, 0, 1, 1, 1, 1, 2, 2, 2]
  return {
    daily: days.map((day, index) => ({
      day,
      scope: scope[index],
      delivered: delivered[index],
      open: scope[index] - delivered[index],
    })),
    forecast: { day: '2026-11-04T00:00:00Z', rate_per_day: 2 / 14 },
    weekly: [
      ['2026-08-31', 36, 0],
      ['2026-09-07', 37, 1],
      ['2026-09-14', 38, 0],
      ['2026-09-21', 39, 2],
      ['2026-09-28', 40, 0],
      ['2026-10-05', 41, 1],
      ['2026-10-12', 42, 1],
      ['2026-10-19', 43, 1],
    ].map(([start, week, count], index) => ({
      week_start: `${start}T00:00:00Z`,
      iso_year: 2026,
      iso_week: week as number,
      delivered: count as number,
      in_progress: index === 7,
    })),
    delivered: 2,
    total: 7,
    open: 5,
    throughput_per_week: 1.5,
    median_cycle_seconds: 2.4 * 86_400,
    status_mix: {
      delivered: 2,
      closed: 1,
      open: [
        { actor: 'operator', count: 1 },
        { actor: 'agent', count: 2 },
        { actor: 'factory', count: 1 },
        { actor: 'dependency', count: 1 },
        { actor: 'none', count: 0 },
      ],
    },
    ...overrides,
  }
}

const documentsView: RequirementInsightDocuments = {
  designs: [
    {
      id: 'component-alpha-store',
      title: 'Alpha store',
      current_version: 4,
      revision_pending: false,
      attached_task_count: 2,
      names_requirement: true,
      attached_task_ids: ['a2', 'a4'],
    },
    {
      id: 'component-alpha-forms',
      title: 'Alpha forms',
      current_version: 2,
      revision_pending: true,
      attached_task_count: 1,
      names_requirement: false,
      attached_task_ids: ['a3'],
    },
    // Associated only by naming the requirement.
    {
      id: 'feature-alpha-notes',
      title: 'Alpha notes',
      current_version: 1,
      revision_pending: false,
      attached_task_count: 0,
      names_requirement: true,
      attached_task_ids: [],
    },
  ],
  decisions: [
    {
      id: 'DEC-12',
      statement: 'Alpha forms validate on the server',
      context: 'The browser cannot be trusted to validate alpha input.',
      status: 'confirmed',
      names_requirement: true,
      cited_by_requirement: false,
      // gone1 serves the requirement but is outside the map's filtered read.
      citing_task_ids: ['a3', 'gone1'],
    },
    {
      id: 'DEC-30',
      statement: 'Alpha copy lands with the store',
      context: 'Copy and store ship together.',
      status: 'proposed',
      names_requirement: false,
      cited_by_requirement: true,
      citing_task_ids: [],
    },
  ],
  acceptance_criteria: [
    { id: 'AC-1.1', statement: 'Alpha opens.', citing_task_ids: ['a1', 'a2'] },
    { id: 'AC-1.2', statement: 'Alpha saves.', citing_task_ids: ['a3'] },
    { id: 'AC-1.3', statement: 'Alpha exports.', citing_task_ids: [] },
    { id: 'AC-2.1', statement: 'Alpha validates.', citing_task_ids: ['a4'] },
  ],
  cited_count: 3,
  total: 4,
}

interface InsightRequest {
  method: string
  view: string
  requirement: string
  workspace: string | null
  csrf?: string
  authorization?: string
}

type Reply = { status: number; json: unknown }

interface Server {
  role?: string
  workspaces?: Array<{ id: string; name: string }>
  map?: (workspace: string) => TaskMapResponse
  summary?: (requirement: string, workspace: string) => RequirementInsightSummary
  generate?: (requirement: string, workspace: string, count: number) => Reply | Promise<Reply>
  progress?: () => RequirementInsightProgress
  documents?: () => RequirementInsightDocuments
}

async function mockBoard(page: Page, server: Server = {}) {
  const seen: InsightRequest[] = []
  let generations = 0
  const workspaces = server.workspaces ?? [{ id: 'demo', name: 'Demo' }]
  await page.addInitScript((id) => {
    if (!localStorage.getItem('conveyor-workspace')) localStorage.setItem('conveyor-workspace', id)
  }, workspaces[0].id)
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const workspace = url.searchParams.get('workspace_id') ?? request.headers()['x-workspace-id'] ?? ''
    if (path === '/v1/workspaces') return route.fulfill({ json: workspaces })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'member', email: 'member@example.test', role: server.role ?? 'operator' }, request),
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace, repos: [{ name: 'conveyor' }] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === '/v1/task-map') return route.fulfill({ json: (server.map ?? (() => mapRead()))(workspace) })
    const insight = path.match(/^\/v1\/requirements\/([^/]+)\/insights\/(summary|progress|documents)$/)
    if (insight) {
      const requirement = decodeURIComponent(insight[1])
      const view = insight[2]
      seen.push({
        method: request.method(),
        view,
        requirement,
        workspace: url.searchParams.get('workspace_id'),
        csrf: request.headers()['x-conveyor-csrf'],
        authorization: request.headers().authorization,
      })
      if (view === 'summary' && request.method() === 'POST') {
        generations++
        const reply = server.generate
          ? await server.generate(requirement, workspace, generations)
          : { status: 200, json: summary() }
        return route.fulfill({ status: reply.status, json: reply.json })
      }
      if (view === 'summary')
        return route.fulfill({ json: (server.summary ?? (() => summary()))(requirement, workspace) })
      if (view === 'progress') return route.fulfill({ json: (server.progress ?? (() => progress()))() })
      return route.fulfill({ json: (server.documents ?? (() => documentsView))() })
    }
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  const count = (method: string, view: string) =>
    seen.filter((entry) => entry.method === method && entry.view === view).length
  return { seen, count }
}

const node = (page: Page, id: string) => page.getByTestId(`rf__node-${id}`)
const hub = (page: Page, id = 'req-alpha') => node(page, `doc:${id}`)
const toolbar = (page: Page, id = 'req-alpha') => hub(page, id).getByRole('toolbar', { name: 'Requirement insights' })
const panel = (page: Page) => page.getByRole('complementary', { name: 'Requirement insights' })
const focusPanel = (page: Page) => page.locator('aside.w-80')
const dim = /\bopacity-(15|20)\b/
const card = (page: Page, id: string) => node(page, id).locator(':scope > div:first-child')

async function zoom(page: Page) {
  return page
    .locator('.react-flow__viewport')
    .evaluate((element) => new DOMMatrixReadOnly(getComputedStyle(element).transform).a)
}

async function waitForCamera(locator: Locator) {
  // A camera move lasts 450 to 650 ms; read positions once two reads 300 ms
  // apart agree.
  let previous = ''
  await expect
    .poll(
      async () => {
        const current = JSON.stringify(await locator.boundingBox())
        const settled = current === previous
        previous = current
        return settled
      },
      { intervals: [300] },
    )
    .toBe(true)
}

// The near zoom, where task titles read and the toolbar renders.
async function zoomNear(page: Page) {
  await expect(hub(page)).toBeVisible()
  await waitForCamera(hub(page))
  await expect
    .poll(
      async () => {
        const current = await zoom(page)
        if (current < 0.62) await page.locator('.react-flow__controls-zoomin').click()
        return current
      },
      { intervals: [250], timeout: 15_000 },
    )
    .toBeGreaterThanOrEqual(0.62)
}

// The node and its toolbar lie inside the map pane, beside the panel.
async function expectHubBesidePanel(page: Page, id = 'req-alpha') {
  await waitForCamera(hub(page, id))
  const pane = await page.locator('.react-flow').boundingBox()
  const side = await panel(page).boundingBox()
  const box = await hub(page, id).boundingBox()
  const bar = await toolbar(page, id).boundingBox()
  if (!pane || !side || !box || !bar) throw new Error('missing map pane, panel, node, or toolbar')
  for (const rect of [box, bar]) {
    expect(rect.x).toBeGreaterThanOrEqual(pane.x - 1)
    expect(rect.y).toBeGreaterThanOrEqual(pane.y - 1)
    expect(rect.x + rect.width).toBeLessThanOrEqual(Math.min(pane.x + pane.width, side.x) + 1)
    expect(rect.y + rect.height).toBeLessThanOrEqual(pane.y + pane.height + 1)
  }
}

test('near zoom shows the requirement pill toolbar and far zoom hides it', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map')
  // The whole map opens below the near zoom: no toolbar.
  await expect(hub(page)).toBeVisible()
  await waitForCamera(hub(page))
  expect(await zoom(page)).toBeLessThan(0.62)
  await expect(page.getByRole('toolbar', { name: 'Requirement insights' })).toHaveCount(0)

  await zoomNear(page)
  const controls = toolbar(page).getByRole('button')
  await expect(controls).toHaveText(['Summary', 'Charts', 'Documents'])
  for (const control of await controls.all()) await expect(control).toHaveAttribute('aria-pressed', 'false')
  // The toolbar hangs centred below the node.
  const box = await hub(page).boundingBox()
  const bar = await toolbar(page).boundingBox()
  if (!box || !bar) throw new Error('missing node or toolbar')
  expect(bar.y).toBeGreaterThan(box.y + box.height)
  expect(Math.abs(bar.x + bar.width / 2 - (box.x + box.width / 2))).toBeLessThan(2)

  // Activating one selects the requirement and opens its tab.
  await toolbar(page).getByRole('button', { name: 'Charts' }).click()
  await expect(page).toHaveURL(/scope=req-alpha/)
  await expect(page).toHaveURL(/insight=charts/)
  await expect(panel(page).getByRole('tab', { name: 'Charts' })).toHaveAttribute('aria-selected', 'true')
  await expect(toolbar(page).getByRole('button', { name: 'Charts' })).toHaveAttribute('aria-pressed', 'true')
  // The node is outlined while its panel is open.
  await expect(card(page, 'doc:req-alpha')).toHaveClass(/ring-foreground/)

  // Zoomed out again, the toolbar goes away and the panel stays.
  while ((await zoom(page)) >= 0.62) await page.locator('.react-flow__controls-zoomout').click()
  await expect(page.getByRole('toolbar', { name: 'Requirement insights' })).toHaveCount(0)
  await expect(panel(page)).toBeVisible()
})

test('each insight tab survives a URL round trip and reload', async ({ page }) => {
  await mockBoard(page)
  for (const [tab, label] of [
    ['summary', 'Summary'],
    ['charts', 'Charts'],
    ['documents', 'Documents'],
  ] as const) {
    await page.goto(`/?view=map&scope=req-alpha&insight=${tab}`)
    await expect(panel(page).getByRole('tab', { name: label })).toHaveAttribute('aria-selected', 'true')
    await expect(panel(page).getByRole('heading', { name: 'Alpha delivery' })).toBeVisible()
    await page.reload()
    await expect(page).toHaveURL(new RegExp(`insight=${tab}`))
    await expect(panel(page).getByRole('tab', { name: label })).toHaveAttribute('aria-selected', 'true')
  }
  // Choosing a tab in the panel writes it to the address.
  await panel(page).getByRole('tab', { name: 'Summary' }).click()
  await expect(page).toHaveURL(/insight=summary/)
  // Closing keeps the requirement selected and drops the tab.
  await panel(page).getByRole('button', { name: 'Close insights' }).click()
  await expect(panel(page)).toHaveCount(0)
  await expect(page).toHaveURL(/scope=req-alpha/)
  await expect(page).not.toHaveURL(/insight=/)
})

test('invalid insight values and non-requirement scopes are discarded', async ({ page }) => {
  const { seen } = await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=timeline')
  await expect(hub(page)).toBeVisible()
  await expect(page).not.toHaveURL(/insight=/)
  await expect(panel(page)).toHaveCount(0)

  // The group of tasks that serve no requirement names no requirement.
  await page.goto('/?view=map&scope=__none&insight=summary')
  await expect(node(page, 'n1')).toBeVisible()
  await expect(page).not.toHaveURL(/insight=/)
  // A requirement without a group on the map.
  await page.goto('/?view=map&scope=req-missing&insight=charts')
  await expect(hub(page)).toBeVisible()
  await expect(page).not.toHaveURL(/insight=/)
  // Outside the map view, and in the dependency arrangement.
  await page.goto('/?view=list&scope=req-alpha&insight=summary')
  await expect(page).not.toHaveURL(/insight=/)
  await page.goto('/?view=map&arrangement=dependencies&scope=req-alpha&insight=summary')
  await expect(node(page, 'a1')).toBeVisible()
  await expect(page).not.toHaveURL(/insight=/)
  await expect(panel(page)).toHaveCount(0)
  expect(seen).toEqual([])
})

test('switching view or arrangement clears insight', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=charts')
  await expect(panel(page)).toBeVisible()
  await page.getByRole('button', { name: 'Dependencies' }).click()
  await expect(page).toHaveURL(/arrangement=dependencies/)
  await expect(page).not.toHaveURL(/insight=|scope=/)
  await expect(panel(page)).toHaveCount(0)

  await page.goto('/?view=map&scope=req-alpha&insight=documents')
  await expect(panel(page)).toBeVisible()
  await page.getByRole('button', { name: 'List' }).click()
  await expect(page).toHaveURL(/view=list/)
  await expect(page).not.toHaveURL(/insight=|scope=/)

  // Selecting another requirement's group closes the panel.
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toBeVisible()
  await page.getByRole('button', { name: 'Whole map' }).click()
  await waitForCamera(page.getByTestId('rf__node-frame:req-beta'))
  const beta = await page.getByTestId('rf__node-frame:req-beta').boundingBox()
  if (!beta) throw new Error('no beta group')
  await page.mouse.click(beta.x + beta.width / 2, beta.y + 4)
  await expect(page).toHaveURL(/scope=req-beta/)
  await expect(page).not.toHaveURL(/insight=/)
  await expect(panel(page)).toHaveCount(0)
})

test('closed panels and inactive tabs issue no insight reads', async ({ page }) => {
  const { seen, count } = await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha')
  await zoomNear(page)
  await expect(toolbar(page)).toBeVisible()
  expect(seen).toEqual([])

  await toolbar(page).getByRole('button', { name: 'Charts' }).click()
  await expect(panel(page).getByRole('region', { name: 'Burn-down' })).toBeVisible()
  // Only the shown tab reads. (A development build's StrictMode may abandon
  // and repeat a first read, so reads are compared by kind.)
  expect(new Set(seen.map((entry) => `${entry.method} ${entry.view}`))).toEqual(new Set(['GET progress']))
  for (const entry of seen) expect(entry).toMatchObject({ requirement: 'req-alpha', workspace: 'demo' })

  await panel(page).getByRole('tab', { name: 'Documents' }).click()
  await expect(panel(page).getByRole('region', { name: 'System designs' })).toBeVisible()
  expect(count('GET', 'documents')).toBeGreaterThanOrEqual(1)
  expect(count('GET', 'summary')).toBe(0)

  // Activating the open tab's control closes the panel; reads stop with it.
  await toolbar(page).getByRole('button', { name: 'Documents' }).click()
  await expect(panel(page)).toHaveCount(0)
  await expect(page).not.toHaveURL(/insight=/)
  const settled = seen.length
  await page.locator('.react-flow__controls-zoomout').click()
  await page.getByRole('button', { name: 'Finished', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Finished', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await expect(hub(page)).toBeVisible()
  expect(seen.length).toBe(settled)
  expect(count('GET', 'summary')).toBe(0)
})

test('opening an already-selected requirement keeps its hub and toolbar beside the panel', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha')
  await zoomNear(page)
  await toolbar(page).getByRole('button', { name: 'Summary' }).click()
  await expect(panel(page)).toBeVisible()
  await expectHubBesidePanel(page)

  // A reload frames it the same way.
  await page.reload()
  await expect(panel(page)).toBeVisible()
  await expectHubBesidePanel(page)
})

test('a tall requirement group keeps its hub visible when insights open', async ({ page }) => {
  const chain = Array.from({ length: 12 }, (_, index) =>
    task(`t${index}`, `Tall step ${index}`, index ? blocked : queued, {
      depends: index ? [`t${index - 1}`] : [],
      blocking: index ? [`t${index - 1}`] : [],
      reqs: ['req-tall'],
    }),
  )
  await mockBoard(page, { map: () => mapRead([...chain, ...betaTasks]) })
  await page.goto('/?view=map&scope=req-tall&insight=summary')
  await expect(panel(page)).toBeVisible()
  await expect(toolbar(page, 'req-tall')).toBeVisible()
  await expectHubBesidePanel(page, 'req-tall')
  await expect(toolbar(page, 'req-tall').getByRole('button', { name: 'Summary' })).toHaveAttribute(
    'aria-pressed',
    'true',
  )
})

test('summary opens with one generation request for a task creator', async ({ page }) => {
  const generated = summary({
    headline: {
      text: 'Freshly generated alpha headline.',
      model: 'gpt-test-mini',
      generated_at: at,
      generated_by: 'member',
      current: true,
    },
  })
  const { seen, count } = await mockBoard(page, {
    summary: () => summary({ headline: null }),
    generate: () => ({ status: 200, json: generated }),
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toContainText('Freshly generated alpha headline.')
  await expect(panel(page)).toContainText('Generated · informative')
  await expect(panel(page)).toContainText('gpt-test-mini')
  // A development build's StrictMode may abandon and repeat the first read;
  // generation is still requested once.
  expect(count('GET', 'summary')).toBeGreaterThanOrEqual(1)
  expect(count('POST', 'summary')).toBe(1)
  const post = seen.find((entry) => entry.method === 'POST')
  // Cookie authentication with the CSRF proof, in the explicit workspace.
  expect(post).toMatchObject({ requirement: 'req-alpha', workspace: 'demo', csrf: '1', authorization: undefined })
  // The current headline offers no Regenerate.
  await expect(panel(page).getByRole('button', { name: 'Regenerate' })).toBeDisabled()
})

test('summary rerenders and refetches do not duplicate generation during one opening', async ({ page }) => {
  const { count } = await mockBoard(page, {
    summary: () => summary({ headline: null }),
    generate: () => ({ status: 502, json: { error: 'insight_generation_failed', detail: 'upstream timed out' } }),
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page).getByRole('alert')).toContainText('insight_generation_failed')
  // Rerenders: row hovers and a map highlight chip.
  await panel(page).locator('[data-task-row="a1"]').first().hover()
  await page.mouse.move(2, 2)
  await page.getByRole('button', { name: '1 An agent is on it', exact: true }).click()
  // A focus refetch once the read is stale (five seconds) reads again.
  const reads = count('GET', 'summary')
  await expect
    .poll(
      async () => {
        await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')))
        return count('GET', 'summary')
      },
      { intervals: [500], timeout: 15_000 },
    )
    .toBeGreaterThan(reads)
  expect(count('POST', 'summary')).toBe(1)
  await expect(panel(page).getByRole('alert')).toContainText('upstream timed out')
})

test('a member without create_tasks sees no-summary notice and makes no POST', async ({ page }) => {
  const { count } = await mockBoard(page, {
    role: 'contributor',
    summary: () => summary({ headline: null, can_generate: false }),
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toContainText('No summary exists yet.')
  // The computed sections still show.
  await expect(panel(page).getByRole('region', { name: 'Critical path' })).toBeVisible()
  expect(count('GET', 'summary')).toBeGreaterThanOrEqual(1)
  expect(count('POST', 'summary')).toBe(0)
  await expect(panel(page).getByRole('button', { name: 'Regenerate' })).toHaveCount(0)
})

test('current and unauthorized headlines disable Regenerate while stale authorized headlines enable it', async ({
  page,
}) => {
  let current = summary()
  const { count } = await mockBoard(page, { summary: () => current })
  const regenerate = () => panel(page).getByRole('button', { name: 'Regenerate' })
  const stale = summary({ headline: { ...summary().headline!, current: false } })

  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(regenerate()).toBeDisabled()
  await expect(panel(page)).not.toContainText('Out of date')

  current = { ...stale, can_generate: false }
  await page.reload()
  await expect(panel(page)).toContainText('Out of date')
  await expect(regenerate()).toBeDisabled()

  current = stale
  await page.reload()
  await expect(panel(page)).toContainText('Out of date')
  await expect(regenerate()).toBeEnabled()
  await regenerate().click()
  await expect(regenerate()).toBeDisabled()
  await expect(panel(page)).not.toContainText('Out of date')
  expect(count('POST', 'summary')).toBe(1)
})

test('a generation conflict refetches summary without an error notice', async ({ page }) => {
  let current = summary({ headline: { ...summary().headline!, current: false } })
  const { count } = await mockBoard(page, {
    summary: () => current,
    generate: () => {
      // Another member generated meanwhile; the stored headline is current.
      current = summary({ headline: { ...summary().headline!, text: 'Stored by someone else.' } })
      return { status: 409, json: { error: 'insight_headline_current' } }
    },
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page).getByRole('button', { name: 'Regenerate' })).toBeEnabled()
  const reads = count('GET', 'summary')
  await panel(page).getByRole('button', { name: 'Regenerate' }).click()
  await expect(panel(page)).toContainText('Stored by someone else.')
  expect(count('GET', 'summary')).toBe(reads + 1)
  expect(count('POST', 'summary')).toBe(1)
  await expect(panel(page).getByRole('alert')).toHaveCount(0)
  await expect(panel(page).getByRole('button', { name: 'Regenerate' })).toBeDisabled()
})

for (const failure of [
  {
    status: 502,
    json: { error: 'insight_generation_failed', detail: 'model refused: [redacted]' },
    text: /Summary generation failed \(insight_generation_failed\): model refused: \[redacted\]/,
  },
  {
    status: 503,
    json: { error: 'insight_model_unconfigured' },
    text: /No model is configured to generate summaries \(insight_model_unconfigured\)/,
  },
]) {
  test(`generation failures keep computed sections and any stored headline (${failure.status})`, async ({ page }) => {
    const stale = summary({ headline: { ...summary().headline!, text: 'The stored alpha headline.', current: false } })
    const { count } = await mockBoard(page, {
      summary: () => stale,
      generate: () => ({ status: failure.status, json: failure.json }),
    })
    await page.goto('/?view=map&scope=req-alpha&insight=summary')
    await panel(page).getByRole('button', { name: 'Regenerate' }).click()
    await expect(panel(page).getByRole('alert')).toHaveText(failure.text)
    await expect(panel(page)).toContainText('The stored alpha headline.')
    await expect(panel(page)).toContainText('Out of date')
    await expect(panel(page).getByRole('region', { name: 'Critical path' })).toBeVisible()
    await expect(panel(page).getByRole('region', { name: 'Forecast' })).toBeVisible()
    expect(count('POST', 'summary')).toBe(1)
  })
}

test('summary preserves actor ordering and server next-action labels and critical path', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  const groups = panel(page).getByRole('region', { name: /^(Operator|Agent|Factory|Dependency|No actor) · \d+$/ })
  await expect(groups).toHaveCount(5)
  expect(await groups.evaluateAll((elements) => elements.map((element) => element.getAttribute('aria-label')))).toEqual(
    ['Operator · 1', 'Agent · 2', 'Factory · 1', 'Dependency · 1', 'No actor · 0'],
  )
  const agent = panel(page).getByRole('region', { name: 'Agent · 2' })
  await expect(agent.locator('[data-task-row]')).toHaveText([
    /Build the alpha store.*An agent is on it · a2/,
    /Lay the alpha schema.*Waiting for a free agent · a4/,
  ])
  await expect(panel(page).getByRole('region', { name: 'Operator · 1' })).toContainText('Plan waits for you')
  const path = panel(page).getByRole('region', { name: 'Critical path' })
  await expect(path).toContainText('Critical path · 3 tasks')
  await expect(path.locator('[data-task-row]')).toHaveText([
    /Lay the alpha schema/,
    /Build the alpha store/,
    /Wire the alpha form/,
  ])
  await expect(panel(page).getByRole('region', { name: 'Forecast' })).toContainText('Nov 4, 2026')
  await expect(panel(page).getByRole('region', { name: 'Forecast' })).toContainText('1 task a week')
})

test('the headline renders as plain text', async ({ page }) => {
  await mockBoard(page, {
    summary: () =>
      summary({ headline: { ...summary().headline!, text: '<img src=x onerror="window.__owned=1"> **bold**' } }),
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toContainText('<img src=x onerror="window.__owned=1"> **bold**')
  await expect(panel(page).locator('img')).toHaveCount(0)
  expect(await page.evaluate(() => (window as { __owned?: number }).__owned)).toBeUndefined()
})

test('charts toggle burn-down and burn-up and expose all values in tables', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=charts')
  const figures = panel(page).locator('dl').first()
  await expect(figures).toContainText('Delivered2/7')
  await expect(figures).toContainText('Open5')
  await expect(figures).toContainText('Throughput1.5/wk')
  await expect(figures).toContainText('Median cycle2.4d')

  const burn = panel(page).getByRole('region', { name: 'Burn-down' })
  await expect(burn.locator('[data-series="open"]')).toHaveCount(1)
  await expect(burn.locator('[data-series="forecast"]')).toHaveAttribute('stroke-dasharray', '4 4')
  await expect(burn).toContainText('5 open')
  // Hover reads a day's values.
  const plot = burn.locator('svg rect').last()
  const box = await plot.boundingBox()
  if (!box) throw new Error('no plot')
  await page.mouse.move(box.x + 2, box.y + box.height / 2)
  await expect(burn).toContainText('Oct 12')
  await expect(burn).toContainText('Open 4 · Scope 4')

  await burn.getByText('Table', { exact: true }).click()
  const rows = burn.locator('tbody tr')
  await expect(rows).toHaveCount(days.length + 1)
  await expect(rows.first()).toHaveText(/Oct 12, 2026\s*4\s*4\s*0/)
  await expect(rows.last()).toContainText('Forecast: Nov 4, 2026')

  await burn.getByRole('button', { name: 'Burn-up' }).click()
  const up = panel(page).getByRole('region', { name: 'Burn-up' })
  await expect(up.locator('[data-series="delivered"]')).toHaveCount(1)
  await expect(up).toContainText('2 delivered')

  const mix = panel(page).getByRole('region', { name: 'Status mix' })
  await expect(mix).toContainText('8 tasks')
  await mix.getByText('Table', { exact: true }).click()
  await expect(mix.locator('tbody tr')).toHaveText([
    /Delivered\s*2/,
    /Closed\s*1/,
    /Open · Operator\s*1/,
    /Open · Agent\s*2/,
    /Open · Factory\s*1/,
    /Open · Dependency\s*1/,
    /Open · No actor\s*0/,
  ])
})

test('eight weekly bars mark the current week in progress', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=charts')
  const weekly = panel(page).getByRole('region', { name: 'Delivered per week' })
  await expect(weekly.locator('[data-week]')).toHaveCount(8)
  await expect(weekly.locator('[data-in-progress]')).toHaveCount(1)
  await expect(weekly.locator('[data-in-progress]')).toHaveAttribute('data-week', '2026-W43')
  await expect(weekly.locator('[data-in-progress]')).toHaveAttribute('opacity', '0.45')
  await expect(weekly).toContainText('in progress')
  await weekly.getByText('Table', { exact: true }).click()
  await expect(weekly.locator('tbody tr')).toHaveCount(8)
  await expect(weekly.locator('tbody tr').first()).toHaveText(/2026-W36\s*Aug 31, 2026\s*0/)
  await expect(weekly.locator('tbody tr').last()).toHaveText(/2026-W43\s*Oct 19, 2026\s*1 \(in progress\)/)
})

test('empty progress, zero totals, and null forecast and cycle time remain readable', async ({ page }) => {
  await mockBoard(page, {
    progress: () =>
      progress({
        daily: [],
        forecast: null,
        weekly: progress().weekly.map((week) => ({ ...week, delivered: 0 })),
        delivered: 0,
        total: 0,
        open: 0,
        throughput_per_week: 0,
        median_cycle_seconds: null,
        status_mix: {
          delivered: 0,
          closed: 0,
          open: progress().status_mix.open.map((entry) => ({ ...entry, count: 0 })),
        },
      }),
  })
  await page.goto('/?view=map&scope=req-alpha&insight=charts')
  const figures = panel(page).locator('dl').first()
  await expect(figures).toContainText('Delivered0/0')
  await expect(figures).toContainText('Median cyclenone')
  await expect(panel(page)).toContainText('No task serves it yet.')
  await expect(panel(page)).toContainText('No forecast exists')
  await expect(panel(page).getByRole('region', { name: 'Status mix' })).toContainText('0 tasks')
  await expect(panel(page).locator('[data-week]')).toHaveCount(8)
  expect(await panel(page).innerHTML()).not.toMatch(/NaN|Infinity/)

  // One recorded day and no forecast.
  await mockBoard(page, {
    progress: () => progress({ daily: [{ day: days[9], scope: 3, open: 3, delivered: 0 }], forecast: null }),
  })
  await page.reload()
  await expect(panel(page).getByRole('region', { name: 'Burn-down' })).toContainText('3 open')
  expect(await panel(page).innerHTML()).not.toMatch(/NaN|Infinity/)
})

test('documents show association evidence and cited criteria without satisfaction claims', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=documents')
  const designs = panel(page).getByRole('region', { name: 'System designs' })
  await expect(designs.locator('[data-design]')).toHaveText([
    /Alpha store.*component-alpha-store.*v4.*attached to 2 tasks.*names this requirement/,
    /Alpha forms.*component-alpha-forms.*v2.*Revision pending.*attached to 1 task/,
    /Alpha notes.*feature-alpha-notes.*v1.*attached to 0 tasks.*names this requirement/,
  ])
  await expect(designs.locator('[data-design="component-alpha-forms"]')).not.toContainText('names this requirement')
  const decisions = panel(page).getByRole('region', { name: 'Decisions' })
  await expect(decisions.locator('[data-decision]')).toHaveText([
    /Alpha forms validate on the server.*DEC-12.*confirmed.*names this requirement.*cited by 2 tasks/,
    /Alpha copy lands with the store.*DEC-30.*proposed.*cited by the requirement/,
  ])
  const criteria = panel(page).getByRole('region', { name: 'Acceptance criteria' })
  await expect(criteria).toContainText('3 of 4 cited by a task.')
  await expect(criteria).toContainText('AC-1.3 is not cited by any task.')
  await expect(criteria.getByRole('button', { name: 'AC-1.1: cited by 2 tasks' })).toBeVisible()
  await expect(criteria.getByRole('button', { name: 'AC-1.3: not cited by a task' })).toBeVisible()
  await expect(panel(page)).not.toContainText(/satisfied/i)
})

test('design activation opens its document and decisions expand in place', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=documents')
  const decision = panel(page).locator('[data-decision="DEC-12"]')
  await expect(decision).toHaveAttribute('aria-expanded', 'false')
  await decision.click()
  await expect(decision).toHaveAttribute('aria-expanded', 'true')
  const details = panel(page).locator(`#${await decision.getAttribute('aria-controls')}`)
  await expect(details).toContainText('Alpha forms validate on the server')
  await expect(details).toContainText('The browser cannot be trusted to validate alpha input.')
  await expect(details).toContainText(/Status\s*confirmed/i)
  // Expanding stays in the panel.
  await expect(page).toHaveURL(/insight=documents/)
  await decision.click()
  await expect(details).toHaveCount(0)

  const design = panel(page).locator('[data-design="component-alpha-forms"]')
  await expect(design).toHaveAttribute('href', '/system-design?document=component-alpha-forms')
  await design.click()
  await expect(page).toHaveURL(/\/system-design\?document=component-alpha-forms/)
})

test('task and document hover emphasize their named tasks and clear on leave', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toBeVisible()
  await expect(node(page, 'a1')).toBeVisible()
  const group = ['a1', 'a2', 'a3', 'a4', 'a5']
  const expectEmphasis = async (named: string[]) => {
    for (const id of group) {
      if (named.includes(id)) await expect(card(page, id)).not.toHaveClass(dim)
      else await expect(card(page, id)).toHaveClass(dim)
    }
    // Other groups keep their treatment.
    await expect(card(page, 'b1')).not.toHaveClass(dim)
  }
  const cleared = async () => {
    for (const id of group) await expect(card(page, id)).not.toHaveClass(dim)
  }

  // A task row names its own task.
  await panel(page).getByRole('region', { name: 'Operator · 1' }).locator('[data-task-row="a1"]').hover()
  await expectEmphasis(['a1'])
  await page.mouse.move(2, 2)
  await cleared()

  await panel(page).getByRole('tab', { name: 'Documents' }).click()
  // A design row names its attached tasks.
  await panel(page).locator('[data-design="component-alpha-store"]').hover()
  await expectEmphasis(['a2', 'a4'])
  // A decision row names its citing tasks; one is outside the map.
  await panel(page).locator('[data-decision="DEC-12"]').hover()
  await expectEmphasis(['a3'])
  // A criterion names the tasks that cite it.
  await panel(page).getByRole('button', { name: 'AC-1.1: cited by 2 tasks' }).hover()
  await expectEmphasis(['a1', 'a2'])
  await page.mouse.move(2, 2)
  await cleared()
  // Leaving through a tab change also clears.
  await panel(page).locator('[data-design="component-alpha-forms"]').hover()
  await expectEmphasis(['a3'])
  await panel(page).getByRole('tab', { name: 'Charts' }).focus()
  await page.keyboard.press('Enter')
  await cleared()
})

test('ordinary map hover still dims no task after panel hover clears', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(node(page, 'a3')).toBeVisible()
  await waitForCamera(node(page, 'a3'))
  await panel(page).locator('[data-task-row="a2"]').first().hover()
  await expect(card(page, 'a1')).toHaveClass(dim)
  await page.mouse.move(2, 2)
  await expect(card(page, 'a1')).not.toHaveClass(dim)

  const own = page.getByTestId('rf__edge-a2->a3').locator('path').first()
  await node(page, 'a3').hover()
  await expect(own).toHaveAttribute('style', /var\(--color-muted\)/)
  await expect(page.locator('.react-flow__node-task > .opacity-20')).toHaveCount(0)
  expect(page.url()).not.toContain('focus=')
})

test('task activation replaces insights and clearing focus restores the selected tab', async ({ page }) => {
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await panel(page).getByRole('region', { name: 'Dependency · 1' }).locator('[data-task-row="a3"]').click()
  await expect(page).toHaveURL(/focus=a3/)
  await expect(page).toHaveURL(/insight=summary/)
  await expect(panel(page)).toHaveCount(0)
  await expect(focusPanel(page).getByRole('heading', { name: 'Wire the alpha form' })).toBeVisible()
  // The map selects it as tracing specifies: the task in focus, others dimmed.
  await expect(card(page, 'a3')).toHaveClass(/ring-foreground/)
  await expect(card(page, 'a1')).toHaveClass(dim)
  await expect(node(page, 'a3')).toBeInViewport()

  await focusPanel(page).getByRole('button', { name: 'Close' }).click()
  await expect(page).not.toHaveURL(/focus=/)
  await expect(panel(page).getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true')

  // The same holds from Charts, which keeps its tab.
  await panel(page).getByRole('tab', { name: 'Charts' }).click()
  await waitForCamera(node(page, 'a1'))
  await node(page, 'a1').click()
  await expect(page).toHaveURL(/focus=a1/)
  await expect(panel(page)).toHaveCount(0)
  await focusPanel(page).getByRole('button', { name: 'Close' }).click()
  await expect(panel(page).getByRole('tab', { name: 'Charts' })).toHaveAttribute('aria-selected', 'true')
})

test('workspace switching isolates insight requests and late generation results', async ({ page }) => {
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  const { seen } = await mockBoard(page, {
    workspaces: [
      { id: 'demo', name: 'Demo' },
      { id: 'other', name: 'Other' },
    ],
    summary: (_requirement, workspace) =>
      workspace === 'demo'
        ? summary({ headline: null })
        : summary({ headline: { ...summary().headline!, text: 'The other workspace headline.' } }),
    generate: async () => {
      await held
      return {
        status: 200,
        json: summary({ headline: { ...summary().headline!, text: 'The demo workspace headline.' } }),
      }
    },
  })
  await page.goto('/?view=map&scope=req-alpha&insight=summary')
  await expect(panel(page)).toContainText('Generating a summary')
  expect(seen.filter((entry) => entry.method === 'POST')).toHaveLength(1)

  // A switch lands on the Board; the other workspace's panel opens from its
  // own map.
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }).click()
  await page.getByRole('button', { name: 'Graph' }).click()
  await expect(hub(page)).toBeVisible()
  await waitForCamera(page.getByTestId('rf__node-frame:req-alpha'))
  const frame = await page.getByTestId('rf__node-frame:req-alpha').boundingBox()
  if (!frame) throw new Error('no alpha group')
  await page.mouse.click(frame.x + frame.width / 2, frame.y + 4)
  await expect(page).toHaveURL(/scope=req-alpha/)
  await zoomNear(page)
  await toolbar(page).getByRole('button', { name: 'Summary' }).click()
  await expect(panel(page)).toContainText('The other workspace headline.')
  release()
  // The late demo result never lands in the other workspace's panel.
  await expect.poll(() => seen.filter((entry) => entry.workspace === 'other').length).toBeGreaterThan(0)
  await expect(panel(page)).toContainText('The other workspace headline.')
  await expect(panel(page)).not.toContainText('The demo workspace headline.')
  const afterSwitch = seen.slice(seen.findIndex((entry) => entry.workspace === 'other'))
  expect(afterSwitch.every((entry) => entry.workspace === 'other')).toBe(true)
  expect(afterSwitch.some((entry) => entry.method === 'POST')).toBe(false)
})

test('the insight panel and toolbar remain operable without overflow on a narrow viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockBoard(page)
  await page.goto('/?view=map&scope=req-alpha&insight=charts')
  await expect(panel(page)).toBeVisible()
  const width = await page.evaluate(() => document.documentElement.clientWidth)
  const box = await panel(page).boundingBox()
  if (!box) throw new Error('no panel')
  expect(box.x).toBeGreaterThanOrEqual(0)
  expect(box.x + box.width).toBeLessThanOrEqual(width + 1)
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
  await panel(page).getByRole('tab', { name: 'Documents' }).click()
  await expect(page).toHaveURL(/insight=documents/)
  await expect(panel(page).getByRole('region', { name: 'System designs' })).toBeVisible()
  await panel(page).getByRole('button', { name: 'Close insights' }).click()
  await expect(panel(page)).toHaveCount(0)

  // The toolbar opens the panel again at the near zoom.
  await zoomNear(page)
  await toolbar(page).getByRole('button', { name: 'Summary' }).click()
  await expect(panel(page).getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true')
})
