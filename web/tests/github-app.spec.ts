import { expect, type Page, test } from '@playwright/test'
import type { WorkspaceGitHubAppStatus, WorkspaceGitHubWebhookStatus } from '../src/lib/types'
import { callerIdentity } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'

// The webhook projection in the shape GET …/github-app serves it
// (internal/httpapi/workspace_github_app.go#gitHubWebhookView).
const webhookEvents = ['pull_request', 'check_suite', 'check_run', 'status', 'push', 'issues']
const receiverURL = 'https://conveyor.example/v1/github/webhook/demo'
const unconfiguredWebhook: WorkspaceGitHubWebhookStatus = {
  receiver_url: receiverURL,
  events: webhookEvents,
  secret_configured: false,
  secret_set_at: null,
  last_delivery_at: null,
  configured: false,
}
const configuredWebhook: WorkspaceGitHubWebhookStatus = {
  receiver_url: receiverURL,
  events: webhookEvents,
  secret_configured: true,
  secret_set_at: '2026-10-30T08:00:00Z',
  last_delivery_at: '2026-10-31T09:15:42Z',
  configured: true,
}
const disconnected: WorkspaceGitHubAppStatus = {
  connected: false,
  app_slug: '',
  installation_account: '',
  repositories: [],
  webhook: unconfiguredWebhook,
}
const connected: WorkspaceGitHubAppStatus = {
  connected: true,
  app_slug: 'conveyor-demo',
  installation_account: 'demo-org',
  repositories: [
    { name: 'frontend', covered: true },
    { name: 'backend', covered: false },
  ],
  installation_url: 'https://github.com/apps/conveyor-demo/installations/new?state=installation-state',
  webhook: unconfiguredWebhook,
}
const manifest = {
  name: 'Conveyor Demo',
  url: 'https://conveyor.example',
  public: true,
  redirect_url: 'https://conveyor.example/v1/workspaces/demo/github-app/callback',
  setup_url: 'https://conveyor.example/v1/workspaces/demo/github-app/setup',
  setup_on_update: true,
  hook_attributes: { active: true, url: receiverURL },
  default_events: webhookEvents,
  default_permissions: {
    metadata: 'read',
    checks: 'read',
    contents: 'write',
    pull_requests: 'write',
    issues: 'write',
    statuses: 'write',
  },
}

async function mockApp(page: Page, role = 'operator') {
  const state = {
    status: { ...disconnected },
    gets: [] as string[],
    deletes: 0,
    posts: 0,
    secretPosts: 0,
    secrets: [] as string[],
    error: '',
    failMethod: '',
  }
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'second', name: 'Second' },
        ],
      })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity(
          { id: 'usr_owner', display_name: 'Ada', email: 'ada@example.test', role },
          route.request(),
        ),
      })
    if (path.endsWith('/github-app/webhook-secret')) {
      state.secretPosts++
      expect(request.method()).toBe('POST')
      expect(request.headers()['x-conveyor-csrf']).toBe('1')
      if (state.failMethod === 'SECRET')
        return route.fulfill({ status: 404, json: { error: 'github_app_not_connected', message: state.error } })
      // 32 random bytes, hex-encoded, as the server generates them.
      const secret = `${String(state.secretPosts).padStart(2, '0')}${'5ec7e7'.repeat(10)}aa`
      state.secrets.push(secret)
      const setAt = `2026-11-01T10:0${state.secretPosts}:00Z`
      state.status = {
        ...state.status,
        webhook: { ...state.status.webhook, secret_configured: true, secret_set_at: setAt },
      }
      return route.fulfill({
        headers: { 'Cache-Control': 'no-store' },
        json: { secret, secret_set_at: setAt, receiver_url: receiverURL, events: webhookEvents },
      })
    }
    if (path.endsWith('/github-app/manifest')) {
      state.posts++
      expect(request.method()).toBe('POST')
      expect(request.headers()['x-conveyor-csrf']).toBe('1')
      if (state.failMethod === 'POST')
        return route.fulfill({ status: 503, json: { error: 'public_url_required', message: state.error } })
      return route.fulfill({ json: { manifest, state: 'manifest/state+value' } })
    }
    if (path.endsWith('/github-app')) {
      if (request.method() === 'GET') state.gets.push(path)
      if (request.method() === 'DELETE') {
        state.deletes++
        expect(request.headers()['x-conveyor-csrf']).toBe('1')
      }
      if (state.failMethod === request.method())
        return route.fulfill({ status: 403, json: { error: 'github_app_permission', message: state.error } })
      if (request.method() === 'DELETE') {
        state.status = { ...disconnected }
        return route.fulfill({ status: 204, body: '' })
      }
      return route.fulfill({ json: state.status })
    }
    if (path === '/v1/pending-proposals') return route.fulfill({ json: { items: [], attention: { total: 0 } } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    return route.fulfill({ json: [] })
  })
  return state
}

