import { expect, type Page, test } from '@playwright/test'
import { mockSettingsPage, openSettingsRow, settingsRowHeader } from './helpers/settings'

// The MCP setup dialog and the Settings connection row show browser sign-in
// to the remote `/mcp` URL by default, with the access token setup as an
// explicit second option (component-web-dashboard invariant 26; req-mcp-oauth
// REQ-5). No setup shows a credential value.
//
// The released `conveyor mcp install` has no `--auth` flag and writes only an
// access token entry. So the browser sign-in steps are manual, the access
// token steps use the released CLI, and every command with `--auth` sits in a
// note that names the CLI version it needs.

const clients = ['Cursor', 'OpenCode', 'Claude Code', 'Codex', 'Other']

// The CLI `--tool` value and the file each manual step names.
const toolOf: Record<string, string> = {
  Cursor: 'cursor',
  OpenCode: 'opencode',
  'Claude Code': 'claude',
  Codex: 'codex',
}
const fileOf: Record<string, string> = {
  Cursor: '~/.cursor/mcp.json',
  OpenCode: '~/.config/opencode/opencode.json',
  'Claude Code': '~/.claude.json',
  Codex: '~/.codex/config.toml',
}

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

// Every use of the word `conveyor` that names the MCP registration and not the
// CLI. Any other use counts as a command, so a new command cannot escape the
// check below by its form.
const registrationNames = [
  /"conveyor": \{/g,
  /\[mcp_servers\.conveyor\]/g,
  /cursor-agent mcp list-tools conveyor\b/g,
  /codex mcp login conveyor\b/g,
  /The conveyor line must read connected/g,
  /Select conveyor and approve/g,
]

/** Each `conveyor` command in the text: its subcommand and its --server values. */
function conveyorCommands(text: string) {
  const commands = registrationNames.reduce((rest, name) => rest.replace(name, ''), text)
  return [...commands.matchAll(/\bconveyor\b((?: [a-z]+)*)((?: --[a-z-]+(?: [^\s,)]+)?)*)/g)].map((match) => ({
    subcommand: match[1].trim(),
    servers: [...match[2].matchAll(/ --server ([^\s,)]+)/g)].map((server) => server[1].replace(/\.$/, '')),
  }))
}

// Each client and mode, with the commands its instructions and snippet show.
const shownCommands: Array<[mode: 'Browser sign-in' | 'Access token', client: string, commands: string[]]> = [
  ['Browser sign-in', 'Cursor', ['mcp install']],
  ['Browser sign-in', 'OpenCode', ['mcp install']],
  ['Browser sign-in', 'Claude Code', ['mcp install']],
  ['Browser sign-in', 'Codex', ['mcp install']],
  ['Browser sign-in', 'Other', []],
  ['Access token', 'Cursor', ['auth login', 'mcp install', 'auth token']],
  ['Access token', 'OpenCode', ['auth login', 'mcp install', 'auth token']],
  ['Access token', 'Claude Code', ['auth login', 'mcp install']],
  ['Access token', 'Codex', ['auth login', 'mcp install', 'auth token']],
  ['Access token', 'Other', ['mcp stdio']],
]

test('every command in the setup dialog names the server of the displayed endpoint', async ({ page }) => {
  const { dialog } = await openDialog(page)
  const endpoint = await dialog.getByTestId('mcp-endpoint').innerText()
  const server = new URL(endpoint).origin
  // The table covers every client in both modes.
  expect(shownCommands).toHaveLength((await dialog.getByRole('tab').count()) * 2)
  for (const [mode, client, commands] of shownCommands) {
    await dialog.getByRole('button', { name: mode }).click()
    await dialog.getByRole('tab', { name: client }).click()
    const panel = dialog.getByRole('tabpanel')
    await expect(panel.locator('pre')).toContainText(endpoint)
    expect(conveyorCommands(await panel.innerText()), `${mode}, ${client}`).toEqual(
      commands.map((subcommand) => ({ subcommand, servers: [server] })),
    )
  }
})

