import { expect, type Page, type Route, test } from '@playwright/test'

// Workspace verification tab (component-web-dashboard VK-WEB-4) against a
// mocked VK-11 projection (feature-verification-kit-execution v6).

const config = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}] },
  execution: {
    spec_approval: true,
    merge_approval: false,
    verify_stage: false,
    require_verification_evidence: false,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

const exercise = (overrides: Record<string, unknown> = {}) => ({
  id: 'create-and-read',
  description: 'Posts a report fixture and polls the read endpoint until it appears.',
  stages: ['verify'],
  kind: 'script',
  argv: ['./private-entrypoint.sh'],
  cwd: 'private-cwd',
  timeout_seconds: 120,
  prerequisites: [{ id: 'fixture-api', kind: 'service', environment_binding: 'fixture-api' }],
  permissions: [{ kind: 'network', target_binding: 'fixture-api' }],
  inputs: [{ name: 'private_input', type: 'string', required: true, sensitive: false }],
  required_assertions: [
    { id: 'created-record-readable', description: 'The created report is returned by GET with identical fields.' },
  ],
  retry_policy: 'reconciliation_required',
  safety_basis: '',
  operations: [{ id: 'create-record', target_binding: 'fixture-api' }],
  evidence_outputs: [{ type: 'api_exchange', schema_version: 1, minimum_items: 1 }],
  supports: [{ document_id: 'req-reporting', version: 3, acceptance_criterion_id: 'AC-1.1' }],
  ...overrides,
})

const kit = (id: string, status: string, overrides: Record<string, unknown> = {}) => ({
  id,
  name: `Kit ${id}`,
  version: '1.0.0',
  description: `Exercises the ${id} fixture.\nSecond line of detail.`,
  path: `.conveyor/kits/${id}`,
  digest: 'a'.repeat(64),
  stages: ['verify'],
  status,
  pins: [{ kind: 'requirement', document_id: 'req-reporting', version: 3, status: 'current' }],
  diagnostics: [],
  exercises: [exercise()],
  ...overrides,
})

const registry = {
  repositories: [
    {
      repository: 'reporting',
      base: 'main',
      commit_sha: 'c'.repeat(40),
      state: 'ok',
      schema_version: 2,
      diagnostics: [],
      kits: [
        kit('reporting-api', 'current', {
          name: 'Reporting API exercises',
          description: 'Creates a report through the public API and reads it back.',
          pins: [
            { kind: 'requirement', document_id: 'req-reporting', version: 3, status: 'current' },
            { kind: 'system_design', document_id: 'component-reporting', version: 2, status: 'current' },
          ],
        }),
        kit('export-csv', 'behind', {
          name: 'CSV export',
          pins: [
            { kind: 'requirement', document_id: 'req-reporting', version: 1, status: 'behind', current_version: 3 },
          ],
        }),
        kit('draft-kit', 'pending', {
          pins: [{ kind: 'requirement', document_id: 'req-reporting', version: 4, status: 'pending' }],
        }),
        kit('orphan', 'unresolved', {
          pins: [{ kind: 'requirement', document_id: 'req-retired', version: 1, status: 'unresolved' }],
        }),
        kit('scratch', 'unpinned', { pins: [] }),
        kit('broken-root', 'invalid', {
          digest: '',
          diagnostics: [{ path: 'tree.broken-root', message: 'missing exact-revision tree' }],
        }),
        kit('markup', 'current', {
          name: 'Markup literal',
          description: '<b>not bold</b> [link](https://example.com) https://example.com/raw',
        }),
      ],
    },
    {
      repository: 'legacy',
      base: 'main',
      commit_sha: 'd'.repeat(40),
      state: 'ok',
      schema_version: 1,
      diagnostics: [],
      kits: [
        kit('legacy', 'current', {
          name: 'Legacy kit',
          description: '',
          exercises: [
            exercise({ description: '', required_assertions: [{ id: 'fixture-observed', description: '' }] }),
          ],
        }),
      ],
    },
    { repository: 'docs', base: 'main', commit_sha: 'e'.repeat(40), state: 'no_manifest', diagnostics: [], kits: [] },
    {
      repository: 'future',
      base: 'main',
      commit_sha: 'f'.repeat(40),
      state: 'invalid',
      schema_version: 3,
      diagnostics: [{ path: 'manifest.schema_version', message: 'unsupported schema; expected 1 or 2' }],
      kits: [
        kit('future-kit', 'invalid', {
          diagnostics: [{ path: 'manifest.schema_version', message: 'unsupported schema; expected 1 or 2' }],
        }),
      ],
    },
    ...(['no_app', 'permission', 'unknown_revision', 'transport'] as const).map((reason) => ({
      repository: `blocked-${reason}`,
      base: 'main',
      commit_sha: '',
      state: 'unavailable',
      reason,
      diagnostics: [],
      kits: [],
    })),
  ],
}

async function mockAPIs(page: Page, role = 'operator') {
  const calls = { registry: 0 }
  let submitted: Record<string, unknown> | undefined
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: `usr_${role}`, role } })
    if (path === '/v1/workspaces/demo/verification-kits') {
      calls.registry++
      return route.fulfill({ json: registry })
    }
    if (path === '/v1/workspace/config') {
      if (route.request().method() === 'PUT') {
        submitted = route.request().postDataJSON()
        return route.fulfill({ json: { document: submitted!.document, version: 2, event_id: 7 } })
      }
      return route.fulfill({ json: { document: config, version: 1 } })
    }
    if (path === '/v1/workers') return route.fulfill({ json: { workers: [], worker_expected: false } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return { calls, submitted: () => submitted }
}

async function openVerification(page: Page) {
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Verification' }).click()
  await expect(page.getByRole('heading', { name: 'reporting' })).toBeVisible()
}

test('manager tab orders tabs, moves the evidence switch and saves both switches', async ({ page }) => {
  const api = await mockAPIs(page)
  await page.goto('/workspace')
  await expect(page.getByRole('tab')).toHaveText([/^General/, /^Policy/, /^Verification/, /^Workers/, /^Members/])
  await expect(page.getByLabel('Require verification evidence')).toHaveCount(0)

  await page.getByRole('tab', { name: 'Verification' }).click()
  await page.getByLabel('Verify before review').click()
  await page.getByLabel('Require verification evidence').click()
  await expect(page.getByRole('tab', { name: 'Verification' }).getByTitle('Unsaved changes')).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Policy' }).getByTitle('Unsaved changes')).toHaveCount(0)
  const registryCalls = api.calls.registry
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByText('Recorded config.updated event 7')).toBeVisible()
  const execution = (api.submitted()?.document as typeof config).execution
  expect(execution.verify_stage).toBe(true)
  expect(execution.require_verification_evidence).toBe(true)
  expect(api.calls.registry).toBe(registryCalls)
})

