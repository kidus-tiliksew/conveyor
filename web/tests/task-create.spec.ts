import { expect, test, type Page, type Route } from '@playwright/test'

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
      await route.fulfill({ json: { workspace: 'demo', database: 'postgres', max_bounces: 2, repos: [{ name: 'conveyor', base: 'main' }], routing: [] } })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({ json: { workers: [], auto_available: false, auto_unavailable_reason: 'manual test' } })
      return
    }
    if (url.pathname === '/v1/tasks' && route.request().method() === 'POST') {
      submitted = route.request().postData() ?? ''
      await route.fulfill({ status: 201, json: { id: 'generated', workspace: 'demo', title: 'Generated title', body: 'Generate this title from context', repo: 'conveyor', state: 'queued', created_at: '2026-07-18T00:00:00Z' } })
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
  await expect(create).toBeEnabled()
  await create.click()

  await expect.poll(submitted).toContain('Generate this title from context')
  expect(submitted()).not.toContain('"title"')
})
