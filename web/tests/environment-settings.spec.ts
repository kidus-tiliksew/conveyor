// The Environments tab of the Workspace page (req-environments AC-1.1 to
// AC-1.6, AC-1.8, AC-1.9, AC-2.1 to AC-2.3; req-production-feedback AC-2.3;
// component-environments invariant 18; component-production-feedback
// invariant 21). A fake server answers in the shapes internal/httpapi
// environment.go and feedback_signals.go write: environment rows with keys
// and listening state, the 201 issue answer that alone holds a key value, and
// `invalid_environment` refusals that name a field. Every value is synthetic.
// A mocked answer proves the page, not the server contract; the Go handler
// tests prove the server.
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Page, type Request, type Route, test } from '@playwright/test'
import type { EnvironmentKey, EnvironmentRow, EnvironmentSuggestion } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'

const configDocument = {
  workspace: 'demo',
  max_bounces: 3,
  work_order_queue_timeout: '24h',
  stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
  review: { seats: [{}] },
  execution: { default_plan_state: 'needed', merge_approval: true },
  repos: [
    { name: 'conveyor', github: 'acme/conveyor', base: 'main' },
    { name: 'web-app', github: 'acme/web-app', base: 'main' },
  ],
  monitor: { enabled: false, repositories: [], poll_interval: '1m', startup_window: '24h' },
}

function key(id: string, label: string, overrides: Partial<EnvironmentKey> = {}): EnvironmentKey {
  return {
    id,
    label,
    created_by: 'usr_operator',
    created_at: '2026-11-20T09:00:00Z',
    last_used_at: null,
    ...overrides,
  }
}

function environment(id: string, name: string, overrides: Partial<EnvironmentRow> = {}): EnvironmentRow {
  return {
    id,
    name,
    tracked_branches: {},
    github_environments: [],
    version_rule: null,
    timing_window_minutes: 60,
    archived: false,
    created_by: 'usr_operator',
    created_at: '2026-11-01T09:00:00Z',
    keys: [],
    listening: false,
    last_signal_at: null,
    newest_unresolved: null,
    ...overrides,
  }
}

const production = () =>
  environment('env-production', 'production', {
    tracked_branches: { conveyor: 'main' },
    github_environments: ['production'],
    version_rule: { kind: 'tag_template', text: 'v{version}' },
    keys: [key('envkey-prod', 'prod watcher', { last_used_at: '2026-11-26T09:58:00Z' })],
    listening: true,
    last_signal_at: '2026-11-26T09:58:00Z',
    newest_unresolved: {
      service: 'api',
      repository: 'conveyor',
      version: '2026.11.3',
      deployed_at: '2026-11-26T09:30:00Z',
      reason: 'no tag matches the version rule',
    },
  })

const staging = () =>
  environment('env-staging', 'staging', {
    tracked_branches: { conveyor: 'develop', 'web-app': 'develop' },
    github_environments: ['staging', 'preview'],
    timing_window_minutes: 30,
    listening: false,
    last_signal_at: '2026-11-25T18:00:00Z',
  })

const qa = () => environment('env-qa', 'qa', { listening: false, last_signal_at: null })

const legacy = () => environment('env-legacy', 'legacy', { archived: true, github_environments: ['old-prod'] })

const canary: EnvironmentSuggestion = {
  github_environment: 'canary',
  repository: 'web-app',
  last_reported_at: '2026-11-26T08:00:00Z',
}

const issuedValue = 'cv_env_synthetic-one-time-value-0123456789'

type Reply = { status: number; json?: unknown } | undefined
interface ServerOptions {
  role?: 'viewer' | 'operator'
  active?: EnvironmentRow[]
  archived?: EnvironmentRow[]
  suggestions?: EnvironmentSuggestion[]
}

