import { expect, type Locator, type Page } from '@playwright/test'
import { callerIdentity } from './caller-identity'

// Settings rows open in place (component-web-dashboard, Settings). A row's
// content stays mounted but hidden while the row is closed, so a spec that
// reaches a control opens its row first.

/** The header button of a Settings row, found by the row title. */
export function settingsRowHeader(page: Page, title: string): Locator {
  return page.locator(`button[data-settings-row="${title}"]`)
}

/** Opens a Settings row unless it is open already, and returns its region. */
export async function openSettingsRow(page: Page, title: string): Promise<Locator> {
  const header = settingsRowHeader(page, title)
  await expect(header).toBeVisible()
  if ((await header.getAttribute('aria-expanded')) !== 'true') await header.click()
  await expect(header).toHaveAttribute('aria-expanded', 'true')
  return page.getByRole('region', { name: title, exact: true })
}

// The fixture the Settings layout was measured with: two access tokens, the
// four stages and two review seats set, and one registered repository that the
// connected GitHub App covers. Every value is synthetic.
export const settingsTokens = [
  {
    id: 'pat_laptop',
    user_id: 'usr_ada',
    label: 'Laptop CLI',
    deployment_credential: false,
    created_at: '2026-10-01T08:00:00Z',
    last_used_at: '2026-11-12T16:45:00Z',
  },
  {
    id: 'pat_agent',
    user_id: 'usr_ada',
    label: 'Agent sessions',
    deployment_credential: false,
    created_at: '2026-10-20T08:00:00Z',
    last_used_at: '2026-11-13T07:30:00Z',
  },
  {
    id: 'pat_old',
    user_id: 'usr_ada',
    label: 'Old laptop',
    deployment_credential: false,
    created_at: '2026-06-01T08:00:00Z',
    last_used_at: '2026-11-13T09:00:00Z',
    revoked_at: '2026-09-01T08:00:00Z',
  },
]

export const settingsStages = {
  plan: { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
  implement: { harness: 'claude-code', model: 'claude-opus-5-5', effort: 'high' },
  verify: { harness: 'claude-code', model: 'claude-sonnet-5-5', effort: '' },
  review: {
    primary: { harness: 'claude-code', model: 'claude-opus-5-5', effort: 'high' },
    seats: [
      { harness: 'codex', model: 'gpt-6.1-sol', effort: 'high' },
      { harness: 'claude-code', model: 'claude-fable-5-2', effort: '' },
    ],
  },
}

export const settingsGitHubApp = {
  connected: true,
  app_slug: 'conveyor-demo',
  installation_account: 'demo-org',
  repositories: [{ name: 'conveyor', covered: true }],
  installation_url: 'https://github.com/apps/conveyor-demo/installations/new?state=installation-state',
  webhook: {
    receiver_url: 'https://conveyor.example/v1/github/webhook/demo',
    events: ['pull_request', 'check_suite', 'check_run', 'status', 'push', 'issues'],
    secret_configured: true,
    secret_set_at: '2026-10-30T08:00:00Z',
    last_delivery_at: '2026-11-13T07:58:00Z',
    configured: true,
  },
}

/** The synthetic value of a token the mock issues. */
export const issuedTokenValue = 'cv_pat_settings-synthetic-value'

/**
 * Serves every read the Settings page makes from the fixture above. `role`
 * sets the caller's workspace role; only `operator` holds manage_workspace.
 * `state.gets` records each GET path, and `state.writes` each other request.
 */
export async function mockSettingsPage(page: Page, { role = 'operator' }: { role?: string } = {}) {
  const state = {
    gets: [] as string[],
    writes: [] as string[],
    tokens: settingsTokens.map((item) => ({ ...item })) as Array<Record<string, unknown>>,
    stages: settingsStages as unknown,
    githubApp: settingsGitHubApp as unknown,
    githubFailure: false,
  }
  await page.addInitScript(() => {
    if (sessionStorage.getItem('settings-seeded')) return
    sessionStorage.setItem('settings-seeded', 'true')
    localStorage.setItem('conveyor-theme', 'light')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.method() === 'GET') state.gets.push(path)
    else state.writes.push(`${request.method()} ${path}`)
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity({ id: 'usr_ada', display_name: 'Ada Lovelace', email: 'ada@example.test', role }, request),
      })
    if (path === '/v1/tokens' && request.method() === 'POST') {
      const body = request.postDataJSON() as { label: string }
      const created = {
        id: `pat_${state.tokens.length + 1}`,
        user_id: 'usr_ada',
        label: body.label,
        deployment_credential: false,
        created_at: '2026-11-13T08:00:00Z',
        last_used_at: undefined as string | undefined,
      }
      state.tokens.push(created)
      // Only the issuance response carries the value; the listing never does.
      return route.fulfill({ status: 201, json: { ...created, value: issuedTokenValue } })
    }
    if (path === '/v1/tokens') return route.fulfill({ json: state.tokens })
    if (path === '/v1/me/stage-preferences')
      return route.fulfill({ json: { version: 4, updated_at: '2026-11-12T09:00:00Z', stages: state.stages } })
    if (path === '/v1/workspaces/demo/github-app') {
      if (state.githubFailure)
        return route.fulfill({ status: 403, json: { error: 'github_app_permission', message: 'refused' } })
      return route.fulfill({ json: state.githubApp })
    }
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: [] } })
    if (path === '/v1/pending-proposals')
      return route.fulfill({ json: { items: [], attention: { total: 0, task_count: 0, pending_proposal_count: 0 } } })
    return route.fulfill({ json: [] })
  })
  return state
}

/** The Settings row titles in page order. */
export function settingsRowTitles(page: Page) {
  return page
    .getByTestId('settings-scroll')
    .locator('[data-settings-row]')
    .evaluateAll((rows) => rows.map((row) => row.getAttribute('data-settings-row')))
}
