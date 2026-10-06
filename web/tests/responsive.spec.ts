// Narrow viewports: the shell folds its two navigation columns into a drawer
// behind a top bar, the document pages fold their tree into a drawer above the
// canvas, and no surface scrolls sideways at phone width.
import { expect, type Locator, type Page, test } from '@playwright/test'
import { waitForSheetSettled } from './helpers/sheet'

const phone = { width: 390, height: 844 }
const tabletPortrait = { width: 820, height: 1180 }
const tabletLandscape = { width: 1180, height: 820 }

const designVersion = {
  document_id: 'design-dispatch',
  version: 1,
  content: '# Dispatch\n\nThe dispatcher owns durable stage transitions.',
  governs: [{ repository: 'conveyor', paths: ['internal/dispatch/**'] }],
  origin: 'operator',
  confirmed: true,
  workspace: 'demo',
  created_at: '2026-08-05T08:00:00Z',
}
const design = {
  document: {
    id: 'design-dispatch',
    slug: 'dispatch',
    title: 'Dispatch ownership',
    category: 'Architecture',
    current_version: 1,
    workspace: 'demo',
    created_at: '2026-08-05T08:00:00Z',
    updated_at: '2026-08-05T09:00:00Z',
  },
  current_version: designVersion,
  pending_versions: [],
  versions: [designVersion],
  lineage_total: 0,
  lineage: [],
  drift: [],
}
const { content: _content, governs: _governs, ...designVersionSummary } = designVersion
const designSummary = {
  document: design.document,
  current_version: designVersionSummary,
  pending_versions: [],
  pending_version_count: 0,
  drift_count: 0,
}

const activity = [
  {
    task: {
      id: 'task-recent',
      workspace: 'demo',
      source: 'operator',
      title: 'Fold the navigation into a drawer on narrow viewports',
      repo: 'conveyor',
      branch: 'conveyor/task-recent',
      state: 'running',
      created_at: '2026-09-14T10:00:00Z',
    },
    latest_stage: 'implement',
    last_event_at: '2026-09-14T11:00:00Z',
    needs_attention: false,
  },
  {
    task: {
      id: 'task-review',
      workspace: 'demo',
      source: 'cli',
      title: 'Stack the tasks table below the md breakpoint',
      repo: 'web',
      branch: 'web/task-review',
      state: 'awaiting_human',
      created_at: '2026-09-13T10:00:00Z',
    },
    latest_stage: 'review',
    last_event_at: '2026-09-14T09:00:00Z',
    needs_attention: true,
  },
]

const operations = activity.map((item) => ({
  ...item,
  plan: { state: 'approved', version: 1 },
}))

async function mockShell(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    const paged = (items: unknown[]) =>
      route.fulfill({
        headers: {
          'X-Conveyor-Total': String(items.length),
          'X-Conveyor-Limit': String(items.length || 1),
          'X-Conveyor-Offset': '0',
        },
        json: items,
      })
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'other', name: 'Other' },
        ],
      })
    if (path === '/v1/me')
      return route.fulfill({
        json: { id: 'usr_operator', email: 'operator@example.test', display_name: 'Operator', role: 'operator' },
      })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }] },
      })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { task_count: 1, pending_proposal_count: 0, total: 1 } } })
    if (path === '/v1/activity') return paged(activity)
    if (path === '/v1/task-operations') return paged(operations)
    if (path === '/v1/system-designs') return route.fulfill({ json: [designSummary] })
    if (path === '/v1/system-designs/design-dispatch') return route.fulfill({ json: design })
    if (path === '/v1/workers')
      return route.fulfill({
        json: { workers: [], worker_expected: false, worker_available: false, setup_serviceability: {} },
      })
    return route.fulfill({ json: [] })
  })
}

// Task detail at narrow widths: the header title gives way to fixed-size
// controls, each attached document keeps its ID under its title, and the plan
// header wraps whole groups. The long title is what made the header overlap.
const layoutTitle =
  'Filter ambiguous processes during validation cache cleanup without widening the disposable deletion boundary'
