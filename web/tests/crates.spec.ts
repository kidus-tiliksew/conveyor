// The crates list, the crate page, and task creation from a crate
// (req-crates REQ-3, REQ-5; component-crates invariants 17 to 19). A fake
// server serves the crate rows and crate reads in the shapes
// internal/httpapi/crates.go writes; the dashboard derives none of the
// counts, the shipped flag, or the next action labels.
import { expect, type Page, type Route } from '@playwright/test'
import type { Crate, CrateActivityEvent, CrateDetail } from '../src/lib/types'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

const createdAt = '2026-10-01T09:00:00Z'

// A calendar date `days` from the viewer's today, as the server writes a
// target date (YYYY-MM-DD).
function dateFromToday(days: number) {
  const date = new Date()
  date.setDate(date.getDate() + days)
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

function crateRow(overrides: Partial<Crate> & Pick<Crate, 'id' | 'name'>): Crate {
  return {
    target_date: null,
    state: 'open',
    shipped: false,
    created_by: 'usr_operator',
    created_at: createdAt,
    closed_at: null,
    open_task_count: 0,
    merged_task_count: 0,
    total_task_count: 0,
    ...overrides,
  }
}

// The served order of the open list: soonest target date first, then crates
// with no date, then name (AC-5.1). Names are chosen so that no client-side
// sort by name or count could reproduce it.
const openCrates: Crate[] = [
  crateRow({
    id: 'crate-zeta',
    name: 'Zeta rollout',
    target_date: dateFromToday(3),
    open_task_count: 1,
    merged_task_count: 0,
    total_task_count: 1,
  }),
  crateRow({
    id: 'crate-billing',
    name: 'Billing v2',
    target_date: dateFromToday(12),
    open_task_count: 2,
    merged_task_count: 3,
    total_task_count: 5,
  }),
  crateRow({ id: 'crate-alpha', name: 'Alpha cleanup', open_task_count: 4, merged_task_count: 1, total_task_count: 6 }),
]

const closedCrates: Crate[] = [
  crateRow({
    id: 'crate-closed',
    name: 'Spring migration',
    state: 'closed',
    target_date: '2026-03-01',
    closed_at: '2026-03-04T12:00:00Z',
    open_task_count: 0,
    merged_task_count: 2,
    total_task_count: 3,
  }),
]

const nextActionVerbatim = 'Waiting on review seat 2 of 3 (served text)'

const billing: CrateDetail = {
  ...openCrates[1],
  requirements: [
    { id: 'req-billing', title: 'Customers pay by invoice', task_count: 2 },
    { id: 'req-refunds', title: 'Operators issue refunds', task_count: 0 },
  ],
  undeclared_requirements: [{ id: 'req-audit', title: 'Every money movement is audited', task_count: 1 }],
  tasks: [
    {
      id: '261101-aaaaaa',
      title: 'Send invoices by email',
      state: 'running',
      branch: 'conveyor/task-261101-aaaaaa',
      pull_request: { number: 412, url: 'https://github.com/acme/app/pull/412', repository: 'acme/app' },
      next_action: {
        label: nextActionVerbatim,
        actor: 'agent',
        kind: 'working',
        judgment: false,
        summary: 'An agent is reviewing.',
        deadline: null,
      },
    },
    {
      id: '261101-bbbbbb',
      title: 'Record invoice payments',
      state: 'merged',
      branch: 'conveyor/task-261101-bbbbbb',
      pull_request: { number: 398, url: 'https://github.com/acme/app/pull/398', repository: 'acme/app' },
      next_action: {
        label: 'Merged',
        actor: 'none',
        kind: 'terminal',
        judgment: false,
        summary: 'The task merged.',
        deadline: null,
      },
    },
  ],
}

const shipped: CrateDetail = {
  ...crateRow({
    id: 'crate-shipped',
    name: 'Onboarding refresh',
    target_date: dateFromToday(-2),
    shipped: true,
    open_task_count: 0,
    merged_task_count: 2,
    total_task_count: 3,
  }),
  requirements: [],
  undeclared_requirements: [],
  tasks: [],
}

const closed: CrateDetail = { ...closedCrates[0], requirements: [], undeclared_requirements: [], tasks: [] }

const undated: CrateDetail = {
  ...openCrates[2],
  requirements: [],
  undeclared_requirements: [],
  tasks: [],
}

// Newest first, as the server pages them (AC-5.5).
const billingActivity: CrateActivityEvent[] = [
  {
    id: 30,
    kind: 'pull_request.opened',
    at: '2026-11-18T10:00:00Z',
    actor_id: 'usr_agent',
    actor_role: 'executor',
    task_id: '261101-aaaaaa',
    task_title: 'Send invoices by email',
    pull_request: { number: 412, url: 'https://github.com/acme/app/pull/412', repository: 'acme/app' },
  },
  {
    id: 20,
    kind: 'merge.confirmed',
    at: '2026-11-17T10:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261101-bbbbbb',
    task_title: 'Record invoice payments',
  },
  {
    id: 10,
    kind: 'task.crate_set',
    at: '2026-11-10T10:00:00Z',
    actor_id: 'usr_operator',
    actor_role: 'operator',
    task_id: '261101-aaaaaa',
    task_title: 'Send invoices by email',
  },
]

type Role = 'viewer' | 'maintainer' | 'operator'

const memberNextAction = {
  label: 'Agent working',
  actor: 'agent',
  kind: 'working',
  judgment: false,
  summary: 'The agent is working.',
  deadline: null,
}

async function serveCrates(page: Page, options: { role?: Role } = {}) {
  const role = options.role ?? 'operator'
  const details: Record<string, CrateDetail> = {
    [billing.id]: structuredClone(billing),
    [shipped.id]: structuredClone(shipped),
    [closed.id]: structuredClone(closed),
    [undated.id]: structuredClone(undated),
  }
  const listRequests: string[] = []
  const created: Record<string, unknown>[] = []
  const patched: Record<string, unknown>[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()
    if (path === '/v1/workspaces')
      return route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: createdAt }] })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: `usr_${role}`, role }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: {
          workspace: 'demo',
          database: 'postgres',
          max_bounces: 2,
          default_plan_state: 'needed',
          repos: [{ name: 'conveyor', base: 'main' }],
        },
      })
    if (path === '/v1/crates' && method === 'GET') {
      const state = url.searchParams.get('state') ?? 'open'
      listRequests.push(state)
      return route.fulfill({ json: state === 'closed' ? closedCrates : openCrates })
    }
    const activity = path.match(/^\/v1\/crates\/([^/]+)\/activity$/)
    if (activity) {
      return route.fulfill({ json: { events: activity[1] === billing.id ? billingActivity : [] } })
    }
    const one = path.match(/^\/v1\/crates\/([^/]+)$/)
    if (one) {
      const crate = details[decodeURIComponent(one[1])]
      if (!crate) return route.fulfill({ status: 404, json: { error: 'crate not found' } })
      if (method === 'PATCH') {
        const body = request.postDataJSON() as Partial<CrateDetail>
        patched.push(body as Record<string, unknown>)
        Object.assign(crate, body)
        return route.fulfill({ json: crate })
      }
      return route.fulfill({ json: crate })
    }
    if (path === '/v1/requirements')
      return route.fulfill({
        json: [
          ['req-billing', 'Customers pay by invoice'],
          ['req-refunds', 'Operators issue refunds'],
          ['req-other', 'Operators see a weekly digest'],
        ].map(([id, title]) => ({
          requirement: { id, title, current_version: 1 },
          current_version: { requirement_id: id, version: 1, confirmed: true },
          pending_versions: [],
          serving_blueprints: [],
          planning_sessions: [],
          artifacts: [],
        })),
      })
    if (path === '/v1/tasks' && method === 'POST') {
      // The intake form posts multipart data with the task as a JSON part.
      const task = (request.postData() ?? '').match(/name="task"\r\n\r\n(.*)\r\n/)
      created.push(JSON.parse(task?.[1] ?? '{}') as Record<string, unknown>)
      return route.fulfill({
        status: 201,
        json: {
          id: '261119-created',
          workspace: 'demo',
          title: 'Generated title',
          body: '',
          repo: 'conveyor',
          state: 'queued',
          created_at: createdAt,
        },
      })
    }
    // Task detail for any member task; each one belongs to Billing v2.
    const taskRead = path.match(/^\/v1\/tasks\/([^/]+)\/(activity|next-action)$/)
    if (taskRead && method === 'GET') {
      const id = decodeURIComponent(taskRead[1])
      if (taskRead[2] === 'next-action')
        return route.fulfill({ json: { ...memberNextAction, choices: [], details: {} } })
      return route.fulfill({
        json: {
          task: {
            id,
            workspace: 'demo',
            title: billing.tasks.find((task) => task.id === id)?.title ?? 'Send invoices by email',
            body: 'Send each invoice by email.',
            repo: 'conveyor',
            state: 'running',
            branch: `conveyor/task-${id}`,
            created_at: createdAt,
            crate: { id: billing.id, name: billing.name },
          },
          jobs: [],
          events: [],
          interventions: [],
          work_orders: [],
          reviews: [],
          latest_stage: 'implement',
          next_action: memberNextAction,
          checkout_available: false,
          checkout_guidance: '',
          needs_attention: false,
        },
      })
    }
    return route.fulfill({ json: [] })
  })
  return { listRequests, created, patched }
}

