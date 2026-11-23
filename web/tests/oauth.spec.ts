import { expect, type Page, type Request, type Route, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'
import { mockSettingsPage, openSettingsRow, settingsRowHeader } from './helpers/settings'

// MCP OAuth in the dashboard (req-mcp-oauth AC-3.1, AC-3.2, AC-3.4, AC-4.4,
// AC-4.5; component-web-dashboard invariants 22, 23, 25, 37; DEC-146(3)).
// Every /v1 answer is a mock in the shape that internal/httpapi/oauth.go
// serves, so these specs prove the page's behavior and request composition.
// The server suites prove code minting, the cookie, and authorization.

const redirectURI = 'http://127.0.0.1:33418/callback'
const metadataClientID = 'https://client.example/mcp/client.json'
const challenge = 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM'

// An authorization query as a client writes it. `state` holds an encoded
// slash and plus sign and a JSON-like number, `~` stays raw, and the order is
// the client's own. The router would rewrite each of these in its own search
// encoding, so the page must send this string unchanged.
const consentQuery = [
  'response_type=code',
  `client_id=${encodeURIComponent(metadataClientID)}`,
  `redirect_uri=${encodeURIComponent(redirectURI)}`,
  `code_challenge=${challenge}`,
  'code_challenge_method=S256',
  'state=1e3~a%2Fb%2Bc',
].join('&')
const consentPath = `/oauth/authorize?${consentQuery}`

const metadataView = {
  client: {
    client_id: metadataClientID,
    client_name: 'Metadata Agent',
    kind: 'metadata_document',
    origin: 'https://client.example',
  },
  redirect_host: '127.0.0.1:33418',
  user: { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' },
  resource: 'http://127.0.0.1/mcp',
}

const dynamicView = {
  client: { client_id: 'cvc_dynamic', client_name: 'Local Agent', kind: 'dynamic_registration' },
  redirect_host: '127.0.0.1:33418',
  user: { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' },
  resource: 'http://127.0.0.1/mcp',
}

const approvedRedirect = `${redirectURI}?code=cv_oac_synthetic-code&state=1e3~a%2Fb%2Bc`
const deniedRedirect = `${redirectURI}?error=access_denied&state=1e3~a%2Fb%2Bc`

interface RecordedRequest {
  method: string
  path: string
  search: string
  authorization: string
  csrf: string
  origin: string
  body: unknown
}

function record(request: Request): RecordedRequest {
  const url = new URL(request.url())
  let body: unknown
  try {
    body = request.postDataJSON()
  } catch {
    body = request.postData()
  }
  return {
    method: request.method(),
    path: url.pathname,
    search: url.search,
    authorization: request.headers().authorization ?? '',
    csrf: request.headers()['x-conveyor-csrf'] ?? '',
    origin: request.headers().origin ?? '',
    body,
  }
}

// A string body is the plain-text answer the server's http.Error writes, such
// as `internal server error` or `unauthorized`. Anything else is JSON.
function answer(status: number, body: unknown) {
  return typeof body === 'string' ? { status, contentType: 'text/plain', body } : { status, json: body }
}

interface ConsentMock {
  signedIn: boolean
  view: unknown
  authorizeStatus: number
  authorizeBody?: unknown
  decision: (
    body: Record<string, string>,
  ) => { status: number; json: unknown } | Promise<{ status: number; json: unknown }>
}

/**
 * Serves the consent and sign-in routes. `requests` records every /v1 call.
 * The client's callback is served too, so a navigation to it is observable.
 */
async function mockConsent(page: Page, overrides: Partial<ConsentMock> = {}) {
  const mock: ConsentMock = {
    signedIn: true,
    view: metadataView,
    authorizeStatus: 200,
    decision: (body) => ({
      status: 200,
      json: { redirect_to: body.decision === 'approve' ? approvedRedirect : deniedRedirect },
    }),
    ...overrides,
  }
  const requests: RecordedRequest[] = []
  const callbacks: Array<{ url: string; referer: string }> = []
  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const recorded = record(request)
    requests.push(recorded)
    const { path, method } = recorded
    if (path === '/v1/sign-in/password' && method === 'POST') {
      mock.signedIn = true
      return route.fulfill({
        json: {
          user: { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' },
          expires_at: '2026-12-01T00:00:00Z',
        },
      })
    }
    if (path === '/v1/sign-in/redeem' && method === 'POST') {
      mock.signedIn = true
      return route.fulfill({
        json: {
          user: { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' },
          expires_at: '2026-12-01T00:00:00Z',
          onboarding_required: true,
        },
      })
    }
    if (!mock.signedIn) return route.fulfill({ status: 401, contentType: 'text/plain', body: 'unauthorized' })
    if (path === '/v1/me' && method === 'PUT')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' }, request),
      })
    if (path === '/v1/password' && method === 'POST') return route.fulfill({ status: 204, body: '' })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' }, request),
      })
    if (path === '/v1/oauth/authorize' && method === 'GET') {
      if (mock.authorizeStatus !== 200) return route.fulfill(answer(mock.authorizeStatus, mock.authorizeBody))
      return route.fulfill({
        headers: { 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer' },
        json: mock.view,
      })
    }
    if (path === '/v1/oauth/authorize' && method === 'POST') {
      const decided = await mock.decision(recorded.body as Record<string, string>)
      return route.fulfill(answer(decided.status, decided.json))
    }
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })
  await page.route('http://127.0.0.1:33418/**', async (route) => {
    callbacks.push({ url: route.request().url(), referer: route.request().headers().referer ?? '' })
    return route.fulfill({
      contentType: 'text/html',
      body: '<!doctype html><title>Client callback</title><p>Client callback reached</p>',
    })
  })
  return { mock, requests, callbacks }
}

