import { expect, type Locator, type Page, test } from '@playwright/test'
import type { StagePreferenceStages, StagePreferences } from '../src/lib/stage-preferences'
import { callerIdentity } from './helpers/caller-identity'

// The Settings stage preferences card (component-stage-preferences;
// component-web-dashboard invariant 25; req-stage-preferences AC-2.1, AC-2.4).
// The mock serves GET and PUT /v1/me/stage-preferences in the shape the server
// returns (internal/httpapi/stage_preferences.go) and keeps the server's
// If-Match rule: a stale version answers 412 and changes nothing.

const unset: StagePreferenceStages = { plan: null, implement: null, verify: null, review: null }

const savedSet: StagePreferenceStages = {
  plan: { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
  implement: { harness: 'claude-code', model: 'claude-opus-5-5', effort: 'high' },
  verify: null,
  review: {
    primary: { harness: 'claude-code', model: 'claude-opus-5-5', effort: '' },
    seats: [
      { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
      { harness: 'claude-code', model: 'claude-sonnet-5-5', effort: '' },
    ],
  },
}

type MockState = {
  set: StagePreferences
  puts: Array<{ ifMatch: string | undefined; csrf: string | undefined; body: { stages: StagePreferenceStages } }>
  gets: string[]
  // Each queued answer replaces the next PUT's normal handling.
  putAnswers: Array<{ status: number; json: unknown; changeTo?: StagePreferenceStages }>
  getFailures: number
}

async function mockSettings(page: Page, stages: StagePreferenceStages = unset, version = 0) {
  const state: MockState = {
    set: { version, updated_at: version ? '2026-11-12T09:00:00Z' : null, stages },
    puts: [],
    gets: [],
    putAnswers: [],
    getFailures: 0,
  }
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-theme', 'dark')
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'second', name: 'Second' },
        ],
      })
    if (path === '/v1/me')
      return route.fulfill({
        json: callerIdentity(
          { id: 'usr_ada', display_name: 'Ada', email: 'ada@example.test', role: 'viewer' },
          request,
        ),
      })
    if (path === '/v1/me/stage-preferences') {
      if (request.method() === 'GET') {
        state.gets.push(url.search)
        if (state.getFailures > 0) {
          state.getFailures--
          return route.fulfill({ status: 500, contentType: 'text/plain', body: 'internal server error\n' })
        }
        return route.fulfill({ json: state.set })
      }
      const headers = request.headers()
      const body = request.postDataJSON() as { stages: StagePreferenceStages }
      state.puts.push({ ifMatch: headers['if-match'], csrf: headers['x-conveyor-csrf'], body })
      const answer = state.putAnswers.shift()
      if (answer) {
        if (answer.changeTo) {
          state.set = { version: state.set.version + 1, updated_at: '2026-11-12T10:00:00Z', stages: answer.changeTo }
        }
        return route.fulfill({ status: answer.status, json: answer.json })
      }
      if (headers['if-match'] !== `"${state.set.version}"`)
        return route.fulfill({
          status: 412,
          json: { error: 'version_conflict', message: 'Your stage preferences changed since you read them.' },
        })
      state.set = { version: state.set.version + 1, updated_at: '2026-11-12T10:00:00Z', stages: body.stages }
      return route.fulfill({ json: state.set })
    }
    if (path === '/v1/tokens') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') return route.fulfill({ json: { items: [], attention: { total: 0 } } })
    if (path.endsWith('/github-app')) return route.fulfill({ status: 404, json: { error: 'workspace_not_found' } })
    return route.fulfill({ json: [] })
  })
  return state
}

function card(page: Page) {
  return page
    .locator('div')
    .filter({ has: page.getByText('Stage preferences', { exact: true }) })
    .filter({ has: page.getByRole('form', { name: 'Edit stage preferences' }) })
    .last()
}

function stage(page: Page, name: string): Locator {
  return page.getByRole('region', { name: `${name} stage` })
}

async function fillEntry(page: Page, label: string, harness: string, model: string, effort = '') {
  await page.getByLabel(`${label} harness`, { exact: true }).fill(harness)
  await page.getByLabel(`${label} model`, { exact: true }).fill(model)
  await page.getByLabel(`${label} effort`, { exact: true }).fill(effort)
}

test('a person with no saved preferences sees every stage unset', async ({ page }) => {
  const state = await mockSettings(page)
  await page.goto('/w/demo/settings')
  await expect(page.getByText('These preferences apply in every workspace.')).toBeVisible()
  for (const name of ['Plan', 'Implement', 'Verify', 'Review']) {
    await expect(stage(page, name).getByText('Not set').first()).toBeVisible()
  }
  await expect(page.getByRole('button', { name: 'Save stage preferences' })).toBeDisabled()
  // One person-scoped read, with no workspace in the request.
  expect(state.gets).toEqual([''])
})