// No horizontal page scroll: neither the document nor any scroll container
// under `root` is wider than the viewport. The create form checks only its
// sheet, because the Tasks kanban behind it scrolls sideways by design.
async function expectNoHorizontalScroll(page: Page, root = 'body') {
  const overflow = await page.evaluate((root) => {
    const width = document.documentElement.clientWidth
    const wide: string[] = []
    if (document.documentElement.scrollWidth > width) wide.push('document')
    for (const element of Array.from(document.querySelectorAll<HTMLElement>(`${root} *`))) {
      const style = getComputedStyle(element)
      if (!/(auto|scroll)/.test(style.overflowX)) continue
      // A deliberately scrolling strip, such as a segmented control, is fine.
      if (element.closest('fieldset')) continue
      if (element.scrollWidth > element.clientWidth + 1 && element.clientWidth >= width - 32)
        wide.push(`${element.tagName.toLowerCase()}.${element.className} ${element.scrollWidth}>${element.clientWidth}`)
    }
    return wide
  }, root)
  expect(overflow).toEqual([])
}

test('primary navigation opens the crates list directly after Tasks', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/inbox')
  const nav = page.getByRole('navigation', { name: 'Primary' })
  // evaluateAll reads once without waiting; every entry renders in one commit.
  await expect(nav.getByRole('link', { name: 'Crates', exact: true })).toBeVisible()
  const labels = await nav
    .locator('a')
    .evaluateAll((links) => links.map((link) => link.textContent?.replace(/\d+$/, '').trim()))
  expect(labels.slice(0, 2)).toEqual(['Tasks', 'Crates'])
  await nav.getByRole('link', { name: 'Crates', exact: true }).click()
  await expect(page).toHaveURL(/\/w\/demo\/crates$/)
  await expect(page.getByRole('heading', { name: 'Crates', exact: true })).toBeVisible()
  await expect(nav.getByRole('link', { name: 'Crates', exact: true })).toHaveAttribute('aria-current', 'page')
})

