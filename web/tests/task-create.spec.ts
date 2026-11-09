import { expect, type Page, type Route } from '@playwright/test'
import { test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'

const createdAt = '2026-07-18T00:00:00Z'

// The description editor; the optional Plan field is the sheet's other
// textarea (component-web-task-surfaces invariant 63).
function descriptionEditor(page: Page) {
  return page.getByRole('dialog', { name: 'New task' }).getByRole('textbox', { name: 'Task description' })
}

async function mockTaskCreateAPIs(
  page: Page,
  options: {
    taskList?: 'success' | 'error' | 'delayed'
    createDependencyError?: boolean
    candidates?: Array<Record<string, unknown>>
    role?: 'viewer' | 'executor' | 'contributor' | 'maintainer' | 'operator'
    // The workspace's served default plan state; null serves a read that
    // states none yet.
    defaultPlanState?: 'needed' | 'not_required' | null
    createRefusal?: { status: number; message: string }
    capabilities?: string[]
  } = {},
) {
  let submitted = ''
  let taskListRequests = 0
  let taskListQuery = ''
  let workerRequests = 0
  let releaseTaskList = () => {}
  const taskListGate = new Promise<void>((resolve) => {
    releaseTaskList = resolve
  })
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({
        json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: '2026-07-18T00:00:00Z' }],
      })
      return
    }
    if (url.pathname === '/v1/me') {
      const role = options.role ?? 'operator'
      await route.fulfill({
        json: callerIdentity({ id: `usr_${role}`, role, capabilities: options.capabilities }, route.request()),
      })
      return
    }
    if (url.pathname === '/v1/workspace') {
      await route.fulfill({
        json: {
          workspace: 'demo',
          database: 'postgres',
          max_bounces: 2,
          repos: [
            { name: 'conveyor', base: 'main' },
            { name: 'api', base: 'develop' },
          ],
          ...(options.defaultPlanState === null ? {} : { default_plan_state: options.defaultPlanState ?? 'needed' }),
        },
      })
      return
    }
    // The server serves no worker routes (DEC-97(1)); a read here is a regression.
    if (url.pathname.startsWith('/v1/workers')) {
      workerRequests++
      await route.fulfill({ status: 404, json: { error: 'not_found' } })
      return
    }
    if (url.pathname === '/v1/requirements') {
      expect(route.request().headers().authorization).toBeUndefined()
      await route.fulfill({
        json: [
          {
            requirement: { id: 'req-context', title: 'Confirmed product outcome', current_version: 1 },
            current_version: { requirement_id: 'req-context', version: 1, confirmed: true },
            pending_versions: [],
            serving_blueprints: [],
            planning_sessions: [],
            artifacts: [],
            lineage: [],
          },
          {
            requirement: { id: 'req-second', title: 'Operators can resume a stalled task', current_version: 3 },
            current_version: { requirement_id: 'req-second', version: 3, confirmed: true },
            pending_versions: [],
            serving_blueprints: [],
            planning_sessions: [],
            artifacts: [],
            lineage: [],
          },
          {
            // Never confirmed: it has no current version and must stay unselectable.
            requirement: { id: 'req-draft', title: 'Unconfirmed product outcome', current_version: 0 },
            current_version: null,
            pending_versions: [],
            serving_blueprints: [],
            planning_sessions: [],
            artifacts: [],
            lineage: [],
          },
        ],
      })
      return
    }
    if (url.pathname === '/v1/system-designs') {
      expect(route.request().headers().authorization).toBeUndefined()
      await route.fulfill({
        json: [
          {
            document: { id: 'design-context', title: 'Confirmed technical guidance', current_version: 2 },
            current_version: { document_id: 'design-context', version: 2, confirmed: true },
            pending_versions: [],
            versions: [],
            lineage: [],
            drift: [],
          },
          {
            document: { id: 'design-draft', title: 'Unconfirmed technical guidance', current_version: 0 },
            current_version: null,
            pending_versions: [],
            versions: [],
            lineage: [],
            drift: [],
          },
        ],
      })
      return
    }
    if (url.pathname === '/v1/tasks' && route.request().method() === 'POST') {
      submitted = route.request().postData() ?? ''
      if (options.createRefusal) {
        await route.fulfill({ status: options.createRefusal.status, json: { error: options.createRefusal.message } })
        return
      }
      if (options.createDependencyError) {
        await route.fulfill({
          status: 400,
          contentType: 'application/json',
          json: { error: 'invalid_dependencies', message: 'The selected dependency creates a cycle.' },
        })
        return
      }
      await route.fulfill({
        status: 201,
        json: {
          id: 'generated',
          workspace: 'demo',
          title: 'Generated title',
          body: 'Generate this title from context',
          repo: 'conveyor',
          state: 'queued',
          created_at: '2026-07-18T00:00:00Z',
        },
      })
      return
    }
    if (url.pathname === '/v1/tasks' && route.request().method() === 'GET') {
      taskListRequests++
      taskListQuery = url.search
      if (options.taskList === 'delayed') await taskListGate
      if (options.taskList === 'error') {
        await route.fulfill({ status: 401, body: 'unauthorized' })
        return
      }
      await route.fulfill({
        json: options.candidates ?? [
          {
            id: '260729-dependency',
            workspace: 'demo',
            title: 'Finish persistence first',
            body: '',
            repo: 'conveyor',
            state: 'running',
            created_at: createdAt,
          },
          {
            id: '260729-cross-repo',
            workspace: 'demo',
            title: 'Publish API contract',
            body: '',
            repo: 'api',
            state: 'running',
            created_at: createdAt,
          },
          {
            id: '260729-terminal',
            workspace: 'demo',
            title: 'Already merged',
            body: '',
            repo: 'conveyor',
            state: 'merged',
            created_at: createdAt,
          },
        ],
      })
      return
    }
    if (url.pathname === '/v1/tasks/generated/activity') {
      await route.fulfill({
        json: {
          task: {
            id: 'generated',
            workspace: 'demo',
            title: 'Generated title',
            body: 'Generate this title from context',
            repo: 'conveyor',
            state: 'queued',
            created_at: '2026-07-18T00:00:00Z',
          },
          jobs: [],
          events: [],
          interventions: [],
          work_orders: [],
          checkout_available: false,
          checkout_guidance: '',
          needs_attention: false,
        },
      })
      return
    }
    await route.fulfill({ json: [] })
  })
  return {
    submitted: () => submitted,
    taskListRequests: () => taskListRequests,
    taskListQuery: () => new URLSearchParams(taskListQuery),
    workerRequests: () => workerRequests,
    releaseTaskList,
  }
}