function consentReads(requests: RecordedRequest[]) {
  return requests.filter((request) => request.path === '/v1/oauth/authorize' && request.method === 'GET')
}

function decisions(requests: RecordedRequest[]) {
  return requests.filter((request) => request.path === '/v1/oauth/authorize' && request.method === 'POST')
}

// Every value the browser could have kept. None may hold a code, a state, or
// a redirect target.
async function browserStorage(page: Page) {
  return page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }))
}

const expectedDecisionFields = {
  response_type: 'code',
  client_id: metadataClientID,
  redirect_uri: redirectURI,
  code_challenge: challenge,
  code_challenge_method: 'S256',
  state: '1e3~a/b+c',
}

test.describe('OAuth consent page', () => {
  test('shows the verified request and approves it with one session decision', async ({ page }) => {
    const { requests, callbacks } = await mockConsent(page)
    await page.goto(consentPath)

    await expect(page.getByRole('heading', { name: 'Allow Metadata Agent to use Conveyor?' })).toBeVisible()
    await expect(page.getByTestId('oauth-client-name')).toHaveText('Metadata Agent')
    await expect(page.getByTestId('oauth-client-origin')).toHaveText('https://client.example')
    await expect(page.getByTestId('oauth-client-dynamic')).toHaveCount(0)
    await expect(page.getByTestId('oauth-redirect-host')).toHaveText('127.0.0.1:33418')
    await expect(page.getByTestId('oauth-signed-in-user')).toHaveText('Ada Lovelace (ada@example.test)')
    await expect(page.getByTestId('oauth-scope-notice')).toContainText("can use only Conveyor's MCP endpoint, /mcp")

    // The page reads the caller and the request, and nothing in a workspace.
    // React's development double effect can send an aborted first read, so
    // every read is checked.
    const reads = consentReads(requests)
    expect(reads.length).toBeGreaterThan(0)
    for (const read of reads) {
      expect(read.search).toBe(`?${consentQuery}`)
      expect(read.authorization).toBe('')
    }
    expect(requests.some((request) => request.path === '/v1/me' && request.search === '')).toBe(true)
    expect(requests.filter((request) => request.path.startsWith('/v1/workspace'))).toEqual([])

    const appOrigin = new URL(page.url()).origin
    await page.getByRole('button', { name: 'Approve' }).click()
    await expect(page).toHaveURL(approvedRedirect)
    await expect(page.getByText('Client callback reached')).toBeVisible()

    const sent = decisions(requests)
    expect(sent).toHaveLength(1)
    expect(sent[0].csrf).toBe('1')
    expect(sent[0].authorization).toBe('')
    expect(sent[0].origin).toBe(appOrigin)
    expect(sent[0].search).toBe('')
    // An omitted resource stays omitted, so the server applies its default.
    expect(sent[0].body).toEqual({ ...expectedDecisionFields, decision: 'approve' })
    // The callback navigation sends no referrer, so the consent query stays here.
    expect(callbacks).toEqual([{ url: approvedRedirect, referer: '' }])
  })

  test('denies with an explicit decision and follows only the server answer', async ({ page }) => {
    const { requests, callbacks } = await mockConsent(page)
    await page.goto(consentPath)
    await page.getByRole('button', { name: 'Deny' }).click()
    await expect(page).toHaveURL(deniedRedirect)
    const sent = decisions(requests)
    expect(sent).toHaveLength(1)
    expect(sent[0].body).toEqual({ ...expectedDecisionFields, decision: 'deny' })
    expect(sent[0].csrf).toBe('1')
    expect(sent[0].authorization).toBe('')
    expect(callbacks.map((callback) => callback.url)).toEqual([deniedRedirect])
  })

  test('labels a dynamically registered client and passes an explicit resource through', async ({ page }) => {
    const { requests } = await mockConsent(page, { view: dynamicView })
    const query = `${consentQuery.replace(encodeURIComponent(metadataClientID), 'cvc_dynamic')}&resource=${encodeURIComponent('http://127.0.0.1/mcp')}`
    await page.goto(`/oauth/authorize?${query}`)
    await expect(page.getByTestId('oauth-client-name')).toHaveText('Local Agent')
    await expect(page.getByTestId('oauth-client-dynamic')).toContainText('Dynamic registration')
    await expect(page.getByTestId('oauth-client-origin')).toHaveCount(0)
    expect(consentReads(requests)[0].search).toBe(`?${query}`)
    await page.getByRole('button', { name: 'Approve' }).click()
    await expect(page).toHaveURL(approvedRedirect)
    expect(decisions(requests)[0].body).toEqual({
      ...expectedDecisionFields,
      client_id: 'cvc_dynamic',
      resource: 'http://127.0.0.1/mcp',
      decision: 'approve',
    })
  })

  test('disables both buttons while one decision is pending', async ({ page }) => {
    let release: () => void = () => {}
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    const { requests } = await mockConsent(page, {
      decision: async () => {
        await held
        return { status: 200, json: { redirect_to: approvedRedirect } }
      },
    })
    await page.goto(consentPath)
    const approve = page.getByRole('button', { name: 'Approve' })
    await approve.click()
    await expect(page.getByRole('button', { name: 'Approving…' })).toBeDisabled()
    await expect(page.getByRole('button', { name: 'Deny' })).toBeDisabled()
    await page.getByRole('button', { name: 'Approving…' }).click({ force: true })
    release()
    await expect(page).toHaveURL(approvedRedirect)
    expect(decisions(requests)).toHaveLength(1)
  })

  test('keeps codes, states, and redirect targets out of browser storage', async ({ page }) => {
    await mockConsent(page)
    await page.goto(consentPath)
    await expect(page.getByTestId('oauth-client-name')).toBeVisible()
    const before = await browserStorage(page)
    for (const secret of ['cv_oac_', '1e3~a', '127.0.0.1:33418', challenge]) expect(before).not.toContain(secret)
  })

  // req-mcp-oauth AC-3.4: every refusal of the consent read stays on
  // Conveyor's page with no decision controls and no outbound navigation,
  // including a refusal that carries a redirect_to.
  const readRefusals: Array<{ name: string; status: number; body: unknown; text: string }> = [
    {
      name: 'an unverified client',
      status: 400,
      body: { error: 'invalid_client', error_description: 'the client could not be verified' },
      text: 'The client could not be verified.',
    },
    {
      name: 'an unregistered redirect URI',
      status: 400,
      body: { error: 'invalid_redirect_uri', error_description: 'the redirect URI is not registered for this client' },
      text: 'The redirect URI is not registered for this client.',
    },
    {
      name: 'a malformed query',
      status: 400,
      body: { error: 'invalid_request', error_description: 'client_id appears more than once' },
      text: 'Client_id appears more than once.',
    },
    {
      name: 'a wrong resource with a redirect',
      status: 400,
      body: {
        error: 'invalid_target',
        error_description: 'resource must be http://127.0.0.1/mcp',
        redirect_to: `${redirectURI}?error=invalid_target&state=x`,
      },
      text: 'Resource must be http://127.0.0.1/mcp.',
    },
    {
      name: 'OAuth turned off',
      status: 404,
      body: { error: 'oauth_unavailable' },
      text: 'This Conveyor server does not offer MCP sign-in.',
    },
    { name: 'a server failure', status: 500, body: 'internal server error', text: 'internal server error' },
  ]
  for (const refusal of readRefusals) {
    test(`shows ${refusal.name} on Conveyor's page`, async ({ page }) => {
      const { requests, callbacks } = await mockConsent(page, {
        authorizeStatus: refusal.status,
        authorizeBody: refusal.body,
      })
      await page.goto(consentPath)
      await expect(page.getByRole('heading', { name: 'This sign-in request cannot continue' })).toBeVisible()
      await expect(page.getByRole('alert')).toContainText(refusal.text)
      await expect(page.getByRole('button', { name: 'Approve' })).toHaveCount(0)
      await expect(page.getByRole('button', { name: 'Deny' })).toHaveCount(0)
      expect(new URL(page.url()).pathname).toBe('/oauth/authorize')
      expect(decisions(requests)).toEqual([])
      expect(callbacks).toEqual([])
    })
  }

  test('sends an explicit empty resource as written and shows its refusal', async ({ page }) => {
    const { requests, callbacks } = await mockConsent(page, {
      authorizeStatus: 400,
      authorizeBody: {
        error: 'invalid_target',
        error_description: 'resource must be http://127.0.0.1/mcp',
        redirect_to: `${redirectURI}?error=invalid_target`,
      },
    })
    await page.goto(`${consentPath}&resource=`)
    await expect(page.getByRole('alert')).toContainText('Resource must be')
    expect(consentReads(requests)[0].search).toBe(`?${consentQuery}&resource=`)
    expect(callbacks).toEqual([])
  })

  // A refused or unsafe decision answer never navigates, and the person can
  // try again.
  const decisionRefusals: Array<{ name: string; status: number; json: unknown; text: string }> = [
    {
      name: 'a refused decision with a redirect',
      status: 400,
      json: {
        error: 'invalid_target',
        error_description: 'resource must be http://127.0.0.1/mcp',
        redirect_to: deniedRedirect,
      },
      text: 'Resource must be http://127.0.0.1/mcp.',
    },
    { name: 'a server failure', status: 500, json: 'internal server error', text: 'internal server error' },
    { name: 'an ended session', status: 401, json: 'unauthorized', text: 'Your sign-in session ended.' },
    {
      name: 'an answer with no redirect',
      status: 200,
      json: {},
      text: 'Conveyor did not return the address of the client.',
    },
    {
      name: 'a redirect to another host',
      status: 200,
      json: { redirect_to: 'https://evil.example/callback?code=x' },
      text: 'not safe',
    },
    {
      name: 'a redirect to another port',
      status: 200,
      json: { redirect_to: 'http://127.0.0.1:1/callback?code=x' },
      text: 'not safe',
    },
    {
      name: 'a redirect to another path',
      status: 200,
      json: { redirect_to: 'http://127.0.0.1:33418/other?code=x' },
      text: 'not safe',
    },
    {
      name: 'a redirect that extends the path',
      status: 200,
      json: { redirect_to: 'http://127.0.0.1:33418/callbackX?code=x' },
      text: 'not safe',
    },
    { name: 'a script address', status: 200, json: { redirect_to: 'javascript:alert(1)' }, text: 'not safe' },
    {
      name: 'a plain http address off loopback',
      status: 200,
      json: { redirect_to: 'http://client.example/callback?code=x' },
      text: 'not safe',
    },
    {
      name: 'a redirect with user information',
      status: 200,
      json: { redirect_to: 'http://user@127.0.0.1:33418/callback?code=x' },
      text: 'not safe',
    },
  ]
  for (const refusal of decisionRefusals) {
    test(`does not navigate after ${refusal.name}`, async ({ page }) => {
      const { requests, callbacks } = await mockConsent(page, {
        decision: () => ({ status: refusal.status, json: refusal.json }),
      })
      await page.route('https://evil.example/**', (route) => route.fulfill({ body: 'evil' }))
      await page.goto(consentPath)
      await page.getByRole('button', { name: 'Approve' }).click()
      await expect(page.getByRole('alert')).toContainText(refusal.text)
      await expect(page.getByRole('button', { name: 'Approve' })).toBeEnabled()
      await expect(page.getByRole('button', { name: 'Deny' })).toBeEnabled()
      expect(new URL(page.url()).pathname).toBe('/oauth/authorize')
      expect(decisions(requests)).toHaveLength(1)
      expect(callbacks).toEqual([])
    })
  }
})

