import { expect, type Locator, type Page } from '@playwright/test'
import type { NextActionCompact, TaskMapItem, TaskMapRequirement, TaskMapResponse } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { waitForSheetSettled } from './helpers/sheet'

// The Board map (req-board-map v2; req-operator-next-action AC-1.7, AC-3.4,
// AC-6.1; component-web-task-surfaces, Board map). GET /v1/task-map is mocked
// in the shape internal/httpapi/task_map.go serves, so every assertion reads
// the rendering of the server's stated next actions and relations; the map
// derives no actor, kind, or reason of its own (DEC-61).

const at = '2026-10-18T09:00:00Z'

function action(label: string, actor: NextActionCompact['actor'], kind: string, judgment = false): NextActionCompact {
  return { label, actor, kind, judgment, summary: `Server summary: ${label}.`, deadline: null }
}

const working = action('Agent working', 'agent', 'working')
const queued = action('Queued for an agent', 'agent', 'queued')
const blocked = action('Blocked by a dependency', 'dependency', 'blocked')
const planGate = action('Plan approval', 'operator', 'plan_approval', true)
const mergeGate = action('Merge approval', 'operator', 'merge_approval', true)
const proposal = action('Document proposal', 'operator', 'document_proposal', true)
const question = action('Agent question', 'operator', 'agent_question', true)
const unsatisfiable = action('Dependency closed', 'operator', 'dependency_unsatisfiable', true)
const merging = action('Merging', 'factory', 'merging')
const jobFailure = action('Job failed', 'operator', 'job_failure')
const finished = action('Finished', 'none', 'terminal')

const titles: Record<string, string> = {
  'req-alpha': 'Alpha delivery',
  'req-beta': 'Beta review',
  'req-gamma': 'Gamma decisions',
  'req-delta': 'Delta follow-up',
}

function task(
  id: string,
  title: string,
  next: NextActionCompact,
  {
    state = 'running',
    repo = 'conveyor',
    depends = [],
    blocking = [],
    closedBlockers = [],
    reqs = [],
  }: {
    state?: TaskMapItem['state']
    repo?: string
    depends?: string[]
    blocking?: string[]
    closedBlockers?: string[]
    reqs?: string[]
  } = {},
): TaskMapItem {
  return {
    id,
    title,
    repo,
    state,
    last_event_at: at,
    next_action: next,
    depends_on: depends,
    blocking_task_ids: blocking,
    unsatisfiable_task_ids: closedBlockers,
    requirements: reqs.map((req) => ({ id: req, title: titles[req] ?? req })),
  }
}

// Four requirement clusters and the open work that serves no requirement:
// - alpha holds two tasks blocked by another task;
// - beta holds one, plus two gates and the tie-broken t1;
// - gamma holds three judgments and the factory's merge;
// - delta is served only by m1, whose neighbours home it in alpha, so delta
//   has no tile.
// Lifecycle states deliberately disagree with the stated actions.
const openTasks: TaskMapItem[] = [
  task('a1', 'Wire the alpha form', blocked, {
    state: 'running',
    depends: ['a2'],
    blocking: ['a2'],
    reqs: ['req-alpha'],
  }),
  task('a2', 'Build the alpha store', working, { state: 'queued', depends: ['f1'], reqs: ['req-alpha'] }),
  task('a3', 'Document the alpha store', blocked, {
    state: 'queued',
    depends: ['a2'],
    blocking: ['a2'],
    reqs: ['req-alpha'],
  }),
  task('m1', 'Plan the delta follow-up', queued, {
    depends: ['a2'],
    blocking: ['a2'],
    reqs: ['req-alpha', 'req-delta'],
  }),
  task('b1', 'Review the beta flow', blocked, { depends: ['a2'], blocking: ['a2'], reqs: ['req-beta'] }),
  task('b2', 'Plan the beta export', planGate, { state: 'awaiting_human', reqs: ['req-beta'] }),
  task('b3', 'Merge the beta import', mergeGate, { state: 'awaiting_human', reqs: ['req-beta'] }),
  task('t1', 'Shared beta and gamma groundwork', queued, { state: 'queued', reqs: ['req-beta', 'req-gamma'] }),
  task('g1', 'Confirm the gamma design', proposal, { reqs: ['req-gamma'] }),
  task('g2', 'Answer the gamma question', question, { reqs: ['req-gamma'] }),
  task('g3', 'Replace the closed gamma prerequisite', unsatisfiable, {
    depends: ['c1'],
    blocking: ['c1'],
    closedBlockers: ['c1'],
    reqs: ['req-gamma'],
  }),
  task('g4', 'Land the gamma copy', merging, { state: 'approved', reqs: ['req-gamma'] }),
  task('n1', 'Tidy the web build', queued, { repo: 'web', state: 'queued' }),
  task('o1', 'Rerun the failed check', jobFailure, { state: 'awaiting_human' }),
]