// DEC-60: intake follows create_tasks, which only maintainers and operators
// are granted. Every opener and the sheet itself follow the caller's role bundle;
// the server authorizes the request again.
for (const [role, allowed] of [
  ['viewer', false],
  ['executor', false],
  ['contributor', false],
  ['maintainer', true],
  ['operator', true],
] as const) {
  test(`task creation capability follows every workspace role ${role}`, async ({ page }) => {
    await mockTaskCreateAPIs(page, { role })
    const count = allowed ? 1 : 0

    await page.goto('/w/demo/tasks')
    await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'New task' })).toHaveCount(count)

    await page.goto('/w/demo/tasks?create=true')
    await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
    await expect(page.getByRole('dialog', { name: 'New task' })).toHaveCount(count)

    await page.goto('/')
    await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(count)
    if (allowed) {
      await page.getByRole('button', { name: 'New task' }).click()
      await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
    }
  })
}

test('new task removes title input and submits description for AI title generation', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')

  await expect(page.getByPlaceholder('What should change?')).toHaveCount(0)
  await expect(page.getByText('AI generates the task title from this context')).toBeVisible()
  const create = page.getByRole('button', { name: 'Create task' })
  await expect(create).toBeDisabled()
  await descriptionEditor(page).fill('Generate this title from context')
  await expect(page.getByLabel(/setup/i)).toHaveCount(0)
  await expect(create).toBeEnabled()
  await create.click()

  await expect.poll(submitted).toContain('Generate this title from context')
  expect(submitted()).not.toContain('setup')
  await expect(page).toHaveURL(/\/tasks\/generated$/)
  expect(submitted()).not.toContain('"title"')
  // DEC-122(2), DEC-122(3): no execution-mode selector, and no hold is sent.
  expect(submitted()).not.toContain('"mode"')
  expect(submitted()).not.toContain('"hold"')
})

