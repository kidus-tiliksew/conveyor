import { expect, type Page, test } from '@playwright/test'
import { githubSlug } from '../src/lib/repository'
import type { WorkspaceConfigDocument, WorkspaceConfigRepo } from '../src/lib/types'

const notice = 'This workspace has no registered repository.'
const installTask = { id: '260908-install', state: 'queued' }

async function mockRepositories(page: Page, initial: WorkspaceConfigRepo[] = [], reject = false) {
  let document: WorkspaceConfigDocument = {
    workspace: 'demo',
    max_bounces: 3,
    work_order_queue_timeout: '24h',
    stage_timeouts: { spec: '30m', implement: '4h', review: '1h' },
    review: { seats: [{}] },
    execution: {
      spec_approval: true,
      merge_approval: true,
      require_verification_evidence: false,
      implement_concurrency: 1,
      review_concurrency: 1,
      first_activity_timeout: '2m',
    },
    repos: initial,
  }
  let submitted: WorkspaceConfigDocument | undefined
  let workspaceReads = 0
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'operator', role: 'operator' } })
    if (path === '/v1/workspace/config') {
      if (route.request().method() === 'PUT') {
        submitted = route.request().postDataJSON().document
        if (reject)
          return route.fulfill({
            status: 422,
            json: {
              error: 'validation_failed',
              fields: [
                {
                  field: 'repos[0].github',
                  message: 'supplied GitHub slug "old/repo" differs from derived slug "owner/repo"',
                },
              ],
            },
          })
        document = {
          ...submitted!,
          repos: submitted!.repos.map((repo) => ({ ...repo, github: 'owner/repo', install_task: installTask })),
        }
        return route.fulfill({ json: { document, version: 2, event_id: 42 } })
      }
      return route.fulfill({ json: { document, version: 1 } })
    }
    if (path === '/v1/workspace') {
      workspaceReads++
      return route.fulfill({ json: { workspace: 'demo', database: 'postgres', max_bounces: 3, repos: document.repos } })
    }
    if (path === '/v1/workers') return route.fulfill({ json: { workers: [], worker_expected: false } })
    if (path === '/v1/activity') return route.fulfill({ json: [], headers: { 'X-Conveyor-Total': '0' } })
    return route.fulfill({ json: [] })
  })
  return { submitted: () => submitted, reads: () => workspaceReads }
}

async function openGeneral(page: Page) {
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'General' }).click()
}

for (const url of [
  'https://github.com/Owner/Repo.git',
  'http://github.com:80/Owner/Repo.git/',
  'ssh://git@github.com:22/Owner/Repo.git',
  'git@github.com:Owner/Repo.git',
  'https://github.com:443/Owner/Repo.git',
  'git://github.com:9418/Owner/Repo.git',
  '  https://GITHUB.COM/Owner/Repo.git  ',
]) {
  test(`normalizes ${url}`, () => expect(githubSlug(url)).toBe('owner/repo'))
}
for (const url of [
  'https://gitlab.com/owner/repo.git',
  'https://github.com:444/owner/repo',
  'ssh://git@github.com:2222/owner/repo',
  '/local/repo',
  'https://github.com/',
  'https://github.com/a%ZZ',
  'https://github.com/a%5Cb',
  'not a URL',
]) {
  test(`omits a slug for ${url}`, () => expect(githubSlug(url)).toBe(''))
}
test('preserves Go normalization of path escapes, dot segments and suffix case', () => {
  expect(githubSlug('https://github.com/%4Fwner/Repo.git')).toBe('owner/repo')
  expect(githubSlug('https://github.com/Owner/../Repo.git')).toBe('owner/../repo')
  expect(githubSlug('https://github.com/Owner/Repo.GIT')).toBe('owner/repo.git')
})