// AC-5.1: the order the server returns, each open crate's name, target date,
// and open, merged, and total counts; closed crates only on request.
test('crates list keeps the served order and shows closed crates only on request', async ({ page }) => {
  const { listRequests } = await serveCrates(page)
  await page.goto('/w/demo/crates')

  const open = page.getByRole('list', { name: 'Open crates' })
  await expect(open.locator('[data-crate-name]')).toHaveText(['Zeta rollout', 'Billing v2', 'Alpha cleanup'])
  const row = open.locator('[data-crate-row="crate-billing"]')
  await expect(row.locator('[data-count="open"]')).toHaveText('2 open')
  await expect(row.locator('[data-count="merged"]')).toHaveText('3 merged')
  await expect(row.locator('[data-count="total"]')).toHaveText('5 total')
  await expect(row.locator('[data-target-date]:visible')).toHaveCount(1)
  await expect(row.getByText('12 days left').first()).toBeAttached()
  await expect(open.locator('[data-crate-row="crate-alpha"]').getByText('No target date').first()).toBeAttached()
  await expect(page.getByText('Spring migration')).toHaveCount(0)
  expect(listRequests).toEqual(['open'])

  await page.getByRole('group', { name: 'Crate state' }).getByRole('button', { name: 'Closed' }).click()
  await expect(page).toHaveURL(/show=closed/)
  const closedList = page.getByRole('list', { name: 'Closed crates' })
  await expect(closedList.locator('[data-crate-name]')).toHaveText(['Spring migration'])
  expect(listRequests).toContain('closed')

  await closedList.getByRole('link').first().click()
  await expect(page).toHaveURL(/\/w\/demo\/crates\/crate-closed$/)
})

