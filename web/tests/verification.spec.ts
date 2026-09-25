import { expect, type Page, test } from '@playwright/test'

const at = '2026-09-20T10:00:00Z'
const sha = 'a'.repeat(40)
const metadata = (
  id: string,
  values: Record<string, string>,
  state = 'evidence',
  context = 'current',
  run = 'run-1',
) => ({
  id,
  context_id: context,
  run_id: run,
  state,
  at,
  metadata: values,
})
const report = 'All four fixtures passed; evidence read back from the factory.'
const current = metadata(
  'current',
  {
    source_sha: sha,
    work_order_id: 'verify-1',
    claimant: 'worker-verifier',
    attempt_count: '2',
    sealed_at: at,
    outcome: 'succeeded',
    deciding_actor: 'worker-verifier',
    disposition: 'selected_kits',
    required_action: report,
  },
  'sealed',
)
const historical = metadata(
  'historical',
  {
    source_sha: 'b'.repeat(40),
    work_order_id: 'verify-h',
    sealed_at: at,
    outcome: 'feedback',
    deciding_actor: 'earlier-verifier',
  },
  'sealed',
  'historical',
)
const oldest = metadata(
  'oldest',
  { source_sha: 'c'.repeat(40), work_order_id: 'verify-o', sealed_at: at, outcome: 'feedback' },
  'sealed',
  'oldest',
)

// Every verify execution is a job with a work order; the timeline entry finds
// its context through the order id.
const verifyJob = (id: string, started_at: string, state: 'done' | 'running') => ({
  id,
  task_id: 'verification-fixture',
  stage: 'verify',
  harness: 'codex',
  model_tier: 'operator-owned',
  runner: 'mcp',
  confinement: 'none',
  tokens_in: 0,
  tokens_out: 0,
  state,
  started_at,
  ended_at: state === 'done' ? started_at : undefined,
})
const verifyOrder = (id: string, created_at: string, state: 'completed' | 'claimed') => ({
  id,
  task_id: 'verification-fixture',
  job_id: id,
  stage: 'verify',
  state,
  claimed_by: 'worker-verifier',
  model: 'fixture-model',
  created_at,
  updated_at: created_at,
  execution_started_at: created_at,
})

