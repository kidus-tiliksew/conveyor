import { expect, type Page, test } from '@playwright/test'
import type { WorkspaceGitHubAppStatus } from '../src/lib/types'

const disconnected: WorkspaceGitHubAppStatus = {
  connected: false,
  app_slug: '',
  installation_account: '',
  repositories: [],
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
}
const manifest = {
  name: 'Conveyor Demo',
  url: 'https://conveyor.example',
  public: true,
  redirect_url: 'https://conveyor.example/v1/workspaces/demo/github-app/callback',
  setup_url: 'https://conveyor.example/v1/workspaces/demo/github-app/setup',
  setup_on_update: true,
  hook_attributes: { active: false, url: 'https://conveyor.example' },
  default_permissions: { contents: 'write' },
}

async function mockApp(page: Page, role = 'operator') {
  const state = { status: { ...disconnected }, gets: [] as string[], deletes: 0, posts: 0, error: '', failMethod: '' }
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
      return route.fulfill({ json: { id: 'usr_owner', display_name: 'Ada', email: 'ada@example.test', role } })
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
    await page.goto('/settings')
    await expect(page.getByText('Workspace GitHub App', { exact: true })).toBeVisible()
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
    expect(posted.hook_attributes.url).toBe('https://conveyor.example')
    expect(posted.hook_attributes.active).toBe(false)
    expect(state.posts).toBe(1)
  })
}

test('connected status shows mixed coverage and disconnect requires confirmation and refetches', async ({ page }) => {
  const state = await mockApp(page)
  state.status = { ...connected }
  await page.goto('/settings')
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
  await page.goto('/settings')
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeVisible()
  state.status = { ...connected, app_slug: 'conveyor-second' }
  await page.goto('/settings?workspace=second')
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
  await page.goto('/settings')
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
    await page.goto('/settings')
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
  await page.goto('/settings')
  await expect(page.getByLabel('Display name', { exact: true })).toHaveValue('Ada')
  await expect(page.getByText('Workspace GitHub App', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toHaveCount(0)
  expect(state.gets).toEqual([])
})

test('machine-only status errors show readable failure copy', async ({ page }) => {
  const state = await mockApp(page)
  state.failMethod = 'GET'
  await page.goto('/settings')
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
  await page.goto('/settings')
  const settingsURL = page.url()
  await page.getByRole('button', { name: 'Connect GitHub', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText(state.error)
  await expect(page.getByRole('button', { name: 'Connect GitHub', exact: true })).toBeEnabled()
  expect(state.posts).toBe(1)
  expect(githubRequests).toBe(0)
  expect(page.url()).toBe(settingsURL)
  expect(await page.locator('form[action^="https://github.com/"]').count()).toBe(0)
})
