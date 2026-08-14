import { expect, type Page, type Route, test } from '@playwright/test'

const workspaceConfig = {
  workspace: 'demo',
  max_bounces: 2,
  work_order_queue_timeout: '24h',
  execution_settings: {
    control_plane: { triage: { model: 'gpt', timeout: '20m' } },
    spec: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '30m' },
    implementation: { model: 'gpt', model_policy: 'explicit', harness: 'codex', timeout: '2h' },
    review: { execution: 'mcp', timeout: '1h', fallback_model: 'gpt', fallback_harness: 'codex' },
  },
  routing: { stages: {} },
  harnesses: [],
  review: { seats: [] },
  setups: [],
  default_setup: '',
  execution: {
    spec_approval: true,
    merge_approval: true,
    implement_concurrency: 1,
    review_concurrency: 1,
    first_activity_timeout: '2m',
  },
  repos: [],
}

const notFound = { status: 404, json: { error: 'workspace_not_found', message: 'workspace not found' } }

type MembershipState = {
  members: Array<{
    user_id: string
    email: string
    display_name: string
    role: 'viewer' | 'executor' | 'contributor' | 'maintainer' | 'operator'
  }>
  invitations: Array<{
    email: string
    role: 'viewer' | 'executor' | 'contributor' | 'maintainer' | 'operator'
    invited_by_display_name: string
  }>
  soleOperator: boolean
}

function membershipDefaults(): MembershipState {
  return {
    members: [
      { user_id: 'usr_owner', email: 'owner@example.test', display_name: 'Ada Owner', role: 'operator' },
      { user_id: 'usr_member', email: 'member@example.test', display_name: 'Bo Member', role: 'contributor' },
    ],
    invitations: [],
    soleOperator: false,
  }
}

/**
 * Mount the workspace page as either an operator or an ordinary member. The
 * member case reproduces the server contract exactly: the operator-only reads
 * answer 404, which is how the surface learns it must stay read-only.
 */
async function mockWorkspace(page: Page, role: 'operator' | 'member' | 'viewer', state: MembershipState) {
  await page.addInitScript((sessionOnly) => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
    if (!sessionOnly) sessionStorage.setItem('conveyor-token', 'caller-token')
  }, role === 'viewer')
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const operatorOnly = role === 'operator'

    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') {
      const workspaceRole = role === 'operator' ? 'operator' : role === 'viewer' ? 'viewer' : 'contributor'
      return route.fulfill({
        json: { id: `usr_${role}`, email: `${role}@example.test`, display_name: role, role: workspaceRole },
      })
    }
    if (path === '/v1/activity') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') return route.fulfill({ json: { items: [], attention: { total: 0 } } })
    if (path === '/v1/task-operations') return route.fulfill({ json: { items: [], total: 0, limit: 50, offset: 0 } })
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [] } })
    if (path === '/v1/workspace/config') return route.fulfill({ json: { document: workspaceConfig, version: 1 } })
    if (path === '/v1/harness-templates')
      return operatorOnly ? route.fulfill({ json: { templates: [] } }) : route.fulfill(notFound)
    if (path === '/v1/workers') return route.fulfill({ json: { workers: [], auto_available: false } })

    if (path === '/v1/workspaces/demo/members' && request.method() === 'GET') {
      return route.fulfill({
        json: state.members.map((member) => ({ ...member, workspace_id: 'demo', created_at: '2026-08-01T00:00:00Z' })),
      })
    }
    if (path === '/v1/workspaces/demo/members' && request.method() === 'POST') {
      const body = request.postDataJSON() as {
        email: string
        role: 'viewer' | 'executor' | 'contributor' | 'maintainer' | 'operator'
      }
      state.invitations.push({ email: body.email, role: body.role, invited_by_display_name: 'Ada Owner' })
      return route.fulfill({ status: 201, json: { email: body.email, role: body.role } })
    }
    if (path.startsWith('/v1/workspaces/demo/members/') && request.method() === 'DELETE') {
      if (state.soleOperator) {
        return route.fulfill({
          status: 409,
          json: {
            error: 'last_workspace_operator',
            message: 'cannot revoke the sole workspace operator; grant another operator first',
          },
        })
      }
      const userID = decodeURIComponent(path.split('/').pop() ?? '')
      state.members = state.members.filter((member) => member.user_id !== userID)
      return route.fulfill({ status: 204, body: '' })
    }
    if (path === '/v1/workspaces/demo/invitations' && request.method() === 'GET') {
      if (!operatorOnly) return route.fulfill(notFound)
      return route.fulfill({
        json: state.invitations.map((invitation) => ({
          ...invitation,
          workspace_id: 'demo',
          invited_by: 'usr_owner',
          created_at: '2026-08-02T00:00:00Z',
        })),
      })
    }
    if (path.startsWith('/v1/workspaces/demo/invitations/') && request.method() === 'DELETE') {
      const email = decodeURIComponent(path.split('/').pop() ?? '')
      state.invitations = state.invitations.filter((invitation) => invitation.email !== email)
      return route.fulfill({ status: 204, body: '' })
    }
    return route.fulfill({ json: [] })
  })
}

async function openMembers(page: Page, role: 'operator' | 'member' | 'viewer') {
  await page.goto('/workspace')
  if (role === 'operator') await page.getByRole('tab', { name: 'Members' }).click()
}

