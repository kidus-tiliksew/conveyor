import { expect, type Page, type Route, test } from '@playwright/test'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'

// component-web-dashboard: gated controls follow the capability list that
// GET /v1/me serves for the selected workspace, and the membership picker
// follows the served role chain. The dashboard holds no role-to-capability
// table, so these cases serve lists directly, including role and capability
// strings the dashboard has never seen (req-accounts-and-membership AC-5.1).
// The server side of the contract is proven by
// internal/httpapi/identity_me_capabilities_test.go.

type Identity = { status?: number; role?: string; capabilities?: string[]; roles?: string[] }

const workspaceConfig = {
  workspace: 'alpha',
  max_bounces: 2,
  work_order_queue_timeout: '24h',
  review: { seats: [] },
  execution: { spec_approval: true, merge_approval: true, implement_concurrency: 1, review_concurrency: 1 },
  repos: [],
}

const managerCapabilities = ['view_workspace', 'manage_membership', 'manage_workspace']

type ShellOptions = {
  identities: Record<string, Identity | (() => Identity | Promise<Identity>)>
  members?: Record<string, Array<{ user_id: string; email: string; display_name: string; role: string }>>
  grants?: Array<{ workspace: string; email: string; role: string }>
  identityRequests?: string[]
}

async function mockShell(page: Page, options: ShellOptions) {
  const workspaces = Object.keys(options.identities)
  await page.addInitScript((selected) => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', selected)
  }, workspaces[0])
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: workspaces.map((id) => ({ id, name: id.charAt(0).toUpperCase() + id.slice(1), config_version: 1 })),
      })
    if (path === '/v1/me') {
      const workspace = url.searchParams.get('workspace_id') ?? ''
      options.identityRequests?.push(workspace)
      const source = options.identities[workspace]
      const identity = typeof source === 'function' ? await source() : source
      if (!identity) return route.fulfill({ status: 404, json: { error: 'workspace_not_found' } })
      if (identity.status) return route.fulfill({ status: identity.status, json: { error: 'identity unavailable' } })
      return route.fulfill({
        json: {
          id: 'usr_caller',
          email: 'caller@example.test',
          display_name: 'Caller',
          ...identity,
        },
      })
    }
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: url.searchParams.get('workspace_id'), max_bounces: 2, repos: [] } })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document: workspaceConfig, version: 1 } })
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })
    const members = /^\/v1\/workspaces\/([^/]+)\/members$/.exec(path)
    if (members && request.method() === 'GET')
      return route.fulfill({
        json: (options.members?.[members[1]] ?? []).map((member) => ({
          ...member,
          workspace_id: members[1],
          created_at: '2026-08-01T00:00:00Z',
        })),
      })
    if (members && request.method() === 'POST') {
      const body = request.postDataJSON() as { email: string; role: string }
      options.grants?.push({ workspace: members[1], email: body.email, role: body.role })
      return route.fulfill({ status: 201, json: { email: body.email, role: body.role, delivery: 'sent' } })
    }
    if (/^\/v1\/workspaces\/[^/]+\/invitations$/.test(path)) return route.fulfill({ json: [] })
    if (path === '/v1/task-operations') return route.fulfill({ json: { items: [], total: 0, limit: 50, offset: 0 } })
    return route.fulfill({ json: [] })
  })
}

async function openMembers(page: Page) {
  await page.goto('/workspace')
  await page.getByRole('tab', { name: 'Members' }).click()
  return page.getByRole('form', { name: 'Invite a member' })
}

async function refetchIdentity(page: Page) {
  await withQueryClient(
    page,
    (client) =>
      (client as unknown as { invalidateQueries(filter: { queryKey: unknown[] }): Promise<void> }).invalidateQueries({
        queryKey: ['caller-identity'],
      }),
    undefined,
  )
}