test('new row defaults on, derives slug while typing and consumes the saved projection', async ({ page }, testInfo) => {
  const api = await mockRepositories(page)
  await openGeneral(page)
  await page.getByRole('button', { name: 'Add repository' }).click()
  const row = page.getByTestId('repository-row')
  await expect(row.getByRole('switch', { name: 'Install Conveyor' })).toBeChecked()
  await expect(row.getByRole('textbox', { name: 'GitHub slug', exact: true })).toHaveCount(0)
  await row.getByLabel('Name', { exact: true }).fill('example')
  await row.getByLabel('URL', { exact: true }).fill('git@github.com:Owner/Repo.git')
  await expect(row.getByText('GitHub slug: owner/repo')).toBeVisible()
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(row.getByRole('link', { name: installTask.id })).toHaveAttribute('href', `/tasks/${installTask.id}`)
  expect(api.submitted()?.repos[0]).toMatchObject({ install_conveyor: true })
  expect(api.submitted()?.repos[0].github).toBeUndefined()
  await expect(row.getByText('queued', { exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('repository-onboarding.png'), fullPage: true })
  await row.getByRole('switch').click()
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect.poll(() => api.submitted()?.repos[0].install_conveyor).toBe(false)
  await expect(row.getByRole('switch')).not.toBeChecked()
  await expect(row.getByRole('link', { name: installTask.id })).toBeVisible()
  await row.getByLabel('URL', { exact: true }).fill('https://gitlab.com/owner/repo')
  await expect(row.getByText(/GitHub slug:/)).toHaveCount(0)
})

test('existing stored false and install task remain visible; validation is shown on the row', async ({ page }) => {
  await mockRepositories(
    page,
    [
      {
        name: 'example',
        url: 'https://github.com/Owner/Repo.git',
        github: 'owner/repo',
        base: 'main',
        install_conveyor: false,
        install_task: installTask,
      },
    ],
    true,
  )
  await openGeneral(page)
  const row = page.getByTestId('repository-row')
  await expect(row.getByRole('switch')).not.toBeChecked()
  await expect(row.getByRole('link', { name: installTask.id })).toBeVisible()
  await row.getByLabel('Base', { exact: true }).fill('develop')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(row.getByRole('alert')).toHaveText(
    'supplied GitHub slug "old/repo" differs from derived slug "owner/repo"',
  )
})

test('registration clears board and sheet notices through the shared cache without reload', async ({ page }) => {
  const api = await mockRepositories(page)
  await page.goto('/')
  await page.evaluate(() => {
    ;(window as Window & { onboardingDocument?: boolean }).onboardingDocument = true
  })
  await expect(page.getByText(notice, { exact: false })).toBeVisible()
  await page.getByRole('button', { name: 'New task', exact: true }).click()
  const sheet = page.getByRole('dialog', { name: 'New task' })
  await expect(sheet.getByText(notice, { exact: false })).toBeVisible()
  await expect(sheet.getByRole('button', { name: 'Create task', exact: true })).toBeDisabled()
  await sheet.getByRole('link', { name: 'Register a repository on the Workspace page' }).click()
  await expect(page).toHaveURL(/\/workspace$/)
  await page.getByRole('tab', { name: 'General' }).click()
  await page.getByRole('button', { name: 'Add repository' }).click()
  const row = page.getByTestId('repository-row')
  await row.getByLabel('Name', { exact: true }).fill('example')
  await row.getByLabel('URL', { exact: true }).fill('https://github.com/Owner/Repo.git')
  const readsBeforeSave = api.reads()
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect.poll(api.reads).toBeGreaterThan(readsBeforeSave)
  await page.getByRole('link', { name: 'Board', exact: true }).click()
  await expect(page.getByText(notice, { exact: false })).toHaveCount(0)
  await page.getByRole('button', { name: 'New task', exact: true }).click()
  await expect(sheet.getByText(notice, { exact: false })).toHaveCount(0)
  await expect(sheet.getByRole('combobox', { name: 'Repository' })).toHaveValue('example')
  await sheet.getByRole('textbox', { name: 'Task description' }).fill('Fix the README typo')
  await expect(sheet.getByRole('button', { name: 'Create task', exact: true })).toBeEnabled()
  expect(await page.evaluate(() => (window as Window & { onboardingDocument?: boolean }).onboardingDocument)).toBe(true)
})

test('task list creation sheet links to repository registration', async ({ page }) => {
  await mockRepositories(page)
  await page.goto('/tasks?create=true')
  const sheet = page.getByRole('dialog', { name: 'New task' })
  await expect(sheet.getByText(notice, { exact: false })).toBeVisible()
  await sheet.getByRole('textbox', { name: 'Task description' }).fill('Fix a typo')
  await expect(sheet.getByRole('button', { name: 'Create task', exact: true })).toBeDisabled()
  await sheet.getByRole('link', { name: 'Register a repository on the Workspace page' }).click()
  await expect(page).toHaveURL(/\/workspace$/)
  await expect(sheet).toHaveCount(0)
})