// The finished prerequisites the default read returns because open tasks
// depend on them, and one more finished task only include_finished returns.
const finishedPrerequisites: TaskMapItem[] = [
  task('f1', 'Lay the alpha foundation', finished, { state: 'merged', reqs: ['req-alpha'] }),
  task('c1', 'Abandoned gamma prerequisite', finished, { state: 'closed', reqs: ['req-gamma'] }),
]
const finishedOnly = task('f2', 'Ship the first alpha slice', finished, { state: 'merged', reqs: ['req-alpha'] })

const requirements: TaskMapRequirement[] = [
  { id: 'req-alpha', title: titles['req-alpha'], open_task_count: 4, merged_task_count: 1, total_task_count: 5 },
  { id: 'req-beta', title: titles['req-beta'], open_task_count: 4, merged_task_count: 4, total_task_count: 8 },
  { id: 'req-delta', title: titles['req-delta'], open_task_count: 1, merged_task_count: 1, total_task_count: 2 },
  { id: 'req-gamma', title: titles['req-gamma'], open_task_count: 5, merged_task_count: 0, total_task_count: 5 },
]

interface MapRead {
  repositories: string[]
  includeFinished: boolean
}

type MapServer = (read: MapRead) => TaskMapResponse

const defaultServer: MapServer = ({ repositories, includeFinished }) => {
  const tasks = [...openTasks, ...finishedPrerequisites, ...(includeFinished ? [finishedOnly] : [])]
  return {
    tasks: tasks.filter((item) => repositories.length === 0 || repositories.includes(item.repo)),
    requirements,
  }
}

async function mockMap(page: Page, server: MapServer = defaultServer) {
  const seen = { reads: [] as MapRead[], taskOperations: 0 }
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'operator', email: 'operator@example.test', role: 'operator' }, route.request()),
      })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', repos: [{ name: 'conveyor' }, { name: 'web' }] },
      })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    if (path === '/v1/task-operations') {
      seen.taskOperations++
      return route.fulfill({ json: [] })
    }
    if (path === '/v1/task-map') {
      expect(url.searchParams.get('workspace_id')).toBe('demo')
      const read = {
        repositories: url.searchParams.getAll('repository'),
        includeFinished: url.searchParams.get('include_finished') === 'true',
      }
      seen.reads.push(read)
      return route.fulfill({ json: server(read) })
    }
    const next = path.match(/^\/v1\/tasks\/([^/]+)\/next-action$/)
    if (next) {
      const item = openTasks.find((candidate) => candidate.id === next[1])
      return route.fulfill({ json: { ...item?.next_action, choices: [], details: {} } })
    }
    const detail = path.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (detail) {
      const item = openTasks.find((candidate) => candidate.id === detail[1])
      return route.fulfill({
        json: {
          task: {
            id: detail[1],
            workspace: 'demo',
            title: item?.title ?? detail[1],
            repo: item?.repo ?? 'conveyor',
            branch: `conveyor/${detail[1]}`,
            state: item?.state ?? 'running',
            created_at: at,
          },
          last_event_at: at,
          needs_attention: false,
          next_action: item?.next_action,
          jobs: [],
          events: [],
          work_orders: [],
          reviews: [],
          interventions: [],
          attachments: [],
          plan: { state: 'approved', version: 1 },
        },
      })
    }
    if (path.endsWith('/stream')) return route.fulfill({ status: 204 })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    return route.fulfill({ json: [] })
  })
  return seen
}

const tile = (page: Page, id: string) => page.getByTestId(`rf__node-frame:${id}`)
const node = (page: Page, id: string) => page.getByTestId(`rf__node-${id}`)
const edge = (page: Page, id: string) => page.getByTestId(`rf__edge-${id}`)
const taskNodes = (page: Page) => page.locator('.react-flow__node-task')
const panel = (page: Page) => page.locator('aside.w-80')

async function zoom(page: Page) {
  return page
    .locator('.react-flow__viewport')
    .evaluate((element) => new DOMMatrixReadOnly(getComputedStyle(element).transform).a)
}