test('an operator invites a member, sees the pending invitation, and revokes it', async ({ page }) => {
  const state = membershipDefaults()
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  await expect(page.getByText('Ada Owner')).toBeVisible()
  await expect(page.getByText('Bo Member')).toBeVisible()
  await expect(page.getByText('No pending invitations.')).toBeVisible()

  const form = page.getByRole('form', { name: 'Invite a member' })
  await form.getByLabel('Email address').fill('invited@example.test')
  await form.getByLabel('Role').selectOption('viewer')
  await form.getByRole('button', { name: 'Invite' }).click()

  await expect(page.getByText('invited@example.test')).toBeVisible()
  await expect(page.getByText('Invited by Ada Owner')).toBeVisible()

  await page.getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByText('No pending invitations.')).toBeVisible()
  await expect(page.getByText('invited@example.test')).toHaveCount(0)
})

test('an operator can invite executors and maintainers and sees their role labels', async ({ page }) => {
  const state = membershipDefaults()
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  const form = page.getByRole('form', { name: 'Invite a member' })
  for (const [email, role, label] of [
    ['executor@example.test', 'executor', 'Executor'],
    ['maintainer@example.test', 'maintainer', 'Maintainer'],
  ] as const) {
    await form.getByLabel('Email address').fill(email)
    await form.getByLabel('Role').selectOption(role)
    await form.getByRole('button', { name: 'Invite' }).click()
    const invitation = page.getByText(email).locator('../..')
    await expect(invitation).toBeVisible()
    await expect(invitation.getByText(label, { exact: true })).toBeVisible()
  }
})

test('a signed-in viewer sees the workspace and role but no mutation affordances', async ({ page }) => {
  const state = membershipDefaults()
  state.members.push({
    user_id: 'usr_viewer',
    email: 'viewer@example.test',
    display_name: 'Vi Viewer',
    role: 'viewer',
  })
  await mockWorkspace(page, 'viewer', state)
  await openMembers(page, 'viewer')

  await expect(page.getByText('Vi Viewer')).toBeVisible()
  await expect(page.getByText('Viewer', { exact: true })).toBeVisible()
  await expect(page.getByText('View only')).toBeVisible()
  await expect(page.getByRole('form', { name: 'Invite a member' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Remove Vi Viewer/ })).toHaveCount(0)

  await page.goto('/tasks')
  await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)
  await page.goto('/pending-proposals')
  await expect(page.getByRole('button', { name: 'Confirm' })).toHaveCount(0)
})

test('revoking the last operator explains what to do instead', async ({ page }) => {
  const state = { ...membershipDefaults(), soleOperator: true }
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  await page.getByRole('button', { name: 'Remove Ada Owner' }).click()
  await expect(page.getByText(/Make someone else an operator first/)).toBeVisible()
  await expect(page.getByText('Ada Owner')).toBeVisible()
})

test('a member sees the roster read-only, with no management controls', async ({ page }) => {
  const state = membershipDefaults()
  await mockWorkspace(page, 'member', state)
  await openMembers(page, 'member')

  await expect(page.getByText('Ada Owner')).toBeVisible()
  await expect(page.getByText('Bo Member')).toBeVisible()
  await expect(page.getByText('View only')).toBeVisible()
  await expect(page.getByRole('form', { name: 'Invite a member' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Remove Ada Owner' })).toHaveCount(0)
  await expect(page.getByText('Pending invitations')).toHaveCount(0)
})

test('a token secret is shown once, is never re-rendered, and the token can be revoked', async ({ page }) => {
  const secret = 'cv_pat_pat_abc123_supersecretvalue'
  const tokens: Array<{
    id: string
    user_id: string
    label: string
    created_at: string
    last_used_at?: string
    revoked_at?: string
  }> = [
    {
      id: 'pat_existing',
      user_id: 'usr_owner',
      label: 'Laptop CLI',
      created_at: '2026-08-01T00:00:00Z',
      last_used_at: '2026-08-10T00:00:00Z',
    },
  ]
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'caller-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/tokens' && request.method() === 'GET') return route.fulfill({ json: tokens })
    if (path === '/v1/tokens' && request.method() === 'POST') {
      const body = request.postDataJSON() as { label: string }
      const created = { id: 'pat_new', user_id: 'usr_owner', label: body.label, created_at: '2026-08-13T00:00:00Z' }
      tokens.push(created)
      // Only the issuance response carries the value; the listing never does.
      return route.fulfill({ status: 201, json: { ...created, value: secret } })
    }
    if (path.startsWith('/v1/tokens/') && request.method() === 'DELETE') {
      const id = decodeURIComponent(path.split('/').pop() ?? '')
      for (const item of tokens) if (item.id === id) item.revoked_at = '2026-08-13T01:00:00Z'
      return route.fulfill({ status: 204, body: '' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/settings')
  const card = page.getByRole('form', { name: 'Create an access token' })
  await expect(page.getByText('Laptop CLI')).toBeVisible()
  await expect(page.getByText(/Last used/)).toBeVisible()

  await card.getByLabel('Token name').fill('Release runner')
  await card.getByRole('button', { name: 'Create token' }).click()

  await expect(page.getByText('Copy your token now — it will not be shown again.')).toBeVisible()
  await expect(page.getByText(secret)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy token' })).toBeVisible()

  await page.getByRole('button', { name: 'Done' }).click()
  await expect(page.getByText(secret)).toHaveCount(0)
  await expect(page.getByText('Release runner')).toBeVisible()

  // A reload proves the value was never persisted anywhere the surface reads.
  await page.reload()
  await expect(page.getByText('Release runner')).toBeVisible()
  await expect(page.getByText(secret)).toHaveCount(0)

  await page.getByRole('button', { name: 'Revoke Release runner' }).click()
  await expect(page.getByText('Revoked')).toBeVisible()
  await expect(page.getByText(secret)).toHaveCount(0)
})
