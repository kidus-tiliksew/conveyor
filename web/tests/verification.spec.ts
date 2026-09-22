import { expect, type Page, test } from '@playwright/test'

const at = '2026-09-20T10:00:00Z'
const sha = 'a'.repeat(40)
const metadata = (id: string, values: Record<string, string>, state = 'evidence', context = 'current') => ({
  id,
  context_id: context,
  run_id: 'run-1',
  state,
  at,
  metadata: values,
})
const current = metadata(
  'current',
  {
    source_sha: sha,
    claimant: 'worker-verifier',
    attempt_count: '2',
    sealed_at: at,
    outcome: 'succeeded',
    deciding_actor: 'worker-verifier',
    disposition: 'selected_kits',
  },
  'sealed',
)
const historical = metadata(
  'historical',
  { source_sha: 'b'.repeat(40), sealed_at: at, outcome: 'feedback', deciding_actor: 'earlier-verifier' },
  'sealed',
  'historical',
)

async function fixture(page: Page, options: { viewer?: boolean; active?: boolean; review?: boolean } = {}) {
  const counts = new Map<string, number>()
  const observations: Record<string, unknown>[] = []
  let release = () => {}
  const stream = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const context = options.active
    ? metadata('current', { source_sha: sha, claimant: 'worker-verifier', attempt_count: '2' }, 'open')
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
    jobs: [],
    events: options.review ? [reviewEvent] : [],
    interventions: [],
    checkout_available: true,
    checkout_guidance: '',
    needs_attention: false,
    at_merge_gate: Boolean(options.review),
    attachments: [],
    verification_evidence: [],
    work_orders: [
      {
        id: 'verify-1',
        task_id: task.id,
        job_id: 'verify-1',
        stage: 'verify',
        state: options.active ? 'claimed' : 'completed',
        claimed_by: 'worker-verifier',
        created_at: at,
        updated_at: at,
        execution_started_at: at,
      },
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
          json: {
            head_sha: sha,
            current_context_id: 'current',
            contexts: {
              items: [metadata('oldest', { source_sha: 'c'.repeat(40), outcome: 'feedback' }, 'sealed', 'oldest')],
            },
            overview: {},
          },
        })
      return route.fulfill({
        json: {
          head_sha: sha,
          current_context_id: 'current',
          contexts: { items: [context, historical], next_cursor: 'earlier' },
          overview: {
            selections: {
              items: [
                metadata('kit', {
                  kit_id: 'fixture-kit',
                  digest: 'd'.repeat(64),
                  eligibility: 'eligible',
                  reasons: 'Governing pins match',
                }),
              ],
            },
            obligations: {
              items: [metadata('ordinary', { obligation_id: 'ordinary-test', description: 'Ordinary test coverage' })],
            },
            assertions: {
              items: [
                metadata('assert-required', { assertion_id: 'required-state', outcome: 'pass', required: 'true' }),
                metadata('assert-optional', { assertion_id: 'optional-detail', outcome: 'fail', required: 'false' }),
              ],
            },
            operations: {
              items: [
                metadata(
                  'op-1',
                  { step_id: 'publish', target: 'fixture', retry_policy: 'operator_action_required' },
                  'outcome_unknown',
                ),
              ],
            },
            publications: { items: [metadata('publication', { head_sha: sha }, 'retrying')] },
          },
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
    if (path.endsWith('/contexts')) return route.fulfill({ json: { items: [current] } })
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
    if (path.includes('/verification/')) return route.fulfill({ json: { items: [] } })
    return route.fulfill({ json: [] })
  })
  return { counts, observations, release }
}

