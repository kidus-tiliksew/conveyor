import { expect, type Locator, type Page, test } from '@playwright/test'
import type { StagePreferenceStages, StagePreferences } from '../src/lib/stage-preferences'
import { callerIdentity } from './helpers/caller-identity'
import { installQueryClientProbe, withQueryClient } from './helpers/query-client'
import { openSettingsRow, settingsRowHeader } from './helpers/settings'

// The Settings stage preferences card (component-stage-preferences;
// component-web-dashboard invariant 25; req-stage-preferences AC-2.1, AC-2.4).
// The mock serves GET and PUT /v1/me/stage-preferences in the shape the server
// returns (internal/httpapi/stage_preferences.go) and keeps the server's
// If-Match rule: a stale version answers 412 and changes nothing.

const unset: StagePreferenceStages = { plan: null, implement: null, verify: null, risk: null, review: null }

const savedSet: StagePreferenceStages = {
  plan: { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
  implement: { harness: 'claude-code', model: 'claude-opus-5-5', effort: 'high' },
  verify: null,
  risk: { harness: 'claude-code', model: 'claude-haiku-5-5', effort: 'low' },
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
  await openSettingsRow(page, 'Stage preferences')
  await expect(page.getByText('These preferences apply in every workspace.')).toBeVisible()
  for (const name of ['Plan', 'Implement', 'Verify', 'Risk', 'Review']) {
    await expect(stage(page, name).getByText('Not set').first()).toBeVisible()
  }
  await expect(page.getByRole('button', { name: 'Save stage preferences' })).toBeDisabled()
  // One person-scoped read, with no workspace in the request.
  expect(state.gets).toEqual([''])
})