// AC-5.2 to AC-5.5 on one open crate with tasks.
test('crate page shows the header, requirements with the uncovered mark, verbatim next actions, and activity', async ({
  page,
}) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')

  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByRole('heading', { level: 1 })).toHaveText('Billing v2')
  await expect(hero.locator('[data-stat="merged"]')).toHaveText('3')
  await expect(hero.locator('[data-stat="total"]')).toHaveText('5')
  await expect(hero.locator('[data-stat="open"]')).toHaveText('2')
  await expect(hero.locator('p').first()).toHaveText(/^3 of 5 merged\s*·\s*,\s*2 open$/)
  await expect(hero.getByText('Target date', { exact: true })).toBeVisible()
  await expect(hero.getByText('12 days left')).toBeVisible()
  await expect(hero.locator('[data-merged-bar]')).toHaveCount(1)
  await expect(hero.locator('[data-shipped-statement]')).toHaveCount(0)

  // Declared requirements with their task counts and the uncovered mark, and
  // the undeclared requirement apart (AC-5.4).
  const declared = page.getByRole('list', { name: 'Declared requirements' })
  await expect(declared.locator('[data-requirement="req-billing"]')).toContainText('2 tasks')
  await expect(declared.locator('[data-requirement="req-billing"] [data-uncovered-mark]')).toHaveCount(0)
  await expect(declared.locator('[data-requirement="req-refunds"] [data-uncovered-mark]')).toHaveText('Uncovered')
  await expect(page.getByText('1 declared requirement has no task yet.')).toBeVisible()
  const undeclared = page.getByRole('list', { name: 'Undeclared requirements' })
  await expect(undeclared.locator('[data-requirement]')).toHaveCount(1)
  await expect(undeclared.locator('[data-requirement="req-audit"]')).toContainText('1 task')
  await expect(undeclared.locator('[data-uncovered-mark]')).toHaveCount(0)

  // Member rows print the served label verbatim, the state, the branch, and
  // the pull request, and open the task (AC-5.3).
  const member = page.locator('[data-crate-task="261101-aaaaaa"]')
  await expect(member.locator('[data-next-action-label]')).toHaveText(nextActionVerbatim)
  await expect(member.locator('[data-task-state]')).toHaveAttribute('data-task-state', 'running')
  await expect(member.locator('[data-task-branch]')).toHaveText('conveyor/task-261101-aaaaaa')
  await expect(member.getByRole('link', { name: '#412' })).toHaveAttribute(
    'href',
    'https://github.com/acme/app/pull/412',
  )
  await expect(page.locator('[data-crate-task="261101-bbbbbb"] [data-next-action-label]')).toHaveText('Merged')

  // Activity newest first, with the pull request entry linked (AC-5.5).
  const activity = page.getByRole('region', { name: 'Activity' })
  await expect(activity.locator('[data-activity-kind]')).toHaveCount(3)
  expect(
    await activity
      .locator('[data-activity-kind]')
      .evaluateAll((items) => items.map((item) => item.dataset.activityKind)),
  ).toEqual(['pull_request.opened', 'merge.confirmed', 'task.crate_set'])
  await expect(activity.getByRole('link', { name: /Pull request #412 opened/ })).toHaveAttribute(
    'href',
    'https://github.com/acme/app/pull/412',
  )

  await member.locator('[data-task-row]').click()
  await expect(page).toHaveURL(/\/w\/demo\/tasks\/261101-aaaaaa$/)
})

