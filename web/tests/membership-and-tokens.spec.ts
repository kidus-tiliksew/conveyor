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
  resendDeliveries: Record<string, 'sent' | 'fallback'>
}

function membershipDefaults(): MembershipState {
  return {
    members: [
      { user_id: 'usr_owner', email: 'owner@example.test', display_name: 'Ada Owner', role: 'operator' },
      { user_id: 'usr_member', email: 'member@example.test', display_name: 'Bo Member', role: 'contributor' },
    ],
    invitations: [],
    soleOperator: false,
    resendDeliveries: {},
  }
}

/**
 * Mount the workspace page as either an operator or an ordinary member. The
 * member case reproduces the server contract exactly: the operator-only reads
 * answer 404, which is how the surface learns it must stay read-only.
 */
async function mockWorkspace(page: Page, role: 'operator' | 'member' | 'viewer', state: MembershipState) {
  await page.addInitScript((_sessionOnly) => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
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
    if (path === '/v1/workers')
      return route.fulfill({ json: { workers: [], worker_expected: false, worker_available: false } })

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
      const email = body.email.trim().toLowerCase()
      const member = state.members.find((candidate) => candidate.email.toLowerCase() === email)
      if (member) {
        if (state.soleOperator && member.role === 'operator' && body.role !== 'operator') {
          return route.fulfill({
            status: 409,
            json: {
              error: 'last_workspace_operator',
              message: 'cannot demote the sole workspace operator; grant another operator first',
            },
          })
        }
        member.role = body.role
        return route.fulfill({ status: 201, json: { email, role: body.role, delivery: 'sent' } })
      }
      state.invitations.push({ email, role: body.role, invited_by_display_name: 'Ada Owner' })
      return route.fulfill({ status: 201, json: { email, role: body.role, delivery: 'sent' } })
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
    if (path.endsWith('/resend') && request.method() === 'POST') {
      const email = decodeURIComponent(path.split('/').at(-2) ?? '').toLowerCase()
      const delivery = state.resendDeliveries[email] ?? 'sent'
      const membership = state.members.find((member) => member.email.toLowerCase() === email)
      const invitation = state.invitations.find((candidate) => candidate.email.toLowerCase() === email)
      return route.fulfill({
        json: {
          email,
          role: membership?.role ?? invitation?.role ?? 'contributor',
          delivery,
          ...(delivery === 'fallback' ? { sign_in_url: `https://conveyor.example/sign-in/${email}` } : {}),
        },
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

  await page.getByRole('button', { name: 'Resend' }).click()
  await expect(page.getByText('Invitation sent')).toBeVisible()

  await page.getByRole('button', { name: 'Revoke' }).click()
  await expect(page.getByText('No pending invitations.')).toBeVisible()
  await expect(page.getByText('invited@example.test')).toHaveCount(0)
})

test('an operator sends existing-member sign-in links for sent and fallback delivery', async ({ page }) => {
  const state = membershipDefaults()
  state.resendDeliveries['owner@example.test'] = 'fallback'
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  await page.getByRole('button', { name: 'Send sign-in link to Bo Member' }).click()
  await expect(page.getByText('Sign-in link sent')).toBeVisible()
  await expect(page.getByText(/get back in if they forgot their password/)).toBeVisible()
  await page.getByRole('button', { name: 'Dismiss' }).click()

  await page.getByRole('button', { name: 'Send sign-in link to Ada Owner' }).click()
  await expect(page.getByText('Sign-in link ready to share')).toBeVisible()
  await expect(page.getByText(/Email delivery is unavailable/)).toBeVisible()
  await expect(page.getByText('https://conveyor.example/sign-in/owner@example.test')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy sign-in link' })).toBeVisible()
})

test('inviting an existing member with a different role is presented as a role change', async ({ page }) => {
  const state = membershipDefaults()
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  const form = page.getByRole('form', { name: 'Invite a member' })
  await form.getByLabel('Email address').fill('MEMBER@EXAMPLE.TEST')
  await form.getByLabel('Role').selectOption('maintainer')
  await expect(form.getByText(/Role change: member@example.test already has the Contributor role/)).toBeVisible()
  await form.getByRole('button', { name: 'Change role' }).click()

  await expect(page.getByText('Role updated')).toBeVisible()
  await expect(page.getByText('member@example.test changed from Contributor to Maintainer.')).toBeVisible()
  const memberRow = page.getByText('Bo Member').locator('../..')
  await expect(memberRow.getByText('Maintainer', { exact: true })).toBeVisible()
})

test('a sole-operator role-change refusal names the existing account and attempted roles', async ({ page }) => {
  const state = { ...membershipDefaults(), soleOperator: true }
  await mockWorkspace(page, 'operator', state)
  await openMembers(page, 'operator')

  const form = page.getByRole('form', { name: 'Invite a member' })
  await form.getByLabel('Email address').fill('owner@example.test')
  await form.getByLabel('Role').selectOption('contributor')
  await expect(form.getByText(/already has the Operator role/)).toBeVisible()
  await form.getByRole('button', { name: 'Change role' }).click()

  await expect(form.getByText(/account already exists as owner@example.test with the Operator role/)).toBeVisible()
  await expect(form.getByText(/attempted role change to Contributor was refused/)).toBeVisible()
  await expect(form.getByText(/Grant another member the Operator role first/)).toBeVisible()
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
  await expect(page.getByRole('button', { name: /Send sign-in link/ })).toHaveCount(0)

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
  await expect(page.getByRole('button', { name: /Send sign-in link/ })).toHaveCount(0)
  await expect(page.getByText('Pending invitations')).toHaveCount(0)
})

test('a token secret is shown once, is never re-rendered, and the token can be revoked', async ({ page }) => {
  const secret = 'cv_pat_pat_abc123_supersecretvalue'
  const tokens: Array<{
    id: string
    user_id: string
    label: string
    deployment_credential: boolean
    created_at: string
    last_used_at?: string
    revoked_at?: string
  }> = [
    {
      id: 'pat_deployment',
      user_id: 'usr_owner',
      label: 'legacy API token',
      deployment_credential: true,
      created_at: '2026-07-31T00:00:00Z',
    },
    {
      id: 'pat_existing',
      user_id: 'usr_owner',
      label: 'Laptop CLI',
      deployment_credential: false,
      created_at: '2026-08-01T00:00:00Z',
      last_used_at: '2026-08-10T00:00:00Z',
    },
  ]
  const deleteRequests: string[] = []
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/tokens' && request.method() === 'GET') return route.fulfill({ json: tokens })
    if (path === '/v1/tokens' && request.method() === 'POST') {
      const body = request.postDataJSON() as { label: string }
      const created = {
        id: 'pat_new',
        user_id: 'usr_owner',
        label: body.label,
        deployment_credential: false,
        created_at: '2026-08-13T00:00:00Z',
      }
      tokens.push(created)
      // Only the issuance response carries the value; the listing never does.
      return route.fulfill({ status: 201, json: { ...created, value: secret } })
    }
    if (path.startsWith('/v1/tokens/') && request.method() === 'DELETE') {
      const id = decodeURIComponent(path.split('/').pop() ?? '')
      deleteRequests.push(id)
      for (const item of tokens) if (item.id === id) item.revoked_at = '2026-08-13T01:00:00Z'
      return route.fulfill({ status: 204, body: '' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/settings')
  const card = page.getByRole('form', { name: 'Create an access token' })
  await expect(page.getByText('Laptop CLI')).toBeVisible()
  await expect(page.getByText(/Last used/)).toBeVisible()
  const deploymentBadge = page.getByText('Deployment credential')
  await expect(deploymentBadge).toBeVisible()
  await expect(deploymentBadge).toHaveAttribute('title', 'The CONVEYOR_API_TOKEN mapping created at first boot.')

  await page.getByRole('button', { name: 'Revoke legacy API token' }).click()
  const deploymentDialog = page.getByRole('dialog', { name: 'Revoke deployment credential' })
  await expect(deploymentDialog).toContainText('blocks the next conveyord start')
  await expect(deploymentDialog).toContainText('Remove or change the environment variable')
  await expect(deploymentDialog).toContainText('rotate CONVEYOR_API_TOKEN and restart instead')
  const confirmation = deploymentDialog.getByLabel(/Type REVOKE DEPLOYMENT CREDENTIAL/)
  const confirmRevoke = deploymentDialog.getByRole('button', { name: 'Revoke deployment credential' })
  await expect(confirmRevoke).toBeDisabled()
  await confirmation.fill('REVOKE')
  await confirmation.press('Enter')
  expect(deleteRequests).toEqual([])
  await expect(confirmRevoke).toBeDisabled()
  await confirmation.fill('REVOKE DEPLOYMENT CREDENTIAL')
  await confirmRevoke.click()
  await expect(deploymentDialog).toHaveCount(0)
  expect(deleteRequests).toEqual(['pat_deployment'])

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
  await expect(page.getByRole('dialog', { name: 'Revoke deployment credential' })).toHaveCount(0)
  expect(deleteRequests).toEqual(['pat_deployment', 'pat_new'])
  await expect(page.getByText('Revoked')).toHaveCount(2)
  await expect(page.getByText(secret)).toHaveCount(0)
})

test('a user stores, replaces, and deletes a write-only GitHub token from settings', async ({ page }) => {
  const firstSecret = 'github_pat_synthetic_first_secret'
  const replacementSecret = 'github_pat_synthetic_replacement_secret'
  const invalidSecret = 'github_pat_synthetic_invalid_secret'
  let status: { configured: boolean; forge_login?: string; stored_at?: string } = { configured: false }
  const writes: Array<{ method: string; body?: { token: string } }> = []

  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/tokens') return route.fulfill({ json: [] })
    if (path === '/v1/forge-token' && request.method() === 'GET') return route.fulfill({ json: status })
    if (path === '/v1/forge-token' && request.method() === 'PUT') {
      const body = request.postDataJSON() as { token: string }
      writes.push({ method: 'PUT', body })
      if (body.token === invalidSecret) {
        return route.fulfill({
          status: 422,
          contentType: 'text/plain',
          body: 'forge token validation failed: authenticated forge identity read failed\n',
        })
      }
      status = {
        configured: true,
        forge_login: body.token === firstSecret ? 'octocat' : 'delivery-cat',
        stored_at: body.token === firstSecret ? '2026-08-21T12:00:00Z' : '2026-08-21T13:00:00Z',
      }
      return route.fulfill({ json: status })
    }
    if (path === '/v1/forge-token' && request.method() === 'DELETE') {
      writes.push({ method: 'DELETE' })
      status = { configured: false }
      return route.fulfill({ status: 204, body: '' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/settings')
  const card = page.getByText('GitHub token', { exact: true }).locator('../..')
  await expect(card.getByText('A GitHub token is required before you can execute tasks.')).toBeVisible()
  await expect(card).toContainText('Contents read and write')
  await expect(card).toContainText('Pull requests read and write')
  await expect(card).toContainText('Issues read and write')

  const input = card.locator('input[aria-label="GitHub token"]')
  await input.fill(firstSecret)
  await card.getByRole('button', { name: 'Store token' }).click()
  await expect(card.getByText('Connected as octocat')).toBeVisible()
  await expect(card.getByText(/Stored 8\/21\/2026/)).toBeVisible()
  await expect(card).toContainText('Issues read and write')
  await expect(input).toHaveValue('')
  expect(writes).toEqual([{ method: 'PUT', body: { token: firstSecret } }])

  await input.fill(invalidSecret)
  await card.getByRole('button', { name: 'Replace token' }).click()
  await expect(card.getByText('forge token validation failed: authenticated forge identity read failed')).toBeVisible()
  await expect(card.getByText('Connected as octocat')).toBeVisible()
  await expect(input).toHaveValue('')

  await input.fill(replacementSecret)
  await card.getByRole('button', { name: 'Replace token' }).click()
  await expect(card.getByText('Connected as delivery-cat')).toBeVisible()
  await expect(input).toHaveValue('')

  await page.reload()
  await expect(page.getByText('Connected as delivery-cat')).toBeVisible()
  await expect(page.locator('body')).not.toContainText(firstSecret)
  await expect(page.locator('body')).not.toContainText(replacementSecret)
  await expect(page.locator('body')).not.toContainText(invalidSecret)
  await expect(page.getByRole('button', { name: /reveal/i })).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete token' }).click()
  await expect(page.getByText('A GitHub token is required before you can execute tasks.')).toBeVisible()
  expect(writes.at(-1)).toEqual({ method: 'DELETE' })
})

test('the GitHub token card shows loading and status errors', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/tokens') return route.fulfill({ json: [] })
    if (path === '/v1/forge-token') {
      await new Promise((resolve) => setTimeout(resolve, 150))
      return route.fulfill({ status: 500, contentType: 'text/plain', body: 'forge token status unavailable\n' })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/settings')
  await expect(page.getByText('Loading your GitHub token status…')).toBeVisible()
  await expect(page.getByText('forge token status unavailable')).toBeVisible()
  await expect(page.getByRole('form', { name: /GitHub token/ })).toHaveCount(0)
})
