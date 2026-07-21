import { expect, test, type Page, type Route } from '@playwright/test'

const codexHarness = {
  name: 'codex', mcp_transport: 'toml_override',
  command: ['codex', 'exec', '{prompt}', '--config', '{mcp_config}'],
  model_args: ['--model', '{model}'], effort_args: { high: ['--config', 'model_reasoning_effort="high"'] },
  probe_command: ['codex', '--version'], probe_timeout: '10s',
}

const document = {
  workspace: 'demo', max_bounces: 2, work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: { triage: { model: 'gpt', timeout: '20m' }, spec: { model: 'gpt', timeout: '30m' } },
    implementation: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '2h' },
    review: { execution: 'mcp', timeout: '1h', fallback_model: 'gpt', fallback_harness: 'codex' },
  },
  routing: { stages: {} }, harnesses: [codexHarness], review: { seats: [{ model: 'gpt', harness: 'codex' }] },
  setups: [], default_setup: '',
  execution: { spec_approval: true, merge_approval: true, implement_concurrency: 1, review_concurrency: 1 },
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
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document, version: 1 } })
    if (path === '/v1/harness-templates') {
      if (templatesFail) return route.fulfill({ status: 500, body: 'catalog unavailable' })
      return route.fulfill({ json: { templates: [{ id: 'codex', label: 'Codex CLI', description: "OpenAI's coding agent", harness: codexHarness }] } })
    }
    if (path === '/v1/workers') return route.fulfill({ json: { workers: [], auto_available: false } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
}

test('harness picker inserts a suffixed template and a blank custom draft', async ({ page }) => {
  await mockAPIs(page)
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Harnesses' }).click()

  await page.getByRole('button', { name: 'Add harness' }).click()
  await expect(page.getByRole('menuitem', { name: /Codex CLI/ })).toContainText("OpenAI's coding agent")
  await expect(page.getByRole('menu')).not.toContainText('TOML')
  await page.getByRole('menuitem', { name: /Codex CLI/ }).click()
  await expect(page.getByLabel('Harness 2 name')).toHaveValue('codex-2')
  await expect(page.locator('input[aria-label="Command argv"]').locator('..').getByTitle('Edit codex')).toBeVisible()

  await page.getByRole('button', { name: 'Add harness' }).click()
  await page.getByRole('menuitem', { name: /Custom/ }).click()
  await expect(page.getByLabel('Harness 3 name')).toHaveValue('')
  await expect(page.getByLabel('MCP transport').last()).toHaveValue('json_file')
  await expect(page.locator('input[aria-label="Command argv"]').last()).toHaveValue('')
})

test('harness picker degrades to Custom when catalog fetch fails', async ({ page }) => {
  await mockAPIs(page, true)
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Add harness' }).click()
  await expect(page.getByRole('menuitem')).toHaveCount(1)
  await expect(page.getByRole('menuitem')).toContainText('Custom')
})