// Cards print their status text from the zoom where full cards draw. A new
// layout first frames itself with a 400 ms fit, which would undo an earlier
// zoom, so zooming starts once the camera has settled and repeats until the
// zoom holds.
async function zoomToCards(page: Page) {
  await expect(taskNodes(page).first()).toBeVisible()
  await waitForCamera(taskNodes(page).first())
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

async function viewport(page: Page) {
  return page.locator('.react-flow__viewport').evaluate((element) => (element as HTMLElement).style.transform)
}

// The order tiles occupy the grid: by row, then by column.
async function tileOrder(page: Page) {
  const tiles = page.locator('.react-flow__node-tile')
  await expect(tiles.first()).toBeVisible()
  const boxes = await tiles.evaluateAll((elements) =>
    elements.map((element) => {
      const box = element.getBoundingClientRect()
      return { id: element.getAttribute('data-id') ?? '', x: Math.round(box.x), y: Math.round(box.y) }
    }),
  )
  return boxes.sort((a, b) => a.y - b.y || a.x - b.x).map((box) => box.id.slice('frame:'.length))
}

async function openTile(page: Page, id: string) {
  await tile(page, id).click()
  await expect(page).toHaveURL(new RegExp(`scope=${id}`))
  await expect(page.getByTestId(`rf__node-doc:${id}`)).toBeVisible()
}

async function waitForCamera(locator: Locator) {
  // A camera flight starts within a frame or two and lasts 450 to 650 ms;
  // read positions once two reads 300 ms apart agree.
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

test('the Graph switch writes view=map, and the address restores arrangement, requirement, and task', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/')
  // Kanban stays the default and is never written to the address.
  await expect(page.getByRole('button', { name: 'Kanban' })).toHaveAttribute('aria-pressed', 'true')
  await page.getByRole('button', { name: 'Graph' }).click()
  await expect(page).toHaveURL(/view=map/)
  await expect(page.getByRole('button', { name: 'Graph' })).toHaveAttribute('aria-pressed', 'true')
  await expect(tile(page, 'req-alpha')).toBeVisible()

  await page.getByRole('button', { name: 'Dependencies' }).click()
  await expect(page).toHaveURL(/arrangement=dependencies/)
  await expect(node(page, 'a1')).toBeVisible()
  await page.reload()
  await expect(page.getByRole('button', { name: 'Dependencies' })).toHaveAttribute('aria-pressed', 'true')
  await expect(node(page, 'a1')).toBeVisible()

  // A shared link names the requirement and the task.
  await page.goto('/?view=map&arrangement=requirements&scope=req-alpha&focus=a1')
  await expect(page.getByRole('heading', { name: 'Alpha delivery' })).toBeVisible()
  await expect(panel(page).getByRole('heading', { name: 'Wire the alpha form' })).toBeVisible()
  await expect(node(page, 'a1')).toBeInViewport()

  // An unknown arrangement is dropped, and leaving the map clears its fields.
  await page.goto('/?view=map&arrangement=designs')
  await expect(page.getByRole('button', { name: 'Requirements' })).toHaveAttribute('aria-pressed', 'true')
  await page.getByRole('button', { name: 'List' }).click()
  await expect(page).toHaveURL(/view=list/)
  await expect(page).not.toHaveURL(/arrangement|scope|focus/)
})

test('zoomed out, the map shows one equal tile per home cluster in blocked-then-judgment order, and nothing else', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map')
  // Delta is served only by m1, whose home is alpha, so it has no tile
  // (req-board-map AC-1.2).
  expect(await tileOrder(page)).toEqual(['req-alpha', 'req-beta', 'req-gamma', '__none'])
  await expect(tile(page, 'req-delta')).toHaveCount(0)

  const sizes = await page
    .locator('.react-flow__node-tile')
    .evaluateAll((elements) => elements.map((element) => `${element.clientWidth}x${element.clientHeight}`))
  expect(new Set(sizes).size).toBe(1)

  // Title, the blocked count or else the judgment count, the open count, and
  // the server's merged/total progress (AC-1.3).
  const alpha = tile(page, 'req-alpha')
  await expect(alpha).toContainText('Alpha delivery')
  await expect(alpha).toContainText('2 blocked')
  await expect(alpha).toContainText('4 open')
  await expect(alpha).toContainText('20%')
  const beta = tile(page, 'req-beta')
  await expect(beta).toContainText('1 blocked')
  await expect(beta).toContainText('4 open')
  await expect(beta).toContainText('50%')
  const gamma = tile(page, 'req-gamma')
  await expect(gamma).toContainText('3 to decide')
  await expect(gamma).toContainText('4 open')
  await expect(gamma).toContainText('0%')
  await expect(tile(page, '__none')).toContainText('No requirement')
  await expect(tile(page, '__none')).toContainText('2 open')

  // Zoomed out, no task node and no edge renders (AC-1.4).
  await expect(taskNodes(page)).toHaveCount(0)
  await expect(page.locator('.react-flow__node-hub')).toHaveCount(0)
  await expect(page.locator('.react-flow__edge')).toHaveCount(0)
})

test('a blocked task outranks any number of tasks needing judgment', async ({ page }) => {
  const decisions = Array.from({ length: 101 }, (_, index) =>
    task(`d${index}`, `Decide item ${index}`, proposal, { reqs: ['req-gamma'] }),
  )
  await mockMap(page, () => ({
    tasks: [
      ...decisions,
      task('x1', 'Waiting beta item', blocked, { depends: ['x2'], blocking: ['x2'], reqs: ['req-beta'] }),
      task('x2', 'Unrequired prerequisite', working),
    ],
    requirements,
  }))
  await page.goto('/?view=map')
  expect(await tileOrder(page)).toEqual(['req-beta', 'req-gamma', '__none'])
  await expect(tile(page, 'req-gamma')).toContainText('101 to decide')
})