for (const organization of ['', 'demo-org']) {
  test(`connect posts a top-level manifest form for ${organization || 'a personal account'}`, async ({ page }) => {
    const state = await mockApp(page)
    let submitted: { url: string; method: string; body: string | null; navigation: boolean } | undefined
    await page.route('https://github.com/**', async (route) => {
      const request = route.request()
      submitted = {
        url: request.url(),
        method: request.method(),
        body: request.postData(),
        navigation: request.isNavigationRequest(),
      }
      await route.fulfill({ contentType: 'text/html', body: '<h1>GitHub app registration</h1>' })
    })
    await page.goto('/w/demo/settings')
    await expect(page.getByText('Workspace GitHub App', { exact: true })).toBeVisible()
    // A disconnected App has no webhook to set up.
    await expect(page.getByRole('region', { name: 'Webhook delivery' })).toHaveCount(0)
    await expect(page.getByLabel('GitHub token', { exact: true })).toHaveCount(0)
    await expect(page.getByLabel('Workspace GitHub token', { exact: true })).toHaveCount(0)
    await page.getByLabel('Organization (optional)').fill(organization)
    await page.getByRole('button', { name: 'Connect GitHub', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'GitHub app registration' })).toBeVisible()
    const target = organization
      ? `https://github.com/organizations/${organization}/settings/apps/new`
      : 'https://github.com/settings/apps/new'
    expect(submitted?.url).toBe(`${target}?state=manifest%2Fstate%2Bvalue`)
    expect(submitted?.method).toBe('POST')
    expect(submitted?.navigation).toBe(true)
    const posted = JSON.parse(new URLSearchParams(submitted?.body ?? '').get('manifest') ?? '{}')
    expect(posted).toEqual(manifest)
    // Check GitHub's required URLs independently of mock/POST equality.
    expect(posted.url).toBe('https://conveyor.example')
    expect(posted.redirect_url).toBe('https://conveyor.example/v1/workspaces/demo/github-app/callback')
    expect(posted.setup_url).toBe('https://conveyor.example/v1/workspaces/demo/github-app/setup')
    expect(posted.hook_attributes.url).toBe(receiverURL)
    expect(posted.hook_attributes.active).toBe(true)
    expect(state.posts).toBe(1)
  })
}

test('connected status shows mixed coverage and disconnect requires confirmation and refetches', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected }
  await page.goto('/w/demo/settings')
  await expect(page.getByText('conveyor-demo', { exact: true })).toBeVisible()
  await expect(page.getByText('Installed on demo-org')).toBeVisible()
  const repositories = page.getByRole('list', { name: 'Repository coverage' })
  await expect(repositories.getByRole('listitem').filter({ hasText: 'frontend' })).toContainText('Covered')
  const uncovered = repositories.getByRole('listitem').filter({ hasText: 'backend' })
  await expect(uncovered).toContainText('Not covered')
  await expect(uncovered.getByRole('link', { name: 'Install on GitHub' })).toHaveAttribute(
    'href',
    connected.installation_url!,
  )
  await expect(page.getByRole('link', { name: 'Manage on GitHub' })).toHaveAttribute(
    'href',
    connected.installation_url!,
  )
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Disconnect GitHub App' })
  await expect(dialog).toBeVisible()
  expect(state.deletes).toBe(0)
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  expect(state.deletes).toBe(0)
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click()
  await dialog.getByRole('button', { name: 'Disconnect', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeVisible()
  expect(state.deletes).toBe(1)
  expect(state.gets.length).toBeGreaterThan(1)
})

test('setup return selects the returned workspace and refreshes status, including a restored page', async ({
  page,
}) => {
  const state = await mockApp(page)
  await page.goto('/w/demo/settings')
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeVisible()
  state.status = { ...connected, app_slug: 'conveyor-second' }
  await page.goto('/w/demo/settings?workspace=second')
  await expect(page.getByText('conveyor-second', { exact: true })).toBeVisible()
  expect(state.gets.at(-1)).toBe('/v1/workspaces/second/github-app')
  state.status = { ...disconnected }
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })))
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeVisible()
  const stored = await page.evaluate(() => Object.keys(localStorage))
  expect(stored.sort()).toEqual(['conveyor-theme', 'conveyor-workspace'])
})

test('an app without an installation offers the server installation remedy', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected, installation_account: '', repositories: [] }
  await page.goto('/w/demo/settings')
  await expect(page.getByText('Not installed yet.')).toBeVisible()
  await expect(page.getByText('No repositories registered.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Manage on GitHub' })).toHaveAttribute(
    'href',
    connected.installation_url!,
  )
})

