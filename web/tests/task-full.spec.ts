import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-15T12:00:00Z'
let emitLiveScrollEvent = () => {}

function activity(taskId: string, overflowing: boolean, liveEventCount = 18) {
	const specContent = taskId === 'long-spec'
		? ['## Specification', '', ...Array.from({ length: 60 }, (_, index) => `Long specification paragraph ${index + 1}.`), '', 'Long spec ending marker.'].join('\n\n')
		: '## Specification\n\nRegression marker at the bottom of the task content.'
	const reviewActivity = taskId === 'live-scroll' ? {
		jobs: [],
		events: Array.from({ length: liveEventCount }, (_, index) => ({
			id: index + 1,
			task_id: taskId,
			kind: 'spec.version_created',
			actor_id: 'system',
			actor_role: 'system' as const,
			payload: { version: index + 1 },
			at: `2026-07-15T12:00:${String(index).padStart(2, '0')}Z`,
		})),
		work_orders: [],
	} : taskId === 'reviews' ? {
		jobs: [
			{ id: 'reviews-review-1-seat-1', task_id: taskId, stage: 'review', harness: 'codex', model_tier: 'gpt-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: createdAt, ended_at: '2026-07-15T12:01:00Z' },
			{ id: 'reviews-review-1-seat-2', task_id: taskId, stage: 'review', harness: 'claude', model_tier: 'claude-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: '2026-07-15T12:02:00Z', ended_at: '2026-07-15T12:03:00Z' },
		],
		events: [
			{ id: 1, task_id: taskId, kind: 'spec.version_approved', actor_id: 'operator', actor_role: 'human', payload: { version: 1 }, at: '2026-07-15T11:59:00Z' },
			{ id: 2, task_id: taskId, job_id: 'reviews-review-1-seat-1', kind: 'review.completed', actor_id: 'worker-1', actor_role: 'runner', payload: { review_work_order_id: 'reviews-review-1-seat-1', review_round: 1, verdict: 'approve', summary: 'Seat one approved', feedback: 'Approved guidance remains visible.', review_seat: 1, reviewer_model: 'gpt-review', required_effort: 'high', model_enforcement: 'worker-pinned' }, at: '2026-07-15T12:01:00Z' },
			{ id: 3, task_id: taskId, kind: 'review.completed', actor_id: 'legacy-worker', actor_role: 'runner', payload: { verdict: 'approve', summary: 'Legacy duplicate', feedback: 'This standalone duplicate stays hidden.' }, at: '2026-07-15T12:01:15Z' },
			{ id: 4, task_id: taskId, kind: 'pull_request.opened', actor_id: 'system', actor_role: 'system', payload: { url: 'https://example.test/pull/1' }, at: '2026-07-15T12:01:30Z' },
			// Deliberately no review_work_order_id: seat matching must fall back
			// to the event's job id for older payloads.
			{ id: 5, task_id: taskId, job_id: 'reviews-review-1-seat-2', kind: 'review.completed', actor_id: 'worker-2', actor_role: 'runner', payload: { verdict: 'changes_requested', summary: 'Seat two requested changes', feedback: 'Changes guidance remains visible.', review_seat: 2, reviewer_model: 'claude-review', required_effort: '', model_enforcement: 'worker-pinned' }, at: '2026-07-15T12:03:00Z' },
			{ id: 6, task_id: taskId, kind: 'pipeline.bounced', actor_id: 'system', actor_role: 'system', payload: { count: 1, reason_code: 'changes_requested', feedback: 'Changes guidance remains visible.' }, at: '2026-07-15T12:04:00Z' },
		],
		work_orders: [
			{ id: 'reviews-review-1-seat-1', task_id: taskId, job_id: 'reviews-review-1-seat-1', stage: 'review', state: 'completed', review_round: 1, review_seat: 1, required_model: 'gpt-review', required_harness: 'codex', required_effort: 'high', model_enforcement: 'worker-pinned', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true, created_at: createdAt, updated_at: '2026-07-15T12:01:00Z' },
			{ id: 'reviews-review-1-seat-2', task_id: taskId, job_id: 'reviews-review-1-seat-2', stage: 'review', state: 'completed', review_round: 1, review_seat: 2, required_model: 'claude-review', required_harness: 'claude', model_enforcement: 'worker-pinned', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true, created_at: createdAt, updated_at: '2026-07-15T12:03:00Z' },
		],
	} : taskId === 'output-invalid' ? {
		jobs: [
			...Array.from({ length: 5 }, (_, index) => ({
				id: `output-invalid-spec-${index + 1}`,
				task_id: taskId,
				stage: 'spec',
				harness: 'codex',
				model_tier: 'gpt-spec',
				auth_mode: 'subscription',
				runner: 'in-process',
				confinement: 'none',
				cost_usd: 0,
				tokens_in: 0,
				tokens_out: 0,
				state: 'done',
				started_at: `2026-07-15T12:0${index}:00Z`,
				ended_at: `2026-07-15T12:0${index}:30Z`,
			})),
			{ id: 'output-invalid-preferred-job', task_id: taskId, stage: 'triage', harness: 'codex', model_tier: 'gpt-triage', runner: 'in-process', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: '2026-07-15T12:05:00Z', ended_at: '2026-07-15T12:05:30Z' },
			{ id: 'output-invalid-preferred-triage', task_id: taskId, stage: 'triage', harness: 'codex', model_tier: 'gpt-triage', runner: 'in-process', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: '2026-07-15T12:06:00Z', ended_at: '2026-07-15T12:06:30Z' },
			{ id: 'output-invalid-preferred-review', task_id: taskId, stage: 'review', harness: 'codex', model_tier: 'gpt-review', runner: 'in-process', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: '2026-07-15T12:07:00Z', ended_at: '2026-07-15T12:07:30Z' },
		],
		events: [
			...Array.from({ length: 4 }, (_, index) => ({
				id: index + 1,
				task_id: taskId,
				job_id: `output-invalid-spec-${index + 1}`,
				kind: 'spec.output_invalid',
				actor_id: 'pipeline',
				actor_role: 'system' as const,
				payload: {
					error: index === 0 ? 'spec requires one conveyor:acceptance block' : `spec validation error ${index + 1}`,
					output: index === 0 ? 'PRIVATE REJECTED OUTPUT' : undefined,
				},
				at: `2026-07-15T12:0${index}:31Z`,
			})),
			{ id: 5, task_id: taskId, job_id: 'output-invalid-spec-5', kind: 'spec.version_created', actor_id: 'pipeline', actor_role: 'system', payload: { version: 1 }, at: '2026-07-15T12:04:31Z' },
			// A same-stage rejection for another job must not mark the accepted
			// spec attempt as rejected.
			{ id: 6, task_id: taskId, job_id: 'output-invalid-unrelated', kind: 'spec.output_invalid', actor_id: 'pipeline', actor_role: 'system', payload: { error: 'unrelated validation error' }, at: '2026-07-15T12:04:32Z' },
			{ id: 7, task_id: taskId, job_id: 'output-invalid-preferred-job', kind: 'triage.output_invalid', actor_id: 'pipeline', actor_role: 'system', payload: { error: 'superseded job error' }, at: '2026-07-15T12:05:31Z' },
			{ id: 8, task_id: taskId, job_id: 'output-invalid-preferred-job', kind: 'job.summary', actor_id: 'runner', actor_role: 'runner', payload: { summary: 'Harness narration wins.' }, at: '2026-07-15T12:05:32Z' },
			{ id: 9, task_id: taskId, job_id: 'output-invalid-preferred-triage', kind: 'triage.output_invalid', actor_id: 'pipeline', actor_role: 'system', payload: { error: 'superseded triage error' }, at: '2026-07-15T12:06:31Z' },
			{ id: 10, task_id: taskId, job_id: 'output-invalid-preferred-triage', kind: 'triage.completed', actor_id: 'runner', actor_role: 'runner', payload: { summary: 'Accepted triage summary wins.' }, at: '2026-07-15T12:06:32Z' },
			{ id: 11, task_id: taskId, job_id: 'output-invalid-preferred-review', kind: 'review.output_invalid', actor_id: 'pipeline', actor_role: 'system', payload: { error: 'superseded review error' }, at: '2026-07-15T12:07:31Z' },
			{ id: 12, task_id: taskId, job_id: 'output-invalid-preferred-review', kind: 'review.completed', actor_id: 'runner', actor_role: 'runner', payload: { summary: 'Accepted review summary wins.', feedback: 'Accepted review feedback wins too.' }, at: '2026-07-15T12:07:32Z' },
			{ id: 13, task_id: taskId, job_id: 'output-invalid-spec-4', kind: 'pipeline.bounce_limit', actor_id: 'pipeline', actor_role: 'system', payload: { source: 'spec.output_invalid', max_bounces: 4 }, at: '2026-07-15T12:08:00Z' },
		],
		work_orders: [],
	} : taskId === 'diagnostics' ? {
		jobs: [
			{ id: 'diagnostics-review-1-seat-1', task_id: taskId, stage: 'review', harness: 'codex', model_tier: 'gpt-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'running', started_at: '2026-07-15T12:00:00Z' },
			{ id: 'diagnostics-review-1-seat-2', task_id: taskId, stage: 'review', harness: 'claude', model_tier: 'claude-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'running', started_at: '2026-07-15T12:00:30Z' },
		],
		events: [
			{ id: 1, task_id: taskId, kind: 'spec.version_approved', actor_id: 'operator', actor_role: 'human', payload: { version: 1 }, at: '2026-07-15T11:59:00Z' },
			{ id: 2, task_id: taskId, kind: 'pull_request.opened', actor_id: 'system', actor_role: 'system', payload: { url: 'https://example.test/pull/1' }, at: '2026-07-15T12:05:00Z' },
		],
		work_orders: [
			{ id: 'diagnostics-review-1-seat-1', task_id: taskId, job_id: 'diagnostics-review-1-seat-1', stage: 'review', state: 'claimed', review_round: 1, review_seat: 1, required_model: 'gpt-review', required_harness: 'codex', queue_entered_at: '2026-07-15T12:00:00Z', queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'diagnostics-review-1-seat-2', task_id: taskId, job_id: 'diagnostics-review-1-seat-2', stage: 'review', state: 'claimed', review_round: 1, review_seat: 2, required_model: 'claude-review', required_harness: 'claude', queue_entered_at: '2026-07-15T12:00:30Z', queue_deadline: '2026-07-16T12:00:30Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : taskId === 'timeout' ? {
		jobs: [{ id: 'timeout-review', task_id: taskId, stage: 'review', harness: 'claude', model_tier: 'claude-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'failed', started_at: '2026-07-15T12:00:00Z', ended_at: '2026-07-15T12:30:00Z' }],
		events: [],
		work_orders: [{ id: 'timeout-review', task_id: taskId, job_id: 'timeout-review', stage: 'review', state: 'timed_out', queue_entered_at: '2026-07-15T11:00:00Z', queue_deadline: '2026-07-16T11:00:00Z', execution_started_at: '2026-07-15T12:00:00Z', execution_deadline: '2026-07-15T12:30:00Z', updated_at: '2026-07-16T08:24:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'recovery' ? {
		jobs: [{ id: 'recovery-review-1-seat-1', task_id: taskId, stage: 'review', state: 'pending', cost_usd: 0, tokens_in: 0, tokens_out: 0 }],
		events: [],
		work_orders: [{ id: 'recovery-review-1-seat-1', task_id: taskId, job_id: 'recovery-review-1-seat-1', stage: 'review', state: 'queued', claimable: false, last_attempt_outcome: 'child_failure', last_failure_message: 'harness exited: status 1', last_failure_exit_status: 1, last_failure_at: createdAt, automatic_retry_count: 3, retry_suppressed: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'interrupted-review' ? {
		jobs: [
			{ id: 'interrupted-review-review-1-seat-1', task_id: taskId, stage: 'review', state: 'done', cost_usd: 0, tokens_in: 0, tokens_out: 0 },
			{ id: 'interrupted-review-review-1-seat-2', task_id: taskId, stage: 'review', state: 'pending', cost_usd: 0, tokens_in: 0, tokens_out: 0 },
		],
		events: [{ id: 1, task_id: taskId, job_id: 'interrupted-review-review-1-seat-1', kind: 'review.completed', actor_id: 'worker-1', actor_role: 'runner', payload: { verdict: 'approve', summary: 'Completed verdict', feedback: 'Retain this verdict.', review_round: 1, review_seat: 1 }, at: createdAt }],
		work_orders: [
			{ id: 'interrupted-review-review-1-seat-1', task_id: taskId, job_id: 'interrupted-review-review-1-seat-1', stage: 'review', state: 'completed', review_round: 1, review_seat: 1, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'interrupted-review-review-1-seat-2', task_id: taskId, job_id: 'interrupted-review-review-1-seat-2', stage: 'review', state: 'queued', claimable: false, review_round: 1, review_seat: 2, last_attempt_outcome: 'expired', retry_suppressed: true, required_harness: 'claude', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : taskId === 'review-retry' ? {
		jobs: [
			{ id: 'review-retry-review-1-seat-1', task_id: taskId, stage: 'review', state: 'done', cost_usd: 0, tokens_in: 0, tokens_out: 0 },
			{ id: 'review-retry-review-1-seat-2', task_id: taskId, stage: 'review', state: 'failed', cost_usd: 0, tokens_in: 0, tokens_out: 0 },
		],
		events: [{ id: 1, task_id: taskId, job_id: 'review-retry-review-1-seat-1', kind: 'review.completed', actor_id: 'worker-1', actor_role: 'runner', payload: { verdict: 'approve', summary: 'Historical approval', feedback: 'Round one feedback remains visible.', review_round: 1, review_seat: 1 }, at: createdAt }],
		work_orders: [
			{ id: 'review-retry-review-1-seat-1', task_id: taskId, job_id: 'review-retry-review-1-seat-1', stage: 'review', state: 'completed', review_round: 1, review_seat: 1, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'review-retry-review-1-seat-2', task_id: taskId, job_id: 'review-retry-review-1-seat-2', stage: 'review', state: 'timed_out', review_round: 1, review_seat: 2, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', execution_deadline: '2026-07-15T13:00:00Z', last_failure_message: 'review harness exhausted retries', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : { jobs: [], events: [], work_orders: taskId === 'no-orders' ? null : [] }
	const reviewDiagnostics = taskId === 'diagnostics' ? [
		{ status: 'claimed_without_verdict', work_order_id: 'diagnostics-review-1-seat-1', review_round: 1, review_seat: 1, claimed_at: '2026-07-15T12:00:00Z', lease_expires_at: '2026-07-15T12:15:00Z', reason: 'review claim is active without a successful submit_review_verdict response' },
		{ status: 'claimed_without_verdict', work_order_id: 'diagnostics-review-1-seat-2', review_round: 1, review_seat: 2, claimed_at: '2026-07-15T12:00:30Z', lease_expires_at: '2026-07-15T12:15:30Z', reason: 'review claim is active without a successful submit_review_verdict response' },
		{ status: 'expired_without_verdict', work_order_id: 'diagnostics-review-0-seat-3', review_round: 0, review_seat: 3, claimed_at: '2026-07-15T12:01:00Z', lease_expires_at: '2026-07-15T12:02:00Z', reason: 'review claim lease expired without terminal verdict submission' },
	] : []
  return {
    task: {
      id: taskId,
      workspace: 'demo',
      source: 'mcp',
      title: overflowing ? 'Overflowing task' : 'Short task',
      body: overflowing ? Array.from({ length: 80 }, (_, index) => `Description line ${index + 1}`).join('\n') : 'A short description.',
      class: 'bug',
      level: 'L2',
      repo: 'conveyor',
      base_branch: 'main',
      branch: `conveyor/task-${taskId}`,
		state: taskId === 'gate' ? 'awaiting_human' : taskId === 'merge-unknown' || taskId === 'merge-conflict' ? 'approved' : 'running',
      next_stage: 'implement',
      created_at: createdAt,
    },
		jobs: reviewActivity.jobs,
		events: reviewActivity.events,
    interventions: [],
    checkout_available: false,
    checkout_guidance: 'Use the assigned worktree.',
    needs_attention: false,
    spec: {
      task_id: taskId,
      version: 1,
      content: specContent,
      acceptance_count: 0,
      acceptance: [],
      decomposition: [],
      approved: true,
      created_at: createdAt,
      approved_at: createdAt,
    },
		work_orders: reviewActivity.work_orders,
		review_diagnostics: reviewDiagnostics,
		review_recovery: taskId === 'review-retry' ? {
			needed: true,
			prior_round: 1,
			reason: 'latest review round is terminal after a reviewer timed out',
			timed_out_orders: reviewActivity.work_orders?.filter((order) => order.state === 'timed_out') ?? [],
		} : undefined,
		interrupted_review_recovery: taskId === 'interrupted-review' ? {
			needed: true,
			review_round: 1,
			reason: 'latest review round has interrupted seats whose claims are no longer authorized',
			eligible_orders: reviewActivity.work_orders?.filter((order) => order.state === 'queued') ?? [],
			retained_orders: reviewActivity.work_orders?.filter((order) => order.state === 'completed') ?? [],
		} : undefined,
		worker_status: taskId === 'interrupted-review' ? {
			available: false,
			required_harnesses: ['claude'],
			reason: 'no healthy worker can serve the task\'s required harnesses; last heartbeat was 2m ago',
			last_heartbeat_at: createdAt,
			last_heartbeat_age: '2m0s',
			queue_context: 'interrupted',
		} : taskId === 'null-worker-status' ? {
			available: false,
			required_harnesses: null,
			reason: 'no healthy worker can serve the task\'s required harnesses',
			queue_context: 'never_started',
		} : undefined,
		merge_readiness: taskId === 'merge-unknown' ? { state: 'UNKNOWN', head_sha: 'head-1' }
			: taskId === 'merge-conflict' ? { state: 'CONFLICTING', head_sha: 'head-1', number: 12 }
			: undefined,
  }
}

async function mockTaskAPIs(page: Page) {
  let liveActivityRequests = 0
  let releaseLiveStream = () => {}
  const liveStreamRelease = new Promise<void>((resolve) => { releaseLiveStream = resolve })
  emitLiveScrollEvent = releaseLiveStream
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    const taskMatch = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (taskMatch) {
      const taskId = decodeURIComponent(taskMatch[1])
      if (taskId === 'live-scroll') liveActivityRequests++
	  await route.fulfill({ json: activity(taskId, taskId === 'overflowing' || taskId === 'gate', liveActivityRequests > 1 ? 19 : 18) })
      return
    }
		if (url.pathname === '/v1/activity') {
			const item = activity('diagnostics', false)
			await route.fulfill({ json: [{ task: item.task, latest_stage: 'review', last_event_at: createdAt, needs_attention: false, review_diagnostics: item.review_diagnostics }] })
			return
		}
    if (url.pathname.endsWith('/events/stream')) {
      if (url.pathname.includes('/live-scroll/')) {
        await liveStreamRelease
        await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
        return
      }
      await route.fulfill({ status: 204 })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test.beforeEach(async ({ page }) => {
  await mockTaskAPIs(page)
})

test('task detail headers show the task name while routes and API lookup keep using the task ID', async ({ page }) => {
	const fullTaskID = 'full-header-id'
	const fullActivity = page.waitForRequest((request) => new URL(request.url()).pathname === `/v1/tasks/${fullTaskID}/activity`)
	await page.goto(`/tasks/${fullTaskID}/full`)
	await fullActivity
	const fullHeader = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to board' }) })
	await expect(fullHeader).toContainText('Short task')
	await expect(fullHeader).not.toContainText(fullTaskID)
	expect(new URL(page.url()).pathname).toBe(`/tasks/${fullTaskID}/full`)

	const sheetTaskID = 'sheet-header-id'
	const sheetActivity = page.waitForRequest((request) => new URL(request.url()).pathname === `/v1/tasks/${sheetTaskID}/activity`)
	await page.goto(`/tasks/${sheetTaskID}`)
	await sheetActivity
	const sheetHeader = page.locator('header').filter({ has: page.getByRole('button', { name: 'Close panel' }) })
	await expect(sheetHeader).toContainText('Short task')
	await expect(sheetHeader).not.toContainText(sheetTaskID)
	expect(new URL(page.url()).pathname).toBe(`/tasks/${sheetTaskID}`)
})

test('new task detail tolerates a null work-order list from the API', async ({ page }) => {
	await page.goto('/tasks/no-orders/full')
	await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
	await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('task detail tolerates null required harnesses from a legacy worker status', async ({ page }) => {
	await page.goto('/tasks/null-worker-status/full')
	await expect(page.getByText('No healthy worker can serve this Auto task')).toBeVisible()
	await expect(page.getByText('Required harnesses: not yet routed.')).toBeVisible()
	await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('suppressed worker order exposes failure state and audited recovery action', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let recoveryRequest = ''
	await page.route('**/v1/work-orders/*/recover*', async (route) => {
		recoveryRequest = route.request().postData() ?? ''
		await route.fulfill({ json: { id: 'recovery-review-1-seat-1', state: 'queued', claimable: true } })
	})
	await page.goto('/tasks/recovery/full')
	await expect(page.getByText(/harness exited: status 1/)).toBeVisible()
	await expect(page.getByText(/Automatic retry is suppressed/)).toBeVisible()
	await page.getByRole('button', { name: 'Recover work order' }).click()
	await expect.poll(() => recoveryRequest).toContain('request_id')
})

test('timed-out review round exposes a reasoned full-round retry and preserves history', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let retryRequest = ''
	await page.route('**/v1/tasks/*/review-round/retry*', async (route) => {
		retryRequest = route.request().postData() ?? ''
		await route.fulfill({ json: { request_id: 'retry-1', task_id: 'review-retry', prior_round: 1, new_round: 2, pr_head: 'abc', work_orders: [{ id: 'seat-1' }, { id: 'seat-2' }] } })
	})
	await page.goto('/tasks/review-retry/full')
	await expect(page.getByText('Review round 1 needs operator attention')).toBeVisible()
	await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
	await expect(page.getByText(/review harness exhausted retries/)).toBeVisible()
	await expect(page.getByText(/Round one feedback remains visible/)).toBeVisible()
	const retry = page.getByRole('button', { name: 'Retry review round' })
	await expect(retry).toBeDisabled()
	await page.getByLabel('Review retry reason').fill('Retry with the corrected current harness configuration')
	await expect(retry).toBeEnabled()
	await retry.click()
	await expect.poll(() => retryRequest).toContain('request_id')
	expect(retryRequest).toContain('corrected current harness configuration')
	await expect(page.getByText('Review round 2 is queued with 2 seats.')).toBeVisible()
})

test('interrupted review round offers one same-round recovery and retains completed verdicts', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let recoveryRequest = ''
	await page.route('**/v1/tasks/*/review-round/recover*', async (route) => {
		recoveryRequest = route.request().postData() ?? ''
		await route.fulfill({ json: { request_id: 'recover-1', task_id: 'interrupted-review', review_round: 1, recovered_orders: [{ id: 'seat-2' }], retained_orders: [{ id: 'seat-1' }] } })
	})
	await page.goto('/tasks/interrupted-review/full')
	await expect(page.getByText('No healthy worker can serve this Auto task')).toBeVisible()
	await expect(page.getByText(/This work was interrupted/)).toBeVisible()
	await expect(page.getByText(/completed verdict retained/)).toBeVisible()
	await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
	const action = page.getByRole('button', { name: 'Recover interrupted review round' })
	await expect(action).toHaveCount(1)
	await action.click()
	await expect.poll(() => recoveryRequest).toContain('request_id')
	await expect(page.getByText(/Recovered 1 interrupted seat; 1 completed verdict retained/)).toBeVisible()
})

test('timeout timeline and duration use the execution deadline', async ({ page }) => {
	await page.goto('/tasks/timeout/full')
	const expectedDeadline = await page.evaluate(() => new Date('2026-07-15T12:30:00Z').toLocaleString('en', {
		month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
	}))
	const timeoutEntry = page.locator('li').filter({ hasText: 'Review — timed out' })
	await expect(timeoutEntry.getByText(expectedDeadline)).toBeVisible()
	await expect(page.locator('article').filter({ hasText: 'claude-review' }).getByText('30m 00s')).toBeVisible()
})

test('overflowing full-screen task content scrolls from top to bottom', async ({ page }) => {
  await page.goto('/tasks/overflowing/full')

  const content = page.getByRole('region', { name: 'Task content' })
  const marker = page.getByText('Regression marker at the bottom of the task content.')
  await expect(content).toBeVisible()

  const initial = await content.evaluate((element) => ({
    clientHeight: element.clientHeight,
    overflowY: getComputedStyle(element).overflowY,
    scrollHeight: element.scrollHeight,
    scrollTop: element.scrollTop,
    touchAction: getComputedStyle(element).touchAction,
  }))
  expect(initial.overflowY).toBe('auto')
  expect(initial.scrollHeight).toBeGreaterThan(initial.clientHeight)
  expect(initial.scrollTop).toBe(0)
  expect(initial.touchAction).not.toBe('none')
  await expect(marker).not.toBeInViewport()

  await content.hover()
  await page.mouse.wheel(0, 500)
  await expect.poll(() => content.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)

  await content.focus()
  await page.keyboard.press('End')
  await expect(marker).toBeInViewport()
  await expect.poll(() => content.evaluate((element) => element.scrollTop + element.clientHeight)).toBeGreaterThanOrEqual(
    await content.evaluate((element) => element.scrollHeight - 1),
  )
})

test('short full-screen task content does not create a nested vertical scrollbar', async ({ page }) => {
  await page.goto('/tasks/short/full')

  const content = page.getByRole('region', { name: 'Task content' })
  await expect(content).toBeVisible()
  const dimensions = await content.evaluate((element) => ({
    clientHeight: element.clientHeight,
    scrollHeight: element.scrollHeight,
  }))
  expect(dimensions.scrollHeight).toBeLessThanOrEqual(dimensions.clientHeight)
})

test('task sheet bounds an overflowing spec and expands and collapses it accessibly', async ({ page }) => {
	await page.goto('/tasks/long-spec')

	const toggle = page.getByRole('button', { name: 'Show more' })
	await expect(toggle).toBeVisible()
	await expect(toggle).toHaveAttribute('aria-expanded', 'false')
	const viewportID = await toggle.getAttribute('aria-controls')
	expect(viewportID).toBeTruthy()
	const viewport = page.locator(`#${viewportID}`)
	await expect.poll(() => viewport.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
	await expect(page.locator('[data-spec-overflow-shadow]')).toBeVisible()
	await expect(page.getByText('Long spec ending marker.')).not.toBeInViewport()

	await toggle.click()
	const collapse = page.getByRole('button', { name: 'Show less' })
	await expect(collapse).toHaveAttribute('aria-expanded', 'true')
	await expect(page.locator('[data-spec-overflow-shadow]')).toHaveCount(0)
	await expect.poll(() => viewport.evaluate((element) => element.scrollHeight === element.clientHeight)).toBe(true)

	await collapse.click()
	await expect(page.getByRole('button', { name: 'Show more' })).toHaveAttribute('aria-expanded', 'false')
	await expect(page.locator('[data-spec-overflow-shadow]')).toBeVisible()
	await expect.poll(() => viewport.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
})

test('short side-view specs and full-page specs remain unbounded', async ({ page }) => {
	await page.goto('/tasks/short')
	await expect(page.getByRole('button', { name: /Show (more|less)/ })).toHaveCount(0)
	await expect(page.locator('[data-spec-overflow-shadow]')).toHaveCount(0)

	await page.goto('/tasks/long-spec/full')
	await expect(page.getByRole('button', { name: /Show (more|less)/ })).toHaveCount(0)
	await expect(page.locator('[data-spec-overflow-shadow]')).toHaveCount(0)
	const marker = page.getByText('Long spec ending marker.')
	await marker.scrollIntoViewIfNeeded()
	await expect(marker).toBeVisible()
})

test('review panel replaces duplicate review and bounce activity notes', async ({ page }) => {
	await page.goto('/tasks/reviews/full')

	// One panel card, not one card per seat (spec §21.12 change 4).
	const panel = page.locator('article').filter({ hasText: 'Panel of 2 · unanimous to pass' })
	await expect(panel).toHaveCount(1)

	// Both seats resolve their verdicts — including the seat whose
	// review.completed event carries no review_work_order_id.
	await expect(panel.getByText('Approved', { exact: true })).toBeVisible()
	await expect(panel.getByText('Changes', { exact: true })).toBeVisible()
	await expect(panel.getByText('pinned', { exact: true })).toHaveCount(2)
	await expect(panel.getByText('2 of 2 verdicts in')).toBeVisible()

	// Feedback from every seat stays visible, attributed in the merged notes.
	await expect(panel.getByText('Approved guidance remains visible.')).toBeVisible()
	await expect(panel.getByText('Changes guidance remains visible.')).toBeVisible()

	// The per-seat audit events fold into the panel — no duplicate rows.
	await expect(page.getByText(/^Independent review:/)).toHaveCount(0)
	await expect(page.getByText('Bounced back to implement (bounce 1)', { exact: true })).toHaveCount(0)

	const timelineRows = page.getByRole('region', { name: 'Execution event timeline' }).locator('ol > li')
	await expect(timelineRows).toHaveCount(3)
	await expect(timelineRows.nth(0)).toContainText('Spec v1 approved')
	await expect(timelineRows.nth(1)).toContainText('Panel of 2 · unanimous to pass')
	await expect(timelineRows.nth(2)).toContainText('Pull request opened')
})

test('output-validation rejections show job-specific errors with warning tone and preserve accepted narration', async ({ page }) => {
	await page.goto('/tasks/output-invalid/full')

	const timeline = page.getByRole('region', { name: 'Execution event timeline' })
	const rejected = timeline.locator('article').filter({ hasText: 'Output rejected by the pipeline' })
	await expect(rejected).toHaveCount(4)
	await expect(rejected.nth(0)).toContainText('spec requires one conveyor:acceptance block')
	await expect(rejected.nth(1)).toContainText('spec validation error 2')
	await expect(rejected.nth(2)).toContainText('spec validation error 3')
	await expect(rejected.nth(3)).toContainText('spec validation error 4')
	for (const card of await rejected.all()) {
		await expect(card).toHaveClass(/border-attention\/40/)
		await expect(card).toHaveClass(/bg-attention-soft/)
		await expect(card.locator('xpath=..').locator('.bg-attention-dot')).toHaveCount(1)
	}

	const acceptedSpec = timeline.locator('article').filter({ hasText: 'Completed.' }).filter({ has: page.getByText('Spec', { exact: true }) })
	await expect(acceptedSpec).toHaveCount(1)
	await expect(acceptedSpec).toHaveClass(/bg-card/)
	await expect(acceptedSpec).not.toHaveClass(/bg-attention-soft/)
	await expect(page.getByText('PRIVATE REJECTED OUTPUT')).toHaveCount(0)

	await expect(timeline.getByText('Harness narration wins.', { exact: true })).toBeVisible()
	await expect(timeline.getByText('Accepted triage summary wins.', { exact: true })).toBeVisible()
	await expect(timeline.getByText(/Accepted review summary wins\.[\s\S]*Accepted review feedback wins too\./)).toBeVisible()
	await expect(timeline.getByText(/superseded (job|triage|review) error/)).toHaveCount(0)

	await expect(timeline.getByText('Review check-in — paused after the configured rounds', { exact: true })).toHaveCount(1)
	await expect(timeline.getByText('Source: spec output validation · maximum 4 bounces', { exact: true })).toBeVisible()
})

test('active review claim diagnostics stay in the review panel instead of standalone history rows', async ({ page }) => {
	await page.goto('/tasks/diagnostics/full')

	await expect(page.getByText('Review claimed without terminal verdict submission')).toHaveCount(0)
	await expect(page.getByText(/review claim is active without a successful submit_review_verdict response/)).toHaveCount(0)
	const panel = page.locator('article').filter({ hasText: 'Panel of 2 · unanimous to pass' })
	await expect(panel).toHaveCount(1)
	await expect(panel.getByText('Deliberating')).toHaveCount(2)
	await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
	await expect(page.getByText(/seat 3 · diagnostics-review-0-seat-3 · review claim lease expired/)).toBeVisible()

	const timelineRows = page.getByRole('region', { name: 'Execution event timeline' }).locator('ol > li')
	await expect(timelineRows).toHaveCount(4)
	await expect(timelineRows.nth(0)).toContainText('Spec v1 approved')
	await expect(timelineRows.nth(1)).toContainText('Panel of 2 · unanimous to pass')
	await expect(timelineRows.nth(2)).toContainText('Review claim expired without verdict submission')
	await expect(timelineRows.nth(3)).toContainText('Pull request opened')

	// The task sheet uses the same historical rendering boundary.
	await page.goto('/tasks/diagnostics')
	await expect(page.getByText('Review claimed without terminal verdict submission')).toHaveCount(0)
	await expect(page.locator('article').filter({ hasText: 'Panel of 2 · unanimous to pass' })).toHaveCount(1)
	await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
})

test('human gate renders as the event timeline tail and the page opens scrolled to it', async ({ page }) => {
	await page.goto('/tasks/gate/full')

	await expect(page.getByRole('heading', { name: 'Event timeline' })).toBeVisible()
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate).toHaveCount(1)
	await expect(gate.getByText('Your review, please')).toBeVisible()

	// The gate is the timeline's last entry — the decision point where the
	// story currently ends.
	const timelineRows = page.getByRole('region', { name: 'Execution event timeline' }).locator('ol > li')
	await expect(timelineRows.last().getByRole('region', { name: 'Human gate' })).toBeVisible()

	// A reviewable task opens scrolled to the gate: the long description
	// overflows the content region, yet the gate is in view without scrolling.
	const content = page.getByRole('region', { name: 'Task content' })
	await expect.poll(() => content.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
	await expect(gate).toBeInViewport()
	await expect.poll(() => content.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)
})

test('merge gate renders pending readiness without offering merge', async ({ page }) => {
	await page.goto('/tasks/merge-unknown/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate.getByText('Checking merge readiness')).toBeVisible()
	await expect(gate.getByRole('button', { name: 'Readiness pending' })).toBeDisabled()
	await expect(gate.getByRole('button', { name: 'Merge pull request' })).toHaveCount(0)
})

test('conflicting readiness makes the idempotent fix dispatch primary', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let dispatches = 0
	await page.route('**/v1/tasks/merge-conflict/merge-conflict-fix*', async (route) => {
		dispatches++
		await route.fulfill({ json: { id: 'merge-conflict-implement-2', reason_code: 'merge-conflict' } })
	})
	await page.goto('/tasks/merge-conflict/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate.getByText('Merge blocked by conflicts')).toBeVisible()
	await expect(gate.getByRole('button', { name: 'Merge pull request' })).toHaveCount(0)
	await gate.getByRole('button', { name: 'Fix merge conflict' }).click()
	await expect.poll(() => dispatches).toBe(1)
})

test('new task events scroll only the timeline container to the newest event', async ({ page }) => {
	await page.goto('/tasks/live-scroll')

	const timeline = page.getByRole('region', { name: 'Execution event timeline' })
	const container = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
	await expect(timeline.getByText('Spec v18 drafted')).toBeVisible()
	await expect.poll(() => container.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
	await expect.poll(() => container.evaluate((element) => element.scrollTop)).toBe(0)

	emitLiveScrollEvent()

	const newest = timeline.getByText('Spec v19 drafted')
	await expect(newest).toBeVisible()
	await expect(newest).toBeInViewport()
	await expect.poll(() => container.evaluate((element) => Math.abs(element.scrollHeight - element.clientHeight - element.scrollTop))).toBeLessThanOrEqual(1)
	await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
})

test('task sheet opens scrolled to the human gate for reviewable tasks', async ({ page }) => {
	await page.goto('/tasks/gate')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate).toBeInViewport()
})

test('board activity surfaces expired-without-verdict state', async ({ page }) => {
	await page.goto('/')
	await expect(page.getByText('Verdict claim expired')).toBeVisible()
})