test('selecting a tile flies into its single-home cluster, flowing into the requirement at a readable zoom', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map')
  await openTile(page, 'req-alpha')
  for (const id of ['a1', 'a2', 'a3', 'm1']) await expect(node(page, id)).toBeInViewport()
  await waitForCamera(node(page, 'a1'))
  expect(await zoom(page)).toBeGreaterThanOrEqual(0.38)
  await expect(node(page, 'a1')).toContainText('Wire the alpha form')
  // The tasks flow down into the requirement's node.
  const hub = await page.getByTestId('rf__node-doc:req-alpha').boundingBox()
  const leaf = await node(page, 'a1').boundingBox()
  expect(hub && leaf && hub.y > leaf.y).toBe(true)
  await expect(page.getByTestId('rf__node-doc:req-alpha')).toContainText('1/5 delivered')

  // Every open task has exactly one home: t1 ties beta and gamma on
  // neighbours and goes to beta, the requirement fewer open tasks serve; m1
  // follows its neighbours to alpha rather than delta.
  await expect(node(page, 't1')).toHaveCount(1)
  await expect(node(page, 'm1')).toHaveCount(1)
  const beta = await page.getByTestId('rf__node-frame:req-beta').boundingBox()
  const t1 = await node(page, 't1').boundingBox()
  expect(beta && t1 && t1.x >= beta.x && t1.x <= beta.x + beta.width && t1.y >= beta.y).toBe(true)
  // The merged prerequisite stays hidden while Finished is off.
  await expect(node(page, 'f1')).toHaveCount(0)
})

test('task nodes print the stated label with a mark from the stated actor, and waiting is a separate mark', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map&scope=req-alpha')
  await expect(node(page, 'a1')).toBeVisible()
  await page.goto('/?view=map&arrangement=dependencies')
  await zoomToCards(page)
  const label = (id: string, text: string) => node(page, id).getByText(text, { exact: true })
  // Each tone follows actor and judgment, never the lifecycle state.
  await expect(label('a1', 'Blocked by a dependency')).toHaveClass(/text-failure/)
  await expect(label('a2', 'Agent working')).toHaveClass(/text-primary/)
  await expect(label('n1', 'Queued for an agent')).toHaveClass(/text-positive/)
  await expect(label('b2', 'Plan approval')).toHaveClass(/text-attention/)
  await expect(label('g1', 'Document proposal')).toHaveClass(/text-attention/)
  await expect(label('g4', 'Merging')).toHaveClass(/text-muted/)
  // A mechanical operator step without judgment is not a decision.
  await expect(label('o1', 'Job failed')).toHaveClass(/text-muted/)

  // m1 is queued for an agent and waits on a2: the dependency mark sits
  // beside the stated status rather than replacing it (AC-2.2).
  await expect(label('m1', 'Queued for an agent')).toHaveClass(/text-positive/)
  const mark = node(page, 'm1').locator('[data-dependency-mark]')
  await expect(mark).toHaveAttribute('data-dependency-mark', 'waiting')
  await expect(mark).toHaveAttribute('aria-label', 'Waiting for Build the alpha store')
  const closed = node(page, 'g3').locator('[data-dependency-mark]')
  await expect(closed).toHaveAttribute('data-dependency-mark', 'unsatisfiable')
  await expect(closed).toHaveAttribute(
    'aria-label',
    'Needs attention: Abandoned gamma prerequisite closed without merging',
  )
  await expect(node(page, 'a2').locator('[data-dependency-mark]')).toHaveCount(0)

  // Every node counts the open tasks that depend on it, zero included.
  await expect(node(page, 'a2')).toContainText('→4')
  await expect(node(page, 'a1')).toContainText('→0')
  await expect(node(page, 'f1')).toContainText('→5')

  // Header chips count by the stated label.
  await expect(page.getByRole('button', { name: '3 Blocked by a dependency' })).toBeVisible()
  await expect(page.getByRole('button', { name: '1 Job failed' })).toBeVisible()
})