// The fake server. `reply` answers one write in place of the default, so a
// spec can refuse a request or hold it.
async function mockServer(page: Page, options: ServerOptions = {}) {
  const state = {
    active: options.active ?? [production(), staging(), qa()],
    archived: options.archived ?? [legacy()],
    suggestions: options.suggestions ?? [canary],
    gets: [] as string[],
    writes: [] as Array<{ method: string; path: string; body: unknown }>,
    reply: undefined as ((request: Request) => Promise<Reply> | Reply) | undefined,
  }
  const role = options.role ?? 'operator'
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    localStorage.setItem('conveyor-theme', 'light')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const method = request.method()
    if (method === 'GET') state.gets.push(`${path}${url.search}`)
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: callerIdentity({ id: `usr_${role}`, role }, request) })
    if (path === '/v1/workspace')
      return route.fulfill({
        json: { workspace: 'demo', max_bounces: 3, database: 'postgres', repos: configDocument.repos },
      })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document: configDocument, version: 1 } })
    if (path === '/v1/workspaces/demo/verification-kits') return route.fulfill({ json: { repositories: [] } })
    if (path.startsWith('/v1/environments') && method !== 'GET') {
      const body = request.postData() ? request.postDataJSON() : undefined
      state.writes.push({ method, path, body })
      const reply = await state.reply?.(request)
      if (reply) return route.fulfill(reply)
      if (path === '/v1/environments')
        return route.fulfill({ status: 201, json: environment('env-new', (body as { name: string }).name) })
      if (path === '/v1/environments/suggestions/ignore') {
        state.suggestions = state.suggestions.filter(
          (item) => item.github_environment !== (body as { github_environment: string }).github_environment,
        )
        return route.fulfill({ status: 204 })
      }
      const keys = path.match(/^\/v1\/environments\/([^/]+)\/keys(?:\/([^/]+))?$/)
      if (keys && method === 'POST') {
        const target = state.active.find((row) => row.id === keys[1])
        const issued = key('envkey-new', (body as { label: string }).label)
        target?.keys?.push(issued)
        return route.fulfill({ status: 201, json: { ...issued, value: issuedValue } })
      }
      if (keys && method === 'DELETE') {
        const target = state.active.find((row) => row.id === keys[1])
        if (target) target.keys = (target.keys ?? []).filter((item) => item.id !== keys[2])
        return route.fulfill({ status: 204 })
      }
      const one = path.match(/^\/v1\/environments\/([^/]+)$/)
      if (one && method === 'PATCH') {
        const patch = body as Partial<EnvironmentRow>
        const all = [...state.active, ...state.archived]
        const target = all.find((row) => row.id === one[1])
        if (!target) return route.fulfill({ status: 404, json: { error: 'environment_not_found' } })
        Object.assign(target, patch)
        state.active = all.filter((row) => !row.archived)
        state.archived = all.filter((row) => row.archived)
        return route.fulfill({ json: target })
      }
    }
    if (path === '/v1/environments/suggestions') return route.fulfill({ json: state.suggestions })
    if (path === '/v1/environments')
      return route.fulfill({ json: url.searchParams.get('archived') === 'true' ? state.archived : state.active })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })
  return state
}

const activeReads = (state: { gets: string[] }) =>
  state.gets.filter((path) => path.startsWith('/v1/environments?archived=false')).length

async function openEnvironmentsTab(page: Page) {
  await page.goto('/w/demo/workspace')
  await page.getByRole('tab', { name: 'Environments' }).click()
  await expect(page.locator('[data-environment="env-production"]')).toBeVisible()
}

const card = (page: Page, id: string) => page.locator(`[data-environment="${id}"]`)

