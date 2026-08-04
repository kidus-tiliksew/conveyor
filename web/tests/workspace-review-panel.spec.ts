import { expect, test, type Page, type Route } from '@playwright/test'

const baseDocument = {
  workspace: 'demo',
  max_bounces: 2,
  work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: {
      triage: { model: 'gpt', effort: 'low', timeout: '20m' },
    },
    spec: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '30m' },
    implementation: { model: 'gpt-implement', model_policy: 'explicit', harness: 'codex', timeout: '4h' },
    review: { execution: 'mcp', timeout: '1h', fallback_model: 'fallback', fallback_harness: 'codex' },
  },
  routing: {
    stages: {
      triage: { model: 'gpt', timeout: '20m', execution: 'in_process' },
      spec: { model: 'gpt', timeout: '30m', execution: 'in_process' },
      implement: { model: 'gpt-implement', harness: 'codex', timeout: '4h', execution: 'mcp' },
      review: { model: 'fallback', harness: 'codex', timeout: '1h', execution: 'mcp' },
    },
  },
  harnesses: [
    {
      name: 'codex',
      mcp_transport: 'toml_override',
      command: ['codex', '{prompt}', '{mcp_config}'],
      model_args: ['--model', '{model}'],
      effort_args: { high: ['--config', 'model_reasoning_effort="high"'] },
      probe_command: ['codex', '--version'],
      probe_timeout: '5s',
      stall_timeout: '10m',
    },
    {
      name: 'claude',
      mcp_transport: 'json_file',
      command: ['claude', '{prompt}', '{mcp_config}'],
      model_args: ['--model', '{model}'],
      effort_args: { high: ['--effort', 'high'] },
      probe_command: ['claude', '--version'],
      probe_timeout: '5s',
      stall_timeout: '10m',
    },
  ],
  review: { seats: [{ model: 'gpt-review' }, { model: 'claude-review', harness: 'claude', effort: 'high' }] },
  execution: {
    default_mode: 'manual',
    spec_approval: true,
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [{ name: 'conveyor', url: 'https://example.test/conveyor', base: 'main' }],
}

async function mockWorkspaceAPIs(page: Page, initialDocument = baseDocument) {
  let document = structuredClone(initialDocument)
  let version = 1
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({
        json: [{ id: 'demo', name: 'Demo', config_version: version, created_at: '2026-07-17T00:00:00Z' }],
      })
      return
    }
    if (url.pathname === '/v1/workspace/config') {
      if (route.request().method() === 'PUT') {
        const body = route.request().postDataJSON() as { document: typeof document }
        if (body.document.review.seats.some((seat) => seat.model === 'invalid-model')) {
          await route.fulfill({
            status: 422,
            json: {
              error: 'validation_failed',
              fields: [{ field: 'review.seats[1].model', message: 'model is unavailable' }],
            },
          })
          return
        }
        document = structuredClone(body.document)
        version++
        await route.fulfill({
          json: { document, version, event_id: 42, actor_id: 'dashboard-operator', sections: ['review'] },
        })
        return
      }
      await route.fulfill({ headers: { ETag: `"${version}"` }, json: { document, version } })
      return
    }
    if (url.pathname === '/v1/harness-templates') {
      await route.fulfill({
        json: {
          templates: [
            {
              id: 'codex',
              label: 'Codex CLI',
              description: "OpenAI's coding agent",
              harness: {
                name: 'codex',
                mcp_transport: 'toml_override',
                command: ['codex', 'exec', '{prompt}', '--config', '{mcp_config}'],
                model_args: ['--model', '{model}'],
                probe_command: ['codex', '--version'],
                probe_timeout: '10s',
                stall_timeout: '10m',
              },
            },
          ],
        },
      })
      return
    }
    if (url.pathname === '/v1/workers') {
      await route.fulfill({
        json: {
          workers: [],
          auto_available: false,
          auto_unavailable_reason: 'manual test',
          rate_limits: [
            {
              work_order_id: 'order-1',
              harness: 'codex',
              model: 'gpt-5',
              rate_limit: { status: 'limited', limit: 1000, remaining: 125, reset_at: '2026-07-28T12:00:00Z' },
              observed_at: '2026-07-28T11:55:00Z',
            },
          ],
        },
      })
      return
    }
    if (url.pathname === '/v1/workspace') {
      await route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [], routing: [] } })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test('workspace renders harness stall configuration and observational rate-limit health', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Toggle codex' }).click()
  await expect(page.getByLabel('Stall timeout')).toHaveValue('10m')

  await page.getByRole('tab', { name: 'Workers' }).click()
  await expect(page.getByText('Provider rate limits')).toBeVisible()
  await expect(page.getByText('codex / gpt-5')).toBeVisible()
  await expect(page.getByText('125 of 1000 remaining')).toBeVisible()
  await expect(page.getByText(/does not use it to gate or route work/)).toBeVisible()
})

async function expandDefaultSetup(page: Page) {
  await page.getByRole('button', { name: 'Toggle default setup' }).click()
}

test('review panel keeps rejected values and reflects a saved panel after reload', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  // Setups are collapsed by default: seat editors only appear after expanding.
  await expect(page.getByText('2 seats')).toBeVisible()
  await expect(page.getByLabel('Pinned model')).toHaveCount(0)
  await expandDefaultSetup(page)
  const models = page.getByLabel('Pinned model')
  await expect(models).toHaveCount(2)
  await expect(models.nth(0)).toHaveValue('gpt-review')
  await expect(models.nth(1)).toHaveValue('claude-review')

  await models.nth(1).fill('invalid-model')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText('review.seats[1].model: model is unavailable')).toBeVisible()
  await expect(models.nth(1)).toHaveValue('invalid-model')

  await models.nth(1).fill('claude-review-v2')
  await page.getByLabel('Seat 2 harness').selectOption('claude')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated event 42/)).toBeVisible()

  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('Pinned model').nth(1)).toHaveValue('claude-review-v2')
  await expect(page.getByLabel('Seat 2 harness')).toHaveValue('claude')
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Seat 2 reasoning effort')).toHaveValue('high')

  await page.getByLabel('Seat 1 reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('high')

  await page.getByLabel('Seat 1 reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('Seat 1 reasoning effort')).toHaveValue('')
})