test('the card sets each stage and edits review seats, then saves the whole set', async ({ page }) => {
  const state = await mockSettings(page)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
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
      risk: null,
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

// The Risk row (req-stage-preferences AC-1.1, AC-2.1; DEC-135(13)): it sits
// between Verify and Review at 375 px and at a desktop width, in the form and
// in the closed row.
for (const width of [375, 1280]) {
  test(`the Risk row sits between Verify and Review at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1600 })
    await mockSettings(page, savedSet, 3)
    await page.goto('/w/demo/settings')
    const header = settingsRowHeader(page, 'Stage preferences')
    await expect(header).toContainText('Risk: claude-code · claude-haiku-5-5 · low')
    const closed = (await header.textContent()) ?? ''
    const verifyLine = closed.indexOf('Verify: Not set')
    const riskLine = closed.indexOf('Risk: claude-code · claude-haiku-5-5 · low')
    const reviewLine = closed.indexOf('Review primary:')
    expect(verifyLine).toBeGreaterThanOrEqual(0)
    expect(riskLine).toBeGreaterThan(verifyLine)
    expect(reviewLine).toBeGreaterThan(riskLine)

    await openSettingsRow(page, 'Stage preferences')
    const sections = await page
      .getByRole('form', { name: 'Edit stage preferences' })
      .getByRole('region')
      .evaluateAll((regions) => regions.map((region) => region.getAttribute('aria-label')))
    expect(sections).toEqual(['Plan stage', 'Implement stage', 'Verify stage', 'Risk stage', 'Review stage'])
    const [verifyBox, riskBox, reviewBox] = await Promise.all(
      ['Verify', 'Risk', 'Review'].map((name) => stage(page, name).boundingBox()),
    )
    expect(riskBox!.y).toBeGreaterThan(verifyBox!.y)
    expect(reviewBox!.y).toBeGreaterThan(riskBox!.y)
    await expect(stage(page, 'Risk').getByRole('heading', { name: 'Risk' })).toBeVisible()
    await expect(page.getByLabel('Risk model', { exact: true })).toHaveValue('claude-haiku-5-5')
    await expect(page.getByLabel('Risk effort', { exact: true })).toHaveValue('low')
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(0)
  })
}

test('the card sets, edits, saves, and clears the Risk stage', async ({ page }) => {
  const state = await mockSettings(page, { ...unset, verify: { harness: 'opencode', model: 'local', effort: '' } }, 1)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await expect(stage(page, 'Risk').getByText('Not set')).toBeVisible()
  await page.getByRole('button', { name: 'Set risk' }).click()
  await fillEntry(page, 'Risk', ' claude-code ', ' claude-haiku-5-5 ', ' low ')
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts[0].ifMatch).toBe('"1"')
  expect(state.puts[0].body).toEqual({
    stages: {
      plan: null,
      implement: null,
      verify: { harness: 'opencode', model: 'local', effort: '' },
      risk: { harness: 'claude-code', model: 'claude-haiku-5-5', effort: 'low' },
      review: null,
    },
  })
  await expect(page.getByLabel('Risk harness', { exact: true })).toHaveValue('claude-code')

  // A blank model is caught before any request, in the card's words.
  await page.getByLabel('Risk model', { exact: true }).fill('  ')
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Risk: enter a model.' })).toBeVisible()
  expect(state.puts).toHaveLength(1)

  // The server's refusal of a risk field is named in the card's words.
  await page.getByLabel('Risk model', { exact: true }).fill('claude-sonnet-5-5')
  state.putAnswers.push({
    status: 422,
    json: {
      error: 'validation_failed',
      fields: [{ field: 'stages.risk.model', message: 'must be at most 128 bytes' }],
    },
  })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Risk model: must be at most 128 bytes' })).toBeVisible()

  await page.getByRole('button', { name: 'Clear risk' }).click()
  await expect(stage(page, 'Risk').getByText('Not set')).toBeVisible()
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  const last = state.puts[state.puts.length - 1]
  expect(last.ifMatch).toBe('"2"')
  expect(last.body.stages.risk).toBeNull()
  expect(last.body.stages.verify).toEqual({ harness: 'opencode', model: 'local', effort: '' })
  expect(state.set.version).toBe(3)
  // The closed row reads the saved set: Risk is unset and Verify is kept.
  const header = settingsRowHeader(page, 'Stage preferences')
  await header.click()
  await expect(header).toContainText('Risk: Not set')
  await expect(header).toContainText('Verify: opencode · local')
})

test('a stale save keeps a Risk draft and shows the saved Risk entry beside it', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Risk model', { exact: true }).fill('claude-sonnet-5-5')
  state.putAnswers.push({
    status: 412,
    json: { error: 'version_conflict', message: 'changed' },
    changeTo: { ...savedSet, risk: { harness: 'codex', model: 'risk-saved-elsewhere', effort: '' } },
  })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const saved = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(saved.getByText('codex, risk-saved-elsewhere')).toBeVisible()
  await expect(page.getByLabel('Risk model', { exact: true })).toHaveValue('claude-sonnet-5-5')
  await saved.getByRole('button', { name: 'Save my changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"', '"4"'])
  expect(state.puts[1].body.stages.risk).toEqual({ harness: 'claude-code', model: 'claude-sonnet-5-5', effort: 'low' })
})

test('review keeps at most eight seat entries, and an empty review is saved as unset', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 4)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
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
  await openSettingsRow(page, 'Stage preferences')
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
  await openSettingsRow(page, 'Stage preferences')
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

test('a stale save whose follow-up reads fail never resolves against the cached set', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
  // Another session saved version 4. The PUT answers 412, and the read that
  // follows and its one automatic retry both fail.
  const elsewhere: StagePreferenceStages = {
    ...savedSet,
    verify: { harness: 'opencode', model: 'saved-elsewhere', effort: '' },
  }
  state.putAnswers.push({ status: 412, json: { error: 'version_conflict', message: 'changed' }, changeTo: elsewhere })
  state.getFailures = 2
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const conflict = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(conflict.getByText('Conveyor could not read the saved preferences.')).toBeVisible()
  // Neither the cached version 3 set nor a way to resolve against it shows.
  await expect(conflict.getByText('codex, gpt-6.1-sol, medium effort')).toHaveCount(0)
  await expect(conflict.getByRole('button', { name: 'Save my changes' })).toHaveCount(0)
  await expect(conflict.getByRole('button', { name: 'Discard my changes' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Save stage preferences' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Discard changes' })).toHaveCount(0)
  // The draft stays.
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')
  expect(state.puts).toHaveLength(1)

  // The draft stays editable while the read is failed: an existing field
  // and a review seat change, and a seat moves.
  await page.getByLabel('Implement effort', { exact: true }).fill('low')
  await page.getByLabel('Review seat 2 model', { exact: true }).fill('edited-while-failed')
  await page.getByRole('button', { name: 'Move seat 2 up' }).click()
  await expect(page.getByLabel('Implement effort', { exact: true })).toHaveValue('low')
  await expect(page.getByLabel('Review seat 1 model', { exact: true })).toHaveValue('edited-while-failed')
  await expect(page.getByLabel('Review seat 2 harness', { exact: true })).toHaveValue('codex')
  await expect(conflict.getByRole('button', { name: 'Save my changes' })).toHaveCount(0)

  // An explicit retry reads the real new set, and only then can the person
  // choose. The edits made while the read failed survive.
  await conflict.getByRole('button', { name: 'Read saved preferences again' }).click()
  await expect(conflict.getByText('opencode, saved-elsewhere')).toBeVisible()
  await expect(conflict.getByText('Conveyor could not read the saved preferences.')).toHaveCount(0)
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')
  await expect(page.getByLabel('Implement effort', { exact: true })).toHaveValue('low')
  await expect(page.getByLabel('Review seat 1 model', { exact: true })).toHaveValue('edited-while-failed')
  await conflict.getByRole('button', { name: 'Save my changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"', '"4"'])
  expect(state.puts[1].body.stages.plan?.model).toBe('gpt-7')
  expect(state.puts[1].body.stages.implement?.effort).toBe('low')
  expect(state.puts[1].body.stages.review?.seats).toEqual([
    { harness: 'claude-code', model: 'edited-while-failed', effort: '' },
    { harness: 'codex', model: 'gpt-6.1-sol', effort: 'medium' },
  ])
})

test('a failed save over a freshly read set can be retried against that set', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
  const elsewhere: StagePreferenceStages = {
    ...savedSet,
    verify: { harness: 'opencode', model: 'saved-elsewhere', effort: '' },
  }
  state.putAnswers.push({ status: 412, json: { error: 'version_conflict', message: 'changed' }, changeTo: elsewhere })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const conflict = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(conflict.getByText('opencode, saved-elsewhere')).toBeVisible()
  // Saving over the fresh set fails in the store.
  state.putAnswers.push({ status: 500, json: { error: 'internal' } })
  await conflict.getByRole('button', { name: 'Save my changes' }).click()
  await expect(
    page.getByRole('status').filter({ hasText: 'Conveyor could not save your stage preferences.' }),
  ).toBeVisible()
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')
  // The retry sends the fresh version, not the stale one, and saves.
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"', '"4"', '"4"'])
  expect(state.puts[2].body.stages.plan?.model).toBe('gpt-7')
  expect(state.set.version).toBe(5)
})

test('discarding after a failed and then successful read shows the real new set', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Implement harness', { exact: true }).fill('cursor')
  const elsewhere: StagePreferenceStages = { ...savedSet, implement: { harness: 'codex', model: 'other', effort: '' } }
  state.putAnswers.push({ status: 412, json: { error: 'version_conflict', message: 'changed' }, changeTo: elsewhere })
  state.getFailures = 2
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const conflict = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(conflict.getByText('Conveyor could not read the saved preferences.')).toBeVisible()
  await conflict.getByRole('button', { name: 'Read saved preferences again' }).click()
  await conflict.getByRole('button', { name: 'Discard my changes' }).click()
  await expect(conflict).toHaveCount(0)
  await expect(page.getByLabel('Implement harness', { exact: true })).toHaveValue('codex')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('other')
  expect(state.puts).toHaveLength(1)
})

test('a failed refetch with a draft keeps the draft and never shows the cached set as saved', async ({ page }) => {
  await installQueryClientProbe(page)
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
  // The set changed elsewhere, and the next reads fail.
  state.set = {
    version: 4,
    updated_at: '2026-11-12T10:00:00Z',
    stages: { ...savedSet, plan: { harness: 'codex', model: 'saved-elsewhere', effort: '' } },
  }
  state.getFailures = 2
  await withQueryClient(
    page,
    (client) =>
      (client as unknown as { refetchQueries(filter: { queryKey: readonly unknown[] }): Promise<void> }).refetchQueries(
        { queryKey: ['stage-preferences'] },
      ),
    undefined,
  )
  const failure = page.getByRole('alert').filter({ hasText: 'Conveyor could not read your saved stage preferences.' })
  await expect(failure).toBeVisible()
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')

  // Discarding the draft while the read is failed shows no cached set.
  await page.getByRole('button', { name: 'Discard changes' }).click()
  await expect(page.getByRole('form', { name: 'Edit stage preferences' })).toHaveCount(0)
  await expect(
    page.getByRole('alert').getByText('Conveyor could not read your stage preferences. Try again.'),
  ).toBeVisible()

  // A successful retry shows the real new set.
  await page.getByRole('button', { name: 'Try again' }).click()
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('saved-elsewhere')
  expect(state.puts).toHaveLength(0)
})

test('a failed refetch with a draft still saves the draft against its own version', async ({ page }) => {
  await installQueryClientProbe(page)
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')
  state.getFailures = 2
  await withQueryClient(
    page,
    (client) =>
      (client as unknown as { refetchQueries(filter: { queryKey: readonly unknown[] }): Promise<void> }).refetchQueries(
        { queryKey: ['stage-preferences'] },
      ),
    undefined,
  )
  await expect(
    page.getByRole('alert').filter({ hasText: 'Conveyor could not read your saved stage preferences.' }),
  ).toBeVisible()
  // The draft stays editable while the read is failed: an existing field,
  // a cleared stage, and review seat edits all apply.
  await page.getByLabel('Implement model', { exact: true }).fill('edited-while-failed')
  await page.getByRole('button', { name: 'Clear plan' }).click()
  await page.getByLabel('Review seat 1 effort', { exact: true }).fill('low')
  await page.getByRole('button', { name: 'Remove seat 2' }).click()
  await page.getByRole('button', { name: 'Add seat' }).click()
  await page.getByLabel('Review seat 2 harness', { exact: true }).fill('cursor')
  await page.getByLabel('Review seat 2 model', { exact: true }).fill('auto')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('edited-while-failed')
  await expect(stage(page, 'Plan').getByText('Not set')).toBeVisible()
  await expect(page.getByLabel('Review seat 1 effort', { exact: true })).toHaveValue('low')
  await expect(page.getByLabel('Review seat 2 harness', { exact: true })).toHaveValue('cursor')

  await page.getByRole('button', { name: 'Read saved preferences again' }).click()
  await expect(
    page.getByRole('alert').filter({ hasText: 'Conveyor could not read your saved stage preferences.' }),
  ).toHaveCount(0)
  // The edits survive the successful read and are saved.
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('edited-while-failed')
  await expect(page.getByLabel('Review seat 2 harness', { exact: true })).toHaveValue('cursor')
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"'])
  expect(state.puts[0].body.stages.plan).toBeNull()
  expect(state.puts[0].body.stages.implement?.model).toBe('edited-while-failed')
  expect(state.puts[0].body.stages.review?.seats).toEqual([
    { harness: 'codex', model: 'gpt-6.1-sol', effort: 'low' },
    { harness: 'cursor', model: 'auto', effort: '' },
  ])
})

test('read, validation, and store failures are reported and keep the draft', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 2)
  // The first read and its one automatic retry fail.
  state.getFailures = 2
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
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
  await openSettingsRow(page, 'Stage preferences')
  await expect(page.getByLabel('Implement model', { exact: true })).toHaveValue('claude-opus-5-5')
  await page.goto('/w/second/settings')
  await openSettingsRow(page, 'Stage preferences')
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
    await openSettingsRow(page, 'Stage preferences')
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
    // The read after the stale save fails first, then an explicit retry
    // reads the new set.
    state.getFailures = 2
    await page.getByRole('button', { name: 'Save stage preferences' }).click()
    const conflict = page.getByRole('region', { name: 'Saved stage preferences' })
    await expect(conflict.getByText('Conveyor could not read the saved preferences.')).toBeVisible()
    await shoot(card(page), `stage-preferences-stale-read-failed-${width}.png`)
    await conflict.getByRole('button', { name: 'Read saved preferences again' }).click()
    await expect(conflict.getByText('saved-elsewhere')).toBeVisible()
    await shoot(card(page), `stage-preferences-stale-${width}.png`)

    await mockSettings(page)
    await page.goto('/w/demo/settings')
    await openSettingsRow(page, 'Stage preferences')
    await expect(stage(page, 'Plan').getByText('Not set')).toBeVisible()
    await shoot(card(page), `stage-preferences-unset-${width}.png`)
  })
}

// The closed Stage preferences row on Settings (req-stage-preferences AC-2.1):
// one line per stage and per review seat, "Not set" for an unset stage, and
// the statement that the set applies in every workspace, without opening.
test('the closed row shows every stage, each seat, and the every-workspace statement', async ({ page }) => {
  await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  const header = settingsRowHeader(page, 'Stage preferences')
  await expect(header).toHaveAttribute('aria-expanded', 'false')
  for (const line of [
    'Plan: codex · gpt-6.1-sol · medium',
    'Implement: claude-code · claude-opus-5-5 · high',
    'Verify: Not set',
    'Risk: claude-code · claude-haiku-5-5 · low',
    'Review primary: claude-code · claude-opus-5-5',
    'Review seat 1: codex · gpt-6.1-sol · medium',
    'Review seat 2: claude-code · claude-sonnet-5-5',
    'They apply in every workspace.',
  ])
    await expect(header).toContainText(line)
  await expect(page.getByLabel('Plan harness', { exact: true })).toBeHidden()
  await expect(page.getByRole('button', { name: 'Set verify' })).toHaveCount(0)
})

test('the closed row shows Not set for every stage of an empty set', async ({ page }) => {
  await mockSettings(page)
  await page.goto('/w/demo/settings')
  const header = settingsRowHeader(page, 'Stage preferences')
  for (const stage of ['Plan', 'Implement', 'Verify', 'Risk', 'Review primary'])
    await expect(header).toContainText(`${stage}: Not set`)
  await expect(header).not.toContainText('Review seat')
  await expect(header).toContainText('They apply in every workspace.')
})

test('a failed read never shows the cached set in the closed row', async ({ page }) => {
  await installQueryClientProbe(page)
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  const header = settingsRowHeader(page, 'Stage preferences')
  await expect(header).toContainText('Plan: codex')
  state.getFailures = 2
  await withQueryClient(
    page,
    (client) =>
      (client as unknown as { refetchQueries(filter: { queryKey: readonly unknown[] }): Promise<void> }).refetchQueries(
        { queryKey: ['stage-preferences'] },
      ),
    undefined,
  )
  await expect(header).toContainText('Conveyor could not read your saved stage preferences.')
  await expect(header).not.toContainText('Plan: codex')
  await expect(header).toContainText('They apply in every workspace.')
})

test('unsaved edits and the stale-save panel survive closing and reopening the row', async ({ page }) => {
  const state = await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  const header = settingsRowHeader(page, 'Stage preferences')
  await page.getByLabel('Plan model', { exact: true }).fill('gpt-7')

  // A closed row with an unsaved edit says so, and keeps the edit.
  await header.click()
  await expect(header).toContainText('You have unsaved changes.')
  await expect(header).toContainText('Plan: codex · gpt-6.1-sol · medium')
  await openSettingsRow(page, 'Stage preferences')
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')

  // A stale save keeps its panel and base version across close and reopen.
  state.putAnswers.push({
    status: 412,
    json: { error: 'version_conflict', message: 'changed' },
    changeTo: { ...savedSet, verify: { harness: 'opencode', model: 'saved-elsewhere', effort: '' } },
  })
  await page.getByRole('button', { name: 'Save stage preferences' }).click()
  const conflict = page.getByRole('region', { name: 'Saved stage preferences' })
  await expect(conflict.getByText('saved-elsewhere')).toBeVisible()
  await header.click()
  await expect(header).toContainText('You have unsaved changes.')
  await expect(conflict).toHaveCount(0)
  await openSettingsRow(page, 'Stage preferences')
  await expect(conflict.getByText('saved-elsewhere')).toBeVisible()
  await expect(page.getByLabel('Plan model', { exact: true })).toHaveValue('gpt-7')
  await conflict.getByRole('button', { name: 'Save my changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Stage preferences saved.' })).toBeVisible()
  expect(state.puts.map((put) => put.ifMatch)).toEqual(['"3"', '"4"'])
  expect(state.puts[1].body.stages.plan?.model).toBe('gpt-7')
})

test('each entry keeps harness, model, and effort on one line at 375 px', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 1600 })
  await mockSettings(page, savedSet, 3)
  await page.goto('/w/demo/settings')
  await openSettingsRow(page, 'Stage preferences')
  for (const label of ['Plan', 'Implement', 'Risk', 'Review primary', 'Review seat 1', 'Review seat 2']) {
    const [harness, model, effort] = await Promise.all(
      ['harness', 'model', 'effort'].map((name) => page.getByLabel(`${label} ${name}`, { exact: true }).boundingBox()),
    )
    expect(Math.abs(harness!.y - model!.y)).toBeLessThanOrEqual(1)
    expect(Math.abs(harness!.y - effort!.y)).toBeLessThanOrEqual(1)
    expect(effort!.width).toBeLessThan(harness!.width)
    expect(effort!.width).toBeLessThan(model!.width)
    await expect(page.getByLabel(`${label} effort`, { exact: true })).toHaveAttribute('placeholder', 'Optional')
  }
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  )
  expect(overflow).toBeLessThanOrEqual(0)
  const scroll = page.getByTestId('settings-scroll')
  expect(await scroll.evaluate((element) => element.scrollWidth - element.clientWidth)).toBeLessThanOrEqual(0)
})