test('a viewer reads each environment with its keys and listening state, without controls', async ({ page }) => {
  const state = await mockServer(page, { role: 'viewer' })
  await page.goto('/w/demo/workspace')

  // The read-only view has no tab strip, and the panel sits in it.
  await expect(page.getByRole('tab', { name: 'Environments' })).toHaveCount(0)
  const prod = card(page, 'env-production')
  await expect(prod).toBeVisible()
  await expect(prod).toContainText('conveyor: main')
  await expect(prod).toContainText('production')
  await expect(prod).toContainText('Tag template: v{version}')
  await expect(prod).toContainText('60 minutes')
  await expect(prod.locator('[data-watcher-state="listening"]')).toContainText('A watcher is listening.')
  await expect(prod.locator('[data-newest-unresolved]')).toContainText('api: 2026.11.3')
  await expect(prod.locator('[data-newest-unresolved]')).toContainText('Reason: no tag matches the version rule')
  const prodKey = prod.locator('[data-environment-key="envkey-prod"]')
  await expect(prodKey).toContainText('prod watcher')
  await expect(prodKey).toContainText('Created by usr_operator')
  await expect(prodKey).toContainText('Last used')
  await expect(prodKey.locator('time').first()).toHaveAttribute('datetime', '2026-11-20T09:00:00Z')

  const stage = card(page, 'env-staging')
  await expect(stage).toContainText('conveyor: develop')
  await expect(stage).toContainText('web-app: develop')
  await expect(stage).toContainText('staging, preview')
  await expect(stage).toContainText('30 minutes')
  await expect(stage.locator('[data-watcher-state="not-listening"]')).toContainText('No watcher is listening.')
  await expect(stage.locator('[data-watcher-state] time')).toHaveAttribute('datetime', '2026-11-25T18:00:00Z')
  await expect(card(page, 'env-qa')).toContainText('No watcher is listening.')
  await expect(card(page, 'env-qa')).toContainText('No signal has arrived.')

  for (const name of ['Add environment', 'Issue key', 'Edit', 'Archive', 'Revoke']) {
    await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0)
  }
  // Suggestions are for managers only and appear on no other page.
  expect(state.gets.some((path) => path.startsWith('/v1/environments/suggestions'))).toBe(false)
  expect(state.gets.some((path) => path.includes('archived=true'))).toBe(false)

  await page.getByRole('button', { name: 'Show archived environments' }).click()
  const archived = page.getByRole('region', { name: 'Archived environments' })
  await expect(archived.locator('[data-environment="env-legacy"]')).toContainText('Archived')
  await expect(archived.getByRole('button', { name: /Restore/ })).toHaveCount(0)
  expect(state.gets.filter((path) => path.includes('archived=true'))).toHaveLength(1)
})

test('a manager creates an environment with one branch field for each repository', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  const before = activeReads(state)

  await page.getByRole('button', { name: 'Add environment' }).click()
  const form = page.getByRole('form', { name: 'Add environment' })
  await expect(form.getByLabel('conveyor', { exact: true })).toHaveValue('')
  await expect(form.getByLabel('web-app', { exact: true })).toHaveValue('')
  await form.getByLabel('Name', { exact: true }).fill('eu-staging')
  await form.getByLabel('web-app', { exact: true }).fill('release')
  await form.getByLabel('GitHub environment names').fill('eu-staging\n  eu-preview  \n')
  await form.getByLabel('Version rule kind').selectOption('commit_pattern')
  await form.getByLabel('Version rule text').fill('build-{commit}')
  await form.getByLabel('Timing window in minutes').fill('45')
  await form.getByRole('button', { name: 'Add environment' }).click()

  await expect(form).toHaveCount(0)
  expect(state.writes).toEqual([
    {
      method: 'POST',
      path: '/v1/environments',
      body: {
        name: 'eu-staging',
        tracked_branches: { 'web-app': 'release' },
        github_environments: ['eu-staging', 'eu-preview'],
        version_rule: { kind: 'commit_pattern', text: 'build-{commit}' },
        timing_window_minutes: 45,
      },
    },
  ])
  await expect.poll(() => activeReads(state)).toBeGreaterThan(before)
  // The configuration draft did not change.
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)
})