// A valid authorization query longer than 16 KiB. Its `state` is 2,000
// percent-encoded slashes, and an unknown field that the server ignores adds
// 12,000 characters. The decoded decision body stays far under its 16 KiB
// limit, so the return path must keep this request whole (req-mcp-oauth
// AC-3.1).
const longQuery = `${consentQuery.replace('state=1e3~a%2Fb%2Bc', `state=${'%2F'.repeat(2_000)}`)}&client_note=${'x'.repeat(12_000)}`

// Node's HTTP parser, which the Vite dev server uses, refuses a request line
// over 16 KiB. The browser keeps the long address, and this route answers its
// document request with the dashboard document of the short path. Every /v1
// read is mocked, so no long address reaches a server.
async function serveLongDocuments(page: Page) {
  await page.route(
    (url) => ['/oauth/authorize', '/sign-in', '/onboarding'].includes(url.pathname) && url.href.length > 8_000,
    async (route) => {
      const url = new URL(route.request().url())
      return route.fulfill({ response: await route.fetch({ url: `${url.origin}${url.pathname}` }) })
    },
  )
}

const returnCases = [
  { name: '', query: consentQuery },
  { name: ' with a query longer than 16 KiB', query: longQuery },
]

test.describe('OAuth sign-in return path', () => {
  test('the long test query is above the old 16 KiB cutoff', () => {
    expect(`/oauth/authorize?${longQuery}`.length).toBeGreaterThan(16_384)
  })

  for (const { name, query } of returnCases) {
    const path = `/oauth/authorize?${query}`

    test(`a signed-out browser signs in with a password and returns to the exact request${name}`, async ({ page }) => {
      const { requests } = await mockConsent(page, { signedIn: false })
      await serveLongDocuments(page)
      await page.goto(path)
      await expect(page).toHaveURL(/\/sign-in\?/)
      // `next` is encoded once and decodes to the exact path and query.
      const signIn = new URL(page.url())
      expect(signIn.pathname).toBe('/sign-in')
      expect(signIn.searchParams.getAll('next')).toEqual([path])
      expect(signIn.hash).toBe('')

      const form = page.getByRole('form', { name: 'Password sign-in' })
      await form.getByLabel('Email address').fill('ada@example.test')
      await form.getByLabel('Password').fill('a-long-password')
      const returned = page.waitForRequest((request) => request.url().includes('/v1/oauth/authorize?'))
      await form.getByRole('button', { name: 'Sign in' }).click()
      await returned
      await expect(page.getByTestId('oauth-client-name')).toHaveText('Metadata Agent')
      const reads = consentReads(requests)
      expect(reads.at(-1)?.search).toBe(`?${query}`)
    })

    test(`a sign-in link returns through onboarding to the exact request${name}`, async ({ page }) => {
      const { requests } = await mockConsent(page, { signedIn: false })
      await serveLongDocuments(page)
      const seen: string[] = []
      page.on('framenavigated', (frame) => {
        if (frame === page.mainFrame()) seen.push(frame.url())
      })
      await page.goto(path)
      await expect(page).toHaveURL(/\/sign-in\?/)
      const signInURL = new URL(page.url())
      await page.goto(`${signInURL.pathname}${signInURL.search}#token=cv_signin_synthetic`)
      await expect(page).toHaveURL(/\/onboarding\?/)
      const onboarding = new URL(page.url())
      expect(onboarding.searchParams.getAll('next')).toEqual([path])
      // The redemption token stays in no address after the fragment is removed.
      expect(page.url()).not.toContain('cv_signin_')

      const form = page.getByRole('form', { name: 'Complete your profile' })
      await form.getByLabel('Display name').fill('Ada Lovelace')
      await form.getByLabel('New password', { exact: true }).fill('a-long-password')
      await form.getByLabel('Confirm new password').fill('a-long-password')
      await form.getByRole('button', { name: 'Complete setup' }).click()
      await expect(page.getByTestId('oauth-client-name')).toHaveText('Metadata Agent')
      expect(consentReads(requests).at(-1)?.search).toBe(`?${query}`)
      expect(seen.filter((url) => url.includes('/oauth/authorize')).every((url) => !url.includes('cv_signin_'))).toBe(
        true,
      )
      expect(requests.every((request) => !request.search.includes('cv_signin_'))).toBe(true)
    })
  }
})