test('a dependency diamond counts each open dependent once', async ({ page }) => {
  // d4 waits on d2 and d3, which both wait on d1: d1 unblocks three tasks,
  // not four (req-board-map AC-2.2).
  await mockMap(page, () => ({
    tasks: [
      task('d1', 'Diamond root', working),
      task('d2', 'Diamond left', blocked, { depends: ['d1'], blocking: ['d1'] }),
      task('d3', 'Diamond right', blocked, { depends: ['d1'], blocking: ['d1'] }),
      task('d4', 'Diamond join', blocked, { depends: ['d2', 'd3'], blocking: ['d2', 'd3'] }),
    ],
    requirements: [],
  }))
  await page.goto('/?view=map&arrangement=dependencies')
  await zoomToCards(page)
  await expect(node(page, 'd1')).toContainText('→3')
  await expect(node(page, 'd2')).toContainText('→1')
  await expect(node(page, 'd4')).toContainText('→0')
  await expect(node(page, 'd4').locator('[data-dependency-mark]')).toHaveAttribute(
    'aria-label',
    'Waiting for Diamond left, Diamond right',
  )
})

test('a stated-label chip highlights its tasks and dims the rest, in both arrangements', async ({ page }) => {
  await mockMap(page)
  const dimmed = (id: string) => node(page, id).locator(':scope > div')
  const dim = /\bopacity-(15|20)\b/
  const cases = [
    { address: '/?view=map&scope=req-alpha', chip: '1 Agent working', match: ['a2'], other: ['a1', 'a3', 'm1'] },
    {
      address: '/?view=map&arrangement=dependencies',
      chip: '3 Blocked by a dependency',
      match: ['a1', 'a3', 'b1'],
      other: ['a2', 'n1', 'g1'],
    },
  ]
  for (const { address, chip, match, other } of cases) {
    await page.goto(address)
    await expect(node(page, match[0])).toBeVisible()
    const button = page.getByRole('button', { name: chip, exact: true })
    await button.click()
    await expect(button).toHaveAttribute('aria-pressed', 'true')
    for (const id of match) await expect(dimmed(id)).not.toHaveClass(dim)
    for (const id of other) await expect(dimmed(id)).toHaveClass(dim)
    // Turning the chip off restores every task.
    await button.click()
    await expect(button).toHaveAttribute('aria-pressed', 'false')
    for (const id of [...match, ...other]) await expect(dimmed(id)).not.toHaveClass(dim)
  }
})

test('selecting a task traces both directions, dims the rest, draws its cross-cluster edges, and opens the panel', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map&scope=req-beta')
  await expect(node(page, 'b1')).toBeVisible()
  // b1 waits on a2 in alpha: that edge is not drawn until b1 is selected.
  await expect(edge(page, 'a2->b1')).toHaveCount(0)
  await node(page, 'b1').click()
  await expect(page).toHaveURL(/focus=b1/)
  await expect(edge(page, 'a2->b1')).toHaveCount(1)
  await expect(node(page, 'b2').locator(':scope > div')).toHaveClass(/opacity-20/)

  await page.goto('/?view=map&scope=req-alpha&focus=a2')
  await expect(node(page, 'a2').locator(':scope > div')).toHaveClass(/ring-foreground/)
  // Upstream and downstream take two treatments, and both animate.
  const upstream = edge(page, 'f1->a2')
  const downstream = edge(page, 'a2->a1')
  await page.getByRole('button', { name: 'Finished', exact: true }).click()
  await expect(upstream.locator('path').first()).toHaveAttribute('style', /var\(--color-primary\)/)
  await expect(downstream.locator('path').first()).toHaveAttribute('style', /var\(--color-attention-dot\)/)
  await expect(downstream).toHaveClass(/animated/)
  await expect(upstream).toHaveClass(/animated/)
  // Its cross-cluster dependent b1 is drawn while a2 is selected.
  await expect(edge(page, 'a2->b1')).toHaveCount(1)

  // The panel: the stated action, direct relations with their labels, the
  // open upstream count, and the served requirements.
  await page.goto('/?view=map&scope=req-alpha&focus=a1')
  const aside = panel(page)
  await expect(aside.getByRole('heading', { name: 'Wire the alpha form' })).toBeVisible()
  await expect(aside).toContainText('Blocked by a dependency.')
  await expect(aside).toContainText('Server summary: Blocked by a dependency.')
  await expect(aside.locator('[data-dependency-mark]')).toHaveAttribute(
    'aria-label',
    'Waiting for Build the alpha store',
  )
  await expect(aside).toContainText('Waits on · 1 open of 2 upstream')
  await expect(aside).toContainText('Unblocks · 0 open downstream')
  await expect(aside.getByRole('button', { name: /Build the alpha store/ })).toContainText('a2 · Agent working')
  await expect(aside.getByRole('button', { name: 'Alpha delivery' })).toBeVisible()
  await expect(aside).not.toContainText('Planning can proceed')
  // A listed task is selectable in place.
  await aside.getByRole('button', { name: /Build the alpha store/ }).click()
  await expect(page).toHaveURL(/focus=a2/)
  await expect(aside).toContainText('Unblocks · 4 open downstream')
  await expect(aside.getByRole('button', { name: /Wire the alpha form/ })).toContainText('a1 · Blocked by a dependency')
})

