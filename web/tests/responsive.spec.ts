// Narrow viewports: the shell folds its two navigation columns into a drawer
// behind a top bar, the document pages fold their tree into a drawer above the
// canvas, and no surface scrolls sideways at phone width.
import { expect, type Page, test } from '@playwright/test'

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
})