test('the card sets each stage and edits review seats, then saves the whole set', async ({ page }) => {
  const state = await mockSettings(page)
  await page.goto('/w/demo/settings')
  await page.getByRole('button', { name: 'Set plan' }).click()
  await fillEntry(page, 'Plan', ' codex ', 'gpt-6.1-sol', 'medium')
  await page.getByRole('button', { name: 'Set verify' }).click()
  await fillEntry(page, 'Verify', 'opencode', 'local')
  await page.getByRole('button', { name: 'Set primary' }).click()
  await fillEntry(page, 'Review primary', 'claude-code', 'claude-opus-5-5', 'high')
  for (const [index, harness] of ['first', 'second', 'third'].entries()) {
    await page.getByRole('button', { name: 'Add seat' }).click()
    await fillEntry(page, `Review seat ${index + 1}`, harness, `model-${harness}`)
  }
  // Move seat 3 to seat 1's place, then remove the seat now at position 2.
  await page.getByRole('button', { name: 'Move seat 3 up' }).click()
  await page.getByRole('button', { name: 'Move seat 2 up' }).click()
  await expect(page.getByLabel('Review seat 1 harness', { exact: true })).toHaveValue('third')
  await page.getByRole('button', { name: 'Remove seat 2' }).click()
  await expect(page.getByLabel('Review seat 2 harness', { exact: true })).toHaveValue('second')
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts).toHaveLength(1)
  expect(state.puts[0].ifMatch).toBe('"0"')
  expect(state.puts[0].csrf).toBe('1')
  expect(state.puts[0].body).toEqual({
    stages: {
      plan: { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
      implement: null,
      verify: { harness: 'opencode', model: 'local', effort: '' },
      review: {
        primary: { harness: 'claude-code', model: 'claude-opus-5-5', effort: 'high' },
        seats: [
          { harness: 'third', model: 'model-third', effort: '' },
          { harness: 'second', model: 'model-second', effort: '' },
        ],
      },
    },
  })
  // The saved set is shown, and the next save starts from version 1.
  await expect(page.getByLabel('Plan harness', { exact: true })).toHaveValue('codex')
  await page.getByRole('button', { name: 'Clear plan' }).click()
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(stage(page, 'Plan').getByText('Not set')).toBeVisible()
  expect(state.puts[1].ifMatch).toBe('"1"')
  expect(state.puts[1].body.stages.plan).toBeNull()
  expect(state.set.version).toBe(2)
})

test('review keeps at most eight seat entries, and an empty review is saved as unset', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 4)
  await page.goto('/w/demo/settings')
  await expect(page.getByLabel('Review seat 2 model', { exact: true })).toHaveValue('claude-sonnet-5-5')
  for (let seat = 3; seat <= 8; seat++) {
    await page.getByRole('button', { name: 'Add seat' }).click()
    await fillEntry(page, `Review seat ${seat}`, 'codex', `m${seat}`)
  }
  await expect(page.getByRole('button', { name: 'Add seat' })).toBeDisabled()
  await expect(page.getByText('Review can have 8 seat entries at most.')).toBeVisible()
  await page.getByRole('button', { name: 'Clear review' }).click()
  await expect(stage(page, 'Review').getByText('Not set').first()).toBeVisible()
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts[0].ifMatch).toBe('"4"')
  expect(state.puts[0].body.stages.review).toBeNull()
  expect(state.puts[0].body.stages.plan).toEqual(savedSet.plan)
})