test('a shipped crate shows the shipped statement and no bar', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-shipped')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.locator('[data-shipped-statement]')).toHaveText(
    'Shipped. Every task in this crate is merged or closed.',
  )
  await expect(hero.locator('[data-merged-bar]')).toHaveCount(0)
  await expect(hero.locator('[data-stat="merged"]')).toHaveText('2')
  await expect(hero.locator('[data-stat="total"]')).toHaveText('3')
  // A shipped crate is never late.
  await expect(hero.getByText('Target date', { exact: true })).toBeVisible()
  await expect(hero.getByText(/days? late/)).toHaveCount(0)
})

test('a closed crate states its closing date, hides the target date, and offers reopening', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-closed')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByText('Closed on Mar 4, 2026')).toBeVisible()
  await expect(hero.getByText('Target date', { exact: true })).toHaveCount(0)
  await expect(hero.locator('[data-merged-bar]')).toHaveCount(0)
  await expect(hero.locator('[data-stat="open"]')).toHaveText('0')
  await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Reopen crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Add an existing task' })).toHaveCount(0)
})

// The ghost button opens the crate form on the date field; after the save,
// focus lands on the new date block, and closing a dialog returns focus to
// its opener.
test('"Set a target date" opens the form on the date field and focus moves to the saved date', async ({ page }) => {
  const { patched } = await serveCrates(page)
  await page.goto('/w/demo/crates/crate-alpha')
  const hero = page.getByRole('region', { name: 'Crate summary' })
  await expect(hero.getByText('Target date', { exact: true })).toHaveCount(0)
  const setDate = hero.getByRole('button', { name: 'Set a target date' })

  await setDate.click()
  const dialog = page.getByRole('dialog', { name: 'Edit crate' })
  await expect(dialog.getByLabel('Target date')).toBeFocused()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(setDate).toBeFocused()

  await setDate.click()
  await expect(dialog.getByLabel('Target date')).toBeFocused()
  const date = dateFromToday(0)
  await dialog.getByLabel('Target date').fill(date)
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toHaveCount(0)
  expect(patched.at(-1)).toMatchObject({ name: 'Alpha cleanup', target_date: date })

  await expect(hero.locator('[data-target-date]')).toBeVisible()
  await expect(hero.getByText('Due today')).toBeVisible()
  await expect(hero.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.querySelector('[data-target-date]') != null))
    .toBe(true)
})

test('the edit form focuses the name field first', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await page.getByRole('menuitem', { name: 'Edit crate' }).click()
  await expect(page.getByRole('dialog', { name: 'Edit crate' }).getByLabel('Crate name')).toBeFocused()
})

test.describe('with reduced motion', () => {
  test.use({ reducedMotion: 'reduce' })

  test('the merged bar shows its share at once', async ({ page }) => {
    await serveCrates(page)
    await page.goto('/w/demo/crates/crate-billing')
    const fill = page.locator('[data-merged-bar] > div')
    await expect(fill).toHaveAttribute('style', /width: 60%/)
  })
})

test('the merged bar grows to the served share', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.locator('[data-merged-bar] > div')).toHaveAttribute('style', /width: 60%/)
})

