import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-18T00:00:00Z'

async function mockTaskCreateAPIs(page: Page) {
  let submitted = ''
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: '2026-07-18T00:00:00Z' }] })
      return
    }
    if (url.pathname === '/v1/workspace') {
      await route.fulfill({ json: { workspace: 'demo', database: 'postgres', max_bounces: 2, repos: [{ name: 'conveyor', base: 'main' }], routing: [], default_setup: 'backend', setups: [
        { name: 'backend', execution_settings: { implementation: { harness: 'codex', model: 'gpt' }, review: { fallback_harness: 'codex' } }, review: { seats: [{ model: 'gpt-review' }] } },
        { name: 'frontend', execution_settings: { implementation: { harness: 'claude', model: 'claude-ui' }, review: { fallback_harness: 'claude' } }, review: { seats: [{ model: 'claude-review' }] } },
      ] } })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({ json: { workers: [], auto_available: false, auto_unavailable_reason: 'manual test', setup_serviceability: { backend: { auto_available: false }, frontend: { auto_available: true } } } })
      return
    }
    if (url.pathname === '/v1/tasks' && route.request().method() === 'POST') {
      submitted = route.request().postData() ?? ''
      await route.fulfill({ status: 201, json: { id: 'generated', workspace: 'demo', title: 'Generated title', body: 'Generate this title from context', repo: 'conveyor', state: 'queued', created_at: '2026-07-18T00:00:00Z' } })
      return
    }
    if (url.pathname === '/v1/tasks' && route.request().method() === 'GET') {
      await route.fulfill({ json: [
        { id: '260729-dependency', workspace: 'demo', title: 'Finish persistence first', body: '', repo: 'conveyor', state: 'running', created_at: createdAt },
        { id: '260729-terminal', workspace: 'demo', title: 'Already merged', body: '', repo: 'conveyor', state: 'merged', created_at: createdAt },
      ] })
      return
    }
    await route.fulfill({ json: [] })
  })
  return () => submitted
}

test('new task removes title input and submits description for AI title generation', async ({ page }) => {
  const submitted = await mockTaskCreateAPIs(page)
  await page.goto('/new')

  await expect(page.getByPlaceholder('What should change?')).toHaveCount(0)
  await expect(page.getByText('AI generates the task title from this context')).toBeVisible()
  const create = page.getByRole('button', { name: 'Create task' })
  await expect(create).toBeDisabled()
  await page.locator('textarea').fill('Generate this title from context')
  await page.getByLabel('Execution setup').selectOption('frontend')
  await page.getByText('Composition').click()
  await expect(page.getByText(/Implement: claude/)).toBeVisible()
  await expect(create).toBeEnabled()
  await create.click()

  await expect.poll(submitted).toContain('Generate this title from context')
  await expect.poll(submitted).toContain('frontend')
  expect(submitted()).not.toContain('"title"')
  // §21.31: no execution-mode selector; hold defaults off and is omitted.
  expect(submitted()).not.toContain('"mode"')
  expect(submitted()).not.toContain('"hold"')
})

test('intake offers a hold toggle and advisory worker warning instead of modes', async ({ page }) => {
  const submitted = await mockTaskCreateAPIs(page)
  await page.goto('/new')

  await expect(page.getByRole('radiogroup', { name: 'Execution mode' })).toHaveCount(0)
  // backend (the default setup) is unserviceable in the mock: advisory only.
  await expect(page.getByText(/No worker can run backend right now/)).toBeVisible()
  await page.locator('textarea').fill('Hold this one for me')
  await page.getByRole('switch', { name: 'Hold for hands-on work' }).click()
  await expect(page.getByText(/No worker can run backend/)).toHaveCount(0)
  await page.getByRole('button', { name: 'Create task' }).click()

  await expect.poll(submitted).toContain('"hold":true')
})

test('intake declares an optional dependency from the advanced selector', async ({ page }) => {
  const submitted = await mockTaskCreateAPIs(page)
  await page.goto('/new')
  await page.locator('textarea').fill('Implement the dependent task')
  await page.getByText('Advanced options').click()
  await page.getByLabel('Search dependency tasks').fill('persistence')
  await page.getByText('Finish persistence first').click()
  await expect(page.getByText('Already merged')).toHaveCount(0)
  await page.getByRole('button', { name: 'Create task' }).click()
  await expect.poll(submitted).toContain('"depends_on":["260729-dependency"]')
})

test('intake markdown editor formats, toggles, previews, and restores selection', async ({ page }) => {
  await mockTaskCreateAPIs(page)
  await page.goto('/new')

  const editor = page.locator('textarea')
  const toolbar = page.getByRole('toolbar', { name: 'Markdown formatting' })
  for (const name of ['Heading', 'Bold', 'Italic', 'Quote', 'Inline code', 'Code block', 'Link', 'Bullet list', 'Numbered list', 'Task list']) {
    await expect(toolbar.getByRole('button', { name, exact: true })).toBeVisible()
  }
  await expect(editor).toHaveCSS('font-family', /monospace|Mono/i)

  await editor.fill('format me')
  await editor.evaluate((element: HTMLTextAreaElement) => element.setSelectionRange(0, 6))
  await toolbar.getByRole('button', { name: 'Bold', exact: true }).click()
  await expect(editor).toHaveValue('**format** me')
  await expect(editor).toBeFocused()
  await expect.poll(() => editor.evaluate((element: HTMLTextAreaElement) => [element.selectionStart, element.selectionEnd])).toEqual([2, 8])
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
  await expect.poll(() => editor.evaluate((element: HTMLTextAreaElement) => element.value.slice(element.selectionStart, element.selectionEnd))).toBe('url')

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

  for (const [name, expected] of [['Inline code', '`code`'], ['Code block', '```\ncode\n```']] as const) {
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