for (const method of ['GET', 'POST', 'DELETE']) {
  test(`${method} failure shows the server remedy and preserves connection state`, async ({ page }) => {
    const state = await mockApp(page)
    state.failMethod = method
    state.error = 'Connect the GitHub App in workspace settings for repository backend.'
    if (method === 'DELETE') state.status = { ...connected }
    await page.goto('/w/demo/settings')
    if (method === 'POST') await page.getByRole('button', { name: 'Connect GitHub', exact: true }).click()
    if (method === 'DELETE') {
      await page.getByRole('button', { name: 'Disconnect', exact: true }).click()
      await page.getByRole('dialog').getByRole('button', { name: 'Disconnect', exact: true }).click()
    }
    await expect(page.getByRole('alert')).toContainText(state.error)
    if (method === 'DELETE') {
      await expect(page.getByText('conveyor-demo', { exact: true })).toBeVisible()
      await expect(page.getByRole('dialog')).toBeVisible()
    }
  })
}

test('members cannot see or fetch the workspace GitHub App card', async ({ page }) => {
  const state = await mockApp(page, 'contributor')
  await page.goto('/w/demo/settings')
  await expect(page.getByLabel('Display name', { exact: true })).toHaveValue('Ada')
  await expect(page.getByText('Workspace GitHub App', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Webhook delivery' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Generate secret', exact: true })).toHaveCount(0)
  expect(state.gets).toEqual([])
  expect(state.secretPosts).toBe(0)
})

test('machine-only status errors show readable failure copy', async ({ page }) => {
  const state = await mockApp(page)
  state.failMethod = 'GET'
  await page.goto('/w/demo/settings')
  await expect(page.getByRole('alert')).toHaveText('Could not load the GitHub connection.')
  await expect(page.locator('body')).not.toContainText('github_app_permission')
})

test('missing public URL refuses connection without navigating to GitHub', async ({ page }) => {
  const state = await mockApp(page)
  state.failMethod = 'POST'
  state.error = 'Configure the public URL before connecting GitHub.'
  let githubRequests = 0
  await page.route('https://github.com/**', async (route) => {
    githubRequests++
    await route.abort()
  })
  await page.goto('/w/demo/settings')
  const settingsURL = page.url()
  await page.getByRole('button', { name: 'Connect GitHub', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText(state.error)
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeEnabled()
  expect(state.posts).toBe(1)
  expect(githubRequests).toBe(0)
  expect(page.url()).toBe(settingsURL)
  expect(await page.locator('form[action^="https://github.com/"]').count()).toBe(0)
})

// Everything the browser could keep after the dialog closes: both storages,
// cookies, the URL, and history state (component-web-dashboard, Webhook panel).
async function browserState(page: Page) {
  return page.evaluate(() =>
    JSON.stringify({
      local: Object.entries(localStorage),
      session: Object.entries(sessionStorage),
      cookie: document.cookie,
      url: location.href,
      history: history.state,
    }),
  )
}

// Every Query and mutation cache entry the application's client holds.
async function cacheState(page: Page) {
  return withQueryClient(
    page,
    (client) =>
      JSON.stringify({
        queries: client
          .getQueryCache()
          .getAll()
          .map((query) => [query.queryKey, query.state.data]),
        mutations: client
          .getMutationCache()
          .getAll()
          .map((mutation) => [mutation.state.data, mutation.state.variables]),
      }),
    undefined,
  )
}

test('webhook panel shows unconfigured delivery, the receiver URL, events, and setup steps', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected, webhook: { ...unconfiguredWebhook } }
  await page.goto('/w/demo/settings')
  const panel = page.getByRole('region', { name: 'Webhook delivery' })
  await expect(panel.getByText('Webhook delivery is not configured', { exact: true })).toBeVisible()
  await expect(panel.getByTestId('webhook-last-delivery')).toHaveText('No delivery accepted yet')
  await expect(panel.getByTestId('webhook-secret-set')).toHaveText('No secret generated yet')
  await expect(panel.getByLabel('Receiver URL', { exact: true })).toHaveValue(receiverURL)
  const events = panel.getByRole('list', { name: 'Webhook events' }).getByRole('listitem')
  await expect(events).toHaveText([
    'Pull request pull_request',
    'Check suite check_suite',
    'Check run check_run',
    'Status status',
    'Push push',
    'Issues issues',
  ])
  const steps = panel.getByRole('list', { name: 'Webhook setup steps' }).getByRole('listitem')
  await expect(steps).toHaveCount(4)
  await expect(steps.nth(1)).toHaveText('Under Webhook, turn on Active.')
  await expect(steps.nth(2)).toContainText('receiver URL as the Webhook URL')
  await expect(steps.nth(3)).toContainText('subscribe the events')
  await expect(steps.nth(0).getByRole('link', { name: 'App settings on a personal account' })).toHaveAttribute(
    'href',
    'https://github.com/settings/apps/conveyor-demo',
  )
  await expect(steps.nth(0).getByRole('link', { name: 'App settings on organization demo-org' })).toHaveAttribute(
    'href',
    'https://github.com/organizations/demo-org/settings/apps/conveyor-demo',
  )
  expect(state.secretPosts).toBe(0)
})