test('the setup dialog defaults to browser sign-in with no credential in any client setup', async ({ page }) => {
  const { dialog } = await openDialog(page)
  const endpoint = new URL('/mcp', page.url()).toString()
  await expect(dialog.getByTestId('mcp-endpoint')).toHaveText(endpoint)
  await expect(dialog.getByRole('button', { name: 'Browser sign-in' })).toHaveAttribute('aria-pressed', 'true')
  await expect(dialog.getByRole('button', { name: 'Access token' })).toHaveAttribute('aria-pressed', 'false')

  for (const client of clients) {
    await dialog.getByRole('tab', { name: client }).click()
    const panel = dialog.getByRole('tabpanel')
    const snippet = await panel.locator('pre').innerText()
    expect(snippet).toContain(endpoint)
    for (const marker of credentialMarkers) expect(snippet).not.toContain(marker)
    // The steps are the manual setup that works with the released CLI: put
    // the endpoint in the client's file, then sign in from the client.
    const steps = panel.locator('ol > li')
    const stepText = await steps.allInnerTexts()
    for (const marker of ['conveyor mcp install', '--auth', 'CONVEYOR_API_TOKEN', 'auth token', 'auth login'])
      expect(stepText.join('\n')).not.toContain(marker)
    expect(stepText.join('\n')).toMatch(/approve the request/)
    const note = panel.getByTestId('mcp-installer-note')
    if (client === 'Other') {
      expect(stepText[1]).toContain('Add the endpoint below. Do not add an Authorization header.')
      await expect(note).toHaveCount(0)
    } else {
      expect(stepText[0]).toBe(
        `${client === 'Codex' ? 'Add' : 'Merge'} the configuration below ${client === 'Codex' ? 'to' : 'into'} ${fileOf[client]}. It holds only the Conveyor endpoint.`,
      )
      // The installer command is labeled with the CLI version it needs.
      await expect(note).toHaveText(
        `Needs a Conveyor CLI version that supports --auth: conveyor mcp install --tool ${toolOf[client]} --server ${new URL(endpoint).origin} writes this entry for you. The current CLI writes an access token entry instead.`,
      )
    }
    // The copied text is the snippet on screen.
    await panel.getByRole('button', { name: `Copy ${client} setup` }).click()
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(snippet)
  }

  await dialog.getByRole('button', { name: 'Copy MCP endpoint' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(endpoint)
})

test('the access token option uses the released CLI and holds only placeholders', async ({ page }) => {
  const { dialog } = await openDialog(page)
  const server = new URL(page.url()).origin
  await dialog.getByRole('button', { name: 'Access token' }).click()
  await expect(dialog.getByRole('button', { name: 'Access token' })).toHaveAttribute('aria-pressed', 'true')
  for (const client of ['Cursor', 'OpenCode', 'Claude Code', 'Codex']) {
    await dialog.getByRole('tab', { name: client }).click()
    const tab = dialog.getByRole('tabpanel')
    const stepText = await tab.locator('ol > li').allInnerTexts()
    // The released CLI: save a token, then install without --auth.
    expect(stepText[0]).toBe(`Run conveyor auth login --server ${server} to save an access token.`)
    expect(stepText[1]).toContain(`Run conveyor mcp install --tool ${toolOf[client]} --server ${server}`)
    expect(stepText.join('\n')).not.toContain('--auth')
    // A file edited by hand gets a placeholder or an environment reference.
    expect(stepText[2]).toMatch(/^To edit the file yourself, /)
    expect(stepText[2]).toContain(
      client === 'Claude Code'
        ? 'replace the placeholder with one of your access tokens'
        : `export CONVEYOR_API_TOKEN=$(conveyor auth token --server ${server})`,
    )
    await expect(tab.getByTestId('mcp-installer-note')).toHaveText(
      'With a Conveyor CLI version that supports --auth, add --auth pat to the install command. Without it, that version installs browser sign-in.',
    )
    expect(await tab.locator('pre').innerText()).toMatch(
      /<CONVEYOR_API_TOKEN>|CONVEYOR_API_TOKEN\}|"CONVEYOR_API_TOKEN"/,
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
  // The released CLI first, and --auth only for a CLI version that has it.
  const server = new URL(page.url()).origin
  await expect(tokenOption).toContainText(
    `run conveyor auth login --server ${server}, then conveyor mcp install --server ${server}. The current CLI installs an access token entry. With a CLI version that supports --auth, add --auth pat.`,
  )
  await expect(tokenOption).not.toContainText('install --auth')
  expect(conveyorCommands(await region.innerText())).toEqual([
    { subcommand: 'auth login', servers: [server] },
    { subcommand: 'mcp install', servers: [server] },
  ])
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