// Every spelling of `next` that is not exactly the consent page, with an
// optional query, is refused (req-mcp-oauth AC-3.1; component-web-dashboard
// invariants 22 and 23). Each value is the decoded `next` parameter.
const refusedNext: Array<[string, string]> = [
  ['an absolute URL', 'https://evil.example/oauth/authorize?x=1'],
  ['an absolute URL on this host', 'http://127.0.0.1/oauth/authorize'],
  ['a protocol-relative URL', '//evil.example/oauth/authorize'],
  ['three slashes', '///evil.example/oauth/authorize'],
  ['a slash and backslash', '/\\evil.example/oauth/authorize'],
  ['two backslashes', '\\\\evil.example/oauth/authorize'],
  ['a backslash in the path', '/oauth\\authorize'],
  ['a backslash after the path', '/oauth/authorize\\'],
  ['a backslash in the query', '/oauth/authorize?x=\\'],
  ['an encoded slash in the path', '/oauth%2Fauthorize'],
  ['an encoded double slash', '/%2F%2Fevil.example'],
  ['an encoded lowercase slash', '/oauth%2fauthorize'],
  ['an encoded backslash', '/%5Cevil.example'],
  ['an encoded backslash in the path', '/oauth%5Cauthorize'],
  ['an encoded letter', '/%6Fauth/authorize'],
  ['an encoded suffix', '/oauth/authorize%3Fx=1'],
  ['a line feed', '/oauth/authorize\n'],
  ['a carriage return in the query', '/oauth/authorize?x=1\r\n'],
  ['a null character', '/oauth/authorize?x=\u0000'],
  ['a tab before slashes', '/\t/evil.example'],
  ['a leading tab', '\t/oauth/authorize'],
  ['a delete character', '/oauth/authorize?x=\u007f'],
  ['a space', '/oauth/authorize ?x=1'],
  ['a leading space', ' /oauth/authorize'],
  ['a non-ASCII character', '/oauth/authorize?x=é'],
  ['a full-width question mark', '/oauth/authorize？x=1'],
  ['a quote the browser encodes', '/oauth/authorize?x="y"'],
  ['a different path', '/settings'],
  ['a workspace path', '/w/demo/settings'],
  ['the root path', '/'],
  ['a path prefix', '/oauth/authorizeX'],
  ['a trailing slash', '/oauth/authorize/'],
  ['a dot segment', '/oauth/authorize/../settings'],
  ['a parent dot segment', '/oauth/../oauth/authorize'],
  ['an upper-case path', '/OAUTH/AUTHORIZE'],
  ['a file extension', '/oauth/authorize.json'],
  ['a fragment', '/oauth/authorize#token=x'],
  ['a fragment after a query', '/oauth/authorize?x=1#frag'],
  ['an empty query and fragment', '/oauth/authorize?#'],
  ['an empty query', '/oauth/authorize?'],
  ['a script URL', 'javascript:alert(1)'],
  ['a data URL', 'data:text/html,hello'],
  ['a relative path', 'oauth/authorize'],
  ['an empty value', ''],
]

