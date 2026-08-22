import { expect, test, type Page, type Route } from '@playwright/test'

const workspaces = [
  { id: 'beta-ventures', name: 'Beta Ventures', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
  { id: 'design', name: 'Design', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
  { id: 'funnel', name: 'Funnel', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
]

async function mockShell(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'design')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: workspaces })
      return
    }
    if (url.pathname === '/v1/me') {
      await route.fulfill({
        json: { id: 'usr_operator', email: 'operator@example.test', display_name: 'Operator', role: 'operator' },
      })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test('workspace rail uses Slack-like tiles and preserves switching and creation', async ({ page }) => {
  await mockShell(page)
  await page.goto('/')

  const rail = page.getByRole('navigation', { name: 'Workspaces' })
  const beta = rail.getByRole('button', { name: 'Switch to Beta Ventures' })
  const design = rail.getByRole('button', { name: 'Switch to Design' })
  const funnel = rail.getByRole('button', { name: 'Switch to Funnel' })
  const add = rail.getByRole('link', { name: 'Add workspace' })

  await expect(beta).toHaveText('BV')
  await expect(design).toHaveText('DE')
  await expect(funnel).toHaveText('FU')
  await expect(design).toHaveAttribute('aria-current', 'true')
  await expect(add).toHaveAttribute('href', '/workspaces/new')

  const visual = await rail.evaluate((node) => {
    const [inactive, active] = Array.from(node.querySelectorAll('button'))
    const railStyle = getComputedStyle(node)
    const inactiveStyle = getComputedStyle(inactive)
    const activeStyle = getComputedStyle(active)
    const inactiveBox = inactive.getBoundingClientRect()
    const activeBox = active.getBoundingClientRect()
    const addBox = node.querySelector('a')!.getBoundingClientRect()
    return {
      railBackground: railStyle.backgroundColor,
      inactiveBackground: inactiveStyle.backgroundColor,
      activeBackground: activeStyle.backgroundColor,
      activeShadow: activeStyle.boxShadow,
      radius: inactiveStyle.borderRadius,
      inactiveSize: [inactiveBox.width, inactiveBox.height],
      faceGap: activeBox.top - inactiveBox.bottom,
      addBelowWorkspaces:
        addBox.top > Array.from(node.querySelectorAll('button')).at(-1)!.getBoundingClientRect().bottom,
    }
  })

  expect(visual.inactiveBackground).not.toBe(visual.railBackground)
  expect(visual.activeBackground).not.toBe(visual.inactiveBackground)
  expect(visual.activeShadow).not.toBe('none')
  expect(visual.radius).toBe('9px')
  expect(visual.inactiveSize).toEqual([32, 32])
  expect(visual.faceGap).toBeGreaterThanOrEqual(24)
  expect(visual.addBelowWorkspaces).toBe(true)

  await beta.click()
  await expect(beta).toHaveAttribute('aria-current', 'true')
  await expect(design).not.toHaveAttribute('aria-current')
  await expect.poll(() => page.evaluate(() => localStorage.getItem('conveyor-workspace'))).toBe('beta-ventures')

  await add.click()
  await expect(page).toHaveURL(/\/workspaces\/new$/)
  await expect(page.getByRole('dialog', { name: 'Create workspace' })).toBeVisible()
})