test('intake attaches confirmed product and design context with authenticated reads', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')
  await descriptionEditor(page).fill('Implement the attached desired state')

  // One Context control now, not a checkbox list per document tier.
  const search = page.getByRole('combobox', { name: 'Search context' })
  await expect(page.getByRole('listbox', { name: 'Context' })).toHaveCount(0)
  await search.click()
  const list = page.getByRole('listbox', { name: 'Context' })
  await expect(list).toBeVisible()
  await expect(list.getByRole('group', { name: 'Requirements' })).toBeVisible()
  await expect(list.getByRole('group', { name: 'System Design' })).toBeVisible()
  await list.getByRole('option', { name: /Confirmed product outcome/ }).click()
  await list.getByRole('option', { name: /Confirmed technical guidance/ }).click()
  await expect(list.getByRole('option', { name: /Confirmed product outcome/ })).toHaveAttribute('aria-selected', 'true')

  await page.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('"requirement_ids":["req-context"]')
  await expect.poll(submitted).toContain('"system_design_ids":["design-context"]')
})

test('context dropdown searches, keeps selections visible, and removes them', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')
  await descriptionEditor(page).fill('Attach several documents')

  const search = page.getByRole('combobox', { name: 'Search context' })
  await search.click()
  const list = page.getByRole('listbox', { name: 'Context' })
  await expect(list.getByRole('option')).toHaveCount(3)

  // Search spans both groups and matches title or ID.
  await search.fill('stalled')
  await expect(list.getByRole('option')).toHaveCount(1)
  await expect(list.getByRole('option', { name: /Operators can resume a stalled task/ })).toBeVisible()
  await search.fill('design-context')
  await expect(list.getByRole('option', { name: /Confirmed technical guidance/ })).toBeVisible()
  await expect(list.getByRole('group', { name: 'Requirements' })).toHaveCount(0)

  await search.fill('nothing matches this')
  await expect(list.getByRole('option')).toHaveCount(0)
  await expect(page.getByText('No context matches your search.')).toBeVisible()

  // Multi-select across both groups, driven from the keyboard.
  await search.fill('')
  await expect(list.getByRole('option')).toHaveCount(3)
  await search.press('ArrowDown')
  await search.press('Enter')
  await search.press('ArrowDown')
  await search.press('ArrowDown')
  await search.press('Enter')
  await expect(page.getByRole('button', { name: 'Remove context Confirmed product outcome' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove context Confirmed technical guidance' })).toBeVisible()

  // Selections survive closing the dropdown.
  await search.press('Escape')
  await expect(list).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Remove context Confirmed product outcome' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove context Confirmed technical guidance' })).toBeVisible()

  await page.getByRole('button', { name: 'Remove context Confirmed product outcome' }).click()
  await expect(page.getByRole('button', { name: 'Remove context Confirmed product outcome' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('"system_design_ids":["design-context"]')
  expect(submitted()).not.toContain('"requirement_ids"')
})

test('context dropdown never offers unconfirmed documents', async ({ page }) => {
  await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')
  await page.getByRole('combobox', { name: 'Search context' }).click()
  const list = page.getByRole('listbox', { name: 'Context' })
  await expect(list.getByRole('option', { name: /Confirmed product outcome/ })).toBeVisible()
  await expect(list.getByRole('option', { name: /Unconfirmed product outcome/ })).toHaveCount(0)
  await expect(list.getByRole('option', { name: /Unconfirmed technical guidance/ })).toHaveCount(0)
  await expect(list.getByRole('option')).toHaveCount(3)
})

test('intake offers no hold control and no worker availability warning', async ({ page }) => {
  const { submitted, workerRequests } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')

  // A task carries no hold or execution reservation (DEC-122(3)), and a filed
  // task waits for an agent session with no availability warning
  // (req-task-lifecycle-and-queue AC-9.4; DEC-97(1)).
  await expect(page.getByRole('radiogroup', { name: 'Execution mode' })).toHaveCount(0)
  await expect(page.getByRole('switch', { name: /hold/i })).toHaveCount(0)
  await expect(page.getByText(/Hold for hands-on work/)).toHaveCount(0)
  await descriptionEditor(page).fill('Queue this one openly')
  await expect(page.getByText(/worker/i)).toHaveCount(0)
  await expect(page.getByText(/claim it manually/)).toHaveCount(0)
  await page.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('Queue this one openly')
  expect(submitted()).not.toContain('"hold"')
  expect(workerRequests()).toBe(0)
})

test('dependency candidates load lazily and selected cross-repository chips survive repository changes', async ({
  page,
}) => {
  const { submitted, taskListRequests, taskListQuery } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')
  expect(taskListRequests()).toBe(0)
  await descriptionEditor(page).fill('Implement the dependent task')
  await page.getByText('Advanced options').click()
  await expect.poll(taskListRequests).toBe(1)
  // One bounded read of open tasks, never the workspace's whole history
  // (component-http-api, Task list read).
  expect(taskListQuery().getAll('state')).toEqual([
    'claiming',
    'queued',
    'running',
    'awaiting_human',
    'approved',
    'parked',
  ])
  expect(taskListQuery().get('limit')).toBe('200')
  await expect(page.getByText('2 matching open tasks.')).toBeVisible()
  // The empty query proves terminal tasks are filtered independently of search.
  await expect(page.getByText('Already merged')).toHaveCount(0)
  await page.getByText('Finish persistence first').click()
  await page.getByText('Publish API contract').click()
  // Exact, so the intake select never matches another Repository control behind
  // the sheet.
  await page.getByLabel('Repository', { exact: true }).selectOption('api')
  await expect(page.getByRole('button', { name: 'Remove dependency Finish persistence first' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove dependency Publish API contract' })).toBeVisible()
  await page.getByRole('button', { name: 'Remove dependency Finish persistence first' }).click()
  await page.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('"depends_on":["260729-cross-repo"]')
  await expect.poll(submitted).toContain('"repo":"api"')
})

test('dependency candidates distinguish loading, request failure, empty results, and bounded results', async ({
  page,
}) => {
  const delayed = await mockTaskCreateAPIs(page, { taskList: 'delayed' })
  await page.goto('/w/demo/new')
  await page.getByText('Advanced options').click()
  await expect(page.getByText('Loading dependency candidates…')).toBeVisible()
  delayed.releaseTaskList()
  await expect(page.getByText('2 matching open tasks.')).toBeVisible()

  await page.unroute('**/v1/**')
  await mockTaskCreateAPIs(page, { taskList: 'error' })
  await page.reload()
  await page.getByText('Advanced options').click()
  await expect(page.getByText('Could not load dependency candidates. Check your access and try again.')).toBeVisible()
  await expect(page.getByText('No matching open tasks.')).toHaveCount(0)

  await page.unroute('**/v1/**')
  await mockTaskCreateAPIs(page, { candidates: [] })
  await page.reload()
  await page.getByText('Advanced options').click()
  await expect(page.getByText('No matching open tasks.')).toBeVisible()

  await page.unroute('**/v1/**')
  await mockTaskCreateAPIs(page, {
    candidates: Array.from({ length: 25 }, (_, index) => ({
      id: `candidate-${index}`,
      workspace: 'demo',
      title: `Candidate ${index}`,
      body: '',
      repo: index % 2 ? 'api' : 'conveyor',
      state: 'running',
      created_at: createdAt,
    })),
  })
  await page.reload()
  await page.getByText('Advanced options').click()
  await expect(page.getByText('Showing 20 of 25 matching open tasks. Narrow your search to see more.')).toBeVisible()
  await expect(page.locator('#dependency-results input[type="checkbox"]')).toHaveCount(20)
})

test('structured dependency validation opens Advanced options and renders the error once', async ({ page }) => {
  await mockTaskCreateAPIs(page, { createDependencyError: true })
  await page.goto('/w/demo/new')
  await descriptionEditor(page).fill('Create a cyclic dependency')
  await page.getByText('Advanced options').click()
  await page.getByText('Finish persistence first').click()
  await page.getByText('Advanced options').click()
  await expect(page.getByLabel('Search dependency tasks')).not.toBeVisible()
  await page.getByRole('button', { name: 'Create task' }).click()
  await expect(page.getByLabel('Search dependency tasks')).toBeVisible()
  await expect(page.getByText('The selected dependency creates a cycle.')).toHaveCount(1)
})

test('intake markdown editor formats, toggles, previews, and restores selection', async ({ page }) => {
  await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')

  const editor = descriptionEditor(page)
  const toolbar = page.getByRole('toolbar', { name: 'Markdown formatting' })
  for (const name of [
    'Heading',
    'Bold',
    'Italic',
    'Quote',
    'Inline code',
    'Code block',
    'Link',
    'Bullet list',
    'Numbered list',
    'Task list',
  ]) {
    await expect(toolbar.getByRole('button', { name, exact: true })).toBeVisible()
  }
  await expect(editor).toHaveCSS('font-family', /monospace|Mono/i)

  await editor.fill('format me')
  await editor.evaluate((element: HTMLTextAreaElement) => element.setSelectionRange(0, 6))
  await toolbar.getByRole('button', { name: 'Bold', exact: true }).click()
  await expect(editor).toHaveValue('**format** me')
  await expect(editor).toBeFocused()
  await expect
    .poll(() => editor.evaluate((element: HTMLTextAreaElement) => [element.selectionStart, element.selectionEnd]))
    .toEqual([2, 8])
  await toolbar.getByRole('button', { name: 'Bold', exact: true }).click()
  await expect(editor).toHaveValue('format me')

  await editor.evaluate((element: HTMLTextAreaElement) => element.setSelectionRange(0, 6))
  await editor.press(process.platform === 'darwin' ? 'Meta+b' : 'Control+b')
  await expect(editor).toHaveValue('**format** me')
  await editor.press(process.platform === 'darwin' ? 'Meta+b' : 'Control+b')
  await expect(editor).toHaveValue('format me')

  await editor.press(process.platform === 'darwin' ? 'Meta+i' : 'Control+i')
  await expect(editor).toHaveValue('_format_ me')
  await editor.evaluate((element: HTMLTextAreaElement) => element.setSelectionRange(9, 11))
  await editor.press(process.platform === 'darwin' ? 'Meta+k' : 'Control+k')
  await expect(editor).toHaveValue('_format_ [me](url)')
  await expect
    .poll(() =>
      editor.evaluate((element: HTMLTextAreaElement) =>
        element.value.slice(element.selectionStart, element.selectionEnd),
      ),
    )
    .toBe('url')

  for (const [name, expected] of [
    ['Heading', '## first\n## second'],
    ['Quote', '> first\n> second'],
    ['Bullet list', '- first\n- second'],
    ['Numbered list', '1. first\n2. second'],
  ] as const) {
    await editor.fill('first\nsecond')
    await editor.selectText()
    await toolbar.getByRole('button', { name, exact: true }).click()
    await expect(editor).toHaveValue(expected)
    await toolbar.getByRole('button', { name, exact: true }).click()
    await expect(editor).toHaveValue('first\nsecond')
  }

  for (const [name, expected] of [
    ['Inline code', '`code`'],
    ['Code block', '```\ncode\n```'],
  ] as const) {
    await editor.fill('code')
    await editor.selectText()
    await toolbar.getByRole('button', { name, exact: true }).click()
    await expect(editor).toHaveValue(expected)
    await toolbar.getByRole('button', { name, exact: true }).click()
    await expect(editor).toHaveValue('code')
  }

  await editor.fill('first\nsecond')
  await editor.selectText()
  await toolbar.getByRole('button', { name: 'Task list', exact: true }).click()
  await expect(editor).toHaveValue('- [ ] first\n- [ ] second')
  await toolbar.getByRole('button', { name: 'Task list', exact: true }).click()
  await expect(editor).toHaveValue('first\nsecond')

  await page.getByRole('tab', { name: 'Preview' }).click()
  await expect(page.getByRole('tabpanel')).toContainText('first')
  await page.getByRole('tab', { name: 'Write' }).click()
  await editor.fill('')
  await page.getByRole('tab', { name: 'Preview' }).click()
  await expect(page.getByText('Nothing to preview')).toBeVisible()
})

// ---- Intake plan fields (component-web-task-surfaces invariant 63;
// req-task-plan-state AC-2.1 to AC-2.6) ----

// The task object the multipart intake request carries in its `task` part.
function submittedTask(raw: string): Record<string, unknown> {
  const match = /name="task"\r\n\r\n([\s\S]*?)\r\n--/.exec(raw)
  if (!match) throw new Error(`no task part in ${raw}`)
  return JSON.parse(match[1]) as Record<string, unknown>
}

const validCommit = '0123456789abcdef0123456789abcdef01234567'
const intakePlan = '## Approach\nSplit the reader.\n\n## Done criteria\n- The reader is split.'

test('an unchanged plan state select opens on the workspace default and sends no plan_state', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { defaultPlanState: 'not_required' })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })

  // No plan approval toggle remains (invariant 63).
  await expect(sheet.getByText(/Plan approval gate/)).toHaveCount(0)
  await expect(sheet.getByText(/Auto-approve/)).toHaveCount(0)
  const select = sheet.getByRole('combobox', { name: 'Plan state' })
  await expect(select).toHaveValue('not_required')
  await expect(select.locator('option')).toHaveText(['Planning needed', 'No plan required'])
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText('This task will start at: No plan required.')
  await descriptionEditor(page).fill('Leave the default alone')
  await sheet.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('Leave the default alone')
  const task = submittedTask(submitted())
  expect(task).not.toHaveProperty('plan_state')
  expect(task).not.toHaveProperty('plan')
  expect(task).not.toHaveProperty('plan_approved')
  expect(task).not.toHaveProperty('spec_approval')
})

test('the plan state select reads Workspace default until the workspace read states one', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { defaultPlanState: null })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const select = sheet.getByRole('combobox', { name: 'Plan state' })
  await expect(select.locator('option:checked')).toHaveText('Workspace default')
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText(
    'This task will start at the workspace default plan state.',
  )

  // A changed select sends the chosen state.
  await select.selectOption('not_required')
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText('This task will start at: No plan required.')
  await descriptionEditor(page).fill('Skip planning for this one')
  await sheet.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('Skip planning for this one')
  expect(submittedTask(submitted())).toMatchObject({ plan_state: 'not_required' })
})