test('repository sections render every state, reason and status chip', async ({ page }) => {
  await mockAPIs(page)
  await openVerification(page)
  const reporting = page.getByRole('region', { name: 'reporting' })
  await expect(reporting).toContainText(
    '7 kits · 1 behind · 1 awaiting confirmation · 1 unresolved · 1 without pins · 1 invalid',
  )
  await expect(reporting.getByTitle('c'.repeat(40))).toHaveText('@ cccccccc')
  for (const label of ['Current', 'Behind', 'Awaiting confirmation', 'Unresolved pin', 'No pins', 'Invalid']) {
    await expect(reporting.getByText(label, { exact: true }).first()).toBeVisible()
  }
  await expect(page.getByRole('region', { name: 'docs' })).toContainText(
    'No kits declared in .conveyor/kits/manifest.yaml',
  )
  await expect(page.getByRole('region', { name: 'future' })).toContainText('manifest.schema_version')
  await expect(page.getByRole('region', { name: 'future' })).toContainText('Kit future-kit')
  const noApp = page.getByRole('region', { name: 'blocked-no_app' })
  await expect(noApp).toContainText("Couldn't read the kit manifest")
  await expect(noApp.getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings')
  await expect(page.getByRole('region', { name: 'blocked-permission' })).toContainText('cannot read this repository')
  await expect(page.getByRole('region', { name: 'blocked-unknown_revision' })).toContainText(
    'base branch was not found',
  )
  await expect(page.getByRole('region', { name: 'blocked-transport' })).toContainText('GitHub read failed')
})

test('kit, exercise and run details expand without exposing hidden run fields', async ({ page }) => {
  await mockAPIs(page)
  await openVerification(page)
  const reporting = page.getByRole('region', { name: 'reporting' })
  const kitButton = reporting.getByRole('button', { name: 'Reporting API exercises 1.0.0' })
  await expect(kitButton).toHaveAttribute('aria-expanded', 'false')
  await kitButton.click()
  await expect(kitButton).toHaveAttribute('aria-expanded', 'true')
  await expect(reporting.getByRole('link', { name: 'req-reporting' }).first()).toHaveAttribute(
    'href',
    '/requirements?requirement=req-reporting',
  )
  await expect(reporting.getByRole('link', { name: 'component-reporting' })).toHaveAttribute(
    'href',
    '/system-design?document=component-reporting',
  )
  const exerciseButton = reporting.getByRole('button', { name: /create-and-read/ }).first()
  await exerciseButton.click()
  await expect(
    reporting.getByText('The created report is returned by GET with identical fields.').first(),
  ).toBeVisible()
  await expect(reporting.getByRole('link', { name: 'AC-1.1' }).first()).toBeVisible()
  await reporting.getByRole('button', { name: 'Run details' }).first().click()
  await expect(reporting.getByText('fixture-api (service)').first()).toBeVisible()
  await expect(reporting.getByText('reconciliation required').first()).toBeVisible()
  await expect(reporting.getByText('api exchange ×1').first()).toBeVisible()
  const body = await page.locator('body').innerText()
  for (const hidden of ['private-entrypoint', 'private-cwd', 'private_input', '120']) {
    expect(body).not.toContain(hidden)
  }

  const behind = reporting.getByRole('button', { name: 'CSV export 1.0.0' })
  await behind.click()
  await expect(reporting.getByText('v1 · current is v3')).toBeVisible()
  await reporting.getByRole('button', { name: 'Kit broken-root 1.0.0' }).click()
  await expect(reporting.getByText('missing exact-revision tree')).toBeVisible()
})

test('descriptions render literally and schema-1 assertions show IDs alone', async ({ page }) => {
  await mockAPIs(page)
  await openVerification(page)
  const reporting = page.getByRole('region', { name: 'reporting' })
  await reporting.getByRole('button', { name: 'Markup literal 1.0.0' }).click()
  const literal = '<b>not bold</b> [link](https://example.com) https://example.com/raw'
  await expect(reporting.getByText(literal).last()).toBeVisible()
  await expect(reporting.locator('b', { hasText: 'not bold' })).toHaveCount(0)
  await expect(reporting.getByRole('link', { name: 'link' })).toHaveCount(0)

  const legacy = page.getByRole('region', { name: 'legacy' })
  await legacy.getByRole('button', { name: 'Legacy kit 1.0.0' }).click()
  await legacy.getByRole('button', { name: /create-and-read/ }).click()
  await expect(legacy.getByText('fixture-observed')).toBeVisible()
})

test('members see the kit list without verification switches', async ({ page }) => {
  await mockAPIs(page, 'viewer')
  await page.goto('/workspace')
  await expect(page.getByRole('heading', { name: 'reporting' })).toBeVisible()
  await expect(page.getByRole('tab')).toHaveCount(0)
  await expect(page.getByLabel('Verify before review')).toHaveCount(0)
  await expect(page.getByLabel('Require verification evidence')).toHaveCount(0)
})

test('member view screenshot', async ({ page }) => {
  await mockAPIs(page, 'viewer')
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/workspace')
  await expect(page.getByRole('heading', { name: 'reporting' })).toBeVisible()
  await page.waitForTimeout(400)
  await page.screenshot({ path: 'test-results/shots/verification-member.png', fullPage: true })
})

test('narrow layout stacks kit rows without horizontal overflow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockAPIs(page)
  await openVerification(page)
  await page.getByRole('button', { name: 'Reporting API exercises 1.0.0' }).click()
  await page
    .getByRole('button', { name: /create-and-read/ })
    .first()
    .click()
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(overflow).toBeLessThanOrEqual(0)
})

test('operator review screenshots', async ({ page }) => {
  await mockAPIs(page)
  await page.setViewportSize({ width: 1280, height: 2300 })
  await openVerification(page)
  await page.mouse.move(0, 0)
  await page.waitForTimeout(400)
  await page.screenshot({ path: 'test-results/shots/verification-tab.png', fullPage: true })
  await page.setViewportSize({ width: 1280, height: 900 })
  const reporting = page.getByRole('region', { name: 'reporting' })
  await reporting.getByRole('button', { name: 'Reporting API exercises 1.0.0' }).click()
  await reporting
    .getByRole('button', { name: /create-and-read/ })
    .first()
    .click()
  await reporting.getByRole('button', { name: 'Run details' }).first().click()
  await page.mouse.move(0, 0)
  await page.waitForTimeout(400)
  await reporting.screenshot({ path: 'test-results/shots/verification-kit-expanded.png' })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(400)
  await page.screenshot({ path: 'test-results/shots/verification-narrow.png', fullPage: true })
})