const layoutTask = {
  task: {
    id: 'task-layout',
    workspace: 'demo',
    source: 'operator',
    title: layoutTitle,
    repo: 'conveyor',
    branch: 'conveyor/task-layout',
    state: 'running',
    created_at: '2026-10-05T08:00:00Z',
    context: {
      requirements: [
        { id: 'req-review-gates-evidence', title: 'Review, gates, and evidence', version: 4, archived: false },
        { id: 'req-retired-review', title: 'Retired review outcome', version: 2, archived: true },
      ],
      designs: [
        { id: 'component-verification-strategy', title: 'Verification strategy', version: 22, archived: false },
      ],
    },
  },
  jobs: [],
  events: [],
  interventions: [],
  checkout_available: false,
  checkout_guidance: '',
  needs_attention: false,
  at_merge_gate: false,
  work_orders: [],
  spec: {
    task_id: 'task-layout',
    version: 2,
    content: '## Approach\n\nKeep cache cleanup within the existing shared process inspector.',
    acceptance_count: 0,
    acceptance: [],
    decomposition: [],
    approved: true,
    created_at: '2026-10-05T12:00:00Z',
    approved_at: '2026-10-05T13:36:00Z',
  },
}
const draftLayoutTask = {
  ...layoutTask,
  task: { ...layoutTask.task, id: 'task-layout-draft', state: 'awaiting_human' },
  spec: { ...layoutTask.spec, task_id: 'task-layout-draft', approved: false, approved_at: undefined },
}
const layoutRequirementVersion = {
  requirement_id: 'req-layout',
  version: 1,
  content:
    '# Narrow layouts\n\nKeep record headers usable on phones.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Record headers keep their controls usable at phone width.\n```',
  statements: [{ id: 'REQ-1', statement: 'Record headers keep their controls usable at phone width.' }],
  origin: 'operator',
  confirmed: true,
  workspace: 'demo',
  created_at: '2026-10-05T08:00:00Z',
}
const layoutRequirement = {
  requirement: {
    id: 'req-layout',
    slug: 'narrow-layouts',
    title: 'Narrow layouts keep every record header usable on a phone',
    statement_high_water_mark: 1,
    workspace: 'demo',
    created_at: '2026-10-05T08:00:00Z',
    updated_at: '2026-10-05T08:00:00Z',
  },
  current_version: layoutRequirementVersion,
  pending_versions: [],
  serving_blueprints: [],
  serving_tasks: [],
  planning_sessions: [],
  artifacts: [],
  lineage: [],
  lineage_total: 0,
  lineage_snapshot_id: 1,
  staleness: { delivery_after_intent: false, partial_evaluation: false, deliveries: [], active_drift: [] },
  migrated_seed: false,
  confirmation_eligible: true,
}
const {
  content: _requirementContent,
  statements: _statements,
  ...layoutRequirementVersionSummary
} = layoutRequirementVersion
const layoutRequirementSummary = {
  requirement: layoutRequirement.requirement,
  current_version: layoutRequirementVersionSummary,
  pending_version_count: 0,
  serving_tasks: [],
  staleness: layoutRequirement.staleness,
  confirmation_eligible: true,
}

// Registered after mockShell so these paths win; every other read falls back
// to the shell's fixtures. Lineage reads are recorded so a spec can prove the
// explorer still reads only once its trigger is pressed.
async function mockRecordDetail(page: Page) {
  const lineageReads: string[] = []
  await page.route('**/v1/**', (route) => {
    const path = new URL(route.request().url()).pathname
    const detail = path.match(/^\/v1\/tasks\/(task-layout(?:-draft)?)\/(activity|events\/stream|verification)$/)
    if (detail) {
      const [, taskId, resource] = detail
      if (resource === 'activity')
        return route.fulfill({ json: taskId === 'task-layout' ? layoutTask : draftLayoutTask })
      if (resource === 'events/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' })
      return route.fulfill({ json: { head_sha: '', current_context_id: '', contexts: { items: [] }, overview: {} } })
    }
    if (path.startsWith('/v1/lineage/')) {
      lineageReads.push(path)
      const [, , , type, id] = path.split('/')
      const root = { type, id: decodeURIComponent(id) }
      return route.fulfill({ json: { roots: [root], nodes: [root], links: [], truncated: false } })
    }
    if (path === '/v1/requirements') return route.fulfill({ json: [layoutRequirementSummary] })
    if (path === '/v1/requirements/req-layout') return route.fulfill({ json: layoutRequirement })
    return route.fallback()
  })
  return lineageReads
}