const acceptedNext = [
  '/oauth/authorize',
  consentPath,
  '/oauth/authorize?state=a%2Fb%5Cc&x=%2F%2Fevil.example',
  // Above the old 16 KiB cutoff: a valid request is kept at any size.
  `/oauth/authorize?${longQuery}`,
]

test('the return path validator accepts only the consent page', async ({ page }) => {
  await page.route('**/v1/**', (route) => route.fulfill({ status: 401, body: 'unauthorized' }))
  await page.goto('/sign-in')
  const modulePath = '/src/lib/api.ts'
  const results = await page.evaluate(
    async ({ path, refused, accepted }) => {
      const api = await import(/* @vite-ignore */ path)
      return {
        refused: refused.map(([name, value]) => [name, api.safeOAuthReturnPath(value) ?? null]),
        accepted: accepted.map((value) => api.safeOAuthReturnPath(value) ?? null),
        twice: api.oauthReturnPathFromSearch('?next=%2Foauth%2Fauthorize&next=%2Foauth%2Fauthorize') ?? null,
        once: api.oauthReturnPathFromSearch(`?next=${encodeURIComponent(accepted[1])}`) ?? null,
        notString: [undefined, null, 1, ['/oauth/authorize']].map((value) => api.safeOAuthReturnPath(value) ?? null),
      }
    },
    { path: modulePath, refused: refusedNext, accepted: acceptedNext },
  )
  expect(results.refused).toEqual(refusedNext.map(([name]) => [name, null]))
  expect(results.accepted).toEqual(acceptedNext)
  expect(results.twice).toBeNull()
  expect(results.once).toBe(consentPath)
  expect(results.notString).toEqual([null, null, null, null])
})