test('hover lights only the task’s own edges after a pause and dims nothing, in both arrangements', async ({
  page,
}) => {
  await mockMap(page)
  for (const address of ['/?view=map&scope=req-alpha', '/?view=map&arrangement=dependencies']) {
    await page.goto(address)
    await expect(node(page, 'a3')).toBeVisible()
    await waitForCamera(node(page, 'a3'))
    const own = edge(page, 'a2->a3').locator('path').first()
    const other = edge(page, 'a2->a1').locator('path').first()
    await node(page, 'a3').hover()
    await expect(own).toHaveAttribute('style', /var\(--color-muted\)/)
    await expect(other).not.toHaveAttribute('style', /var\(--color-muted\)/)
    await expect(other).toHaveAttribute('style', /opacity: 1/)
    await expect(page.locator('.react-flow__node-task > .opacity-20')).toHaveCount(0)
    expect(page.url()).not.toContain('focus=')
    // Edges out of a task an agent is working animate without a selection.
    await expect(edge(page, 'a2->a1')).toHaveClass(/animated/)
    await page.mouse.move(2, 2)
    await expect(own).not.toHaveAttribute('style', /var\(--color-muted\)/)
  }
})

test('search by title or ID selects the first match and moves to it', async ({ page }) => {
  await mockMap(page)
  await page.goto('/?view=map')
  await expect(tile(page, 'req-alpha')).toBeVisible()
  const find = page.getByRole('textbox', { name: 'Find a task' })
  await find.fill('gamma question')
  await find.press('Enter')
  await expect(page).toHaveURL(/focus=g2/)
  await expect(panel(page).getByRole('heading', { name: 'Answer the gamma question' })).toBeVisible()
  await expect(node(page, 'g2')).toBeInViewport()
  await find.fill('b3')
  await find.press('Enter')
  await expect(page).toHaveURL(/focus=b3/)
  await expect(node(page, 'b3')).toBeInViewport()
})

test('the dependency arrangement lays every open task and its finished prerequisites out by depth', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map&arrangement=dependencies')
  for (const item of [...openTasks, ...finishedPrerequisites]) await expect(node(page, item.id)).toHaveCount(1)
  await expect(node(page, 'f2')).toHaveCount(0)
  const x = async (id: string) => (await node(page, id).boundingBox())?.x ?? Number.NaN
  expect(await x('f1')).toBeLessThan(await x('a2'))
  expect(await x('a2')).toBeLessThan(await x('a1'))
  expect(await x('c1')).toBeLessThan(await x('g3'))
})

test('repository and Finished filters travel to the one map read in both arrangements', async ({ page }) => {
  const seen = await mockMap(page)
  await page.goto('/?view=map')
  await expect(tile(page, 'req-alpha')).toBeVisible()
  expect(seen.reads.at(-1)).toEqual({ repositories: [], includeFinished: false })

  // Finished on: finished tasks join clusters with open work, and never make
  // a finished-only tile.
  await page.getByRole('button', { name: 'Finished', exact: true }).click()
  await expect.poll(() => seen.reads.at(-1)).toEqual({ repositories: [], includeFinished: true })
  expect(await tileOrder(page)).toEqual(['req-alpha', 'req-beta', 'req-gamma', '__none'])
  await openTile(page, 'req-alpha')
  await expect(node(page, 'f2')).toHaveCount(1)
  await expect(node(page, 'f1')).toHaveCount(1)
  await page.getByRole('button', { name: 'Finished', exact: true }).click()
  await expect(node(page, 'f2')).toHaveCount(0)

  await page.getByRole('button', { name: 'Dependencies' }).click()
  await page.getByRole('combobox', { name: 'Repository' }).selectOption('web')
  await expect.poll(() => seen.reads.at(-1)).toEqual({ repositories: ['web'], includeFinished: false })
  await expect(node(page, 'n1')).toBeVisible()
  await expect(node(page, 'a1')).toHaveCount(0)
  await page.getByRole('button', { name: 'Finished', exact: true }).click()
  await expect.poll(() => seen.reads.at(-1)).toEqual({ repositories: ['web'], includeFinished: true })
  await page.getByRole('button', { name: 'Requirements' }).click()
  await expect(tile(page, '__none')).toBeVisible()
  await expect(tile(page, 'req-alpha')).toHaveCount(0)
  expect(seen.taskOperations).toBe(0)
})