test('a plan state picked before the default arrives is sent even when the default matches it', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { defaultPlanState: 'needed' })
  let releaseWorkspace = () => {}
  const workspaceGate = new Promise<void>((resolve) => {
    releaseWorkspace = resolve
  })
  // Registered last, so it answers first: the workspace read waits until the
  // operator has chosen.
  await page.route('**/v1/workspace?*', async (route) => {
    await workspaceGate
    await route.fallback()
  })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const select = sheet.getByRole('combobox', { name: 'Plan state' })
  await expect(select.locator('option:checked')).toHaveText('Workspace default')
  await select.selectOption('needed')
  releaseWorkspace()
  await expect(select.locator('option')).toHaveText(['Planning needed', 'No plan required'])
  await expect(select).toHaveValue('needed')
  await descriptionEditor(page).fill('Plan this one explicitly')
  await sheet.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('Plan this one explicitly')
  expect(submittedTask(submitted())).toMatchObject({ plan_state: 'needed' })
})

test('Workspace default stays a choice while the default is unknown and sends no plan_state', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { defaultPlanState: null })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const select = sheet.getByRole('combobox', { name: 'Plan state' })
  await select.selectOption('needed')
  await expect(select.locator('option')).toHaveText(['Workspace default', 'Planning needed', 'No plan required'])
  await select.selectOption('')
  await expect(select.locator('option:checked')).toHaveText('Workspace default')
  await descriptionEditor(page).fill('Back to the default')
  await sheet.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('Back to the default')
  expect(submittedTask(submitted())).not.toHaveProperty('plan_state')
})