test.describe('served capabilities control affordances', () => {
  const cases: Array<{ name: string; identity: Identity; newTask: boolean; addWorkspace: boolean }> = [
    {
      name: 'same role without create_tasks',
      identity: { role: 'maintainer', capabilities: ['view_workspace', 'operate_gates'] },
      newTask: false,
      addWorkspace: false,
    },
    {
      name: 'same role with create_tasks',
      identity: { role: 'maintainer', capabilities: ['view_workspace', 'operate_gates', 'create_tasks'] },
      newTask: true,
      addWorkspace: false,
    },
    {
      name: 'future role with create_tasks',
      identity: { role: 'auditor', capabilities: ['view_workspace', 'create_tasks'], roles: ['viewer', 'auditor'] },
      newTask: true,
      addWorkspace: false,
    },
    {
      name: 'role with no capabilities field',
      identity: { role: 'operator' },
      newTask: false,
      addWorkspace: false,
    },
    {
      name: 'role with an empty capability list',
      identity: { role: 'operator', capabilities: [] },
      newTask: false,
      addWorkspace: false,
    },
    {
      name: 'failed identity read',
      identity: { status: 500 },
      newTask: false,
      addWorkspace: false,
    },
    {
      name: 'unknown extra capability',
      identity: { role: 'viewer', capabilities: ['view_workspace', 'frobnicate_tasks'] },
      newTask: false,
      addWorkspace: false,
    },
    {
      name: 'workspace management without task intake',
      identity: { role: 'viewer', capabilities: ['view_workspace', 'manage_workspace'] },
      newTask: false,
      addWorkspace: true,
    },
  ]
  for (const item of cases) {
    test(item.name, async ({ page }) => {
      const identityRequests: string[] = []
      await mockShell(page, { identities: { alpha: item.identity }, identityRequests })
      await page.goto('/')
      await expect(page.getByRole('navigation', { name: 'Workspaces' })).toBeVisible()
      await expect.poll(() => identityRequests.length).toBeGreaterThan(0)
      // Each case first shows a control that every outcome renders, so a
      // hidden gated control is a decision, not an unfinished render.
      await expect(page.getByRole('button', { name: 'Switch to Alpha' })).toHaveAttribute('aria-current', 'true')
      await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(item.newTask ? 1 : 0)
      await expect(page.getByRole('link', { name: 'Add workspace' })).toHaveCount(item.addWorkspace ? 1 : 0)
      expect(new Set(identityRequests)).toEqual(new Set(['alpha']))
    })
  }

  test('a failed refetch withdraws a previously served capability', async ({ page }) => {
    let reads = 0
    await installQueryClientProbe(page)
    await mockShell(page, {
      identities: {
        alpha: () => {
          reads++
          return reads === 1
            ? { role: 'maintainer', capabilities: ['view_workspace', 'create_tasks'] }
            : { status: 500 }
        },
      },
    })
    await page.goto('/')
    await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
    const failed = page.waitForResponse((response) => new URL(response.url()).pathname === '/v1/me')
    await refetchIdentity(page)
    expect((await failed).status()).toBe(500)
    await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)
  })
})

test.describe('served role chain drives membership picker', () => {
  test('options, labels, default, badges, and the sent role follow the chain', async ({ page }) => {
    const grants: Array<{ workspace: string; email: string; role: string }> = []
    await mockShell(page, {
      identities: {
        alpha: {
          role: 'steward',
          capabilities: managerCapabilities,
          roles: ['viewer', 'records_auditor', 'contributor', 'steward'],
        },
      },
      members: {
        alpha: [
          { user_id: 'usr_owner', email: 'owner@example.test', display_name: 'Ada Owner', role: 'steward' },
          { user_id: 'usr_member', email: 'member@example.test', display_name: 'Bo Member', role: 'contributor' },
          { user_id: 'usr_legacy', email: 'legacy@example.test', display_name: 'Cy Legacy', role: 'archivist' },
        ],
      },
      grants,
    })
    const form = await openMembers(page)
    const picker = form.getByLabel('Role')
    await expect(picker.locator('option')).toHaveText(['Viewer', 'Records Auditor', 'Contributor', 'Steward'])
    expect(
      await picker.locator('option').evaluateAll((options) => options.map((o) => (o as HTMLOptionElement).value)),
    ).toEqual(['viewer', 'records_auditor', 'contributor', 'steward'])
    await expect(picker).toHaveValue('contributor')

    const badge = (name: string, label: string) =>
      page
        .locator('div.justify-between')
        .filter({ has: page.getByText(name, { exact: true }) })
        .getByText(label, { exact: true })
    await expect(badge('Ada Owner', 'Steward')).toHaveClass(/text-primary/)
    await expect(badge('Bo Member', 'Contributor')).toHaveClass(/text-muted/)
    await expect(badge('Bo Member', 'Contributor')).not.toHaveClass(/text-primary/)
    await expect(badge('Cy Legacy', 'Archivist')).toHaveClass(/text-muted/)
    await expect(badge('Cy Legacy', 'Archivist')).not.toHaveClass(/text-primary/)

    await form.getByLabel('Email address').fill('new@example.test')
    await picker.selectOption('records_auditor')
    await form.getByRole('button', { name: 'Invite' }).click()
    await expect
      .poll(() => grants)
      .toEqual([{ workspace: 'alpha', email: 'new@example.test', role: 'records_auditor' }])
    await expect(picker).toHaveValue('contributor')
  })

  test('the first served role is the default when contributor is absent', async ({ page }) => {
    await mockShell(page, {
      identities: { alpha: { role: 'lead', capabilities: managerCapabilities, roles: ['observer', 'lead'] } },
    })
    const form = await openMembers(page)
    await expect(form.getByLabel('Role').locator('option')).toHaveText(['Observer', 'Lead'])
    await expect(form.getByLabel('Role')).toHaveValue('observer')
  })

  for (const chain of [undefined, []] as const) {
    test(`${chain ? 'an empty' : 'a missing'} chain prevents submission`, async ({ page }) => {
      const grants: Array<{ workspace: string; email: string; role: string }> = []
      await mockShell(page, {
        identities: { alpha: { role: 'operator', capabilities: managerCapabilities, roles: chain ? [] : undefined } },
        grants,
      })
      const form = await openMembers(page)
      await form.getByLabel('Email address').fill('new@example.test')
      await expect(form.getByLabel('Role').locator('option')).toHaveCount(0)
      await expect(form.getByLabel('Role')).toBeDisabled()
      await expect(form.getByRole('button', { name: 'Invite' })).toBeDisabled()
      await form.getByLabel('Email address').press('Enter')
      expect(grants).toEqual([])
    })
  }

  test('a refreshed chain reconciles a removed selection', async ({ page }) => {
    let reads = 0
    const grants: Array<{ workspace: string; email: string; role: string }> = []
    await installQueryClientProbe(page)
    await mockShell(page, {
      identities: {
        alpha: () => {
          reads++
          return {
            role: 'operator',
            capabilities: managerCapabilities,
            roles:
              reads === 1 ? ['viewer', 'auditor', 'contributor', 'operator'] : ['viewer', 'contributor', 'operator'],
          }
        },
      },
      grants,
    })
    const form = await openMembers(page)
    const picker = form.getByLabel('Role')
    await picker.selectOption('auditor')
    await expect(picker).toHaveValue('auditor')

    const refreshed = page.waitForResponse((response) => new URL(response.url()).pathname === '/v1/me')
    await refetchIdentity(page)
    await refreshed
    await expect(picker.locator('option')).toHaveText(['Viewer', 'Contributor', 'Operator'])
    await expect(picker).toHaveValue('contributor')
    await form.getByLabel('Email address').fill('new@example.test')
    await form.getByRole('button', { name: 'Invite' }).click()
    await expect.poll(() => grants).toEqual([{ workspace: 'alpha', email: 'new@example.test', role: 'contributor' }])
  })
})