test('each refusal shows beside the field the server names and keeps the typed values', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  await page.getByRole('button', { name: 'Add environment' }).click()
  const form = page.getByRole('form', { name: 'Add environment' })
  await form.getByLabel('Name', { exact: true }).fill('Production')
  await form.getByLabel('conveyor', { exact: true }).fill('main')
  await form.getByLabel('GitHub environment names').fill('production')
  await form.getByLabel('Version rule kind').selectOption('tag_template')
  await form.getByLabel('Version rule text').fill('release')
  await form.getByLabel('Timing window in minutes').fill('5000')

  const controls = {
    name: form.getByLabel('Name', { exact: true }),
    tracked_branches: form.getByLabel('conveyor', { exact: true }),
    github_environments: form.getByLabel('GitHub environment names'),
    version_rule: form.getByLabel('Version rule text'),
    timing_window_minutes: form.getByLabel('Timing window in minutes'),
  }
  for (const [field, control] of Object.entries(controls)) {
    const message = `${field} is refused by the server`
    state.reply = () => ({ status: 400, json: { error: 'invalid_environment', field, message } })
    await form.getByRole('button', { name: 'Add environment' }).click()
    await expect(form.getByRole('alert')).toHaveText(message)
    await expect(control).toHaveAttribute('aria-invalid', 'true')
    await expect(form.getByRole('alert')).toHaveAttribute('id', /.+/)
  }
  // Every typed value stays after the refusals.
  await expect(controls.name).toHaveValue('Production')
  await expect(controls.tracked_branches).toHaveValue('main')
  await expect(controls.github_environments).toHaveValue('production')
  await expect(controls.version_rule).toHaveValue('release')
  await expect(controls.timing_window_minutes).toHaveValue('5000')

  // A refusal that names no field of the form shows at the top.
  state.reply = () => ({ status: 500, json: { error: 'internal', message: 'The store failed.' } })
  await form.getByRole('button', { name: 'Add environment' }).click()
  await expect(form.getByRole('alert')).toHaveText('The store failed.')
  expect(state.writes.filter((write) => write.method === 'POST')).toHaveLength(6)
})

test('a manager edits, archives, and restores, and a conflicting restore shows the refusal', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)

  await card(page, 'env-staging').getByRole('button', { name: 'Edit staging' }).click()
  const form = page.getByRole('form', { name: 'Edit environment' })
  await expect(form.getByLabel('Name', { exact: true })).toHaveValue('staging')
  await expect(form.getByLabel('web-app', { exact: true })).toHaveValue('develop')
  await expect(form.getByLabel('GitHub environment names')).toHaveValue('staging\npreview')
  await form.getByLabel('web-app', { exact: true }).fill('')
  await form.getByLabel('Timing window in minutes').fill('20')
  await form.getByRole('button', { name: 'Save environment' }).click()
  await expect(form).toHaveCount(0)
  expect(state.writes.at(-1)).toEqual({
    method: 'PATCH',
    path: '/v1/environments/env-staging',
    body: {
      name: 'staging',
      tracked_branches: { conveyor: 'develop' },
      github_environments: ['staging', 'preview'],
      version_rule: null,
      timing_window_minutes: 20,
    },
  })

  await card(page, 'env-qa').getByRole('button', { name: 'Archive qa' }).click()
  await expect(card(page, 'env-qa')).toHaveCount(0)
  expect(state.writes.at(-1)).toEqual({ method: 'PATCH', path: '/v1/environments/env-qa', body: { archived: true } })

  await page.getByRole('button', { name: 'Show archived environments' }).click()
  const archived = page.getByRole('region', { name: 'Archived environments' })
  state.reply = () => ({
    status: 400,
    json: { error: 'invalid_environment', field: 'name', message: 'environment "legacy" already has this name' },
  })
  await archived.getByRole('button', { name: 'Restore legacy' }).click()
  await expect(archived.locator('[data-environment="env-legacy"]').getByRole('alert')).toHaveText(
    'environment "legacy" already has this name',
  )
  expect(state.writes.at(-1)).toEqual({
    method: 'PATCH',
    path: '/v1/environments/env-legacy',
    body: { archived: false },
  })

  state.reply = undefined
  await archived.getByRole('button', { name: 'Restore qa' }).click()
  await expect(card(page, 'env-qa')).toHaveCount(1)
  await expect(archived.locator('[data-environment="env-qa"]')).toHaveCount(0)
  await expect(page.getByText('Unsaved changes', { exact: true })).toHaveCount(0)
})