type Box = { x: number; y: number; width: number; height: number }

function overlaps(a: Box, b: Box) {
  return a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height
}

// The title and every header control occupy disjoint boxes, the title keeps a
// usable width, and nothing in the header paints past its right edge.
async function expectHeaderControlsClear(header: Locator, title: Locator) {
  const titleBox = await title.boundingBox()
  const headerBox = await header.boundingBox()
  if (!titleBox || !headerBox) throw new Error('the header title is not rendered')
  expect(titleBox.width).toBeGreaterThan(48)
  const controls = await header.locator(':scope > a, :scope > button, :scope > span:has(> button)').all()
  expect(controls.length).toBeGreaterThan(3)
  const boxes: Box[] = []
  for (const control of controls) {
    const box = await control.boundingBox()
    if (!box) throw new Error('a header control is not rendered')
    expect(overlaps(titleBox, box)).toBe(false)
    expect(box.x + box.width).toBeLessThanOrEqual(headerBox.x + headerBox.width)
    for (const other of boxes) expect(overlaps(other, box)).toBe(false)
    boxes.push(box)
  }
  // A clipped label still overflows its button; content width must fit.
  const spills = await header
    .locator('button')
    .evaluateAll((buttons) =>
      buttons
        .filter((button) => button.scrollWidth > button.clientWidth + 1)
        .map((button) => button.ariaLabel ?? button.textContent),
    )
  expect(spills).toEqual([])
}

// Below sm each row's ID and version sit under its title and start at the same
// column; from sm up they share the title's line.
async function expectContextRows(page: Page, stacked: boolean) {
  const card = page.getByRole('region', { name: 'Attached context' })
  for (const [title, meta] of [
    ['Review, gates, and evidence', 'req-review-gates-evidence · v4'],
    ['Retired review outcome', 'req-retired-review · v2'],
    ['Verification strategy', 'component-verification-strategy · v22'],
  ]) {
    const titleBox = await card.getByRole('link', { name: title }).boundingBox()
    const metaBox = await card.getByText(meta, { exact: true }).boundingBox()
    if (!titleBox || !metaBox) throw new Error(`context row ${title} is not rendered`)
    if (stacked) {
      expect(metaBox.y).toBeGreaterThanOrEqual(titleBox.y + titleBox.height - 1)
      expect(Math.abs(metaBox.x - titleBox.x)).toBeLessThanOrEqual(1)
    } else {
      expect(Math.abs(metaBox.y + metaBox.height / 2 - (titleBox.y + titleBox.height / 2))).toBeLessThanOrEqual(4)
      expect(metaBox.x).toBeGreaterThan(titleBox.x + titleBox.width)
    }
  }
  await expect(card.getByRole('button', { name: 'Remove context Review, gates, and evidence' })).toBeVisible()
  await expect(card.getByText('Archived', { exact: true })).toBeVisible()
}

// Below sm the trigger renders only its icon; the label stays in the
// accessibility tree as a 1px screen-reader span, which Playwright still counts
// as visible, so the rendered width is what proves it is icon-only.
async function expectIconOnlyExplorer(explorer: Locator) {
  await expect(explorer).toBeVisible()
  const label = await explorer.getByText('Knowledge explorer').boundingBox()
  expect(label?.width ?? 0).toBeLessThanOrEqual(1)
}