test('a plan disables the plan state select and is sent without plan_state', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { defaultPlanState: 'needed' })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const select = sheet.getByRole('combobox', { name: 'Plan state' })
  await select.selectOption('not_required')
  await expect(sheet.getByRole('textbox', { name: 'Base commit' })).toBeDisabled()

  await sheet.getByRole('textbox', { name: 'Plan' }).fill(intakePlan)
  await expect(select).toBeDisabled()
  await expect(sheet.getByRole('textbox', { name: 'Base commit' })).toBeEnabled()
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText(
    'This task will start at: Plan awaiting approval.',
  )
  await descriptionEditor(page).fill('File the plan with the task')
  await sheet.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('File the plan with the task')
  const task = submittedTask(submitted())
  expect(task.plan).toBe(intakePlan)
  expect(task).not.toHaveProperty('plan_state')
  expect(task).not.toHaveProperty('plan_base_commit')
  expect(task).not.toHaveProperty('plan_approved')
})

test('a base commit other than 40 lowercase hex characters disables Create and says why', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const create = sheet.getByRole('button', { name: 'Create task' })
  await descriptionEditor(page).fill('Plan read against a commit')
  await sheet.getByRole('textbox', { name: 'Plan' }).fill(intakePlan)
  const commit = sheet.getByRole('textbox', { name: 'Base commit' })

  for (const invalid of ['abc123', validCommit.toUpperCase(), `${validCommit}0`, `${validCommit.slice(0, 39)}g`]) {
    await commit.fill(invalid)
    await expect(create).toBeDisabled()
    await expect(sheet.getByRole('alert')).toHaveText(
      'Base commit must be a full commit ID of 40 lowercase hexadecimal characters.',
    )
  }
  // Surrounding whitespace is trimmed before the check and the send.
  await commit.fill(`  ${validCommit} `)
  await expect(sheet.getByRole('alert')).toHaveCount(0)
  await expect(create).toBeEnabled()
  await create.click()
  await expect.poll(submitted).toContain('Plan read against a commit')
  expect(submittedTask(submitted())).toMatchObject({ plan: intakePlan, plan_base_commit: validCommit })
})