test('an issued key shows once in its dialog and leaves no copy in the page, cache, storage, or address', async ({
  page,
  context,
}) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await installQueryClientProbe(page)
  const state = await mockServer(page)
  await openEnvironmentsTab(page)

  await card(page, 'env-staging').getByRole('button', { name: 'Issue key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Issue environment key' })
  await dialog.getByLabel('Label').fill('staging watcher')
  await dialog.getByRole('button', { name: 'Issue key' }).click()
  const value = dialog.locator('[data-environment-key-value]')
  await expect(value).toHaveValue(issuedValue)
  expect(state.writes.at(-1)).toEqual({
    method: 'POST',
    path: '/v1/environments/env-staging/keys',
    body: { label: 'staging watcher' },
  })
  await expect(dialog).toContainText('Conveyor will not show this key again.')
  await dialog.getByRole('button', { name: 'Copy key' }).click()
  await expect(dialog.getByRole('button', { name: 'Copied' })).toBeVisible()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(issuedValue)

  // No cache or storage holds the value while the dialog shows it.
  const cached = await withQueryClient(
    page,
    (client) =>
      JSON.stringify([
        client
          .getQueryCache()
          .getAll()
          .map((query) => query.state.data),
        client
          .getMutationCache()
          .getAll()
          .map((mutation) => mutation.state),
      ]),
    null,
  )
  expect(cached).not.toContain(issuedValue)

  await dialog.getByRole('button', { name: 'Done' }).click()
  await expect(dialog).toHaveCount(0)
  await expect(card(page, 'env-staging').locator('[data-environment-key="envkey-new"]')).toContainText(
    'staging watcher',
  )
  expect(await page.content()).not.toContain(issuedValue)
  const stored = await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }, location.href]))
  expect(stored).not.toContain(issuedValue)

  // Opening the dialog again shows no value.
  await card(page, 'env-staging').getByRole('button', { name: 'Issue key' }).click()
  await expect(dialog.getByLabel('Label')).toHaveValue('')
  await expect(dialog.locator('[data-environment-key-value]')).toHaveCount(0)
})

test('a key answer that arrives after the dialog closes shows nothing', async ({ page }) => {
  const state = await mockServer(page)
  let release: () => void = () => {}
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  let answered: () => void = () => {}
  const done = new Promise<void>((resolve) => {
    answered = resolve
  })
  state.reply = async () => {
    await held
    answered()
    return { status: 201, json: { ...key('envkey-late', 'late'), value: issuedValue } }
  }
  await openEnvironmentsTab(page)
  await card(page, 'env-qa').getByRole('button', { name: 'Issue key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Issue environment key' })
  await dialog.getByLabel('Label').fill('late')
  await dialog.getByRole('button', { name: 'Issue key' }).click()
  await expect(dialog.getByRole('button', { name: 'Issuing…' })).toBeVisible()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toHaveCount(0)
  release()
  await done
  // Give the page time to receive the held answer.
  await page.waitForTimeout(300)
  expect(await page.content()).not.toContain(issuedValue)
  await card(page, 'env-qa').getByRole('button', { name: 'Issue key' }).click()
  await expect(dialog.locator('[data-environment-key-value]')).toHaveCount(0)
})

test('revoking a key asks for confirmation that names its label', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  await card(page, 'env-production').getByRole('button', { name: 'Revoke key prod watcher' }).click()
  const dialog = page.getByRole('dialog', { name: 'Revoke key prod watcher' })
  await expect(dialog.getByRole('heading')).toHaveText('Revoke the key “prod watcher”?')
  expect(state.writes).toHaveLength(0)
  await dialog.getByRole('button', { name: 'Revoke key' }).click()
  await expect(dialog).toHaveCount(0)
  expect(state.writes).toEqual([
    { method: 'DELETE', path: '/v1/environments/env-production/keys/envkey-prod', body: undefined },
  ])
  await expect(card(page, 'env-production').locator('[data-environment-key="envkey-prod"]')).toHaveCount(0)
})

