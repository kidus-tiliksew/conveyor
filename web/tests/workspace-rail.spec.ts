import { expect, type Page, type Route, test } from '@playwright/test'
import { callerIdentity, isScopedIdentityRequest } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'

const workspaces = [
  { id: 'beta-ventures', name: 'Beta Ventures', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
  { id: 'design', name: 'Design', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
  { id: 'funnel', name: 'Funnel', config_version: 1, created_at: '2026-07-20T00:00:00Z' },
]

const refusal =
  'Creating a workspace requires being an operator of an existing workspace or the instance administrator.'

// What an unscoped GET /v1/me serves. `status` answers a failure, and the
// field is passed through as given so missing and malformed values can be
// served on purpose.
type UnscopedIdentity = { status?: number; id?: string; email?: string; can_create_workspaces?: unknown }

type ShellOptions = {
  // The caller's creation right on every read, scoped or unscoped, in the
  // server's shape. The scoped role is operator, so manage_workspace is
  // always served and never decides creation (component-web-dashboard).
  canCreate?: boolean
  // Overrides the unscoped read only, for pending, failed, and malformed cases.
  unscoped?: () => UnscopedIdentity | Promise<UnscopedIdentity>
  // Answers for successive POST /v1/workspaces requests.
  creates?: Array<{ status: number; json?: unknown; body?: string }>
  identityRequests?: Array<{ query: string | null; header: string | undefined }>
}

async function mockShell(page: Page, options: ShellOptions = {}) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'design')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/v1/workspaces' && request.method() === 'POST') {
      const answer = options.creates?.shift() ?? { status: 201, json: { id: 'created', name: 'Created' } }
      await route.fulfill(answer)
      return
    }
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: workspaces })
      return
    }
    if (url.pathname === '/v1/me') {
      options.identityRequests?.push({
        query: url.searchParams.get('workspace_id'),
        header: request.headers()['x-workspace-id'],
      })
      const caller = { id: 'usr_operator', email: 'operator@example.test', display_name: 'Operator' }
      if (!isScopedIdentityRequest(request) && options.unscoped) {
        const served = await options.unscoped()
        if (served.status) {
          await route.fulfill({ status: served.status, json: { error: 'identity unavailable' } })
          return
        }
        await route.fulfill({ json: { ...caller, ...served } })
        return
      }
      await route.fulfill({
        json: callerIdentity(
          { ...caller, role: 'operator', can_create_workspaces: options.canCreate ?? false },
          request,
        ),
      })
      return
    }
    await route.fulfill({ json: [] })
  })
}

function isUnscopedIdentityResponse(response: { url(): string }) {
  const url = new URL(response.url())
  return url.pathname === '/v1/me' && !url.searchParams.has('workspace_id')
}

// Reads the cached unscoped creation-right query state from the application's
// QueryClient, so a hidden control is asserted after the read settled.
async function creationQueryStatus(page: Page) {
  return withQueryClient(
    page,
    (client) =>
      (client as unknown as { getQueryState(key: unknown[]): { status: string } | undefined }).getQueryState([
        'caller-identity',
        '',
      ])?.status,
    undefined,
  )
}

function deferred<T = void>() {
  let resolve: (value: T) => void = () => {}
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

test('workspace rail uses Slack-like tiles and preserves switching and creation', async ({ page }) => {
  await mockShell(page, { canCreate: true })
  await page.goto('/')

  const rail = page.getByRole('navigation', { name: 'Workspaces' })
  const beta = rail.getByRole('button', { name: 'Switch to Beta Ventures' })
  const design = rail.getByRole('button', { name: 'Switch to Design' })
  const funnel = rail.getByRole('button', { name: 'Switch to Funnel' })
  const add = rail.getByRole('link', { name: 'Add workspace' })

  await expect(beta).toHaveText('BV')
  await expect(design).toHaveText('DE')
  await expect(funnel).toHaveText('FU')
  await expect(design).toHaveAttribute('aria-current', 'true')
  await expect(add).toHaveAttribute('href', '/workspaces/new')

  const visual = await rail.evaluate((node) => {
    const [inactive, active] = Array.from(node.querySelectorAll('button'))
    const railStyle = getComputedStyle(node)
    const inactiveStyle = getComputedStyle(inactive)
    const activeStyle = getComputedStyle(active)
    const inactiveBox = inactive.getBoundingClientRect()
    const activeBox = active.getBoundingClientRect()
    const addBox = node.querySelector('a')!.getBoundingClientRect()
    return {
      railBackground: railStyle.backgroundColor,
      inactiveBackground: inactiveStyle.backgroundColor,
      activeBackground: activeStyle.backgroundColor,
      activeShadow: activeStyle.boxShadow,
      radius: inactiveStyle.borderRadius,
      inactiveSize: [inactiveBox.width, inactiveBox.height],
      faceGap: activeBox.top - inactiveBox.bottom,
      addBelowWorkspaces:
        addBox.top > Array.from(node.querySelectorAll('button')).at(-1)!.getBoundingClientRect().bottom,
    }
  })

  expect(visual.inactiveBackground).not.toBe(visual.railBackground)
  expect(visual.activeBackground).not.toBe(visual.inactiveBackground)
  expect(visual.activeShadow).not.toBe('none')
  expect(visual.radius).toBe('9px')
  expect(visual.inactiveSize).toEqual([32, 32])
  expect(visual.faceGap).toBeGreaterThanOrEqual(24)
  expect(visual.addBelowWorkspaces).toBe(true)

  await beta.click()
  await expect(beta).toHaveAttribute('aria-current', 'true')
  await expect(design).not.toHaveAttribute('aria-current')
  await expect.poll(() => page.evaluate(() => localStorage.getItem('conveyor-workspace'))).toBe('beta-ventures')

  await add.click()
  await expect(page).toHaveURL(/\/workspaces\/new$/)
  const dialog = page.getByRole('dialog', { name: 'Create workspace' })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByLabel('Name')).toBeVisible()
})