test('Approve this plan shows only with a plan for operate_gates and sends plan_approved', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, { role: 'operator' })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  const approve = sheet.getByRole('checkbox', { name: 'Approve this plan' })
  await expect(approve).toHaveCount(0)
  await sheet.getByRole('textbox', { name: 'Plan' }).fill(intakePlan)
  await expect(approve).toBeVisible()
  await approve.check()
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText('This task will start at: Planning completed.')
  // Clearing the plan hides the control and sends no approval.
  await sheet.getByRole('textbox', { name: 'Plan' }).fill('')
  await expect(approve).toHaveCount(0)
  await sheet.getByRole('textbox', { name: 'Plan' }).fill(intakePlan)
  await expect(approve).toBeChecked()
  await descriptionEditor(page).fill('Filed and approved together')
  await sheet.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('Filed and approved together')
  expect(submittedTask(submitted())).toMatchObject({ plan: intakePlan, plan_approved: true })
  expect(submittedTask(submitted())).not.toHaveProperty('plan_state')
})

test('Approve this plan is hidden without operate_gates', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page, {
    role: 'maintainer',
    capabilities: ['view_workspace', 'create_tasks'],
  })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  await sheet.getByRole('textbox', { name: 'Plan' }).fill(intakePlan)
  await expect(sheet.getByRole('textbox', { name: 'Base commit' })).toBeEnabled()
  await expect(sheet.getByRole('checkbox', { name: 'Approve this plan' })).toHaveCount(0)
  await expect(sheet.locator('[data-resulting-plan-state]')).toHaveText(
    'This task will start at: Plan awaiting approval.',
  )
  await descriptionEditor(page).fill('Filed by a creator who cannot approve')
  await sheet.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('Filed by a creator who cannot approve')
  expect(submittedTask(submitted())).not.toHaveProperty('plan_approved')
})

test('a refused plan shows the server message', async ({ page }) => {
  await mockTaskCreateAPIs(page, {
    createRefusal: { status: 400, message: 'invalid plan: missing Done criteria heading' },
  })
  await page.goto('/w/demo/new')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  await descriptionEditor(page).fill('A plan the server refuses')
  await sheet.getByRole('textbox', { name: 'Plan' }).fill('## Approach\nOnly an approach.')
  await sheet.getByRole('button', { name: 'Create task' }).click()
  await expect(sheet.getByText('invalid plan: missing Done criteria heading')).toBeVisible()
  // Nothing was created: the sheet stays open on the same input.
  await expect(sheet).toBeVisible()
  await expect(page).not.toHaveURL(/\/tasks\//)
})
