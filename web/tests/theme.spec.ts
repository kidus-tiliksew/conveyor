import { expect, test, type Page, type Route } from '@playwright/test'

async function mockShellAPIs(page: Page) {
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: '2026-07-17T00:00:00Z' }] })
      return
    }
    await route.fulfill({ json: [] })
  })
}

async function themeState(page: Page) {
  return page.evaluate(() => ({
    choice: localStorage.getItem('conveyor-theme'),
    effective: document.documentElement.dataset.theme,
    colorScheme: getComputedStyle(document.documentElement).colorScheme,
    themeColor: document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.content,
  }))
}

test('theme choice is accessible, persists, and System follows preference changes', async ({ page }) => {
  await mockShellAPIs(page)
  await page.emulateMedia({ colorScheme: 'light' })
  await page.goto('/')

  const theme = page.getByLabel('Theme')
  await expect(theme).toHaveValue('system')
  await expect(theme.locator('option')).toHaveText(['Light', 'Dark', 'System'])

  await theme.focus()
  await expect(theme).toBeFocused()
  await page.keyboard.press('d')
  await expect(theme).toHaveValue('dark')
  await expect.poll(() => themeState(page)).toEqual({
    choice: 'dark',
    effective: 'dark',
    colorScheme: 'dark',
    themeColor: '#0f1115',
  })

  await page.reload()
  await expect(page.getByLabel('Theme')).toHaveValue('dark')
  await expect.poll(() => themeState(page)).toEqual({
    choice: 'dark',
    effective: 'dark',
    colorScheme: 'dark',
    themeColor: '#0f1115',
  })

  await page.getByLabel('Theme').selectOption('system')
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect.poll(() => themeState(page)).toEqual({
    choice: 'system',
    effective: 'dark',
    colorScheme: 'dark',
    themeColor: '#0f1115',
  })

  await page.emulateMedia({ colorScheme: 'light' })
  await expect.poll(() => themeState(page)).toEqual({
    choice: 'system',
    effective: 'light',
    colorScheme: 'light',
    themeColor: '#ffffff',
  })

  await page.getByLabel('Theme').selectOption('light')
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect.poll(() => themeState(page)).toEqual({
    choice: 'light',
    effective: 'light',
    colorScheme: 'light',
    themeColor: '#ffffff',
  })
})

test('bootstrap applies saved choice before the React entrypoint loads', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.addInitScript(() => localStorage.setItem('conveyor-theme', 'light'))
  await page.route('**/src/main.tsx', (route) => route.abort())

  await page.goto('/')

  await expect.poll(() => page.evaluate(() => ({
    effective: document.documentElement.dataset.theme,
    colorScheme: document.documentElement.style.colorScheme,
    themeColor: document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.content,
    reactLoaded: Boolean(document.querySelector('#root')?.firstChild),
  }))).toEqual({
    effective: 'light',
    colorScheme: 'light',
    themeColor: '#ffffff',
    reactLoaded: false,
  })
})

test('malformed or inaccessible theme storage falls back to System', async ({ browser }) => {
  const malformed = await browser.newPage({ colorScheme: 'dark' })
  await malformed.addInitScript(() => localStorage.setItem('conveyor-theme', 'sepia'))
  await malformed.route('**/src/main.tsx', (route) => route.abort())
  await malformed.goto('/')
  await expect.poll(() => malformed.evaluate(() => document.documentElement.dataset.theme)).toBe('dark')
  await malformed.close()

  const inaccessible = await browser.newPage({ colorScheme: 'dark' })
  await inaccessible.addInitScript(() => {
    const getItem = Storage.prototype.getItem
    Storage.prototype.getItem = function (key: string) {
      if (key === 'conveyor-theme') throw new DOMException('Storage unavailable')
      return getItem.call(this, key)
    }
  })
  await inaccessible.route('**/src/main.tsx', (route) => route.abort())
  await inaccessible.goto('/')
  await expect.poll(() => inaccessible.evaluate(() => document.documentElement.dataset.theme)).toBe('dark')
  await inaccessible.close()
})
