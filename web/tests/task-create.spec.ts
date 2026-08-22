import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-18T00:00:00Z'

async function mockTaskCreateAPIs(
  page: Page,
  options: {
    taskList?: 'success' | 'error' | 'delayed'
    createDependencyError?: boolean
    candidates?: Array<Record<string, unknown>>
    pullOnly?: boolean
  } = {},
) {
  let submitted = ''
  let taskListRequests = 0
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
      await route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
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
        },
      })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({
        json: {
          workers: [],
          worker_expected: !options.pullOnly,
          worker_available: false,
          worker_unavailable_reason: options.pullOnly ? '' : 'test worker is stale',
        },
      })
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
    releaseTaskList,
  }
}

test('new task removes title input and submits description for AI title generation', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/new')

  await expect(page.getByPlaceholder('What should change?')).toHaveCount(0)
  await expect(page.getByText('AI generates the task title from this context')).toBeVisible()
  const create = page.getByRole('button', { name: 'Create task' })
  await expect(create).toBeDisabled()
  await page.locator('textarea').fill('Generate this title from context')
  await expect(page.getByLabel(/setup/i)).toHaveCount(0)
  await expect(create).toBeEnabled()
  await create.click()

  await expect.poll(submitted).toContain('Generate this title from context')
  expect(submitted()).not.toContain('setup')
  await expect(page).toHaveURL(/\/tasks\?task=generated$/)
  expect(submitted()).not.toContain('"title"')
  // §21.31: no execution-mode selector; hold defaults off and is omitted.
  expect(submitted()).not.toContain('"mode"')
  expect(submitted()).not.toContain('"hold"')
})

test('intake attaches confirmed product and design context with authenticated reads', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/new')
  await page.locator('textarea').fill('Implement the attached desired state')

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
  await page.goto('/new')
  await page.locator('textarea').fill('Attach several documents')

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
  await page.goto('/new')
  await page.getByRole('combobox', { name: 'Search context' }).click()
  const list = page.getByRole('listbox', { name: 'Context' })
  await expect(list.getByRole('option', { name: /Confirmed product outcome/ })).toBeVisible()
  await expect(list.getByRole('option', { name: /Unconfirmed product outcome/ })).toHaveCount(0)
  await expect(list.getByRole('option', { name: /Unconfirmed technical guidance/ })).toHaveCount(0)
  await expect(list.getByRole('option')).toHaveCount(3)
})

test('intake offers a hold toggle and advisory worker warning instead of modes', async ({ page }) => {
  const { submitted } = await mockTaskCreateAPIs(page)
  await page.goto('/new')

  await expect(page.getByRole('radiogroup', { name: 'Execution mode' })).toHaveCount(0)
  await expect(page.getByText(/Worker unavailable/)).toBeVisible()
  await page.locator('textarea').fill('Hold this one for me')
  await page.getByRole('switch', { name: 'Hold for hands-on work' }).click()
  await expect(page.getByText(/Worker unavailable/)).toHaveCount(0)
  await page.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('"hold":true')
})

test('pull-only intake stays quiet when no enrolled worker is expected', async ({ page }) => {
  await mockTaskCreateAPIs(page, { pullOnly: true })
  await page.goto('/new')

  await expect(page.getByText(/Worker unavailable/)).toHaveCount(0)
  await expect(page.getByText(/claim it manually/)).toHaveCount(0)
  await expect(page.getByRole('radiogroup', { name: 'Execution mode' })).toHaveCount(0)
})

test('dependency candidates load lazily and selected cross-repository chips survive repository changes', async ({
  page,
}) => {
  const { submitted, taskListRequests } = await mockTaskCreateAPIs(page)
  await page.goto('/new')
  expect(taskListRequests()).toBe(0)
  await page.locator('textarea').fill('Implement the dependent task')
  await page.getByText('Advanced options').click()
  await expect.poll(taskListRequests).toBe(1)
  await expect(page.getByText('2 matching open tasks.')).toBeVisible()
  // The empty query proves terminal tasks are filtered independently of search.
  await expect(page.getByText('Already merged')).toHaveCount(0)
  await page.getByText('Finish persistence first').click()
  await page.getByText('Publish API contract').click()
  // Exact, because intake now opens over the Tasks list and that surface has a
  // repository filter of its own behind the sheet.
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
  await page.goto('/new')
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
  await page.goto('/new')
  await page.locator('textarea').fill('Create a cyclic dependency')
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
  await page.goto('/new')

  const editor = page.locator('textarea')
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