test('webhook panel shows configured delivery and the last accepted delivery time', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected, installation_account: '', webhook: { ...configuredWebhook } }
  await page.goto('/w/demo/settings')
  const panel = page.getByRole('region', { name: 'Webhook delivery' })
  await expect(panel.getByText('Webhook delivery is configured', { exact: true })).toBeVisible()
  const lastDelivery = panel.getByTestId('webhook-last-delivery').locator('time')
  await expect(lastDelivery).toHaveAttribute('datetime', configuredWebhook.last_delivery_at!)
  const expected = await page.evaluate((value) => new Date(value).toLocaleString(), configuredWebhook.last_delivery_at!)
  await expect(lastDelivery).toHaveText(expected)
  await expect(panel.getByTestId('webhook-secret-set').locator('time')).toHaveAttribute(
    'datetime',
    configuredWebhook.secret_set_at!,
  )
  // Without an installation account only the personal-account address is known.
  await expect(panel.getByRole('link', { name: /^App settings on/ })).toHaveCount(1)
})

test('webhook panel without a receiver URL names the public URL remedy', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected, webhook: { ...unconfiguredWebhook, receiver_url: '' } }
  await page.goto('/w/demo/settings')
  const panel = page.getByRole('region', { name: 'Webhook delivery' })
  await expect(panel.getByRole('status')).toContainText('CONVEYOR_PUBLIC_URL')
  await expect(panel.getByLabel('Receiver URL', { exact: true })).toHaveCount(0)
})

test('generated webhook secret shows once, is replaced on regenerate, and leaves no browser trace', async ({
  page,
  context,
}) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await installQueryClientProbe(page)
  const state = await mockApp(page)
  state.status = { ...connected, webhook: { ...unconfiguredWebhook } }
  await page.goto('/w/demo/settings')
  await page.getByRole('button', { name: 'Generate secret', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Generate webhook secret' })
  await expect(dialog).toBeVisible()
  // Opening the dialog generates nothing and shows no secret.
  await expect(dialog.getByLabel('Webhook secret', { exact: true })).toHaveCount(0)
  expect(state.secretPosts).toBe(0)

  await dialog.getByRole('button', { name: 'Generate', exact: true }).click()
  const field = dialog.getByLabel('Webhook secret', { exact: true })
  await expect(field).toHaveValue(state.secrets[0])
  await expect(dialog).toContainText('Conveyor will not show this secret again.')
  const first = state.secrets[0]
  // The card refetches status and shows when the secret was set.
  await expect(page.getByTestId('webhook-secret-set').locator('time')).toHaveAttribute(
    'datetime',
    '2026-11-01T10:01:00Z',
  )
  await dialog.getByRole('button', { name: 'Copy webhook secret' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(first)
  expect(await browserState(page)).not.toContain(first)
  expect(await cacheState(page)).not.toContain(first)

  await dialog.getByRole('button', { name: 'Generate again', exact: true }).click()
  await expect(field).toHaveValue(state.secrets[1])
  const second = state.secrets[1]
  expect(second).not.toBe(first)
  expect(state.secretPosts).toBe(2)
  expect(await page.content()).not.toContain(first)

  await dialog.getByRole('button', { name: 'Done', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  const html = await page.content()
  expect(html).not.toContain(first)
  expect(html).not.toContain(second)
  const stored = await browserState(page)
  expect(stored).not.toContain(first)
  expect(stored).not.toContain(second)
  const cached = await cacheState(page)
  expect(cached).not.toContain(first)
  expect(cached).not.toContain(second)

  // Reopening shows no secret and warns that a new one replaces the current one.
  await page.getByRole('button', { name: 'Generate secret', exact: true }).click()
  await expect(dialog).toContainText('A new secret replaces the current one.')
  await expect(dialog.getByLabel('Webhook secret', { exact: true })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(state.secretPosts).toBe(2)
})

test('a failed webhook secret request shows the server error and no secret', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected, webhook: { ...configuredWebhook } }
  state.failMethod = 'SECRET'
  state.error = 'Connect the GitHub App before generating a webhook secret.'
  await page.goto('/w/demo/settings')
  await page.getByRole('button', { name: 'Generate secret', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Generate webhook secret' })
  await dialog.getByRole('button', { name: 'Generate', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText(state.error)
  await expect(dialog.getByLabel('Webhook secret', { exact: true })).toHaveCount(0)
  expect(state.secretPosts).toBe(1)
})