async function fixture(page: Page, options: { viewer?: boolean; active?: boolean; review?: boolean } = {}) {
  const counts = new Map<string, number>()
  const observations: Record<string, unknown>[] = []
  let release = () => {}
  const stream = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const context = options.active
    ? metadata(
        'current',
        { source_sha: sha, work_order_id: 'verify-1', claimant: 'worker-verifier', attempt_count: '2' },
        'open',
      )
    : current
  const task = {
    id: 'verification-fixture',
    workspace: 'demo',
    source: 'mcp',
    title: 'Verification fixture',
    body: 'Inspect retained evidence.',
    class: 'feature',
    level: '',
    spec_approval: false,
    merge_approval: true,
    policy_version: 1,
    policy_contract: {
      verify_stage: true,
      max_bounces: 10,
      stage_timeouts: { spec: '30m', implement: '4h', verify: '1h', review: '1h' },
      review: { seats: [{}] },
      refresh_review: 'delta',
    },
    repo: 'conveyor',
    base_branch: 'main',
    branch: 'conveyor/task-verification',
    state: options.review ? 'awaiting_human' : 'running',
    next_stage: options.review ? 'review' : 'verify',
    reviewed_head_sha: sha,
    created_at: at,
  }
  const reviewEvent = {
    id: 4,
    task_id: task.id,
    kind: 'review.completed',
    actor_id: 'user:reviewer',
    actor_role: 'agent',
    payload: { verdict: 'approve', summary: 'Evidence reviewed' },
    at,
    audit_available: true,
  }
  const item = {
    task,
    jobs: [
      verifyJob('verify-o', '2026-09-20T08:00:00Z', 'done'),
      verifyJob('verify-h', '2026-09-20T09:00:00Z', 'done'),
      verifyJob('verify-1', at, options.active ? 'running' : 'done'),
    ],
    events: options.review ? [reviewEvent] : [],
    interventions: [],
    checkout_available: true,
    checkout_guidance: '',
    needs_attention: false,
    at_merge_gate: Boolean(options.review),
    attachments: [],
    verification_evidence: [],
    work_orders: [
      verifyOrder('verify-o', '2026-09-20T08:00:00Z', 'completed'),
      verifyOrder('verify-h', '2026-09-20T09:00:00Z', 'completed'),
      verifyOrder('verify-1', at, options.active ? 'claimed' : 'completed'),
    ],
    spec: {
      task_id: task.id,
      version: 1,
      content: 'Inspect verification metadata and retained evidence.',
      approved: true,
      created_at: at,
    },
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url()),
      path = url.pathname
    counts.set(path, (counts.get(path) ?? 0) + 1)
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me')
      return route.fulfill({ json: { id: 'operator', role: options.viewer ? 'viewer' : 'operator' } })
    if (path === '/v1/activity')
      return route.fulfill({ json: [{ task, latest_stage: 'verify', last_event_at: at, needs_attention: false }] })
    if (path.endsWith('/activity')) return route.fulfill({ json: item })
    if (path.endsWith('/events/stream')) {
      await stream
      return route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
    }
    if (path.endsWith('/audit/event/4'))
      return route.fulfill({
        json: {
          kind: 'event',
          event: {
            ...reviewEvent,
            payload: {
              ...reviewEvent.payload,
              verification_assessment: {
                context_ids: ['current'],
                run_ids: ['run-1'],
                evidence_ids: ['e1'],
                actor: 'user:reviewer',
              },
            },
          },
        },
      })
    if (path.endsWith('/verification')) {
      if (url.searchParams.get('cursor'))
        return route.fulfill({
          json: { head_sha: sha, current_context_id: 'current', contexts: { items: [oldest] }, overview: {} },
        })
      return route.fulfill({
        json: {
          head_sha: sha,
          current_context_id: 'current',
          contexts: { items: [context, historical], next_cursor: 'earlier' },
          overview: {},
        },
      })
    }
    if (path.endsWith('/observations')) {
      observations.push(route.request().postDataJSON())
      if (observations.length === 1)
        return route.fulfill({ status: 500, body: 'Response was unavailable; retry the same observation.' })
      return route.fulfill({ json: { ID: 'receipt-1', EvidenceIDs: ['observation-1'] } })
    }
    if (path.includes('/artifacts/'))
      return route.fulfill({ contentType: 'text/plain', body: 'Retained artifact content' })
    if (path.endsWith('/evidence/e1') || path.endsWith('/evidence/e2'))
      return route.fulfill({
        json: {
          Digest: 'digest',
          Envelope: {
            id: path.endsWith('e1') ? 'e1' : 'e2',
            type: 'state_observation',
            context_id: 'current',
            run_id: 'run-1',
            submitted_by: 'worker-verifier',
            captured_at: at,
            captured_by: { identity: 'fixture-tool', attribution: 'self_reported' },
            payload: { target: 'fixture', value: 'Structured retained observation' },
            artifacts: [{ artifact_id: 'artifact-1', sha256: 'artifact-1', media_type: 'text/plain' }],
          },
        },
      })
    // Only the current context has collections; the earlier ones read empty.
    if (!path.includes('/contexts/current/')) {
      if (path.endsWith('/contexts')) return route.fulfill({ json: { items: [current] } })
      if (path.includes('/verification/')) return route.fulfill({ json: { items: [] } })
      return route.fulfill({ json: [] })
    }
    if (path.endsWith('/selections'))
      return route.fulfill({
        json: {
          items: [
            metadata('kit', {
              kit_id: 'fixture-kit',
              digest: 'd'.repeat(64),
              eligibility: 'eligible',
              reasons: 'Governing pins match',
            }),
          ],
        },
      })
    if (path.endsWith('/obligations'))
      return route.fulfill({
        json: {
          items: [metadata('ordinary', { obligation_id: 'ordinary-test', description: 'Ordinary test coverage' })],
        },
      })
    if (path.endsWith('/assertions'))
      return route.fulfill({
        json: {
          items: [
            metadata('assert-required', {
              assertion_id: 'required-state',
              outcome: 'pass',
              required: 'true',
              evidence_id: 'e1',
            }),
            metadata('assert-optional', { assertion_id: 'optional-detail', outcome: 'fail', required: 'false' }),
          ],
        },
      })
    if (path.endsWith('/operations'))
      return route.fulfill({
        json: {
          items: [
            metadata(
              'op-1',
              { step_id: 'publish', target: 'fixture', retry_policy: 'operator_action_required' },
              'outcome_unknown',
            ),
          ],
        },
      })
    if (path.endsWith('/publications'))
      return route.fulfill({
        json: { items: [metadata('publication', { head_sha: sha, generation: '3' }, 'retrying')] },
      })
    if (path.endsWith('/attempts'))
      return route.fulfill({
        json: {
          items: [
            metadata(
              'run-1',
              {
                kind: 'kit',
                kit_id: 'fixture-kit',
                kit_version: '1.2.0',
                exercise_id: 'fixture-exercise',
                digest: 'd'.repeat(64),
                started_at: at,
                ended_at: options.active ? '' : at,
              },
              options.active ? 'running' : 'succeeded',
            ),
          ],
        },
      })
    if (path.endsWith('/evidence'))
      return route.fulfill({
        json: url.searchParams.get('cursor')
          ? { items: [metadata('e2', { type: 'state_observation', digest: 'digest-2' })] }
          : {
              items: [metadata('e1', { type: 'state_observation', digest: 'digest-1' })],
              next_cursor: 'evidence-page-2',
            },
      })
    if (path.endsWith('/contexts')) return route.fulfill({ json: { items: [current] } })
    return route.fulfill({ json: { items: [] } })
  })
  return { counts, observations, release }
}