// AC-3.1 and AC-3.2: the form names the crate, lists its declared
// requirements first, preselects none, and sends crate_id with only the
// picked requirement_ids.
test('a task created from a crate sends crate_id and only the selected requirements', async ({ page }) => {
  const { created } = await serveCrates(page)
  await page.goto('/w/demo/crates/crate-billing')
  await page.getByRole('link', { name: 'New task' }).click()
  await expect(page).toHaveURL(/create=true/)
  await expect(page).toHaveURL(/crate=crate-billing/)

  const sheet = page.getByRole('dialog', { name: 'New task' })
  const notice = sheet.getByRole('region', { name: 'Crate' })
  await expect(notice.locator('[data-intake-crate-name]')).toHaveText('Billing v2')
  const declared = notice.getByRole('list', { name: 'Requirements the crate declares' })
  await expect(declared.getByRole('button')).toHaveCount(2)
  for (const chip of await declared.getByRole('button').all())
    await expect(chip).toHaveAttribute('aria-pressed', 'false')

  await sheet.getByRole('combobox', { name: 'Search context' }).click()
  const listbox = sheet.getByRole('listbox', { name: 'Context' })
  const groups = listbox.getByRole('group')
  await expect(groups.first()).toHaveAttribute('aria-label', 'Declared by Billing v2')
  await expect(listbox.getByRole('group', { name: 'Declared by Billing v2' }).getByRole('option')).toHaveCount(2)
  await expect(listbox.getByRole('option', { selected: true })).toHaveCount(0)
  // The other requirements group leaves out the declared ones.
  const other = listbox.getByRole('group', { name: 'Other requirements' })
  await expect(other.getByRole('option')).toHaveCount(1)
  await expect(other.getByRole('option', { name: /Operators see a weekly digest/ })).toBeVisible()
  await page.keyboard.press('Escape')

  await declared.getByRole('button', { name: /Operators issue refunds/ }).click()
  await expect(declared.getByRole('button', { name: /Operators issue refunds/ })).toHaveAttribute(
    'aria-pressed',
    'true',
  )
  await sheet.getByRole('textbox', { name: 'Task description' }).fill('Let operators refund an invoice')
  await sheet.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(() => created.length).toBe(1)
  expect(created[0].crate_id).toBe('crate-billing')
  expect(created[0].requirement_ids).toEqual(['req-refunds'])
})

// Every act renders only for a user with create_tasks; the server checks it
// again.
test('a viewer without create_tasks sees no crate acts', async ({ page }) => {
  await serveCrates(page, { role: 'viewer' })
  await page.goto('/w/demo/crates')
  await expect(page.locator('[data-crate-row]')).toHaveCount(3)
  await expect(page.getByRole('button', { name: 'New crate' })).toHaveCount(0)

  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  await expect(page.locator('[data-crate-task]')).toHaveCount(2)
  await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Crate actions' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Remove .* from this crate/ })).toHaveCount(0)

  await page.goto('/w/demo/crates/crate-alpha')
  await expect(page.getByRole('region', { name: 'Crate summary' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Set a target date' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add an existing task' })).toHaveCount(0)
})

test('a maintainer with create_tasks sees every crate act', async ({ page }) => {
  await serveCrates(page, { role: 'maintainer' })
  await page.goto('/w/demo/crates')
  await expect(page.getByRole('button', { name: 'New crate' })).toBeVisible()
  await page.goto('/w/demo/crates/crate-billing')
  await expect(page.getByRole('link', { name: 'New task' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add task' })).toBeVisible()
  await expect(page.getByRole('button', { name: /Remove .* from this crate/ })).toHaveCount(2)
  await page.getByRole('button', { name: 'Crate actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Edit crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Close crate' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Add an existing task' })).toBeVisible()
})

// AC-5.6: task detail names the crate and links to its page.
test('task detail shows the crate fact linked to the crate page', async ({ page }) => {
  await serveCrates(page)
  await page.goto('/w/demo/tasks/261119-member')
  const fact = page.locator('[data-task-crate="crate-billing"]')
  await expect(fact).toHaveText('Billing v2')
  await expect(fact).toHaveAttribute('href', '/w/demo/crates/crate-billing')
})

test.describe('at 375 px', () => {
  test.use({ viewport: { width: 375, height: 812 } })

  test('the crates list, the crate page, and the create form have no horizontal page scroll', async ({ page }) => {
    await serveCrates(page)
    await page.goto('/w/demo/crates')
    await expect(page.locator('[data-crate-row]')).toHaveCount(3)
    await expectNoHorizontalScroll(page)

    await page.goto('/w/demo/crates/crate-billing')
    await expect(page.locator('[data-crate-task]')).toHaveCount(2)
    await expect(page.getByRole('region', { name: 'Activity' }).locator('[data-activity-kind]')).toHaveCount(3)
    await expectNoHorizontalScroll(page)

    await page.goto('/w/demo/?create=true&crate=crate-billing')
    await expect(page.getByRole('dialog', { name: 'New task' }).locator('[data-intake-crate-name]')).toHaveText(
      'Billing v2',
    )
    await expectNoHorizontalScroll(page, '[role="dialog"]')
  })
})