test('opening a task shows the Board sheet over the map, and closing it returns to the same position', async ({
  page,
}) => {
  await mockMap(page)
  await page.goto('/?view=map&scope=req-alpha&focus=a1')
  await expect(node(page, 'a1')).toBeVisible()
  await waitForCamera(node(page, 'a1'))
  await panel(page).getByRole('button', { name: 'Open task' }).click()
  await expect(page).toHaveURL(/\/tasks\/a1\?.*view=map/)
  const sheet = page.getByRole('dialog')
  await waitForSheetSettled(sheet)
  await expect(sheet).toContainText('Wire the alpha form')
  const before = await viewport(page)
  await page.keyboard.press('Escape')
  await expect(sheet).toHaveCount(0)
  await expect(page).toHaveURL(/scope=req-alpha/)
  await expect(page).toHaveURL(/focus=a1/)
  expect(await viewport(page)).toBe(before)
  await expect(panel(page).getByRole('heading', { name: 'Wire the alpha form' })).toBeVisible()

  // Double-click opens the same sheet. The event is sent on its own: the
  // panel's open and close reflow the canvas between a scripted
  // double-click's two presses.
  // A double-click's second click leaves the selection alone.
  await node(page, 'a3').dispatchEvent('click', { detail: 2 })
  await expect(page).toHaveURL(/focus=a1/)
  await node(page, 'a3').dispatchEvent('dblclick')
  await expect(page).toHaveURL(/\/tasks\/a3\?/)
  await waitForSheetSettled(page.getByRole('dialog'))
})

test('zooming across the threshold keeps the requirement under the pointer', async ({ page }) => {
  await mockMap(page)
  await page.goto('/?view=map')
  await expect(tile(page, 'req-gamma')).toBeVisible()
  await expect.poll(() => zoom(page)).toBeLessThan(0.3)
  const box = await tile(page, 'req-gamma').boundingBox()
  if (!box) throw new Error('no gamma tile')
  const pointer = { x: box.x + box.width * 0.6, y: box.y + box.height * 0.4 }
  await page.mouse.move(pointer.x, pointer.y)
  const under = () =>
    page.evaluate(
      ({ x, y }) =>
        document
          .elementsFromPoint(x, y)
          .map((element) => element.closest('.react-flow__node-tile')?.getAttribute('data-id'))
          .find(Boolean),
      pointer,
    )
  while ((await zoom(page)) < 0.3) await page.mouse.wheel(0, -120)
  await expect(taskNodes(page).first()).toBeVisible()
  expect(await under()).toBe('frame:req-gamma')
  while ((await zoom(page)) >= 0.3) await page.mouse.wheel(0, 120)
  await expect(taskNodes(page)).toHaveCount(0)
  expect(await under()).toBe('frame:req-gamma')
})

test('500 open and 2,000 finished tasks show their tiles within two seconds of the read', async ({ page }) => {
  test.setTimeout(60_000)
  const kinds = [working, queued, blocked, planGate, merging]
  const open: TaskMapItem[] = []
  const done: TaskMapItem[] = []
  for (let index = 0; index < 2000; index++)
    done.push(task(`f${index}`, `Finished ${index}`, finished, { state: 'merged', reqs: [`req-${index % 25}`] }))
  for (let index = 0; index < 500; index++) {
    const upstream = index >= 25 ? [`o${index - 25}`] : []
    const next = kinds[index % kinds.length]
    open.push(
      task(`o${index}`, `Open task ${index}`, next, {
        depends: [...upstream, ...[0, 1, 2, 3].map((offset) => `f${index * 4 + offset}`)],
        blocking: next === blocked ? upstream : [],
        reqs: [`req-${index % 25}`],
      }),
    )
  }
  const entries = Array.from({ length: 25 }, (_, index) => ({
    id: `req-${index}`,
    title: `Requirement ${index}`,
    open_task_count: 20,
    merged_task_count: 80,
    total_task_count: 100,
  }))
  await mockMap(page, () => ({ tasks: [...open, ...done], requirements: entries }))
  // The read completes when its response resolves in the page.
  await page.addInitScript(() => {
    const original = window.fetch
    window.fetch = async (...args) => {
      const response = await original(...args)
      const url = String(args[0] instanceof Request ? args[0].url : args[0])
      if (url.includes('/v1/task-map')) {
        const body = await response.clone().json()
        ;(window as unknown as { mapReadAt: number }).mapReadAt = performance.now()
        return new Response(JSON.stringify(body), { status: response.status, headers: response.headers })
      }
      return response
    }
  })
  await page.goto('/?view=map')
  const tilesAt = await page.evaluate(
    () =>
      new Promise<number>((resolve) => {
        const tick = () => {
          if (document.querySelectorAll('.react-flow__node-tile').length >= 25) resolve(performance.now())
          else requestAnimationFrame(tick)
        }
        tick()
      }),
  )
  const readAt = await page.evaluate(() => (window as unknown as { mapReadAt: number }).mapReadAt)
  expect(readAt).toBeGreaterThan(0)
  const elapsed = tilesAt - readAt
  test.info().annotations.push({ type: 'tiles after read (ms)', description: String(Math.round(elapsed)) })
  expect(elapsed).toBeLessThanOrEqual(2000)
  await expect(page.locator('.react-flow__node-tile')).toHaveCount(25)
  await expect(taskNodes(page)).toHaveCount(0)
})