// "Execution plan" and its timestamp each render on a single line.
async function expectPlanHeaderUnbroken(page: Page) {
  const title = page.getByRole('heading', { name: 'Execution plan', exact: true })
  const lineHeight = await title.evaluate((node) => Number.parseFloat(getComputedStyle(node).lineHeight))
  const titleBox = await title.boundingBox()
  if (!titleBox) throw new Error('the plan title is not rendered')
  expect(titleBox.height).toBeLessThan(lineHeight * 1.5)
  const stamp = page.getByText(/^(approved|drafted) /)
  const stampLine = await stamp.evaluate((node) => Number.parseFloat(getComputedStyle(node).lineHeight))
  const stampBox = await stamp.boundingBox()
  if (!stampBox) throw new Error('the plan timestamp is not rendered')
  expect(stampBox.height).toBeLessThan(stampLine * 1.5)
}

// The page body must never scroll sideways: a lane row or a wide table may
// scroll inside its own container, but the viewport stays put.
async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
  }))
  expect(overflow.scrollWidth).toBeLessThanOrEqual(overflow.clientWidth)
}

test.describe('phone', () => {
  test.use({ viewport: phone, isMobile: true, hasTouch: true })

  test('the shell folds the rail and sidebar into a drawer behind a top bar', async ({ page }) => {
    await mockShell(page)
    await page.goto('/')

    await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
    await expect(page.getByRole('navigation', { name: 'Workspaces' })).toBeHidden()
    await expect(page.getByRole('navigation', { name: 'Primary' })).toBeHidden()
    await expect(page.getByRole('link', { name: '1 items need attention' })).toBeVisible()
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-board.png', animations: 'disabled' })

    await page.getByRole('button', { name: 'Open navigation' }).click()
    const drawer = page.getByRole('dialog', { name: 'Navigation' })
    await expect(drawer.getByRole('navigation', { name: 'Workspaces' })).toBeVisible()
    await expect(drawer.getByRole('button', { name: 'Switch to Other' })).toBeVisible()
    await page.screenshot({ path: 'test-results/shots/responsive-phone-drawer.png', animations: 'disabled' })

    await drawer.getByRole('link', { name: 'Tasks' }).click()
    await expect(drawer).toBeHidden()
    await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
  })

  test('the tasks list stacks each row and keeps the page inside the viewport', async ({ page }) => {
    await mockShell(page)
    await page.goto('/tasks')

    const table = page.getByRole('region', { name: 'Tasks table' })
    await expect(table.getByRole('link', { name: 'Stack the tasks table below the md breakpoint' })).toBeVisible()
    // The column header has nothing to head once the cells stack.
    await expect(table.getByText('Updated', { exact: true })).toBeHidden()
    await expectNoHorizontalOverflow(page)
    const scroller = await table.locator('.overflow-x-auto').evaluate((node) => ({
      scrollWidth: node.scrollWidth,
      clientWidth: node.clientWidth,
    }))
    expect(scroller.scrollWidth).toBeLessThanOrEqual(scroller.clientWidth)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-tasks.png', animations: 'disabled' })
  })

  test('the document tree opens as a drawer above the canvas', async ({ page }) => {
    await mockShell(page)
    await page.goto('/system-design')

    await expect(page.getByRole('heading', { name: 'System Design' })).toBeVisible()
    await expect(page.getByRole('navigation', { name: 'Document tree' })).toBeHidden()
    await expect(page.getByRole('separator', { name: 'Resize the document list' })).toBeHidden()
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-system-design.png', animations: 'disabled' })

    await page.getByRole('button', { name: 'Documents' }).click()
    const drawer = page.getByRole('dialog', { name: 'Document tree' })
    const item = drawer.getByRole('button', { name: 'Dispatch ownership' })
    await expect(item).toBeVisible()
    await page.screenshot({ path: 'test-results/shots/responsive-phone-document-drawer.png', animations: 'disabled' })
    await item.click()
    await expect(drawer).toBeHidden()
    await expect(page.getByRole('heading', { name: 'Dispatch ownership' }).first()).toBeVisible()
  })
  test('the task panel header keeps its title clear of the explorer and navigation', async ({ page }) => {
    await mockShell(page)
    const lineageReads = await mockRecordDetail(page)

    // The Tasks list panel carries the most controls, including copy-link.
    await page.goto('/tasks?task=task-layout')
    const panel = page.getByRole('dialog', { name: 'Task detail' })
    const header = panel.locator('header').first()
    const title = header.getByText(layoutTitle, { exact: true })
    await expect(title).toBeVisible()
    await waitForSheetSettled(panel)
    const explorer = header.getByRole('button', { name: 'Knowledge explorer' })
    await expectIconOnlyExplorer(explorer)
    await expect(header.getByRole('button', { name: 'Copy link to this task' })).toBeVisible()
    await expectHeaderControlsClear(header, title)
    await expectContextRows(page, true)
    await expectPlanHeaderUnbroken(page)
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-task-panel.png', animations: 'disabled' })

    // Opening the detail read no lineage; pressing the trigger does.
    expect(lineageReads).toEqual([])
    await explorer.click()
    await expect(page.getByRole('dialog', { name: 'Knowledge explorer' })).toBeVisible()
    expect(lineageReads).toEqual(['/v1/lineage/task/task-layout'])
  })

  test('the board task sheet moves its window note under the controls', async ({ page }) => {
    await mockShell(page)
    await mockRecordDetail(page)

    // The task is outside the shell's Board window, so the header carries the
    // window-edge note as well as every control.
    await page.goto('/tasks/task-layout')
    const sheet = page.getByRole('dialog', { name: 'Task detail' })
    const header = sheet.locator('header').first()
    const title = header.getByText(layoutTitle, { exact: true })
    await expect(title).toBeVisible()
    await waitForSheetSettled(sheet)
    const note = header.getByRole('note')
    await expect(note).toHaveText('This task is outside the loaded Board window.')
    await expectHeaderControlsClear(header, title)
    const titleBox = await title.boundingBox()
    const noteBox = await note.boundingBox()
    if (!titleBox || !noteBox) throw new Error('the board sheet header is not rendered')
    expect(noteBox.y).toBeGreaterThanOrEqual(titleBox.y + titleBox.height)
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-task-sheet.png', animations: 'disabled' })
  })

  test('the full task page stacks context and keeps a draft plan header unbroken', async ({ page }) => {
    await mockShell(page)
    await mockRecordDetail(page)

    await page.goto('/tasks/task-layout-draft/full')
    const header = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to board' }) })
    const title = header.getByText(layoutTitle, { exact: true })
    await expect(title).toBeVisible()
    await expectIconOnlyExplorer(header.getByRole('button', { name: 'Knowledge explorer' }))
    await expectHeaderControlsClear(header, title)
    await expect(page.getByText('Awaiting approval', { exact: true })).toBeVisible()
    await expectContextRows(page, true)
    await expectPlanHeaderUnbroken(page)
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-phone-task-full.png', animations: 'disabled' })
  })

  test('document headers keep the explorer trigger icon-only and named', async ({ page }) => {
    await mockShell(page)
    await mockRecordDetail(page)

    for (const [path, heading] of [
      ['/requirements?requirement=req-layout', 'Narrow layouts keep every record header usable on a phone'],
      ['/system-design?document=design-dispatch', 'Dispatch ownership'],
    ]) {
      await page.goto(path)
      await expect(page.getByRole('heading', { name: heading }).first()).toBeVisible()
      const explorer = page.getByRole('button', { name: 'Knowledge explorer' })
      await expectIconOnlyExplorer(explorer)
      await expectNoHorizontalOverflow(page)
    }
  })
})