test('issue is off at five keys, and a refusal when another manager reached the limit first says why', async ({
  page,
}) => {
  const full = environment('env-full', 'full', {
    keys: Array.from({ length: 5 }, (_, index) => key(`envkey-${index}`, `watcher ${index + 1}`)),
  })
  const four = environment('env-four', 'four', {
    keys: Array.from({ length: 4 }, (_, index) => key(`envkey-four-${index}`, `four ${index + 1}`)),
  })
  const state = await mockServer(page, { active: [production(), full, four] })
  await openEnvironmentsTab(page)

  const issue = card(page, 'env-full').getByRole('button', { name: 'Issue key' })
  await expect(issue).toBeDisabled()
  await expect(card(page, 'env-full')).toContainText(
    'An environment can have five keys. Revoke a key before you issue another.',
  )
  await expect(issue).toHaveAttribute('aria-describedby', 'environment-env-full-key-limit')

  await card(page, 'env-four').getByRole('button', { name: 'Issue key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Issue environment key' })
  await dialog.getByLabel('Label').fill('fifth')
  state.reply = () => ({
    status: 409,
    json: { error: 'environment_key_limit', environment_id: 'env-four', message: 'environment env-four has 5 keys' },
  })
  await dialog.getByRole('button', { name: 'Issue key' }).click()
  await expect(dialog.getByRole('alert')).toHaveText(
    'This environment already has five keys. Revoke a key before you issue another.',
  )
  await expect(dialog.locator('[data-environment-key-value]')).toHaveCount(0)
})

test('a suggestion opens a filled create form without a write, and ignore removes it', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  const suggestions = page.getByRole('region', { name: 'Suggested environments' })
  const row = suggestions.locator('[data-environment-suggestion="canary"]')
  await expect(row).toContainText('canary')
  await expect(row).toContainText('web-app')
  await expect(row.locator('time')).toHaveAttribute('datetime', '2026-11-26T08:00:00Z')

  await row.getByRole('button', { name: 'Create environment' }).click()
  const form = page.getByRole('form', { name: 'Add environment' })
  await expect(form.getByLabel('Name', { exact: true })).toHaveValue('canary')
  await expect(form.getByLabel('GitHub environment names')).toHaveValue('canary')
  expect(state.writes).toHaveLength(0)
  await form.getByRole('button', { name: 'Cancel' }).click()
  expect(state.writes).toHaveLength(0)

  await row.getByRole('button', { name: 'Ignore canary' }).click()
  await expect(suggestions).toHaveCount(0)
  expect(state.writes).toEqual([
    { method: 'POST', path: '/v1/environments/suggestions/ignore', body: { github_environment: 'canary' } },
  ])
})

test('accepting a suggestion saves only when the form is sent', async ({ page }) => {
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  await page
    .locator('[data-environment-suggestion="canary"]')
    .getByRole('button', { name: 'Create environment' })
    .click()
  const form = page.getByRole('form', { name: 'Add environment' })
  await form.getByRole('button', { name: 'Add environment' }).click()
  await expect(form).toHaveCount(0)
  expect(state.writes).toEqual([
    {
      method: 'POST',
      path: '/v1/environments',
      body: { name: 'canary', tracked_branches: {}, github_environments: ['canary'] },
    },
  ])
})

test('a workspace with no environment says what an environment is for and reads on no timer', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page, { active: [], archived: [], suggestions: [] })
  await page.goto('/w/demo/workspace')
  await page.getByRole('tab', { name: 'Environments' }).click()
  const empty = page.locator('[data-environments-empty]')
  await expect(empty).toContainText('This workspace has no environments.')
  await expect(page.getByText('An environment is a place where your software runs')).toBeVisible()
  const reads = activeReads(state)
  await page.clock.runFor(130_000)
  expect(activeReads(state)).toBe(reads)
})

test('the list reads again every 60 seconds while its tab is open and stops when the tab closes', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-11-26T10:00:00Z') })
  const state = await mockServer(page)
  await openEnvironmentsTab(page)
  // The first read can repeat once on window focus at load. The count after
  // load is the baseline.
  await page.clock.runFor(1_000)
  const base = activeReads(state)
  expect(base).toBeGreaterThanOrEqual(1)
  state.active[2].listening = true
  state.active[2].last_signal_at = '2026-11-26T10:00:30Z'

  await page.clock.runFor(57_000)
  expect(activeReads(state)).toBe(base)
  await page.clock.runFor(3_000)
  await expect.poll(() => activeReads(state)).toBe(base + 1)
  // The newer answer replaces the shown state.
  await expect(card(page, 'env-qa').locator('[data-watcher-state="listening"]')).toBeVisible()

  // Suggestions and archived rows read on no timer.
  const suggestionReads = state.gets.filter((path) => path.startsWith('/v1/environments/suggestions')).length
  expect(suggestionReads).toBe(1)

  await page.getByRole('tab', { name: 'Policy' }).click()
  await page.clock.runFor(180_000)
  expect(activeReads(state)).toBe(base + 1)
})