test('a stale save keeps the draft and shows the current set beside it', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
  // Another session saves first: the PUT finds version 4 and answers 412.
  const elsewhere: StagePreferenceStages = {
    ...savedSet,
    verify: { harness: 'opencode', model: 'saved-elsewhere', effort: '' },
  }
  state.putAnswers.push({
    status: 412,
    json: { error: 'version_conflict', message: 'changed' },
    changeTo: elsewhere,
  })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const saved = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(saved.getByText('Your stage preferences changed in another place.')).toBeVisible()
  await expect(saved.getByText('opencode, saved-elsewhere')).toBeVisible()
  // The draft stays in the form, unchanged by the newer read.
  await expect(page.getByRole('heading', { name: 'Your unsaved changes' })).toBeVisible()
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')
  await expect(stage(page, 'Verify').getByText('Not set')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Save stage preferences' })).toHaveCount(0)

  // Saving the draft now is the person's own choice, against version 4.
  await saved.getByRole('button', { name: 'Save my changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  await expect(saved).toHaveCount(0)
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"', '"4"'])
  expect(state.puts[1].body.stages.plan?.model).toBe('gpt-7')
  expect(state.set.version).toBe(5)
})

test('discarding a stale draft shows the current set', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await page.getByLabel('Implement harness', { exact: true }).fill('cursor')
  const elsewhere: StagePreferenceStages = { ...savedSet, implement: { harness: 'codex', model: 'other', effort: '' } }
  state.putAnswers.push({ status: 412, json: { error: 'version_conflict', message: 'changed' }, changeTo: elsewhere })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const saved = page.getByRole('region', { name: 'Saved stage preferences' })
  await saved.getByRole('button', { name: 'Discard my changes' }).click()
  await expect(saved).toHaveCount(0)
  await expect(page.getByLabel('Implement harness', { exact: true })).toHaveValue('codex')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('other')
  expect(state.puts).toHaveLength(1)
})

test('read, validation, and store failures are reported and keep the draft', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 2)
  // The first read and its one automatic retry fail.
  state.getFailures = 2
  await page.goto('/w/demo/settings')
  await expect(
    page.getByRole('alert').getByText('Conveyor could not read your stage preferences. Try again.'),
  ).toBeVisible()
  await expect(page.getByRole('form', { name: 'Edit stage preferences' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Try again' }).click()
  await expect(page.getByLabel('Plan harness', { exact: true })).toHaveValue('codex')

  // A blank model is caught before any request.
  await page.getByLabel('Review seat 2 model', { exact: true }).fill('   ')
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Review seat 2: enter a model.' })).toBeVisible()
  expect(state.puts).toHaveLength(0)

  // The server's refusal names the field and the seat.
  await page.getByLabel('Review seat 2 model', { exact: true }).fill('claude-haiku-5-5')
  state.putAnswers.push({
    status: 422,
    json: {
      error: 'validation_failed',
      fields: [{ field: 'stages.review.seats[1].model', message: 'must be at most 128 bytes', position: 2 }],
    },
  })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(
    page.getByRole('status').filter({ hasText: 'Review seat 2 model: must be at most 128 bytes' }),
  ).toBeVisible()

  // A store failure keeps the draft and saves nothing.
  state.putAnswers.push({ status: 500, json: { error: 'internal' } })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(
    page.getByRole('status').filter({ hasText: 'Conveyor could not save your stage preferences.' }),
  ).toBeVisible()
  await expect(page.getByLabel('Review seat 2 model', { exact: true })).toHaveValue('claude-haiku-5-5')
  expect(state.set.version).toBe(2)
})

test('the same set shows in every workspace', async ({ page }) => {
  await mockSettings(page, savedSet, 6)
  await page.goto('/w/demo/settings')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('claude-opus-5-5')
  await page.goto('/w/second/settings')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('claude-opus-5-5')
  await expect(page.getByLabel('Review seat 1 effort', { exact: true })).toHaveValue('medium')
})

// Visual record for the pull request: the saved set and the stale-save view
// at 375 px and at a desktop width. Each image is attached to the Playwright
// report, which CI uploads, and is also written to
// STAGE_PREFERENCES_SCREENSHOT_DIR when it is set, else to the test's output
// directory. No image is committed.
for (const width of [375, 1280]) {
  test(`stage preferences screenshots at ${width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: width < 640 ? 2600 : 1600 })
    const directory = process.env.STAGE_PREFERENCES_SCREENSHOT_DIR
    const target = (name: string) => (directory ? `${directory}/${name}` : testInfo.outputPath(name))
    const shoot = async (locator: Locator, name: string) => {
      await locator.screenshot({ path: target(name) })
      await testInfo.attach(name, { path: target(name), contentType: 'image/png' })
    }
    const state = await mockSettings(page, savedSet, 3)
    await page.goto('/w/demo/settings')
    await expect(page.getByLabel('Review seat 2 model', { exact: true })).toHaveValue('claude-sonnet-5-5')
    // The page never scrolls sideways.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(0)
    await shoot(card(page), `stage-preferences-saved-${width}.png`)

    await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
    state.putAnswers.push({
      status: 412,
      json: { error: 'version_conflict', message: 'changed' },
      changeTo: { ...savedSet, verify: { harness: 'opencode', model: 'saved-elsewhere', effort: '' } },
    })
    await page.getByRole('button', { name: 'Save stage preferences' }).click()
    await expect(
      page.getByRole('region', { name: 'Saved stage preferences' }).getByText('saved-elsewhere'),
    ).toBeVisible()
    await shoot(card(page), `stage-preferences-stale-${width}.png`)

    await mockSettings(page)
    await page.goto('/w/demo/settings')
    await expect(stage(page, 'Plan').getByText('Not set')).toBeVisible()
    await shoot(card(page), `stage-preferences-unset-${width}.png`)
  })
}