test('workspace switch isolates delayed identity', async ({ page }) => {
  let releaseAlpha: () => void = () => {}
  const alphaHeld = new Promise<void>((resolve) => {
    releaseAlpha = resolve
  })
  let alphaRequested: () => void = () => {}
  const alphaArrived = new Promise<void>((resolve) => {
    alphaRequested = resolve
  })
  const grants: Array<{ workspace: string; email: string; role: string }> = []
  const identityRequests: string[] = []
  await mockShell(page, {
    identities: {
      alpha: async () => {
        alphaRequested()
        await alphaHeld
        return {
          role: 'operator',
          capabilities: [...managerCapabilities, 'create_tasks'],
          roles: ['viewer', 'alpha_only', 'operator'],
        }
      },
      beta: { role: 'warden', capabilities: managerCapabilities, roles: ['reader', 'warden'] },
    },
    members: {
      beta: [{ user_id: 'usr_beta', email: 'beta@example.test', display_name: 'Beta Owner', role: 'warden' }],
    },
    grants,
    identityRequests,
  })
  await page.goto('/')
  await alphaArrived
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Beta' }).click()
  await expect(page.getByRole('button', { name: 'Switch to Beta' })).toHaveAttribute('aria-current', 'true')
  const alphaDelivered = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/v1/me' &&
      new URL(response.url()).searchParams.get('workspace_id') === 'alpha',
  )
  // Beta's own projection grants manage_workspace and not create_tasks.
  await expect(page.getByRole('link', { name: 'Add workspace' })).toBeVisible()
  releaseAlpha()
  await alphaDelivered
  // Alpha's late projection, which grants create_tasks and another chain,
  // never reaches the beta controls.
  await expect(page.getByRole('link', { name: 'Add workspace' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)

  await page.getByRole('link', { name: 'Workspace', exact: true }).first().click()
  await page.getByRole('tab', { name: 'Members' }).click()
  const form = page.getByRole('form', { name: 'Invite a member' })
  await expect(form.getByLabel('Role').locator('option')).toHaveText(['Reader', 'Warden'])
  await expect(form.getByLabel('Role')).toHaveValue('reader')
  await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)
  await form.getByLabel('Email address').fill('new@example.test')
  await form.getByRole('button', { name: 'Invite' }).click()
  await expect.poll(() => grants).toEqual([{ workspace: 'beta', email: 'new@example.test', role: 'reader' }])
  expect(identityRequests.filter((workspace) => workspace !== 'alpha' && workspace !== 'beta')).toEqual([])
})
