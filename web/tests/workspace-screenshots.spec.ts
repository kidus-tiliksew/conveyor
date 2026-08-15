// Visual-capture harness for the workspace settings page: renders the tabs
// against mocked APIs and writes screenshots to test-results/shots/ for
// design review. Asserts nothing beyond successful interaction.
import { test, type Page, type Route } from '@playwright/test'

const baseDocument = {
  workspace: 'conveyor',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: {
      triage: { model: 'openai/gpt-5.6-luna', timeout: '20m' },
    },
    spec: { model: 'openai/gpt-5.6-luna', model_policy: 'explicit', harness: 'codex', timeout: '30m' },
    implementation: { model: 'gpt-5.6-sol', model_policy: 'explicit', harness: 'codex', effort: 'high', timeout: '2h' },
    review: { execution: 'mcp', timeout: '30m' },
  },
  routing: { stages: {} },
  harnesses: [
    {
      name: 'codex',
      mcp_transport: 'toml_override',
      command: ['codex', 'exec', '{prompt}', '--config', '{mcp_config}'],
      model_args: ['--model', '{model}'],
      effort_args: {
        medium: ['--config', 'model_reasoning_effort="medium"'],
        high: ['--config', 'model_reasoning_effort="high"'],
      },
      probe_command: ['codex', '--version'],
      probe_timeout: '10s',
    },
    {
      name: 'claude',
      mcp_transport: 'json_file',
      command: ['claude', '-p', '{prompt}', '--mcp-config', '{mcp_config}'],
      model_args: ['--model', '{model}'],
      effort_args: { medium: ['--effort', 'medium'], high: ['--effort', 'high'] },
      probe_command: ['claude', '--version'],
      probe_timeout: '10s',
    },
    {
      name: 'grok',
      mcp_transport: 'json_file',
      command: ['grok', '--single', '{prompt}', '--mcp-config', '{mcp_config}'],
      model_args: ['--model', '{model}'],
      effort_args: {
        low: ['--reasoning-effort', 'low'],
        medium: ['--reasoning-effort', 'medium'],
        high: ['--reasoning-effort', 'high'],
      },
      probe_command: ['grok', '--version'],
      probe_timeout: '10s',
    },
  ],
  review: {
    seats: [
      { model: 'gpt-5.6-sol', harness: 'codex', effort: 'medium' },
      { model: 'grok-4.5', harness: 'grok', effort: 'high' },
    ],
  },
  setups: [] as unknown[],
  default_setup: '',
  execution: {
    default_mode: 'auto',
    spec_approval: true,
    merge_approval: true,
    implement_concurrency: 5,
    review_concurrency: 2,
    first_activity_timeout: '2m',
  },
  repos: [
    {
      name: 'conveyor',
      url: 'https://github.com/kidus-tiliksew/conveyor',
      github: 'kidus-tiliksew/conveyor',
      base: 'main',
    },
  ],
}
baseDocument.setups = [
  { name: 'setup-1', execution_settings: baseDocument.execution_settings, review: baseDocument.review },
  {
    name: 'claude-heavy',
    execution_settings: {
      ...baseDocument.execution_settings,
      implementation: { model: 'claude-fable-5', model_policy: 'explicit', harness: 'claude', timeout: '2h' },
    },
    review: baseDocument.review,
  },
]
baseDocument.default_setup = 'setup-1'

async function mockAPIs(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'conveyor')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({
        json: [{ id: 'conveyor', name: 'Conveyor', config_version: 1, created_at: '2026-07-17T00:00:00Z' }],
      })
      return
    }
    if (url.pathname === '/v1/me') {
      await route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
      return
    }
    if (url.pathname === '/v1/workspace/config') {
      await route.fulfill({ json: { document: baseDocument, version: 1 } })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({
        json: {
          workers: [
            {
              id: 'wrk-1',
              workspace: 'conveyor',
              name: 'kidus-mbp',
              last_seen_at: new Date().toISOString(),
              lease_expires_at: new Date(Date.now() + 60000).toISOString(),
              created_at: '2026-07-17T00:00:00Z',
              probes: [
                { harness: 'codex', healthy: true, checked_at: new Date().toISOString() },
                { harness: 'grok', healthy: true, checked_at: new Date().toISOString() },
                {
                  harness: 'claude',
                  healthy: false,
                  message: 'claude: command not found',
                  checked_at: new Date().toISOString(),
                },
              ],
            },
          ],
          worker_expected: true,
          worker_available: true,
          setup_serviceability: {
            'setup-1': { worker_expected: true, worker_available: true },
            'claude-heavy': {
              worker_expected: true,
              worker_available: false,
              worker_unavailable_reason: 'required harness claude probe failing',
            },
          },
        },
      })
      return
    }
    if (url.pathname === '/v1/workspace') {
      await route.fulfill({
        json: { workspace: 'conveyor', max_bounces: 3, database: 'postgres', repos: [], routing: [] },
      })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test.use({ colorScheme: 'dark', viewport: { width: 1440, height: 1000 } })

test('capture workspace redesign screenshots', async ({ page }) => {
  await mockAPIs(page)
  await page.goto('/workspace')
  await page.getByText('Execution setups').waitFor()
  await page.screenshot({ path: 'test-results/shots/1-execution-collapsed.png' })

  await page.getByRole('button', { name: 'Toggle setup-1 setup' }).click()
  await page.screenshot({ path: 'test-results/shots/2-execution-expanded.png', fullPage: true })

  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Toggle codex' }).click()
  await page.getByText('Advanced', { exact: false }).first().click()
  await page.screenshot({ path: 'test-results/shots/3-harnesses.png', fullPage: true })

  // Dirty state: edit a field so the save bar appears.
  await page.getByRole('tab', { name: 'General' }).click()
  await page.getByLabel('Work-order queue timeout').fill('36h')
  await page.waitForTimeout(300)
  await page.screenshot({ path: 'test-results/shots/4-general-dirty.png' })

  await page.getByRole('tab', { name: 'Workers' }).click()
  await page.screenshot({ path: 'test-results/shots/5-workers.png' })
})
