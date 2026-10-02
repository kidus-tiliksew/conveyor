import { expect, type Page, test } from '@playwright/test'

// feature-verification-kit-execution VK-13.5 and component-web-dashboard
// VK-WEB-6: a verify order released at its operator checkpoint is labelled
// Verification, names its reason, required act, head and grounds, and offers
// recovery only within the operator's capability.

const at = '2026-10-01T10:00:00Z'
const sha = 'e'.repeat(40)
const taskId = 'verification-checkpoint'
const reason = 'No grant covers network:api after the grant wait.'
const requiredAction = "Recover verify order verify-1, then grant network:api for the next claim's context."

type Ground = {
  kind: string
  subject: { kind: 'kit' | 'ordinary'; kit_id?: string; exercise_id?: string; obligation_id?: string }
  attempt_id?: string
  server_verified: boolean
}

const missingGrant: Ground = {
  kind: 'missing_grant',
  subject: { kind: 'ordinary', obligation_id: 'network-check' },
  server_verified: true,
}
const blockedAndWaiting: Ground[] = [
  {
    kind: 'attempt_blocked',
    subject: { kind: 'kit', kit_id: 'sample', exercise_id: 'login' },
    attempt_id: 'run-b',
    server_verified: true,
  },
  {
    kind: 'attempt_waiting',
    subject: { kind: 'kit', kit_id: 'sample', exercise_id: 'approve' },
    attempt_id: 'run-w',
    server_verified: true,
  },
]

function summary(grounds: Ground[]) {
  return grounds
    .map(
      (g) => `${g.kind} ${g.subject.kind}:${g.subject.obligation_id ?? `${g.subject.kit_id}/${g.subject.exercise_id}`}`,
    )
    .join('; ')
}

async function fixture(
  page: Page,
  options: { role?: string; grounds?: Ground[]; operations?: string[]; claimed?: boolean; refuseFirst?: boolean } = {},
) {
  const grounds = options.grounds ?? [missingGrant]
  const recoveries: Record<string, unknown>[] = []
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const task = {
    id: taskId,
    workspace: 'demo',
    source: 'mcp',
    title: 'Verification checkpoint fixture',
    body: 'A verify order stopped at the operator checkpoint.',
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
    branch: 'conveyor/task-verification-checkpoint',
    state: 'queued',
    next_stage: 'verify',
    reviewed_head_sha: sha,
    created_at: at,
  }
  const order = options.claimed
    ? {
        id: 'verify-1',
        task_id: taskId,
        job_id: 'verify-1',
        stage: 'verify',
        state: 'claimed',
        claimed_by: 'worker-verifier',
        attempt_id: 'attempt-2',
        created_at: at,
        updated_at: at,
        execution_started_at: at,
      }
    : {
        id: 'verify-1',
        task_id: taskId,
        job_id: 'verify-1',
        stage: 'verify',
        state: 'queued',
        claimable: false,
        created_at: at,
        updated_at: at,
        queue_entered_at: at,
        last_attempt_id: 'attempt-1',
        last_attempt_outcome: 'released',
        last_failure_message: 'operator checkpoint reached',
        retry_suppressed: true,
        retry_suppression_reason: 'operator checkpoint reached',
        checkpoint: {
          decision_request: `${reason}\nRequired operator action: ${requiredAction}`,
          verification: {
            context_id: 'cp',
            head_sha: sha,
            reason,
            required_action: requiredAction,
            summary: summary(grounds),
            grounds,
            operation_ids: options.operations,
          },
        },
      }
  const job = {
    id: 'verify-1',
    task_id: taskId,
    stage: 'verify',
    harness: 'codex',
    model_tier: 'operator-owned',
    runner: 'mcp',
    confinement: 'none',
    tokens_in: 0,
    tokens_out: 0,
    state: options.claimed ? 'running' : 'pending',
    started_at: at,
  }
  const context = {
    id: 'cp',
    context_id: 'cp',
    run_id: '',
    state: 'sealed',
    at,
    metadata: {
      work_order_id: 'verify-1',
      source_sha: sha,
      sealed_at: at,
      outcome: 'operator_action_required',
      reason,
      required_action: requiredAction,
      checkpoint_grounds: summary(grounds),
      checkpoint_head: sha,
      checkpoint_attempt: 'attempt-1',
      attempt_count: String(grounds.filter((g) => g.attempt_id).length),
    },
  }
  const item = {
    task,
    jobs: [job],
    events: [],
    interventions: [],
    checkout_available: true,
    checkout_guidance: '',
    needs_attention: !options.claimed,
    at_merge_gate: false,
    attachments: [],
    verification_evidence: [],
    work_orders: [order],
    spec: { task_id: taskId, version: 1, content: 'Verify the checkpoint.', approved: true, created_at: at },
  }
  await page.route('**/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'operator', role: options.role ?? 'maintainer' } })
    if (path === '/v1/activity')
      return route.fulfill({ json: [{ task, latest_stage: 'verify', last_event_at: at, needs_attention: true }] })
    if (path.endsWith('/activity')) return route.fulfill({ json: item })
    if (path.endsWith('/events/stream')) return route.fulfill({ contentType: 'text/event-stream', body: '' })
    if (path === '/v1/work-orders/verify-1/recover') {
      recoveries.push({ body: route.request().postDataJSON(), key: route.request().headers()['x-idempotency-key'] })
      if (options.refuseFirst && recoveries.length === 1)
        return route.fulfill({ status: 409, json: { error: 'work order head changed; recovery refused' } })
      return route.fulfill({ json: { ...order, state: 'queued', claimable: true, retry_suppressed: false } })
    }
    if (path.endsWith('/verification'))
      return route.fulfill({
        json: {
          head_sha: sha,
          current_context_id: options.claimed ? '' : 'cp',
          contexts: { items: options.claimed ? [] : [context] },
          overview: {},
        },
      })
    if (path.includes('/verification/')) return route.fulfill({ json: { items: [] } })
    return route.fulfill({ json: [] })
  })
  return { recoveries }
}