// DEC-75(2): an operator of any workspace may create workspaces, not only
// the deployment owner. The browser still trusts the served boolean alone.
test('nonowner workspace operator with the creation right sees Add workspace', async ({ page }) => {
  await installQueryClientProbe(page)
  const unscopedRead = page.waitForResponse(isUnscopedIdentityResponse)
  await mockShell(page, {
    canCreate: true,
    unscoped: () => ({ id: 'usr_nonowner_operator', email: 'nonowner@example.test', can_create_workspaces: true }),
  })
  await page.goto('/')
  const served = await (await unscopedRead).json()
  expect(served.id).toBe('usr_nonowner_operator')
  expect(served.can_create_workspaces).toBe(true)
  await expect.poll(() => creationQueryStatus(page)).toBe('success')

  const add = page.getByRole('navigation', { name: 'Workspaces' }).getByRole('link', { name: 'Add workspace' })
  await expect(add).toBeVisible()
  await add.click()
  await expect(page).toHaveURL(/\/workspaces\/new$/)
  const dialog = page.getByRole('dialog', { name: 'Create workspace' })
  await expect(dialog.getByLabel('Name')).toBeVisible()
  await expect(dialog.getByText(refusal)).toHaveCount(0)
})

test('workspace operator without the creation right sees no Add workspace', async ({ page }) => {
  await installQueryClientProbe(page)
  const unscopedRead = page.waitForResponse(isUnscopedIdentityResponse)
  const scopedRead = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/v1/me' &&
      new URL(response.url()).searchParams.get('workspace_id') === 'design',
  )
  await mockShell(page, { canCreate: false })
  await page.goto('/w/design/workspace')
  // The scoped projection serves the operator bundle, manage_workspace
  // included, and the unscoped read serves an explicit false.
  expect((await (await scopedRead).json()).capabilities).toContain('manage_workspace')
  expect((await (await unscopedRead).json()).can_create_workspaces).toBe(false)
  await expect.poll(() => creationQueryStatus(page)).toBe('success')

  await expect(page.getByRole('heading', { name: 'Workspace', exact: true })).toBeVisible()
  await expect(
    page.getByRole('navigation', { name: 'Workspaces' }).getByRole('link', { name: 'Add workspace' }),
  ).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'New workspace' })).toHaveCount(0)
})

test.describe('creation right hides Add workspace until a successful true read', () => {
  test('pending, then true, then a failed refetch', async ({ page }) => {
    await installQueryClientProbe(page)
    const held = deferred()
    const arrived = deferred()
    let reads = 0
    await mockShell(page, {
      unscoped: async () => {
        reads++
        if (reads === 1) {
          arrived.resolve()
          await held.promise
          return { can_create_workspaces: true }
        }
        return { status: 500 }
      },
    })
    await page.goto('/')
    const rail = page.getByRole('navigation', { name: 'Workspaces' })
    await arrived.promise
    await expect(rail.getByRole('button', { name: 'Switch to Design' })).toHaveAttribute('aria-current', 'true')
    await expect.poll(() => creationQueryStatus(page)).toBe('pending')
    await expect(rail.getByRole('link', { name: 'Add workspace' })).toHaveCount(0)

    held.resolve()
    await expect(rail.getByRole('link', { name: 'Add workspace' })).toBeVisible()

    // A failed refetch over the earlier true read withdraws the link.
    const failed = page.waitForResponse(isUnscopedIdentityResponse)
    await withQueryClient(
      page,
      (client) =>
        (client as unknown as { invalidateQueries(filter: { queryKey: unknown[] }): Promise<void> }).invalidateQueries({
          queryKey: ['caller-identity', ''],
        }),
      undefined,
    )
    expect((await failed).status()).toBe(500)
    await expect(rail.getByRole('link', { name: 'Add workspace' })).toHaveCount(0)
  })

  for (const item of [
    { name: 'a failed read', served: { status: 500 }, settled: 'error' },
    { name: 'a missing field', served: {}, settled: 'success' },
    { name: 'a non-boolean field', served: { can_create_workspaces: 'true' }, settled: 'success' },
    { name: 'an explicit false', served: { can_create_workspaces: false }, settled: 'success' },
  ] as const) {
    test(`${item.name} denies the creation right`, async ({ page }) => {
      await installQueryClientProbe(page)
      await mockShell(page, { unscoped: () => item.served })
      await page.goto('/')
      const rail = page.getByRole('navigation', { name: 'Workspaces' })
      await expect(rail.getByRole('button', { name: 'Switch to Design' })).toHaveAttribute('aria-current', 'true')
      await expect.poll(() => creationQueryStatus(page)).toBe(item.settled)
      await expect(rail.getByRole('link', { name: 'Add workspace' })).toHaveCount(0)
    })
  }
})

