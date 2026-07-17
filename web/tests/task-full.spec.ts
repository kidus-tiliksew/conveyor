import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-15T12:00:00Z'

function activity(taskId: string, overflowing: boolean) {
	const reviewActivity = taskId === 'reviews' ? {
		jobs: [
			{ id: 'reviews-review-1-seat-1', task_id: taskId, stage: 'review', harness: 'codex', model_tier: 'gpt-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: createdAt, ended_at: '2026-07-15T12:01:00Z' },
			{ id: 'reviews-review-1-seat-2', task_id: taskId, stage: 'review', harness: 'claude', model_tier: 'claude-review', auth_mode: 'byoa', runner: 'worker', confinement: 'none', cost_usd: 0, tokens_in: 0, tokens_out: 0, state: 'done', started_at: '2026-07-15T12:02:00Z', ended_at: '2026-07-15T12:03:00Z' },
		],
		events: [
			{ id: 1, task_id: taskId, job_id: 'reviews-review-1-seat-1', kind: 'review.completed', actor_id: 'worker-1', actor_role: 'runner', payload: { verdict: 'approve', summary: 'Seat one approved', feedback: 'Approved guidance remains visible.', review_seat: 1, reviewer_model: 'gpt-review', required_effort: 'high', model_enforcement: 'worker-pinned' }, at: '2026-07-15T12:01:00Z' },
			{ id: 2, task_id: taskId, job_id: 'reviews-review-1-seat-2', kind: 'review.completed', actor_id: 'worker-2', actor_role: 'runner', payload: { verdict: 'changes_requested', summary: 'Seat two requested changes', feedback: 'Changes guidance remains visible.', review_seat: 2, reviewer_model: 'claude-review', required_effort: '', model_enforcement: 'worker-pinned' }, at: '2026-07-15T12:03:00Z' },
		],
		work_orders: [
			{ id: 'reviews-review-1-seat-1', task_id: taskId, job_id: 'reviews-review-1-seat-1', stage: 'review', state: 'completed', review_round: 1, review_seat: 1, required_model: 'gpt-review', required_harness: 'codex', required_effort: 'high', model_enforcement: 'worker-pinned', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true, created_at: createdAt, updated_at: '2026-07-15T12:01:00Z' },
			{ id: 'reviews-review-1-seat-2', task_id: taskId, job_id: 'reviews-review-1-seat-2', stage: 'review', state: 'completed', review_round: 1, review_seat: 2, required_model: 'claude-review', required_harness: 'claude', model_enforcement: 'worker-pinned', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true, created_at: createdAt, updated_at: '2026-07-15T12:03:00Z' },
		],
	} : taskId === 'recovery' ? {
		jobs: [{ id: 'recovery-review-1-seat-1', task_id: taskId, stage: 'review', state: 'pending', cost_usd: 0, tokens_in: 0, tokens_out: 0 }],
		events: [],
		work_orders: [{ id: 'recovery-review-1-seat-1', task_id: taskId, job_id: 'recovery-review-1-seat-1', stage: 'review', state: 'queued', claimable: false, last_attempt_outcome: 'child_failure', last_failure_message: 'harness exited: status 1', last_failure_exit_status: 1, last_failure_at: createdAt, automatic_retry_count: 3, retry_suppressed: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : { jobs: [], events: [], work_orders: taskId === 'no-orders' ? null : [] }
	const reviewDiagnostics = taskId === 'diagnostics' ? [
		{ status: 'claimed_without_verdict', work_order_id: 'diagnostics-review-1-seat-1', review_round: 1, review_seat: 1, claimed_at: '2026-07-15T12:00:00Z', lease_expires_at: '2026-07-15T12:15:00Z', reason: 'review claim is active without a successful submit_review_verdict response' },
		{ status: 'expired_without_verdict', work_order_id: 'diagnostics-review-1-seat-2', review_round: 1, review_seat: 2, claimed_at: '2026-07-15T11:30:00Z', lease_expires_at: '2026-07-15T11:45:00Z', reason: 'review claim lease expired without terminal verdict submission' },
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
      state: 'running',
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
      content: '## Specification\n\nRegression marker at the bottom of the task content.',
      acceptance_count: 0,
      acceptance: [],
      decomposition: [],
      approved: true,
      created_at: createdAt,
      approved_at: createdAt,
    },
		work_orders: reviewActivity.work_orders,
		review_diagnostics: reviewDiagnostics,
  }
}

async function mockTaskAPIs(page: Page) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    const taskMatch = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (taskMatch) {
      const taskId = decodeURIComponent(taskMatch[1])
      await route.fulfill({ json: activity(taskId, taskId === 'overflowing') })
      return
    }
		if (url.pathname === '/v1/activity') {
			const item = activity('diagnostics', false)
			await route.fulfill({ json: [{ task: item.task, latest_stage: 'review', last_event_at: createdAt, needs_attention: false, review_diagnostics: item.review_diagnostics }] })
			return
		}
    if (url.pathname.endsWith('/events/stream')) {
      await route.fulfill({ status: 204 })
      return
    }
    await route.fulfill({ json: [] })
  })
}

test.beforeEach(async ({ page }) => {
  await mockTaskAPIs(page)
})

test('new task detail tolerates a null work-order list from the API', async ({ page }) => {
	await page.goto('/tasks/no-orders/full')
	await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
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

test('review cards and activity notes retain approve and changes feedback', async ({ page }) => {
	await page.goto('/tasks/reviews/full')

	const approvedCard = page.locator('article').filter({ hasText: 'Seat one approved' })
	await expect(approvedCard.getByText('Reviewer feedback: Approved guidance remains visible.')).toBeVisible()
	await expect(approvedCard.getByText('Seat 1')).toBeVisible()
	await expect(approvedCard.getByText('Effort high')).toBeVisible()
	await expect(approvedCard.getByText('worker-pinned')).toBeVisible()

	const changesCard = page.locator('article').filter({ hasText: 'Seat two requested changes' })
	await expect(changesCard.getByText('Reviewer feedback: Changes guidance remains visible.')).toBeVisible()
	await expect(changesCard.getByText('Seat 2')).toBeVisible()
	await expect(changesCard.getByText('worker-pinned')).toBeVisible()

	await expect(page.locator('span.text-xs.text-muted').filter({ hasText: 'feedback: Approved guidance remains visible.' })).toBeVisible()
	await expect(page.locator('span.text-xs.text-muted').filter({ hasText: 'effort high' })).toBeVisible()
	const legacyReviewActivity = page.locator('span.text-xs.text-muted').filter({ hasText: 'feedback: Changes guidance remains visible.' })
	await expect(legacyReviewActivity).toBeVisible()
	await expect(legacyReviewActivity).not.toContainText('effort')
})

test('review verdict diagnostics distinguish active and expired missing submissions', async ({ page }) => {
	await page.goto('/tasks/diagnostics/full')

	await expect(page.getByText('Review claimed without terminal verdict submission')).toBeVisible()
	await expect(page.getByText(/seat 1 · diagnostics-review-1-seat-1 · review claim is active/)).toBeVisible()
	await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
	await expect(page.getByText(/seat 2 · diagnostics-review-1-seat-2 · review claim lease expired/)).toBeVisible()
})

test('board activity surfaces expired-without-verdict state', async ({ page }) => {
	await page.goto('/')
	await expect(page.getByText('Verdict claim expired')).toBeVisible()
})
