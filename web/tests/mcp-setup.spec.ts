import { expect, test, type Page } from '@playwright/test'

async function routeDashboard(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/workspace') {
      return route.fulfill({
        json: {
          workspace: 'demo',
          repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }],
        },
      })
    }
    if (path === '/v1/me') {
      return route.fulfill({
        json: { id: 'usr-operator', email: 'operator@example.test', display_name: 'Operator', role: 'operator' },
      })
    }
    if (path === '/v1/pending-proposals') {
      return route.fulfill({
        json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } },
      })
    }
    if (path === '/v1/workers') {
      return route.fulfill({
        json: { workers: [], worker_expected: false, worker_available: false, setup_serviceability: {} },
      })
    }
    if (path === '/v1/activity' || path === '/v1/task-operations') {
      return route.fulfill({
        json: [],
        headers: { 'X-Conveyor-Total': '0', 'X-Conveyor-Limit': '100', 'X-Conveyor-Offset': '0' },
      })
    }
    return route.fulfill({ json: [] })
  })
}

test('Board MCP action offers safe client-specific setup and complete dialog behavior', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await routeDashboard(page)
  await page.goto('/')

  const trigger = page.getByRole('button', { name: 'MCP', exact: true })
  await expect(trigger).toBeVisible()
  await trigger.click()

  const dialog = page.getByRole('dialog', { name: 'MCP Setup' })
  const endpoint = `${new URL(page.url()).origin}/mcp`
  await expect(dialog).toBeVisible()
  await expect(page.locator('body')).toHaveCSS('overflow', 'hidden')
  await expect(dialog.getByRole('tab')).toHaveCount(4)
  for (const label of ['Cursor', 'Claude Code', 'Codex', 'Other']) {
    await expect(dialog.getByRole('tab', { name: label, exact: true })).toBeVisible()
  }
  for (const client of ['cursor', 'claude', 'codex']) {
    await expect(dialog.locator(`[data-mcp-client-logo="${client}"] svg`)).toBeVisible()
  }
  await expect(dialog.locator('[data-mcp-client-logo]')).toHaveCount(3)
  await expect(dialog.getByRole('tab', { name: 'Other' }).locator('[data-mcp-client-fallback]')).toBeVisible()
  await expect(dialog).toContainText(endpoint)
  await expect(dialog).toContainText('<CONVEYOR_API_TOKEN>')
  await expect(dialog).not.toContainText('test-token-that-must-not-appear')

  await dialog.getByRole('tab', { name: 'Claude Code' }).click()
  await expect(dialog).toContainText('~/.claude.json')
  await expect(dialog).toContainText('"type": "http"')

  await dialog.getByRole('tab', { name: 'Codex' }).click()
  await expect(dialog).toContainText('bearer_token_env_var = "CONVEYOR_API_TOKEN"')
  await dialog.getByRole('button', { name: 'Copy Codex setup' }).click()
  await expect(dialog.getByRole('button', { name: 'Copied' })).toBeVisible()

  await dialog.getByRole('tab', { name: 'Other' }).click()
  await expect(dialog).toContainText('MCP connection settings')
  await dialog.getByRole('button', { name: 'Completed' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()
  await expect(page.locator('body')).not.toHaveCSS('overflow', 'hidden')

  await trigger.click()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()

  await trigger.click()
  await page.locator('.fixed.inset-0 > [aria-hidden="true"]').click({ position: { x: 4, y: 4 } })
  await expect(dialog).toHaveCount(0)
})

test('Tasks MCP action stays usable at a narrow viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await routeDashboard(page)
  await page.goto('/tasks')

  const trigger = page.getByRole('button', { name: 'MCP', exact: true })
  await expect(trigger).toBeVisible()
  await trigger.click()

  const dialog = page.getByRole('dialog', { name: 'MCP Setup' })
  await expect(dialog).toBeVisible()
  const box = await dialog.boundingBox()
  expect(box).not.toBeNull()
  expect(box!.x).toBeGreaterThanOrEqual(0)
  expect(box!.x + box!.width).toBeLessThanOrEqual(390)

  await dialog.getByRole('tab', { name: 'Codex' }).click()
  const endpoint = `${new URL(page.url()).origin}/mcp`
  await expect(dialog).toContainText(endpoint)
  await expect(dialog.getByRole('button', { name: 'Copy Codex setup' })).toBeVisible()

  await dialog.getByRole('button', { name: 'Completed' }).click()
  await page.goto('/settings')
  const settings = page.getByText('MCP work-order server').locator('..').locator('..')
  await expect(settings).toContainText(endpoint)
  await expect(settings).toContainText('Bearer <CONVEYOR_API_TOKEN>')
})