// The same table through each page: a refused `next` falls back to `/`.
async function mockSignedInShell(page: Page) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const outbound: string[] = []
  await page.route(/^https?:\/\/(?!127\.0\.0\.1)/, (route) => {
    outbound.push(route.request().url())
    return route.abort()
  })
  return { outbound, ...(await mockConsent(page)) }
}

for (const [name, value] of refusedNext) {
  test(`password sign-in ignores ${name} in next`, async ({ page }) => {
    const { outbound, requests } = await mockSignedInShell(page)
    await page.goto(`/sign-in?next=${encodeURIComponent(value)}`)
    const form = page.getByRole('form', { name: 'Password sign-in' })
    await form.getByLabel('Email address').fill('ada@example.test')
    await form.getByLabel('Password').fill('a-long-password')
    await form.getByRole('button', { name: 'Sign in' }).click()
    await expect(page).toHaveURL(/\/w\/demo\/$/)
    expect(consentReads(requests)).toEqual([])
    expect(outbound).toEqual([])
  })

  test(`onboarding ignores ${name} in next`, async ({ page }) => {
    const { outbound, requests } = await mockSignedInShell(page)
    await page.goto(`/onboarding?next=${encodeURIComponent(value)}`)
    const form = page.getByRole('form', { name: 'Complete your profile' })
    await form.getByLabel('Display name').fill('Ada Lovelace')
    await form.getByLabel('New password', { exact: true }).fill('a-long-password')
    await form.getByLabel('Confirm new password').fill('a-long-password')
    await form.getByRole('button', { name: 'Complete setup' }).click()
    await expect(page).toHaveURL(/\/w\/demo\/$/)
    expect(consentReads(requests)).toEqual([])
    expect(outbound).toEqual([])
  })
}

