import { expect, test, type Page, type Route } from '@playwright/test'

const baseDocument = {
  workspace: 'demo',
  max_bounces: 2,
  work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: {
      triage: { model: 'gpt', effort: 'low', timeout: '20m' },
      spec: { model: 'gpt', timeout: '30m' },
    },
    implementation: { model: 'gpt-implement', model_policy: 'explicit', harness: 'codex', timeout: '4h' },
    review: { execution: 'mcp', timeout: '1h', fallback_model: 'fallback', fallback_harness: 'codex' },
  },
  routing: { stages: {
    triage: { model: 'gpt', timeout: '20m', execution: 'in_process' },
    spec: { model: 'gpt', timeout: '30m', execution: 'in_process' },
    implement: { model: 'gpt-implement', harness: 'codex', timeout: '4h', execution: 'mcp' },
    review: { model: 'fallback', harness: 'codex', timeout: '1h', execution: 'mcp' },
  } },
  harnesses: [
    { name: 'codex', mcp_transport: 'toml_override', command: ['codex', '{prompt}', '{mcp_config}'], model_args: ['--model', '{model}'], effort_args: { high: ['--config', 'model_reasoning_effort="high"'] }, probe_command: ['codex', '--version'], probe_timeout: '5s' },
    { name: 'claude', mcp_transport: 'json_file', command: ['claude', '{prompt}', '{mcp_config}'], model_args: ['--model', '{model}'], effort_args: { high: ['--effort', 'high'] }, probe_command: ['claude', '--version'], probe_timeout: '5s' },
  ],
  review: { seats: [{ model: 'gpt-review' }, { model: 'claude-review', harness: 'claude', effort: 'high' }] },
  execution: { default_mode: 'manual', spec_approval: true, merge_approval: true, implement_concurrency: 1, review_concurrency: 1 },
  repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }],
}

async function mockWorkspaceAPIs(page: Page) {
  let document = structuredClone(baseDocument)
  let version = 1
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: version, created_at: '2026-07-17T00:00:00Z' }] })
      return
    }
    if (url.pathname === '/v1/workspace/config') {
      if (route.request().method() === 'PUT') {
        const body = route.request().postDataJSON() as { document: typeof document }
        if (body.document.review.seats.some((seat) => seat.model === 'invalid-model')) {
          await route.fulfill({ status: 422, json: { error: 'validation_failed', fields: [{ field: 'review.seats[1].model', message: 'model is unavailable' }] } })
          return
        }
        document = structuredClone(body.document)
        version++
        await route.fulfill({ json: { document, version, event_id: 42, actor_id: 'dashboard-operator', sections: ['review'] } })
        return
      }
      await route.fulfill({ headers: { ETag: `"${version}"` }, json: { document, version } })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({ json: { workers: [], auto_available: false, auto_unavailable_reason: 'manual test' } })
      return
    }
    if (url.pathname === '/v1/workspace') {
      await route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [], routing: [] } })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test('review panel keeps rejected values and reflects a saved panel after reload', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expect(page.getByText('2 configured seats')).toBeVisible()
  const models = page.getByLabel('Pinned model')
  await expect(models).toHaveCount(2)
  await expect(models.nth(0)).toHaveValue('gpt-review')
  await expect(models.nth(1)).toHaveValue('claude-review')

  await models.nth(1).fill('invalid-model')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('review.seats[1].model: model is unavailable')).toBeVisible()
  await expect(models.nth(1)).toHaveValue('invalid-model')

  await models.nth(1).fill('claude-review-v2')
  await page.getByLabel('Seat 2 harness').selectOption('claude')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()

  await page.reload()
  await expect(page.getByText('2 configured seats')).toBeVisible()
  await expect(page.getByLabel('Pinned model').nth(1)).toHaveValue('claude-review-v2')
  await expect(page.getByLabel('Seat 2 harness')).toHaveValue('claude')
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Seat 2 reasoning effort')).toHaveValue('high')

  await page.getByLabel('Seat 1 reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('high')

  await page.getByLabel('Seat 1 reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('')
})

test('workspace renders contextual execution settings without generic routing controls', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expect(page.getByText('Advanced control-plane settings')).toBeVisible()
  await expect(page.getByLabel('triage model')).toHaveValue('gpt')
  await expect(page.getByLabel('triage reasoning effort')).toHaveValue('low')
  await expect(page.getByLabel('spec reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('triage reasoning effort').locator('option')).toHaveText(['Provider default', 'minimal', 'low', 'medium', 'high'])
  await page.getByLabel('spec reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('spec reasoning effort')).toHaveValue('high')
  await page.getByLabel('triage reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('triage reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Implementation harness')).toHaveValue('codex')
  await expect(page.getByLabel('Implementation model policy')).toHaveValue('explicit')
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('')
  await page.getByLabel('Implementation reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('high')
  await page.getByLabel('Implementation reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Review execution')).toHaveValue('mcp')
  await expect(page.getByLabel('Review timeout')).toHaveValue('1h')
  await expect(page.getByLabel('MCP transport')).toHaveCount(2)
  await expect(page.getByLabel('MCP transport').nth(0)).toHaveValue('toml_override')
  await expect(page.getByLabel('MCP transport').nth(1)).toHaveValue('json_file')
  await page.getByRole('button', { name: 'Add' }).first().click()
  await expect(page.getByLabel('MCP transport').nth(2)).toHaveValue('toml_override')
  await expect(page.getByLabel('Command argv').nth(2)).toHaveValue('["codex","exec","{prompt}","--config","{mcp_config}"]')
  await page.getByLabel('MCP transport').nth(2).selectOption('environment')
  await expect(page.getByLabel('MCP attachment for harness 3')).toHaveValue('conveyor')
  await expect(page.getByText(/forbids \{mcp_config\}/)).toBeVisible()
  await page.getByLabel('MCP attachment for harness 3').fill('conveyor-build')
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()
  await expect(page.getByLabel('MCP transport').nth(2)).toHaveValue('environment')
  await expect(page.getByLabel('MCP attachment for harness 3')).toHaveValue('conveyor-build')
  await expect(page.getByText('Stage routing')).toHaveCount(0)
  await expect(page.getByText('Inherit single harness')).toHaveCount(0)
})

test('fully explicit review seats remove fallback requirements', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await page.getByLabel('Seat 1 harness').selectOption('codex')
  await expect(page.getByText(/Every seat is explicitly routed/)).toBeVisible()
  await expect(page.getByLabel('Review fallback harness')).toHaveCount(0)
})

test('workspace manages named execution setups without losing their contracts', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expect(page.getByText('Execution setups')).toBeVisible()
  await page.getByRole('button', { name: 'Create' }).click()
  await page.getByLabel('Name').first().fill('frontend')
  await page.getByRole('button', { name: 'Duplicate' }).click()
  await expect(page.getByLabel('Selected setup')).toHaveValue('frontend-copy')
  await page.getByRole('button', { name: 'Set default' }).click()
  await page.getByLabel('Selected setup').selectOption('default')
  await page.getByRole('button', { name: 'Delete' }).last().click()
  await page.getByRole('button', { name: 'Save' }).click()
  await page.reload()

  await expect(page.getByLabel('Selected setup')).toHaveValue('frontend-copy')
  await expect(page.getByLabel('Selected setup').locator('option')).toHaveCount(2)
  await expect(page.getByLabel('Implementation harness')).toHaveValue('codex')
})
