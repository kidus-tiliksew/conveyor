import { expect, test, type Route } from '@playwright/test'

const invitationToken = 'cv_signin_invite-once'
const invitationURL = `/sign-in#token=${invitationToken}`

test('an invitation opens a scoped session, guides first token setup, and signs out safely', async ({ page }) => {
  let sessionActive = false
  let invitationUsed = false
  let sessionMutationProved = false
  let signOutProved = false
  const invitations: string[] = []
  const issuedTokens: Array<{ id: string; user_id: string; label: string; created_at: string }> = []
  const requestedURLs: string[] = []

  page.on('request', (request) => requestedURLs.push(request.url()))

  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    if (!sessionStorage.getItem('conveyor-test-initialized')) {
      sessionStorage.setItem('conveyor-test-initialized', '1')
      sessionStorage.setItem('conveyor-token', 'operator-token')
    }
  })
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const authorization = request.headers().authorization ?? ''
    const isOperator = authorization === 'Bearer operator-token'
    const isMemberSession = sessionActive && request.headers().cookie?.includes('conveyor_session=session-one')

    if (path === '/v1/sign-in/redeem' && request.method() === 'POST') {
      const body = request.postDataJSON() as { token: string }
      if (body.token !== invitationToken || invitationUsed) {
        return route.fulfill({ status: 401, body: 'invalid or expired sign-in link' })
      }
      invitationUsed = true
      sessionActive = true
      return route.fulfill({
        status: 200,
        headers: { 'Set-Cookie': 'conveyor_session=session-one; Path=/v1; HttpOnly; SameSite=Strict' },
        json: {
          user: { id: 'usr_invited', email: 'new@example.test', display_name: 'New Member' },
          expires_at: '2026-08-14T00:00:00Z',
        },
      })
    }
    if (path === '/v1/sign-out' && request.method() === 'POST') {
      signOutProved =
        request.headers()['x-conveyor-csrf'] === '1' && request.headers().origin === url.origin && authorization === ''
      sessionActive = false
      return route.fulfill({
        status: 204,
        headers: { 'Set-Cookie': 'conveyor_session=; Path=/v1; Max-Age=0; HttpOnly; SameSite=Strict' },
        body: '',
      })
    }
    if (path === '/v1/workspaces') {
      if (isOperator)
        return route.fulfill({
          json: [
            { id: 'demo', name: 'Demo' },
            { id: 'private', name: 'Private' },
          ],
        })
      if (isMemberSession) return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
      return route.fulfill({ status: 401, body: 'unauthorized' })
    }
    if (path === '/v1/workspaces/demo/members' && request.method() === 'GET') {
      return route.fulfill({
        json: [{ user_id: 'usr_owner', email: 'owner@example.test', display_name: 'Ada Owner', role: 'operator' }],
      })
    }
    if (path === '/v1/workspaces/demo/members' && request.method() === 'POST') {
      const body = request.postDataJSON() as { email: string; role: 'contributor' }
      invitations.push(body.email)
      return route.fulfill({
        status: 201,
        json: {
          email: body.email,
          role: body.role,
          delivery: 'fallback',
          sign_in_url: `${url.origin}${invitationURL}`,
        },
      })
    }
    if (path === '/v1/workspaces/demo/invitations' && request.method() === 'GET') {
      return route.fulfill({
        json: invitations.map((email) => ({
          workspace_id: 'demo',
          email,
          role: 'contributor',
          invited_by: 'usr_owner',
          invited_by_display_name: 'Ada Owner',
          created_at: '2026-08-13T00:00:00Z',
        })),
      })
    }
    if (path.endsWith('/resend') && request.method() === 'POST') {
      return route.fulfill({
        json: {
          email: 'new@example.test',
          role: 'contributor',
          delivery: 'fallback',
          sign_in_url: `${url.origin}/sign-in#token=cv_signin_invite-reissued`,
        },
      })
    }
    if (path === '/v1/tokens' && request.method() === 'GET') return route.fulfill({ json: issuedTokens })
    if (path === '/v1/tokens' && request.method() === 'POST') {
      sessionMutationProved =
        isMemberSession &&
        request.headers()['x-conveyor-csrf'] === '1' &&
        request.headers().origin === url.origin &&
        authorization === ''
      const body = request.postDataJSON() as { label: string }
      const created = {
        id: 'pat_first',
        user_id: 'usr_invited',
        label: body.label,
        created_at: '2026-08-13T01:00:00Z',
      }
      issuedTokens.push(created)
      return route.fulfill({ status: 201, json: { ...created, value: 'cv_pat_first-secret' } })
    }
    if (path === '/v1/workspace') {
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [] } })
    }
    if (path === '/v1/me') {
      return route.fulfill({
        json: isOperator
          ? { id: 'usr_owner', email: 'owner@example.test', display_name: 'Ada Owner', role: 'operator' }
          : { id: 'usr_invited', email: 'new@example.test', display_name: 'New Member', role: 'contributor' },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/workspace')
  await expect(page.getByLabel('Switch to Private')).toBeVisible()
  const invite = page.getByRole('form', { name: 'Invite a member' })
  await invite.getByLabel('Email address').fill('new@example.test')
  await invite.getByRole('button', { name: 'Invite' }).click()
  await expect(page.getByText('Invitation ready to share')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy invitation link' })).toBeVisible()
  await page.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('button', { name: 'Copy invitation link' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Resend' })).toBeVisible()

  await page.goto(invitationURL)
  await expect(page).toHaveURL(/\/settings\?welcome=true$/)
  expect(requestedURLs.filter((requestedURL) => requestedURL.includes('cv_signin_'))).toEqual([])
  await expect(page.getByText('Welcome to Conveyor')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy command' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy connection settings' })).toBeVisible()
  await expect(page.getByLabel('Switch to Demo')).toBeVisible()
  await expect(page.getByLabel('Switch to Private')).toHaveCount(0)
  await expect.poll(() => page.evaluate(() => sessionStorage.getItem('conveyor-token'))).toBeNull()

  const tokenForm = page.getByRole('form', { name: 'Create an access token' })
  await tokenForm.getByLabel('Token name').fill('My laptop')
  await tokenForm.getByRole('button', { name: 'Create token' }).click()
  await expect(page.getByText('cv_pat_first-secret')).toBeVisible()
  expect(sessionMutationProved).toBe(true)

  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByRole('heading', { name: 'Sign in to Conveyor' })).toBeVisible()
  expect(signOutProved).toBe(true)

  await page.goto(invitationURL)
  await expect(page.getByRole('heading', { name: 'This link no longer works' })).toBeVisible()
  await expect(page.getByText(/Ask your operator to resend/)).toBeVisible()

  await page.goto('/')
  await page.getByLabel('Operator token').fill('operator-token')
  await page.getByRole('button', { name: 'Continue as operator' }).click()
  await expect(page.getByLabel('Switch to Private')).toBeVisible()
})

test('a returning user signs in with a password and resets it through an operator link', async ({ page }) => {
  let password = 'first-password-value'
  let sessionActive = false
  let linkEstablished = false
  let passwordSignInProved = false
  let passwordMutationProved = false

  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const body = request.postDataJSON() as Record<string, string> | null

    if (path === '/v1/sign-in/password' && request.method() === 'POST') {
      passwordSignInProved =
        request.headers()['x-conveyor-csrf'] === '1' &&
        request.headers().origin === url.origin &&
        !request.headers().authorization
      if (body?.email !== 'returning@example.test' || body.password !== password) {
        return route.fulfill({ status: 401, body: 'invalid email or password\n' })
      }
      sessionActive = true
      linkEstablished = false
      return route.fulfill({
        status: 200,
        headers: { 'Set-Cookie': 'conveyor_session=password-session; Path=/v1; HttpOnly; SameSite=Strict' },
        json: { user: { id: 'usr_returning', email: body.email }, expires_at: '2026-08-28T00:00:00Z' },
      })
    }
    if (path === '/v1/sign-in/redeem' && request.method() === 'POST') {
      sessionActive = true
      linkEstablished = true
      return route.fulfill({
        status: 200,
        headers: { 'Set-Cookie': 'conveyor_session=link-session; Path=/v1; HttpOnly; SameSite=Strict' },
        json: { user: { id: 'usr_returning', email: 'returning@example.test' }, expires_at: '2026-08-28T00:00:00Z' },
      })
    }
    if (path === '/v1/password' && request.method() === 'POST') {
      passwordMutationProved =
        sessionActive &&
        request.headers()['x-conveyor-csrf'] === '1' &&
        request.headers().origin === url.origin &&
        !request.headers().authorization
      if (!linkEstablished && body?.current_password !== password) {
        return route.fulfill({ status: 401, body: 'current password is incorrect\n' })
      }
      password = body?.new_password ?? password
      return route.fulfill({ status: 204, body: '' })
    }
    if (path === '/v1/sign-out') {
      sessionActive = false
      return route.fulfill({
        status: 204,
        headers: { 'Set-Cookie': 'conveyor_session=; Path=/v1; Max-Age=0' },
        body: '',
      })
    }
    if (path === '/v1/workspaces') {
      return sessionActive
        ? route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
        : route.fulfill({ status: 401, body: 'unauthorized' })
    }
    if (path === '/v1/workspace')
      return route.fulfill({ json: { workspace: 'demo', max_bounces: 2, database: 'postgres', repos: [] } })
    if (path === '/v1/me')
      return route.fulfill({
        json: {
          id: 'usr_returning',
          email: 'returning@example.test',
          display_name: 'Returning User',
          role: 'contributor',
        },
      })
    if (path === '/v1/tokens') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })

  await page.goto('/sign-in')
  await expect(page.getByText(/Forgot your password/)).toBeVisible()
  const signIn = page.getByRole('form', { name: 'Password sign-in' })
  await signIn.getByLabel('Email address').fill('returning@example.test')
  await signIn.getByLabel('Password').fill(password)
  await signIn.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL('/')
  expect(passwordSignInProved).toBe(true)

  await page.goto('/settings')
  const passwordForm = page.getByRole('form', { name: 'Update password' })
  await passwordForm.getByLabel('Current password').fill(password)
  await passwordForm.getByLabel('New password').fill('second-password-value')
  await passwordForm.getByLabel('Confirm new password').fill('second-password-value')
  await passwordForm.getByRole('button', { name: 'Update password' }).click()
  await expect(page.getByText('Password updated.')).toBeVisible()
  expect(passwordMutationProved).toBe(true)

  await page.getByRole('button', { name: 'Sign out' }).click()
  await page.goto('/sign-in#token=operator-reset-link')
  await expect(page).toHaveURL(/\/settings\?welcome=true$/)
  const resetForm = page.getByRole('form', { name: 'Update password' })
  await resetForm.getByLabel('New password').fill('reset-password-value')
  await resetForm.getByLabel('Confirm new password').fill('reset-password-value')
  await resetForm.getByRole('button', { name: 'Update password' }).click()
  await expect(page.getByText('Password updated.')).toBeVisible()
  expect(password).toBe('reset-password-value')
})