test('direct navigation without the creation right shows the explanation', async ({ page }) => {
  const held = deferred()
  const arrived = deferred()
  await mockShell(page, {
    unscoped: async () => {
      arrived.resolve()
      await held.promise
      return { can_create_workspaces: false }
    },
  })
  await page.goto('/workspaces/new')
  const dialog = page.getByRole('dialog', { name: 'Create workspace' })
  await arrived.promise
  await expect(dialog.getByText('Checking workspace access…')).toBeVisible()
  await expect(dialog.getByLabel('Name')).toHaveCount(0)

  held.resolve()
  await expect(dialog.getByText(refusal)).toBeVisible()
  await expect(dialog.getByLabel('Name')).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Create workspace' })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'Close' }).click()
  await expect(page).toHaveURL(/\/$/)
})

test('a refused creation shows the replacement message', async ({ page }) => {
  await mockShell(page, {
    canCreate: true,
    creates: [
      { status: 404, json: { error: 'workspace_not_found', message: 'workspace not found' } },
      { status: 409, json: { error: 'workspace_conflict', message: 'workspace could not be created' } },
      {
        status: 422,
        json: { error: 'validation_failed', fields: [{ field: 'id', message: 'must be a lowercase slug' }] },
      },
      { status: 404, body: '404 page not found' },
    ],
  })
  await page.goto('/workspaces/new')
  const dialog = page.getByRole('dialog', { name: 'Create workspace' })
  await dialog.getByLabel('Name').fill('Engineering')
  const submit = dialog.getByRole('button', { name: 'Create workspace' })

  // The server re-authorizes: a right that changed after the form opened is
  // refused with the uniform 404, which reads as the creation refusal.
  await submit.click()
  await expect(dialog.getByText(refusal)).toBeVisible()
  await expect(dialog.getByText('workspace not found')).toHaveCount(0)

  // Every other failure keeps its existing text.
  await submit.click()
  await expect(dialog.getByText('Error: workspace could not be created')).toBeVisible()
  await expect(dialog.getByText(refusal)).toHaveCount(0)
  await submit.click()
  await expect(dialog.getByText('Error: must be a lowercase slug')).toBeVisible()
  await submit.click()
  await expect(dialog.getByText('Error: 404 page not found')).toBeVisible()
  await expect(dialog.getByText(refusal)).toHaveCount(0)
})

test('the creation-right read stays unscoped', async ({ page }) => {
  const identityRequests: Array<{ query: string | null; header: string | undefined }> = []
  await mockShell(page, { canCreate: true, identityRequests })
  await page.goto('/')
  const rail = page.getByRole('navigation', { name: 'Workspaces' })
  await expect(rail.getByRole('link', { name: 'Add workspace' })).toBeVisible()
  await rail.getByRole('button', { name: 'Switch to Funnel' }).click()
  await expect(rail.getByRole('button', { name: 'Switch to Funnel' })).toHaveAttribute('aria-current', 'true')
  await expect(rail.getByRole('link', { name: 'Add workspace' })).toBeVisible()

  // The stored selection scopes the capability reads but never the creation
  // read, before and after a switch.
  await expect.poll(() => identityRequests.filter((request) => request.query === null).length).toBeGreaterThanOrEqual(2)
  const unscoped = identityRequests.filter((request) => request.query === null)
  expect(unscoped.every((request) => request.header === undefined)).toBe(true)
  expect(identityRequests.some((request) => request.query === 'design')).toBe(true)
  expect(identityRequests.some((request) => request.query === 'funnel')).toBe(true)
})
