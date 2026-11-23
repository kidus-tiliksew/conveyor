import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Page, test } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'
import { mockSettingsPage, openSettingsRow } from './helpers/settings'

// The visual check of every screen the MCP OAuth change alters, at 375 px and
// at a desktop width (AGENTS.md, UI changes). It runs only when
// CONVEYOR_VISUAL_CHECK_DIR names a directory outside the checkout, such as
// $CONVEYOR_TASK_CACHE/tmp/visual, so ordinary runs and CI skip it and no
// screenshot reaches the repository.
const outputDir = process.env.CONVEYOR_VISUAL_CHECK_DIR
test.skip(!outputDir, 'set CONVEYOR_VISUAL_CHECK_DIR to capture the visual check')

const widths = [375, 1280]
const consentPath =
  '/oauth/authorize?response_type=code&client_id=https%3A%2F%2Fclient.example%2Fmcp%2Fclient.json&redirect_uri=http%3A%2F%2F127.0.0.1%3A33418%2Fcallback&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256&state=visual'

const consentView = {
  client: {
    client_id: 'https://client.example/mcp/client.json',
    client_name: 'Metadata Agent',
    kind: 'metadata_document',
    origin: 'https://client.example',
  },
  redirect_host: '127.0.0.1:33418',
  user: { id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' },
  resource: 'https://conveyor.example/mcp',
}

async function mockConsentScreen(page: Page, authorize: { status: number; json: unknown }, signedIn = true) {
  await page.addInitScript(() => localStorage.setItem('conveyor-theme', 'light'))
  await page.route('**/v1/**', (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (!signedIn) return route.fulfill({ status: 401, contentType: 'text/plain', body: 'unauthorized' })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Lovelace' }, request),
      })
    if (path === '/v1/oauth/authorize') return route.fulfill(authorize)
    return route.fulfill({ json: [] })
  })
}

async function capture(page: Page, name: string, width: number) {
  const dir = outputDir as string
  mkdirSync(dir, { recursive: true })
  // Wait for images and for every finite animation or transition, so no
  // screenshot catches a dialog or chevron halfway.
  await page.waitForFunction(() => Array.from(document.images).every((image) => image.complete))
  await page.evaluate(() =>
    Promise.all(
      document
        .getAnimations()
        .filter((animation) => animation.effect?.getComputedTiming().iterations !== Number.POSITIVE_INFINITY)
        .map((animation) => animation.finished.catch(() => undefined)),
    ),
  )
  await page.screenshot({ path: join(dir, `${name}-${width}.png`), fullPage: true })
}

const grants = [
  {
    id: 'grant_new',
    client_id: 'https://client.example/mcp/client.json',
    client_name: 'Metadata Agent',
    resource: 'https://conveyor.example/mcp',
    created_at: '2026-11-20T09:00:00Z',
    last_used_at: '2026-11-22T10:30:00Z',
  },
  {
    id: 'grant_old',
    client_id: 'cvc_dynamic',
    client_name: 'Local Agent',
    resource: 'https://conveyor.example/mcp',
    created_at: '2026-11-01T08:00:00Z',
    last_used_at: null,
  },
]

for (const width of widths) {
  test(`visual: consent page at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await mockConsentScreen(page, { status: 200, json: consentView })
    await page.goto(consentPath)
    await expect(page.getByRole('button', { name: 'Approve' })).toBeVisible()
    await capture(page, 'consent', width)
  })

  test(`visual: consent page for a dynamic client at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await mockConsentScreen(page, {
      status: 200,
      json: {
        ...consentView,
        client: { client_id: 'cvc_dynamic', client_name: 'Local Agent', kind: 'dynamic_registration' },
      },
    })
    await page.goto(consentPath)
    await expect(page.getByTestId('oauth-client-dynamic')).toBeVisible()
    await capture(page, 'consent-dynamic', width)
  })

  test(`visual: invalid request error at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await mockConsentScreen(page, {
      status: 400,
      json: { error: 'invalid_client', error_description: 'the client could not be verified' },
    })
    await page.goto(consentPath)
    await expect(page.getByRole('heading', { name: 'This sign-in request cannot continue' })).toBeVisible()
    await capture(page, 'consent-error', width)
  })

  test(`visual: sign-in with a return path at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await mockConsentScreen(page, { status: 200, json: consentView }, false)
    await page.goto(consentPath)
    await expect(page.getByRole('form', { name: 'Password sign-in' })).toBeVisible()
    await capture(page, 'sign-in-return', width)
  })

  test(`visual: onboarding with a return path at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    await mockConsentScreen(page, { status: 200, json: consentView })
    await page.goto(`/onboarding?next=${encodeURIComponent(consentPath)}`)
    await expect(page.getByRole('form', { name: 'Complete your profile' })).toBeVisible()
    await capture(page, 'onboarding-return', width)
  })

  test(`visual: Settings connected clients and connection row at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1400 })
    await mockSettingsPage(page)
    await page.route('**/v1/oauth/grants', (route) => route.fulfill({ json: grants }))
    await page.goto('/w/demo/settings')
    await capture(page, 'settings-closed', width)
    await openSettingsRow(page, 'Connected MCP clients')
    await openSettingsRow(page, 'Connect a coding agent')
    await expect(page.getByTestId('oauth-grant')).toHaveCount(2)
    await capture(page, 'settings-open', width)
  })

  test(`visual: Settings welcome card at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1400 })
    await mockSettingsPage(page)
    await page.goto('/w/demo/settings?welcome=true')
    await expect(page.getByText('Welcome to Conveyor')).toBeVisible()
    await capture(page, 'settings-welcome', width)
  })

  test(`visual: MCP setup dialog at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 })
    await mockSettingsPage(page)
    await page.goto('/w/demo/')
    await page.getByRole('button', { name: 'MCP', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'MCP Setup' })
    await expect(dialog).toBeVisible()
    await capture(page, 'mcp-setup-oauth', width)
    // The dialog scrolls inside its own panel, so its end gets its own capture.
    await dialog.evaluate((panel) => panel.scrollTo(0, panel.scrollHeight))
    await capture(page, 'mcp-setup-oauth-end', width)
    await dialog.evaluate((panel) => panel.scrollTo(0, 0))
    await dialog.getByRole('button', { name: 'Access token (--auth pat)' }).click()
    await dialog.getByRole('tab', { name: 'Other' }).click()
    await page.mouse.move(0, 0)
    await capture(page, 'mcp-setup-pat', width)
    await dialog.evaluate((panel) => panel.scrollTo(0, panel.scrollHeight))
    await capture(page, 'mcp-setup-pat-end', width)
  })
}