for (const width of [1366, 390]) {
  test(`verification history and disclosure at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const { counts, release } = await fixture(page)
    await page.goto('/tasks/verification-fixture/full')
    const stage = page.getByRole('region', { name: 'Verification', exact: true })
    await expect(stage.getByRole('heading', { name: 'Verify · completed' })).toBeVisible()
    await expect(stage).toContainText('Claimant: worker-verifier · Attempts: 2 · Elapsed:')
    await expect(stage).toContainText('Required assertion')
    await expect(stage).toContainText('Optional assertion')
    await expect(stage).toContainText('optional-detail · fail')
    await expect(stage).toContainText('outcome_unknown')
    await expect(stage).toContainText('retrying')
    const root = '/v1/tasks/verification-fixture/verification'
    expect(counts.get(`${root}/contexts/current/evidence`)).toBeUndefined()
    expect(counts.get(`${root}/contexts/current/evidence/e1`)).toBeUndefined()
    const initialSummaryRequests = counts.get(root)
    release()
    await expect.poll(() => counts.get('/v1/tasks/verification-fixture/activity') ?? 0).toBeGreaterThan(1)
    expect(counts.get(root)).toBe(initialSummaryRequests)
    await stage.getByRole('button', { name: 'Show attempts', exact: true }).click()
    await expect(stage).toContainText('1.2.0')
    await stage.getByRole('button', { name: 'Show evidence', exact: true }).click()
    await stage.getByRole('button', { name: 'Load more evidence', exact: true }).click()
    await expect(stage.getByRole('button', { name: 'Open evidence e2', exact: true })).toBeVisible()
    await stage.getByRole('button', { name: 'Open evidence e1', exact: true }).click()
    await expect(stage.getByLabel('Structured evidence')).toContainText('Structured retained observation')
    expect(counts.get(`${root}/contexts/current/evidence/e1/artifacts/artifact-1`)).toBeUndefined()
    await stage.getByRole('button', { name: 'Open artifact (text/plain)', exact: true }).click()
    await expect(stage.getByLabel('Artifact content')).toContainText('Retained artifact content')
    await stage.getByRole('button', { name: /Historical verification/ }).click()
    await expect(stage.getByRole('heading', { name: `Historical revision · ${'b'.repeat(40)}` })).toBeVisible()
    await stage.getByRole('button', { name: 'Load earlier contexts' }).click()
    await expect(stage.getByRole('heading', { name: `Historical revision · ${'c'.repeat(40)}` })).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  })
}

test('task sheet shows verification and viewer has no observation action', async ({ page }) => {
  await fixture(page, { viewer: true, active: true })
  await page.goto('/tasks/verification-fixture')
  const stage = page.getByRole('region', { name: 'Verification', exact: true })
  await expect(stage.getByRole('heading', { name: 'Verify · running' })).toBeVisible()
  await stage.getByRole('button', { name: 'Show attempts', exact: true }).click()
  await expect(stage).toContainText('fixture-kit · running')
  await expect(stage.getByRole('button', { name: 'Record observation' })).toHaveCount(0)
})

test('operator observation retries retain one key and exclude server provenance', async ({ page }) => {
  const { observations } = await fixture(page, { active: true })
  await page.goto('/tasks/verification-fixture/full')
  const stage = page.getByRole('region', { name: 'Verification', exact: true })
  await stage.getByRole('button', { name: 'Show attempts', exact: true }).click()
  await stage.getByRole('button', { name: 'Record observation', exact: true }).click()
  await stage.getByLabel('Observed fact', { exact: true }).fill('The expected result is visible.')
  await stage.getByLabel('Supporting evidence IDs (comma separated)', { exact: true }).fill('e1')
  await stage.getByRole('button', { name: 'Save observation', exact: true }).click()
  await expect(stage.getByRole('alert')).toContainText('retry the same observation')
  await stage.getByRole('button', { name: 'Save observation', exact: true }).click()
  await expect(stage.getByRole('status')).toContainText('Observation recorded.')
  expect(observations).toHaveLength(2)
  expect(observations[0]).toEqual(observations[1])
  expect(Object.keys(observations[0]).sort()).toEqual(['context_id', 'fact', 'idempotency_key', 'run_id', 'supporting'])
})

test('review disclosure loads the sealed result and cited evidence independently', async ({ page }) => {
  const { counts } = await fixture(page, { review: true })
  await page.goto('/tasks/verification-fixture/full')
  expect(counts.get('/v1/tasks/verification-fixture/audit/event/4')).toBeUndefined()
  await page.getByRole('button', { name: 'Verification relied on by review' }).click()
  await expect(page.getByText(/Review deciding actor: user:reviewer/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Open evidence e1', exact: true })).toBeVisible()
  expect(counts.get('/v1/tasks/verification-fixture/verification/contexts/current/evidence/e1')).toBeUndefined()
})