test('workspace renders contextual execution settings without generic routing controls', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expandDefaultSetup(page)
  await expect(page.getByLabel('triage model')).toHaveValue('gpt')
  await expect(page.getByLabel('triage reasoning effort')).toHaveValue('low')
  await expect(page.getByLabel('planning context artifact references')).toHaveValue('64')
  await expect(page.getByLabel('served requirement authority nodes')).toHaveValue('256')
  await expect(page.getByLabel('served requirement authority nodes')).toHaveAttribute('min', '8')
  await expect(page.getByLabel('spec reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('triage reasoning effort').locator('option')).toHaveText([
    'Provider default',
    'minimal',
    'low',
    'medium',
    'high',
  ])
  await page.getByLabel('spec reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('spec reasoning effort')).toHaveValue('high')
  await page.getByLabel('triage reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('triage reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Implementation harness')).toHaveValue('codex')
  await expect(page.getByLabel('Implementation model policy')).toHaveValue('explicit')
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('')
  await page.getByLabel('Implementation reasoning effort').selectOption('high')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('high')
  await page.getByLabel('Implementation reasoning effort').selectOption('')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await expandDefaultSetup(page)
  await expect(page.getByLabel('Implementation reasoning effort')).toHaveValue('')
  await expect(page.getByLabel('Review execution')).toHaveValue('mcp')
  await expect(page.getByLabel('Review timeout')).toHaveValue('1h')

  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Toggle codex' }).click()
  await expect(page.getByLabel('MCP transport')).toHaveValue('toml_override')
  await page.getByRole('button', { name: 'Toggle codex' }).click()
  await page.getByRole('button', { name: 'Toggle claude' }).click()
  await expect(page.getByLabel('MCP transport')).toHaveValue('json_file')
  await page.getByRole('button', { name: 'Toggle claude' }).click()

  await page.getByRole('button', { name: 'Add harness' }).click()
  await page.getByRole('menuitem', { name: /Codex CLI/ }).click()
  await expect(page.getByLabel('MCP transport')).toHaveValue('toml_override')
  await expect(page.getByRole('button', { name: 'Remove exec from Command argv', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove {mcp_config} from Command argv', exact: true })).toBeVisible()
  await page.getByLabel('MCP transport').selectOption('environment')
  await expect(page.getByLabel('MCP attachment for codex-2')).toHaveValue('conveyor')
  await expect(page.getByText(/forbids \{mcp_config\}/)).toBeVisible()
  await page.getByLabel('MCP attachment for codex-2').fill('conveyor-build')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()
  await page.getByRole('tab', { name: 'Harnesses' }).click()
  await page.getByRole('button', { name: 'Toggle codex-2' }).click()
  await expect(page.getByLabel('MCP transport')).toHaveValue('environment')
  await expect(page.getByLabel('MCP attachment for codex-2')).toHaveValue('conveyor-build')
  await expect(page.getByText('Stage routing')).toHaveCount(0)
  await expect(page.getByText('Inherit single harness')).toHaveCount(0)
})

test('fully explicit review seats remove fallback requirements', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expandDefaultSetup(page)
  await page.getByLabel('Seat 1 harness').selectOption('codex')
  await expect(page.getByText(/Every seat is explicitly routed/)).toBeVisible()
  await expect(page.getByLabel('Review fallback harness')).toHaveCount(0)
})

test('legacy null review seats are normalized before setup rendering', async ({ page }) => {
  const legacyDocument = {
    ...structuredClone(baseDocument),
    review: { seats: null },
    setups: [
      {
        name: 'default',
        execution_settings: structuredClone(baseDocument.execution_settings),
        review: { seats: null },
        refresh_review: 'delta',
      },
    ],
    default_setup: 'default',
  } as unknown as typeof baseDocument
  await mockWorkspaceAPIs(page, legacyDocument)
  await page.goto('/workspace')

  await expect(page.getByText('1 seat')).toBeVisible()
  await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('workspace manages named execution setups without losing their contracts', async ({ page }) => {
  await mockWorkspaceAPIs(page)
  await page.goto('/workspace')

  await expect(page.getByText('Execution setups')).toBeVisible()
  await page.getByRole('button', { name: 'New setup' }).click()
  await page.getByLabel('Setup name').fill('frontend')
  await page.getByRole('button', { name: 'Duplicate frontend' }).click()
  await expect(page.getByRole('button', { name: 'Toggle frontend-copy setup' })).toBeVisible()
  await page.getByRole('button', { name: 'Set frontend-copy as default' }).click()
  await expandDefaultSetup(page)
  await page.getByRole('button', { name: 'Delete default' }).click()
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText(/Recorded config.updated/)).toBeVisible()
  await page.reload()

  await expect(page.getByRole('button', { name: /Toggle .* setup/ })).toHaveCount(2)
  await expect(page.getByRole('button', { name: 'Toggle frontend-copy setup' })).toContainText('Default')
  await page.getByRole('button', { name: 'Toggle frontend-copy setup' }).click()
  await expect(page.getByLabel('Implementation harness')).toHaveValue('codex')
})