// The title row carries only the title and the actions; the shared toolbar
// closes with the icon view switch, which keeps one position in every view
// (component-web-task-surfaces; req-board-map AC-1.1; req-operator-next-action
// AC-6.1).
test('the view switch closes one shared toolbar at the same position in every view', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 })
  await mockMap(page)
  await page.goto('/')
  const title = page.getByRole('heading', { name: 'Board', exact: true, level: 1 })
  const header = page.locator('header').filter({ has: title })
  await expect(header.getByRole('group', { name: 'Board view' })).toHaveCount(0)
  const mcp = header.getByRole('button', { name: 'MCP', exact: true })
  await expect(mcp).toHaveAttribute('title', 'MCP')
  await expect(mcp).toHaveText('')

  const toolbar = page.getByTestId('board-toolbar')
  const views = toolbar.getByRole('group', { name: 'Board view' })
  const button = (name: string) => views.getByRole('button', { name, exact: true })
  const place = async () => {
    const box = await views.boundingBox()
    const bar = await toolbar.boundingBox()
    if (!box || !bar) throw new Error('the view switch is not laid out')
    // The switch is the toolbar's last control: nothing sits to its right
    // but the toolbar's own padding.
    expect(bar.x + bar.width - (box.x + box.width)).toBeLessThanOrEqual(24.5)
    return { right: Math.round(box.x + box.width), top: Math.round(box.y) }
  }
  const expectPressed = async (pressed: string) => {
    for (const name of ['Kanban', 'List', 'Graph']) {
      await expect(button(name)).toHaveAttribute('title', name)
      await expect(button(name)).toHaveText('')
      await expect(button(name)).toHaveAttribute('aria-pressed', String(name === pressed))
    }
    // The selection takes the soft treatment; New task keeps the only fill.
    await expect(button(pressed)).toHaveClass(/bg-primary-soft/)
    await expect(button(pressed)).not.toHaveClass(/(^|\s)bg-primary(\s|$)/)
  }

  await expect(toolbar).toHaveCount(1)
  await expectPressed('Kanban')
  const kanban = await place()

  await button('List').click()
  await expect(page).toHaveURL(/view=list/)
  await expect(toolbar).toHaveCount(1)
  await expectPressed('List')
  expect(await place()).toEqual(kanban)

  await button('Graph').click()
  await expect(page).toHaveURL(/view=map/)
  await expect(tile(page, 'req-alpha')).toBeVisible()
  await expect(toolbar).toHaveCount(1)
  await expect(toolbar.getByRole('group', { name: 'Arrange by' })).toBeVisible()
  await expectPressed('Graph')
  expect(await place()).toEqual(kanban)

  await button('Kanban').click()
  await expect(page).not.toHaveURL(/view=/)
  await expectPressed('Kanban')
})

// A failed map read still leaves the switch in place, so the operator can
// leave the view without reloading.
test('the view switch stays reachable when the map read fails', async ({ page }) => {
  await mockMap(page)
  await page.route('**/v1/task-map**', (route) => route.fulfill({ status: 500, json: { error: 'unavailable' } }))
  await page.goto('/?view=map')
  const views = page.getByTestId('board-toolbar').getByRole('group', { name: 'Board view' })
  await expect(views.getByRole('button', { name: 'Graph', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await views.getByRole('button', { name: 'List', exact: true }).click()
  await expect(page).toHaveURL(/view=list/)
})

// At phone width the title row and the toolbar wrap inside the viewport and
// every control stays reachable, in all three views.
test('at 390 px the Board header and toolbar wrap without overflow in every view', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockMap(page)
  for (const path of ['/', '/?view=list', '/?view=map']) {
    await page.goto(path)
    const header = page.locator('header').filter({ has: page.getByRole('heading', { name: 'Board', exact: true }) })
    const toolbar = page.getByTestId('board-toolbar')
    if (path.endsWith('map')) await expect(toolbar.getByRole('group', { name: 'Arrange by' })).toBeVisible()
    await expect(toolbar.getByRole('group', { name: 'Board view' })).toBeInViewport()
    for (const row of [header, toolbar]) {
      expect(await row.evaluate((node) => node.scrollWidth <= node.clientWidth), `${path} row overflow`).toBe(true)
      for (const control of await row.getByRole('button').all()) {
        const box = await control.boundingBox()
        expect(box, `${path} control laid out`).not.toBeNull()
        expect((box?.x ?? -1) >= 0 && (box?.x ?? 0) + (box?.width ?? 0) <= 390.5, `${path} control inside 390 px`).toBe(
          true,
        )
      }
    }
  }
  await page.goto('/')
  await page.getByRole('button', { name: 'MCP', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'MCP Setup' })).toBeVisible()
})