const entries = (page: Page) => page.getByRole('article', { name: 'Verification', exact: true })

for (const width of [1366, 390]) {
  test(`verification entry, folds and history at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const { counts, release } = await fixture(page)
    await page.goto('/tasks/verification-fixture/full')
    const entry = entries(page).filter({ hasText: 'Verification passed' })
    await expect(entry).toBeVisible()
    await expect(entry).toContainText('1 of 2 assertions passed')
    await expect(entry).toContainText('fixture-kit')
    await expect(entry).toContainText('fixture-exercise · required-state')
    await expect(entry).toContainText('fixture-exercise · optional-detail')
    await expect(entry).toContainText('optional')
    await expect(entry).toContainText('retrying')
    await expect(entry).toContainText('generation 3')
    // The standalone section is gone: verification lives in the timeline.
    await expect(page.getByRole('region', { name: 'Verification', exact: true })).toHaveCount(0)
    // Earlier runs are their own entries, marked as superseded; the oldest
    // sits on the second summary page and is paged in for its entry.
    const superseded = entries(page).filter({ hasText: 'superseded by a later revision' })
    await expect(superseded).toHaveCount(2)
    await expect(superseded.filter({ hasText: `at ${'b'.repeat(7)}` })).toBeVisible()
    await expect(superseded.filter({ hasText: `at ${'c'.repeat(7)}` })).toBeVisible()
    const root = '/v1/tasks/verification-fixture/verification'
    expect(counts.get(`${root}/contexts/current/evidence`)).toBeUndefined()
    expect(counts.get(`${root}/contexts/current/evidence/e1`)).toBeUndefined()
    const initialSummaryRequests = counts.get(root)
    release()
    await expect.poll(() => counts.get('/v1/tasks/verification-fixture/activity') ?? 0).toBeGreaterThan(1)
    expect(counts.get(root)).toBe(initialSummaryRequests)
    // The verifier's report and the collection pages stay folded until asked.
    await expect(entry).not.toContainText(report)
    await entry.locator('summary', { hasText: "Verifier's report" }).click()
    await expect(entry).toContainText(report)
    await entry.locator('summary', { hasText: /^Details$/ }).click()
    await expect(entry).toContainText(sha)
    await entry.locator('summary', { hasText: /^Attempts$/ }).click()
    await expect(entry).toContainText('1.2.0')
    // Evidence is read on demand: the assertion row opens its own record.
    await entry.getByRole('button', { name: 'Show evidence', exact: true }).first().click()
    await expect(entry.getByLabel('Structured evidence').first()).toContainText('Structured retained observation')
    expect(counts.get(`${root}/contexts/current/evidence/e1`)).toBeTruthy()
    await entry.locator('summary', { hasText: /^Evidence$/ }).click()
    await entry.getByRole('button', { name: 'Load more evidence', exact: true }).click()
    await entry.getByRole('button', { name: 'Open evidence e2', exact: true }).click()
    expect(counts.get(`${root}/contexts/current/evidence/e2/artifacts/artifact-1`)).toBeUndefined()
    await entry.getByRole('button', { name: 'Open artifact (text/plain)', exact: true }).last().click()
    await expect(entry.getByLabel('Artifact content')).toContainText('Retained artifact content')
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  })
}

test('task sheet shows a running verification and viewer has no observation action', async ({ page }) => {
  await fixture(page, { viewer: true, active: true })
  await page.goto('/tasks/verification-fixture')
  const entry = entries(page).filter({ hasText: 'Running' })
  await expect(entry).toBeVisible()
  await expect(entry).toContainText('fixture-exercise running')
  await expect(entry.getByRole('button', { name: 'Record observation' })).toHaveCount(0)
})

test('operator observation retries retain one key and exclude server provenance', async ({ page }) => {
  const { observations } = await fixture(page, { active: true })
  await page.goto('/tasks/verification-fixture/full')
  const entry = entries(page).filter({ hasText: 'Running' })
  await entry.getByRole('button', { name: 'Record observation', exact: true }).click()
  await entry.getByLabel('Observed fact', { exact: true }).fill('The expected result is visible.')
  await entry.getByLabel('Supporting evidence IDs (comma separated)', { exact: true }).fill('e1')
  await entry.getByRole('button', { name: 'Save observation', exact: true }).click()
  await expect(entry.getByRole('alert')).toContainText('retry the same observation')
  await entry.getByRole('button', { name: 'Save observation', exact: true }).click()
  await expect(entry.getByRole('status')).toContainText('Observation recorded.')
  expect(observations).toHaveLength(2)
  expect(observations[0]).toEqual(observations[1])
  expect(Object.keys(observations[0]).sort()).toEqual(['context_id', 'fact', 'idempotency_key', 'run_id', 'supporting'])
})

test('merge gate shows one verdict line and loads cited evidence independently', async ({ page }) => {
  const { counts } = await fixture(page, { review: true })
  await page.goto('/tasks/verification-fixture/full')
  await expect(entries(page).filter({ hasText: 'Verification passed' })).toBeVisible()
  await expect(page.getByText('Verification passed', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'See the run' })).toBeVisible()
  expect(counts.get('/v1/tasks/verification-fixture/audit/event/4')).toBeUndefined()
  await page.getByRole('button', { name: 'Verification relied on by review' }).click()
  await expect(page.getByText(/Review deciding actor: user:reviewer/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Open evidence e1', exact: true })).toBeVisible()
  expect(counts.get('/v1/tasks/verification-fixture/verification/contexts/current/evidence/e1')).toBeUndefined()
})