test('a sign-in link with a refused next opens onboarding without it', async ({ page }) => {
  await mockConsent(page, { signedIn: false })
  await page.goto(`/sign-in?next=${encodeURIComponent('//evil.example/oauth/authorize')}#token=cv_signin_synthetic`)
  await expect(page).toHaveURL(/\/onboarding$/)
})

// Settings: the connected MCP clients card (req-mcp-oauth AC-4.4, AC-4.5).
const grants = [
  {
    id: 'grant/new one',
    client_id: metadataClientID,
    client_name: 'Metadata Agent',
    resource: 'http://127.0.0.1/mcp',
    created_at: '2026-11-20T09:00:00Z',
    last_used_at: '2026-11-22T10:30:00Z',
    // A field the served projection never has. The card must not show it.
    refresh_token_hash: 'deadbeefdeadbeefdeadbeef',
  },
  {
    id: 'grant_old',
    client_id: 'cvc_dynamic',
    client_name: '',
    resource: 'http://127.0.0.1/mcp',
    created_at: '2026-11-01T08:00:00Z',
    last_used_at: null,
  },
]

async function mockGrants(
  page: Page,
  {
    listStatus = 200,
    listBody,
    deleteStatus = 200,
  }: { listStatus?: number; listBody?: unknown; deleteStatus?: number } = {},
) {
  const settings = await mockSettingsPage(page)
  const state = { grants: grants.map((grant) => ({ ...grant })), deletes: [] as RecordedRequest[], lists: 0 }
  await page.route('**/v1/oauth/grants**', async (route) => {
    const request = route.request()
    const recorded = record(request)
    if (request.method() === 'GET') {
      state.lists++
      if (listStatus !== 200) return route.fulfill(answer(listStatus, listBody))
      return route.fulfill({ headers: { 'Cache-Control': 'no-store' }, json: state.grants })
    }
    if (request.method() === 'DELETE') {
      state.deletes.push(recorded)
      if (deleteStatus !== 200)
        return route.fulfill({ status: deleteStatus, contentType: 'text/plain', body: 'internal server error' })
      const id = decodeURIComponent(recorded.path.replace('/v1/oauth/grants/', ''))
      const revoked = state.grants.find((grant) => grant.id === id)
      state.grants = state.grants.filter((grant) => grant.id !== id)
      return route.fulfill({ json: { ...revoked, revoked_at: '2026-11-23T00:00:00Z' } })
    }
    return route.fallback()
  })
  return { settings, state }
}