const verifyEntry = (page: Page) => page.getByRole('article', { name: 'Verification', exact: true })

test('pre-execution missing-grant checkpoint names grounds and recovers to verify', async ({ page }) => {
  const { recoveries } = await fixture(page, { refuseFirst: true })
  await page.goto(`/tasks/${taskId}/full`)
  await expect(page.locator('#current-execution-title')).toHaveText(
    'Verification checkpoint — operator action required',
  )
  const entry = verifyEntry(page)
  await expect(entry).toContainText('Needs you')
  await expect(entry).toContainText('Verification checkpoint — waiting on you')
  await expect(entry).toContainText(reason)
  await expect(entry).toContainText(`Required action: ${requiredAction}`)
  await expect(entry.getByRole('list', { name: 'Checkpoint grounds' })).toContainText(
    'Missing grant: ordinary network-check — no attempt ran; evidence missing',
  )
  await expect(entry).toContainText('released by attempt-1')
  const card = page.getByRole('region', { name: 'Verification checkpoint', exact: true })
  await expect(card).toContainText(requiredAction)
  await expect(card).toContainText(sha.slice(0, 12))
  await expect(card).toContainText('Automatic replay is suppressed until an operator recovers this order.')
  await expect(card.getByRole('link', { name: 'cp' })).toHaveAttribute('href', '#verification-cp')
  await expect(card.getByLabel('Operator direction')).toHaveCount(0)
  await card.getByRole('button', { name: 'Recover verification' }).click()
  await expect(card.getByRole('alert')).toContainText('recovery refused')
  await expect(card).toContainText(reason)
  await card.getByRole('button', { name: 'Recover verification' }).click()
  await expect.poll(() => recoveries.length).toBe(2)
  // One request identity covers the refused and the retried attempt; no free
  // text direction is sent.
  expect(recoveries[0].key).toBe(recoveries[1].key)
  expect((recoveries[1].body as Record<string, unknown> | null)?.direction).toBeUndefined()
})

test('blocked and waiting checkpoints link their attempts', async ({ page }) => {
  await fixture(page, { grounds: blockedAndWaiting })
  await page.goto(`/tasks/${taskId}/full`)
  const grounds = verifyEntry(page).getByRole('list', { name: 'Checkpoint grounds' })
  await expect(grounds).toContainText('Blocked attempt: kit sample/login — attempt run-b')
  await expect(grounds).toContainText('Waiting attempt: kit sample/approve — attempt run-w')
  await expect(grounds).not.toContainText('evidence missing')
})

test('unresolved operations require a typed disposition instead of direction', async ({ page }) => {
  await fixture(page, {
    grounds: [
      {
        kind: 'operation_unresolved',
        subject: { kind: 'kit', kit_id: 'sample', exercise_id: 'publish' },
        attempt_id: 'run-p',
        server_verified: true,
      },
    ],
    operations: ['op-7'],
  })
  await page.goto(`/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Verification checkpoint', exact: true })
  await expect(card).toContainText('Typed disposition required')
  await expect(card).toContainText('op-7')
  await expect(card.getByRole('button', { name: 'Recover verification' })).toHaveCount(0)
  await expect(card.getByLabel('Operator direction')).toHaveCount(0)
})

test('viewer sees the verification checkpoint but no recovery', async ({ page }) => {
  await fixture(page, { role: 'viewer' })
  await page.goto(`/tasks/${taskId}`)
  await expect(page.locator('#current-execution-title')).toHaveText(
    'Verification checkpoint — operator action required',
  )
  await expect(verifyEntry(page)).toContainText(requiredAction)
  await expect(page.getByRole('button', { name: 'Recover verification' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Verification checkpoint', exact: true })).toHaveCount(0)
})

test('a running verify order is labelled Verification', async ({ page }) => {
  await fixture(page, { claimed: true })
  await page.goto(`/tasks/${taskId}/full`)
  await expect(page.locator('#current-execution-title')).toHaveText('Verification is in progress')
})