test.describe('tablet', () => {
  test('portrait folds navigation; landscape keeps the desktop columns', async ({ browser }) => {
    const portrait = await browser.newPage({ viewport: tabletPortrait, isMobile: true, hasTouch: true })
    await mockShell(portrait)
    await portrait.goto('/system-design')
    await expect(portrait.getByRole('button', { name: 'Open navigation' })).toBeVisible()
    await expect(portrait.getByRole('button', { name: 'Documents' })).toBeVisible()
    await expectNoHorizontalOverflow(portrait)
    await portrait.screenshot({ path: 'test-results/shots/responsive-tablet-portrait.png', animations: 'disabled' })
    await portrait.close()

    const landscape = await browser.newPage({ viewport: tabletLandscape, isMobile: true, hasTouch: true })
    await mockShell(landscape)
    await landscape.goto('/system-design')
    await expect(landscape.getByRole('button', { name: 'Open navigation' })).toBeHidden()
    await expect(landscape.getByRole('navigation', { name: 'Primary' })).toBeVisible()
    await expect(landscape.getByRole('navigation', { name: 'Document tree' })).toBeVisible()
    await expectNoHorizontalOverflow(landscape)
    await landscape.screenshot({ path: 'test-results/shots/responsive-tablet-landscape.png', animations: 'disabled' })
    await landscape.close()
  })

  // The board sheet takes half the portrait width, so its header is nearly as
  // tight as a phone's.
  test.describe('portrait sheet', () => {
    test.use({ viewport: tabletPortrait, isMobile: true, hasTouch: true })

    test('the board task sheet keeps its header controls clear', async ({ page }) => {
      await mockShell(page)
      await mockRecordDetail(page)

      await page.goto('/tasks/task-layout')
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      const header = sheet.locator('header').first()
      const title = header.getByText(layoutTitle, { exact: true })
      await expect(title).toBeVisible()
      await waitForSheetSettled(sheet)
      await expect(header.getByRole('note')).toHaveText('This task is outside the loaded Board window.')
      await expectHeaderControlsClear(header, title)
      await expectPlanHeaderUnbroken(page)
      await expectNoHorizontalOverflow(page)
      await page.screenshot({ path: 'test-results/shots/responsive-tablet-task-sheet.png', animations: 'disabled' })
    })
  })
})