test.describe('Connected MCP clients', () => {
  test('lists grants without token values and revokes the selected one', async ({ page }) => {
    const { state } = await mockGrants(page)
    await page.goto('/w/demo/settings')
    const header = settingsRowHeader(page, 'Connected MCP clients')
    await expect(header).toContainText('2 clients · Last used')
    const region = await openSettingsRow(page, 'Connected MCP clients')
    const rows = region.getByTestId('oauth-grant')
    await expect(rows).toHaveCount(2)
    await expect(rows.nth(0)).toContainText('Metadata Agent')
    await expect(rows.nth(0)).toContainText('Connected Nov 20, 2026')
    await expect(rows.nth(0)).toContainText('Last used Nov 22, 2026')
    await expect(rows.nth(1)).toContainText('Unnamed MCP client')
    await expect(rows.nth(1)).toContainText('Never used')
    const text = await page.locator('body').innerText()
    for (const hidden of ['deadbeef', 'cv_oat_', 'cv_ort_', 'grant/new one', 'grant_old'])
      expect(text).not.toContain(hidden)

    await region.getByRole('button', { name: 'Revoke Metadata Agent' }).click()
    await expect(rows).toHaveCount(1)
    await expect(rows.nth(0)).toContainText('Unnamed MCP client')
    expect(state.deletes).toHaveLength(1)
    expect(state.deletes[0].path).toBe(`/v1/oauth/grants/${encodeURIComponent('grant/new one')}`)
    expect(state.deletes[0].csrf).toBe('1')
    expect(state.deletes[0].authorization).toBe('')
    expect(state.lists).toBeGreaterThanOrEqual(2)

    // The access token card beside it still lists and creates tokens.
    const tokens = await openSettingsRow(page, 'Access tokens')
    await expect(tokens.getByText('Laptop CLI')).toBeVisible()
    await tokens.getByLabel('Token name').fill('New laptop')
    await tokens.getByRole('button', { name: 'Create token' }).click()
    await expect(tokens.getByText('cv_pat_settings-synthetic-value')).toBeVisible()
  })

  test('keeps a grant and shows the error when revocation fails', async ({ page }) => {
    const { state } = await mockGrants(page, { deleteStatus: 500 })
    await page.goto('/w/demo/settings')
    const region = await openSettingsRow(page, 'Connected MCP clients')
    await region.getByRole('button', { name: 'Revoke Metadata Agent' }).click()
    await expect(region.getByRole('alert')).toContainText('internal server error')
    await expect(region.getByTestId('oauth-grant')).toHaveCount(2)
    expect(state.deletes).toHaveLength(1)
  })

  test('tells an empty list from a failed read', async ({ page }) => {
    await mockGrants(page, { listStatus: 500, listBody: 'internal server error' })
    await page.goto('/w/demo/settings')
    await expect(settingsRowHeader(page, 'Connected MCP clients')).toContainText(
      'Conveyor could not read your connected clients.',
    )
    const region = await openSettingsRow(page, 'Connected MCP clients')
    await expect(region.getByRole('alert')).toContainText('internal server error')
    await expect(region.getByText('No MCP client is connected.')).toHaveCount(0)
  })

  test('shows an empty list', async ({ page }) => {
    const { state } = await mockGrants(page)
    state.grants = []
    await page.goto('/w/demo/settings')
    await expect(settingsRowHeader(page, 'Connected MCP clients')).toContainText('No MCP client is connected.')
  })

  test('says when the server offers no MCP sign-in', async ({ page }) => {
    await mockGrants(page, { listStatus: 404, listBody: { error: 'oauth_unavailable' } })
    await page.goto('/w/demo/settings')
    await expect(settingsRowHeader(page, 'Connected MCP clients')).toContainText(
      'This server does not offer MCP sign-in.',
    )
  })
})