// The visual check of the Environments tab and its forms at 375 px and at a
// desktop width (AGENTS.md, UI changes). It runs only when
// CONVEYOR_VISUAL_CHECK_DIR names a directory outside the checkout, so ordinary
// runs and CI skip it and no screenshot reaches the repository. Mocked values
// only: no screenshot holds a key value.
test.describe('visual check', () => {
  const outputDir = process.env.CONVEYOR_VISUAL_CHECK_DIR
  test.skip(!outputDir, 'set CONVEYOR_VISUAL_CHECK_DIR to capture the visual check')

  async function capture(page: Page, name: string, width: number, locator?: ReturnType<Page['locator']>) {
    const dir = outputDir as string
    mkdirSync(dir, { recursive: true })
    await page.evaluate(() =>
      Promise.all(
        document
          .getAnimations()
          .filter((animation) => animation.effect?.getComputedTiming().iterations !== Number.POSITIVE_INFINITY)
          .map((animation) => animation.finished.catch(() => undefined)),
      ),
    )
    const path = join(dir, `${name}-${width}.png`)
    if (locator) await locator.screenshot({ path })
    else await page.screenshot({ path, fullPage: true })
  }

  for (const width of [375, 1280]) {
    test(`environments tab and forms at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      const state = await mockServer(page)
      await openEnvironmentsTab(page)
      await capture(page, 'environments-tab-manager', width)
      await page.getByRole('button', { name: 'Show archived environments' }).click()
      await expect(page.locator('[data-environment="env-legacy"]')).toBeVisible()
      await capture(page, 'environments-tab-archived', width)

      await page.getByRole('button', { name: 'Add environment' }).click()
      const form = page.getByRole('form', { name: 'Add environment' })
      await form.getByLabel('Name', { exact: true }).fill('Production')
      state.reply = () => ({
        status: 400,
        json: {
          error: 'invalid_environment',
          field: 'name',
          message: 'environment "production" already has this name',
        },
      })
      await form.getByRole('button', { name: 'Add environment' }).click()
      await expect(form.getByRole('alert')).toBeVisible()
      await capture(page, 'environment-form-refusal', width)
      await form.getByRole('button', { name: 'Cancel' }).click()

      await card(page, 'env-staging').getByRole('button', { name: 'Edit staging' }).click()
      await capture(page, 'environment-form-edit', width)
      await page.getByRole('form', { name: 'Edit environment' }).getByRole('button', { name: 'Cancel' }).click()

      // The dialog before issue: the value step is never captured.
      await card(page, 'env-staging').getByRole('button', { name: 'Issue key' }).click()
      await page.getByRole('dialog', { name: 'Issue environment key' }).getByLabel('Label').fill('staging watcher')
      await capture(page, 'environment-key-issue', width)
      await page.getByRole('dialog', { name: 'Issue environment key' }).getByRole('button', { name: 'Cancel' }).click()

      await card(page, 'env-production').getByRole('button', { name: 'Revoke key prod watcher' }).click()
      await capture(page, 'environment-key-revoke', width)
    })

    test(`environments for a viewer and an empty workspace at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      await mockServer(page, { role: 'viewer' })
      await page.goto('/w/demo/workspace')
      await expect(page.locator('[data-environment="env-production"]')).toBeVisible()
      await capture(page, 'environments-viewer', width)
    })

    test(`empty environments tab at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      await mockServer(page, { active: [], archived: [], suggestions: [] })
      await page.goto('/w/demo/workspace')
      await page.getByRole('tab', { name: 'Environments' }).click()
      await expect(page.locator('[data-environments-empty]')).toBeVisible()
      await capture(page, 'environments-empty', width)
    })
  }
})