// The phone and tablet sheet tests measure only after waitForSheetSettled.
// This proves the wait itself, independent of host load: the real slide is
// lengthened and paused half-way, so a wait that returns before the slide
// ends, or that never saw the slide, is caught while the dialog is still
// translated. The helper's collection is recorded so the test knows it
// started waiting on this slide before it checks that the wait is pending.
const pausedSlide = `[role="dialog"].animate-sheet-in {
  animation-duration: 2s !important;
  animation-delay: -1s !important;
  animation-timing-function: linear !important;
  animation-play-state: paused !important;
}`

type SettleProbe = { collections: Animation[][]; collected: Promise<Animation[]> }

for (const [name, viewport] of [
  ['phone', phone],
  ['tablet', tabletPortrait],
] as const) {
  test.describe(`${name} sheet settling`, () => {
    test.use({ viewport, isMobile: true, hasTouch: true })

    test('measurement waits until the paused slide finishes', async ({ page }) => {
      await page.addInitScript((css) => {
        const slide = new CSSStyleSheet()
        slide.replaceSync(css)
        document.adoptedStyleSheets = [...document.adoptedStyleSheets, slide]
        let resolveCollected: (animations: Animation[]) => void = () => {}
        const probe: SettleProbe = {
          collections: [],
          collected: new Promise((resolve) => {
            resolveCollected = resolve
          }),
        }
        Object.assign(window, { settleProbe: probe })
        const getAnimations = Element.prototype.getAnimations
        Element.prototype.getAnimations = function (this: Element, options?: GetAnimationsOptions) {
          const animations = getAnimations.call(this, options)
          if (this.getAttribute('role') === 'dialog') {
            probe.collections.push(animations)
            resolveCollected(animations)
          }
          return animations
        }
      }, pausedSlide)
      await mockShell(page)
      await mockRecordDetail(page)

      await page.goto('/tasks/task-layout')
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      const header = sheet.locator('header').first()
      const title = header.getByText(layoutTitle, { exact: true })
      await expect(title).toBeVisible()
      const offset = () => sheet.evaluate((node) => new DOMMatrixReadOnly(getComputedStyle(node).transform).m41)
      const paused = await offset()
      expect(paused).toBeGreaterThan(0)

      let settled = false
      const settling = waitForSheetSettled(sheet).then(() => {
        settled = true
      })
      const collected = await page.evaluate(async () => {
        const { collected } = (window as unknown as { settleProbe: SettleProbe }).settleProbe
        return (await collected).map((animation) => ({
          name: animation instanceof CSSAnimation ? animation.animationName : '',
          playState: animation.playState,
        }))
      })
      expect(collected).toContainEqual({ name: 'sheet-in', playState: 'paused' })
      // Each round trip after the collection would deliver an early return.
      expect(await offset()).toBe(paused)
      expect(await offset()).toBe(paused)
      expect(settled).toBe(false)

      await page.evaluate(() => {
        for (const animation of (window as unknown as { settleProbe: SettleProbe }).settleProbe.collections[0])
          animation.play()
      })
      await settling
      const rest = await sheet.evaluate((node) => {
        const transform = new DOMMatrixReadOnly(getComputedStyle(node).transform)
        const { collections } = (window as unknown as { settleProbe: SettleProbe }).settleProbe
        return {
          playStates: collections[0].map((animation) => animation.playState),
          translation: [transform.m41, transform.m42],
        }
      })
      expect(rest.playStates.length).toBeGreaterThan(0)
      expect(rest.playStates.every((state) => state === 'finished')).toBe(true)
      expect(rest.translation).toEqual([0, 0])
      await expectHeaderControlsClear(header, title)
      await expectNoHorizontalOverflow(page)

      // A settled sheet has nothing left to wait for and returns at once.
      await waitForSheetSettled(sheet)
      const remaining = await page.evaluate(
        () => (window as unknown as { settleProbe: SettleProbe }).settleProbe.collections.at(-1)?.length,
      )
      expect(remaining).toBe(0)
    })
  })
}

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 900 } })

  test('task detail keeps labeled explorer triggers and single-line context rows', async ({ page }) => {
    await mockShell(page)
    const lineageReads = await mockRecordDetail(page)

    await page.goto('/tasks?task=task-layout')
    const panel = page.getByRole('dialog', { name: 'Task detail' })
    const header = panel.locator('header').first()
    const title = header.getByText(layoutTitle, { exact: true })
    await expect(title).toBeVisible()
    await waitForSheetSettled(panel)
    await expect(
      header.getByRole('button', { name: 'Knowledge explorer' }).getByText('Knowledge explorer'),
    ).toBeVisible()
    await expectHeaderControlsClear(header, title)
    await expectContextRows(page, false)
    await expectPlanHeaderUnbroken(page)
    await expectNoHorizontalOverflow(page)
    await page.screenshot({ path: 'test-results/shots/responsive-desktop-task-panel.png', animations: 'disabled' })

    await page.goto('/tasks/task-layout/full')
    const fullHeader = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to board' }) })
    await expect(
      fullHeader.getByRole('button', { name: 'Knowledge explorer' }).getByText('Knowledge explorer'),
    ).toBeVisible()
    await expectHeaderControlsClear(fullHeader, fullHeader.getByText(layoutTitle, { exact: true }))
    await expectContextRows(page, false)
    await expectPlanHeaderUnbroken(page)
    await page.screenshot({ path: 'test-results/shots/responsive-desktop-task-full.png', animations: 'disabled' })

    for (const path of ['/requirements?requirement=req-layout', '/system-design?document=design-dispatch']) {
      await page.goto(path)
      await expect(
        page.getByRole('button', { name: 'Knowledge explorer' }).getByText('Knowledge explorer'),
      ).toBeVisible()
    }
    expect(lineageReads).toEqual([])
  })
})
