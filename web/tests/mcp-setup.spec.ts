import { expect, type Page, test } from '@playwright/test'
import { mockSettingsPage, openSettingsRow, settingsRowHeader } from './helpers/settings'

// The MCP setup dialog and the Settings connection row show browser sign-in
// to the remote `/mcp` URL by default, with the `--auth pat` access token
// setup as an explicit second option (component-web-dashboard invariant 26;
// req-mcp-oauth REQ-5). No setup shows a credential value.

const clients = ['Cursor', 'OpenCode', 'Claude Code', 'Codex', 'Other']

// Text that marks a token setup. The default setup holds none of it.
const credentialMarkers = [
  'Authorization',
  'Bearer',
  'CONVEYOR_API_TOKEN',
  '<paste-your-token>',
  'auth token',
  'headers',
]

async function openDialog(page: Page) {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  await mockSettingsPage(page)
  await page.goto('/w/demo/')
  const trigger = page.getByRole('button', { name: 'MCP', exact: true })
  await trigger.click()
  const dialog = page.getByRole('dialog', { name: 'MCP Setup' })
  await expect(dialog).toBeVisible()
  return { dialog, trigger }
}

test('the setup dialog defaults to browser sign-in with no credential in any client setup', async ({ page }) => {
  const { dialog } = await openDialog(page)
  const endpoint = new URL('/mcp', page.url()).toString()
  await expect(dialog.getByTestId('mcp-endpoint')).toHaveText(endpoint)
  await expect(dialog.getByRole('button', { name: 'Browser sign-in' })).toHaveAttribute('aria-pressed', 'true')
  await expect(dialog.getByRole('button', { name: 'Access token (--auth pat)' })).toHaveAttribute(
    'aria-pressed',
    'false',
  )

  for (const client of clients) {
    await dialog.getByRole('tab', { name: client }).click()
    const panel = dialog.getByRole('tabpanel')
    const snippet = await panel.locator('pre').innerText()
    expect(snippet).toContain(endpoint)
    for (const marker of credentialMarkers) expect(snippet).not.toContain(marker)
    const steps = await panel.locator('ol').innerText()
    expect(steps).not.toContain('--auth pat')
    for (const marker of ['CONVEYOR_API_TOKEN', 'auth token', 'auth login']) expect(steps).not.toContain(marker)
    // The copied text is the snippet on screen.
    await panel.getByRole('button', { name: `Copy ${client} setup` }).click()
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(snippet)
  }

  await dialog.getByRole('button', { name: 'Copy MCP endpoint' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(endpoint)
})

test('the access token option names --auth pat and holds only placeholders', async ({ page }) => {
  const { dialog } = await openDialog(page)
  await dialog.getByRole('button', { name: 'Access token (--auth pat)' }).click()
  await expect(dialog.getByRole('button', { name: 'Access token (--auth pat)' })).toHaveAttribute(
    'aria-pressed',
    'true',
  )
  for (const client of ['Cursor', 'OpenCode', 'Claude Code', 'Codex']) {
    await dialog.getByRole('tab', { name: client }).click()
    await expect(dialog.getByRole('tabpanel').locator('ol')).toContainText(
      `--tool ${client === 'Claude Code' ? 'claude' : client.toLowerCase()} --auth pat`,
    )
  }
  await dialog.getByRole('tab', { name: 'Other' }).click()
  const panel = dialog.getByRole('tabpanel')
  await expect(panel.locator('pre')).toContainText('"Authorization": "Bearer <CONVEYOR_API_TOKEN>"')
  await expect(panel.locator('ol')).toContainText('conveyor mcp stdio --server')
  expect(await panel.innerText()).not.toMatch(/cv_pat_|cv_oat_/)

  // Back to the default.
  await dialog.getByRole('button', { name: 'Browser sign-in' }).click()
  await expect(panel.locator('pre')).not.toContainText('Authorization')
})

test('the setup dialog keeps its close and keyboard behavior', async ({ page }) => {
  const { dialog, trigger } = await openDialog(page)
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(trigger).toBeFocused()
  await trigger.click()
  await expect(dialog).toBeVisible()
  await dialog.getByRole('button', { name: 'Close MCP Setup' }).click()
  await expect(dialog).toBeHidden()
  await trigger.click()
  await dialog.getByRole('button', { name: 'Done' }).click()
  await expect(dialog).toBeHidden()
})

test('the Settings connection row shows the endpoint setup first and the token option second', async ({ page }) => {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  await mockSettingsPage(page)
  await page.goto('/w/demo/settings')
  const endpoint = new URL('/mcp', page.url()).toString()
  await page.getByRole('button', { name: 'Copy MCP endpoint' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(endpoint)

  const region = await openSettingsRow(page, 'Connect a coding agent')
  const config = await region.locator('pre').first().innerText()
  expect(config).toContain(endpoint)
  for (const marker of credentialMarkers) expect(config).not.toContain(marker)
  await region.getByRole('button', { name: 'Copy connection configuration' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(config)

  const tokenOption = region.getByTestId('connect-agent-token-option')
  await expect(tokenOption).toContainText('conveyor mcp install --auth pat')
  await expect(tokenOption.locator('pre')).toContainText('Bearer <CONVEYOR_API_TOKEN>')
  await expect(settingsRowHeader(page, 'Connect a coding agent')).toHaveAttribute('aria-expanded', 'true')
})

test('the welcome card shows the endpoint setup with no credential', async ({ page }) => {
  await mockSettingsPage(page)
  await page.goto('/w/demo/settings?welcome=true')
  const welcome = page
    .getByText('Welcome to Conveyor')
    .locator('xpath=ancestor::*[contains(@class, "border-primary")][1]')
  const config = await welcome.locator('pre').innerText()
  expect(config).toContain(new URL('/mcp', page.url()).toString())
  for (const marker of credentialMarkers) expect(config).not.toContain(marker)
})
