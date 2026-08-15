import { expect, test, type Locator, type Page, type Route } from '@playwright/test'

const codexEffortArgs = {
  low: ['--config', 'model_reasoning_effort="low"'],
  medium: ['--config', 'model_reasoning_effort="medium"'],
  high: ['--config', 'model_reasoning_effort="high"'],
}

const codexHarness = {
  name: 'codex',
  mcp_transport: 'toml_override',
  command: ['codex', 'exec', '{prompt}', '--config', '{mcp_config}'],
  model_args: ['--model', '{model}'],
  effort_args: codexEffortArgs,
  probe_command: ['codex', '--version'],
  probe_timeout: '10s',
}

const claudeHarness = {
  name: 'claude',
  mcp_transport: 'json_file',
  command: ['claude', '-p', '{prompt}', '--mcp-config', '{mcp_config}'],
  model_args: ['--model', '{model}'],
  effort_args: { low: ['--effort', 'low'], medium: ['--effort', 'medium'], high: ['--effort', 'high'] },
  probe_command: ['claude', '--version'],
  probe_timeout: '10s',
}

const grokHarness = {
  name: 'grok',
  mcp_transport: 'environment',
  mcp_attachment: 'conveyor',
  command: ['grok', '--single', '{prompt}'],
  model_args: ['--model', '{model}'],
  effort_args: {
    low: ['--reasoning-effort', 'low'],
    medium: ['--reasoning-effort', 'medium'],
    high: ['--reasoning-effort', 'high'],
  },
  probe_command: ['grok', '--version'],
  probe_timeout: '30s',
}

const harnessTemplates = [
  { id: 'codex', label: 'Codex CLI', description: "OpenAI's coding agent", harness: codexHarness },
  { id: 'claude', label: 'Claude Code', description: "Anthropic's coding agent", harness: claudeHarness },
  { id: 'grok', label: 'Grok CLI', description: "xAI's coding agent", harness: grokHarness },
]

const document = {
  workspace: 'demo',
  max_bounces: 2,
  work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: { triage: { model: 'gpt', timeout: '20m' } },
    spec: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '30m' },
    implementation: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '2h' },
    review: { execution: 'mcp', timeout: '1h', fallback_model: 'gpt', fallback_harness: 'codex' },
  },
  routing: { stages: {} },
  harnesses: [codexHarness, claudeHarness, grokHarness],
  review: { seats: [{ model: 'gpt', harness: 'codex' }] },
  setups: [],
  default_setup: '',
  execution: {
    spec_approval: true,
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
}

async function mockAPIs(page: Page, templatesFail = false) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document, version: 1 } })
    if (path === '/v1/harness-templates') {
      if (templatesFail) return route.fulfill({ status: 500, body: 'catalog unavailable' })
      return route.fulfill({ json: { templates: harnessTemplates } })
    }
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
}

async function expectArgv(card: Locator, label: string, args: string[]) {
  const editor = card.locator(`input[aria-label="${label}"]`).locator('..')
  await expect(editor.locator('button[title^="Edit "]')).toHaveCount(args.length)
  for (const arg of args) await expect(editor.getByTitle(`Edit ${arg}`, { exact: true })).toBeVisible()
}

test('harness picker preserves complete template efforts in editable unique drafts and keeps Custom blank', async ({
  page,
}) => {
  await mockAPIs(page)
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Harnesses' }).click()

  for (const template of harnessTemplates) {
    await page.getByRole('button', { name: 'Add harness' }).click()
    const menuitem = page.getByRole('menuitem', { name: new RegExp(template.label) })
    await expect(menuitem).toContainText(template.description)
    await expect(page.getByRole('menu')).not.toContainText(template.harness.mcp_transport)
    await menuitem.click()

    const name = `${template.id}-2`
    const card = page.getByRole('button', { name: `Toggle ${name}` }).locator('..')
    await expect(card.getByLabel(/^Harness \d+ name$/)).toHaveValue(name)
    await card.locator('summary').click()
    await expectArgv(card, 'Low effort argv', template.harness.effort_args.low)
    await expectArgv(card, 'Medium effort argv', template.harness.effort_args.medium)
    await expectArgv(card, 'High effort argv', template.harness.effort_args.high)
  }

  const codexCard = page.getByRole('button', { name: 'Toggle codex-2' }).locator('..')
  await codexCard.getByTitle('Edit model_reasoning_effort="low"', { exact: true }).click()
  const lowEffort = codexCard.locator('input[aria-label="Low effort argv"]')
  await lowEffort.fill('model_reasoning_effort="custom-low"')
  await lowEffort.press('Enter')
  await expect(codexCard.getByTitle('Edit model_reasoning_effort="custom-low"', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Add harness' }).click()
  await page.getByRole('menuitem', { name: /Custom/ }).click()
  const customCard = page
    .getByRole('button', { name: /Toggle harness \d+/ })
    .last()
    .locator('..')
  await expect(customCard.getByLabel(/^Harness \d+ name$/)).toHaveValue('')
  await expect(customCard.getByLabel('MCP transport')).toHaveValue('json_file')
  await expectArgv(customCard, 'Command argv', [])
  await customCard.locator('summary').click()
  await expectArgv(customCard, 'Low effort argv', [])
  await expectArgv(customCard, 'Medium effort argv', [])
  await expectArgv(customCard, 'High effort argv', [])
})

test('harness picker degrades to Custom when catalog fetch fails', async ({ page }) => {
  await mockAPIs(page, true)
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Add harness' }).click()
  await expect(page.getByRole('menuitem')).toHaveCount(1)
  await expect(page.getByRole('menuitem')).toContainText('Custom')
})
