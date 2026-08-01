import { expect, test, type Page, type Route } from '@playwright/test'

const createdAt = '2026-07-15T12:00:00Z'
let emitLiveScrollEvent = () => {}
const detailRequestCounts = new Map<string, number>()

function activity(taskId: string, overflowing: boolean, liveEventCount = 18) {
	const specContent = taskId === 'mermaid-valid'
		? '## Specification\n\n```mermaid\ngraph TD\n  A --> B\n```'
		: taskId === 'mermaid-invalid'
			? '## Specification\n\n```mermaid\nthis is deliberately malformed\n```'
		: taskId === 'overflowing' || taskId === 'gate'
			? ['## Specification', '', ...Array.from({ length: 60 }, (_, index) => `Scrollable specification paragraph ${index + 1}.`), '', 'Regression marker at the bottom of the task content.'].join('\n\n')
		: taskId === 'long-spec'
		? ['## Specification', '', ...Array.from({ length: 60 }, (_, index) => `Long specification paragraph ${index + 1}.`), '', 'Long spec ending marker.'].join('\n\n')
		: '## Specification\n\nRegression marker at the bottom of the task content.'
	const reviewActivity = taskId === 'attempt-recovery' ? {
		jobs: [],
		events: [
			{ id: 1, task_id: taskId, job_id: 'attempt-recovery-implement-1', kind: 'work_order.claimed', actor_id: 'worker-1', actor_role: 'runner' as const, payload: { id: 'attempt-recovery-implement-1', attempt_id: 'attempt-1', stage: 'implement', session_id: 'worker-1' }, at: '2026-07-15T12:00:00Z' },
			{ id: 2, task_id: taskId, job_id: 'attempt-recovery-implement-1', kind: 'work_order.lease_renewed', actor_id: 'worker-1', actor_role: 'runner' as const, payload: { attempt_id: 'attempt-1', lease_expires_at: '2026-07-15T12:06:00Z' }, at: '2026-07-15T12:01:00Z' },
			{ id: 3, task_id: taskId, job_id: 'attempt-recovery-implement-1', kind: 'work_order.child_failed', actor_id: 'worker-1', actor_role: 'runner' as const, payload: { attempt_id: 'attempt-1', reason: 'harness exited before completing work order', detail: 'You have reached the provider usage limit. Try again later.', failure_category: 'provider_usage_limit', retry_suppressed: false, next_retry_at: '2026-07-15T12:03:00Z' }, at: '2026-07-15T12:02:00Z' },
			{ id: 4, task_id: taskId, job_id: 'attempt-recovery-implement-1', kind: 'work_order.claimed', actor_id: 'worker-2', actor_role: 'runner' as const, payload: { id: 'attempt-recovery-implement-1', attempt_id: 'attempt-2', stage: 'implement', session_id: 'worker-2' }, at: '2026-07-15T12:03:00Z' },
			{ id: 5, task_id: taskId, job_id: 'attempt-recovery-implement-1', kind: 'work_order.released', actor_id: 'worker-2', actor_role: 'runner' as const, payload: { attempt_id: 'attempt-2', reason: 'checkout_blocked_dirty_primary: primary checkout has 77 pre-existing generated-dashboard changes', outcome: 'released', retry_suppressed: true }, at: '2026-07-15T12:04:00Z' },
		],
		work_orders: [{ id: 'attempt-recovery-implement-1', task_id: taskId, job_id: 'attempt-recovery-implement-1', stage: 'implement', state: 'queued', claimable: false, last_attempt_id: 'attempt-2', last_attempt_outcome: 'released', last_failure_message: 'checkout_blocked_dirty_primary: primary checkout has 77 pre-existing generated-dashboard changes', last_failure_at: '2026-07-15T12:04:00Z', automatic_retry_count: 1, retry_suppressed: true, queue_entered_at: '2026-07-15T12:04:00Z', queue_deadline: '2026-07-16T12:04:00Z', updated_at: '2026-07-15T12:04:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'usage-retry-pending' ? {
		jobs: [],
		events: [{ id: 1, task_id: taskId, job_id: 'usage-retry-pending-implement-1', kind: 'work_order.child_failed', actor_id: 'worker', actor_role: 'runner' as const, payload: { attempt_id: 'usage-attempt-1', reason: 'harness exited before completing work order', detail: 'usage limit reached', failure_category: 'provider_usage_limit', retry_suppressed: false, next_retry_at: '2099-07-15T12:05:00Z' }, at: createdAt }],
		work_orders: [{ id: 'usage-retry-pending-implement-1', task_id: taskId, job_id: 'usage-retry-pending-implement-1', stage: 'implement', state: 'queued', claimable: false, last_attempt_id: 'usage-attempt-1', last_attempt_outcome: 'child_failure', last_failure_category: 'provider_usage_limit', last_failure_message: 'harness exited before completing work order', last_failure_detail: 'usage limit reached', last_failure_at: createdAt, automatic_retry_count: 1, next_retry_at: '2099-07-15T12:05:00Z', retry_suppressed: false, queue_entered_at: createdAt, queue_deadline: '2099-07-16T12:00:00Z', updated_at: createdAt, redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'usage-suppressed' ? {
		jobs: [],
		events: [{ id: 1, task_id: taskId, job_id: 'usage-suppressed-implement-1', kind: 'work_order.child_failed', actor_id: 'worker', actor_role: 'runner' as const, payload: { attempt_id: 'usage-attempt-2', reason: 'harness exited before completing work order', detail: 'usage limit reached', failure_category: 'provider_usage_limit', retry_suppressed: true }, at: createdAt }],
		work_orders: [{ id: 'usage-suppressed-implement-1', task_id: taskId, job_id: 'usage-suppressed-implement-1', stage: 'implement', state: 'queued', claimable: false, last_attempt_id: 'usage-attempt-2', last_attempt_outcome: 'child_failure', last_failure_category: 'provider_usage_limit', last_failure_message: 'harness exited before completing work order', last_failure_detail: 'usage limit reached', last_failure_at: createdAt, automatic_retry_count: 3, retry_suppressed: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', updated_at: createdAt, redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'stage-aware' ? {
		jobs: [],
		events: [{ id: 1, task_id: taskId, job_id: 'stage-aware-spec', kind: 'work_order.child_failed', actor_id: 'worker', actor_role: 'runner' as const, payload: { reason: 'harness exited: status 1', detail: 'provider rejected the configured model', retry_suppressed: true, suppression_reason: 'identical failure output on consecutive attempts' }, at: createdAt }],
		work_orders: [
			{ id: 'stage-aware-spec', task_id: taskId, job_id: 'stage-aware-spec', stage: 'spec', state: 'queued', claimable: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'stage-aware-implement', task_id: taskId, job_id: 'stage-aware-implement', stage: 'implement', state: 'claimed', claimable: false, queue_entered_at: '2026-07-15T12:01:00Z', queue_deadline: '2026-07-16T12:01:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'stage-aware-review', task_id: taskId, job_id: 'stage-aware-review', stage: 'review', state: 'timed_out', claimable: false, queue_entered_at: '2026-07-15T12:02:00Z', queue_deadline: '2026-07-16T12:02:00Z', execution_deadline: '2026-07-15T12:03:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : taskId === 'blocked-suppressed' ? {
		jobs: [],
		events: [{ id: 1, task_id: taskId, job_id: 'blocked-suppressed-implement-1', kind: 'work_order.child_failed', actor_id: 'worker', actor_role: 'runner' as const, payload: { attempt_id: 'blocked-attempt-1', reason: 'harness exited before completing work order', detail: 'provider rejected the configured model', retry_suppressed: true }, at: createdAt }],
		work_orders: [{
			id: 'blocked-suppressed-implement-1', task_id: taskId, job_id: 'blocked-suppressed-implement-1', stage: 'implement' as const, state: 'queued' as const, claimable: false,
			blocking_task_ids: ['blocked-dependency'], last_attempt_id: 'blocked-attempt-1', last_attempt_outcome: 'child_failure' as const,
			last_failure_message: 'harness exited before completing work order', last_failure_detail: 'provider rejected the configured model', last_failure_at: createdAt,
			automatic_retry_count: 3, retry_suppressed: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', updated_at: createdAt,
			redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true,
		}],
	} : taskId === 'blueprint-child' || taskId === 'blocked-refresh' || taskId === 'spec-while-blocked' ? {
		jobs: [],
		events: [],
		work_orders: [
			...(taskId === 'spec-while-blocked' ? [{
				id: 'spec-while-blocked-spec-1', task_id: taskId, job_id: 'spec-while-blocked-spec-1', stage: 'spec' as const, state: 'queued' as const, claimable: true,
				queue_entered_at: '2026-07-15T11:59:00Z', queue_deadline: '2026-07-16T11:59:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true,
			}] : []),
			{
				id: `${taskId}-implement-1`, task_id: taskId, job_id: `${taskId}-implement-1`, stage: 'implement' as const, state: 'queued' as const, claimable: false,
				blocking_task_ids: [taskId === 'blueprint-child' ? 'blueprint-sub-2' : 'refresh-dependency'],
				queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true,
			},
		],
	} : taskId === 'live-scroll' || taskId === 'gate' ? {
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
	} : taskId === 'checkout-blocked-recovery' ? {
		jobs: [{ id: 'checkout-blocked-implement-1', task_id: taskId, stage: 'implement', state: 'failed', cost_usd: 0, tokens_in: 0, tokens_out: 0 }],
		events: [],
		work_orders: [{ id: 'checkout-blocked-implement-1', task_id: taskId, job_id: 'checkout-blocked-implement-1', stage: 'implement', state: 'queued', claimable: false, last_attempt_outcome: 'released', last_failure_message: 'checkout_blocked_dirty_primary: shared primary checkout has pre-existing modifications in CLAUDE.md and conveyor-spec.md; operator changes preserved', automatic_retry_count: 0, retry_suppressed: true, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
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
	} : taskId === 'human-gate-worker-scope' ? {
		jobs: [{ id: 'human-gate-worker-scope-review-1-seat-1', task_id: taskId, stage: 'review', state: 'done', cost_usd: 0, tokens_in: 0, tokens_out: 0, started_at: createdAt, ended_at: '2026-07-15T12:01:00Z' }],
		events: [
			{ id: 1, task_id: taskId, job_id: 'human-gate-worker-scope-review-1-seat-1', kind: 'review.completed', actor_id: 'worker-1', actor_role: 'runner', payload: { verdict: 'approve', summary: 'Review approved', feedback: 'Ready for human review.', review_round: 1, review_seat: 1 }, at: '2026-07-15T12:01:00Z' },
			{ id: 2, task_id: taskId, job_id: 'human-gate-worker-scope-review-1-seat-1', kind: 'review.publication_retry', actor_id: 'system', actor_role: 'system', payload: { review_work_order_id: 'human-gate-worker-scope-review-1-seat-1', last_error: 'GitHub unavailable' }, at: '2026-07-15T12:02:00Z' },
		],
		work_orders: [{ id: 'human-gate-worker-scope-review-1-seat-1', task_id: taskId, job_id: 'human-gate-worker-scope-review-1-seat-1', stage: 'review', state: 'completed', review_round: 1, review_seat: 1, required_harness: 'codex', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
	} : taskId === 'queued-worker' ? {
		jobs: [],
		events: [],
		work_orders: [{ id: 'queued-worker-implement-1', task_id: taskId, job_id: 'queued-worker-implement-1', stage: 'implement', state: 'queued', claimable: true, required_harness: 'codex', queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true }],
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
	} : taskId === 'spec-approval' ? {
		jobs: [],
		events: [
			{ id: 1, task_id: taskId, kind: 'spec.version_created', actor_id: 'pipeline', actor_role: 'system' as const, payload: { version: 2 }, at: '2026-07-15T11:58:00Z' },
			{ id: 2, task_id: taskId, kind: 'spec.version_approved', actor_id: 'operator', actor_role: 'human' as const, payload: { version: 2 }, at: '2026-07-15T11:59:00Z' },
		],
		work_orders: [],
	} : taskId === 'setup-submitted' ? {
		jobs: [],
		events: [],
		// A delivered implement attempt awaiting review: does not block a setup
		// change; only claimed attempts and in-flight verdicts do (spec §21.36).
		work_orders: [
			{ id: 'setup-submitted-implement-1', task_id: taskId, job_id: 'setup-submitted-implement-1', stage: 'implement', state: 'submitted', claimable: false, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
			{ id: 'setup-submitted-review-1-seat-1', task_id: taskId, job_id: 'setup-submitted-review-1-seat-1', stage: 'review', state: 'queued', claimable: true, review_round: 1, review_seat: 1, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : taskId === 'setup-claimed' ? {
		jobs: [],
		events: [],
		work_orders: [
			{ id: 'setup-claimed-implement-1', task_id: taskId, job_id: 'setup-claimed-implement-1', stage: 'implement', state: 'claimed', claimable: false, queue_entered_at: createdAt, queue_deadline: '2026-07-16T12:00:00Z', redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: true },
		],
	} : taskId === 'forge-failure' ? {
		jobs: [],
		events: [
			{ id: 1, task_id: taskId, kind: 'github_issue.publication_failed', actor_id: 'system', actor_role: 'system' as const, payload: { forge_error_category: 'forge_request', last_error: 'create task issue: request timed out' }, at: '2026-07-15T12:00:00Z' },
			{ id: 2, task_id: taskId, job_id: 'review-1', kind: 'review.publication_failed', actor_id: 'system', actor_role: 'system' as const, payload: { review_work_order_id: 'review-1', forge_error_category: 'forge_permission', last_error: 'publish review comment: resource not accessible' }, at: '2026-07-15T12:01:00Z' },
			{ id: 3, task_id: taskId, kind: 'merge.failed', actor_id: 'system', actor_role: 'system' as const, payload: { reason_code: 'forge_merge_failed', forge_error_category: 'forge_rate_limited', error: 'merge pull request: API rate limit exceeded' }, at: '2026-07-15T12:02:00Z' },
		],
		work_orders: [],
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
      title: taskId === 'blueprint-parent' ? 'Phase 6 blueprint' : overflowing ? 'Overflowing task' : 'Short task',
      body: taskId === 'markdown-body'
        ? '# Structured context\n\nA **clear** paragraph with [safe link](https://example.test).\n\n- first item\n- [x] completed item\n\n`inline code`\n\n<script data-unsafe>window.hacked = true</script>'
        : taskId === 'long-body'
          ? Array.from({ length: 30 }, (_, index) => `Description paragraph ${index + 1}.`).join('\n\n')
          : overflowing ? Array.from({ length: 80 }, (_, index) => `Description line ${index + 1}`).join('\n') : 'A short description.',
      class: 'bug',
      level: 'L2',
      repo: 'conveyor',
      base_branch: 'main',
      branch: `conveyor/task-${taskId}`,
		state: taskId === 'gate' || taskId === 'evidence' || taskId === 'human-gate-worker-scope' ? 'awaiting_human' : taskId === 'parked' ? 'parked' : taskId === 'blueprint-parent' || taskId === 'blueprint-child' || taskId === 'blocked-refresh' || taskId === 'blocked-suppressed' || taskId === 'spec-while-blocked' || taskId === 'unsatisfiable' ? 'queued' : taskId.startsWith('merge-') ? 'approved' : 'running',
      next_stage: taskId === 'parked' ? '' : 'implement',
      recovery_stage: taskId === 'parked' ? 'triage' : '',
      setup: taskId.startsWith('setup-') ? 'old' : '',
      setup_contract: taskId.startsWith('setup-') ? {
        name: 'old',
        execution_settings: {
          control_plane: { triage: { model: 'control', timeout: '20m' } },
          spec: { harness: 'codex', model: 'gpt-spec', model_policy: 'explicit', timeout: '30m' },
          implementation: { harness: 'codex', model: 'gpt-old', model_policy: 'explicit', effort: 'medium', timeout: '2h' },
          review: { execution: 'mcp', timeout: '45m', fallback_harness: 'codex' },
        },
        review: { seats: [{ harness: 'codex', model: 'gpt-review', effort: 'medium' }] },
        refresh_review: 'delta',
      } : undefined,
      parent_task_id: taskId === 'blueprint-child' ? 'blueprint-parent' : undefined,
      origin_spec_version: taskId === 'blueprint-child' ? 1 : undefined,
      origin_sub_id: taskId === 'blueprint-child' ? 'SUB-3' : undefined,
      blocking_task_ids: taskId === 'blueprint-child' ? ['blueprint-sub-2']
        : taskId === 'blocked-refresh' ? ['refresh-dependency']
		: taskId === 'blocked-suppressed' ? ['blocked-dependency']
		  : taskId === 'spec-while-blocked' ? ['refresh-dependency']
          : taskId === 'unsatisfiable' ? ['closed-dependency']
            : undefined,
      dependencies: taskId === 'blueprint-child' ? [
        { id: 'blueprint-sub-2', title: 'Runtime', state: 'running' },
        { id: 'blueprint-sub-1', title: 'Persistence', state: 'merged' },
      ] : taskId === 'blocked-refresh' ? [{ id: 'refresh-dependency', title: 'Backend contract', state: 'running' }]
		: taskId === 'blocked-suppressed' ? [{ id: 'blocked-dependency', title: 'Schema migration', state: 'running' }]
		: taskId === 'spec-while-blocked' ? [{ id: 'refresh-dependency', title: 'Backend contract', state: 'running' }]
        : taskId === 'unsatisfiable' ? [{ id: 'closed-dependency', title: 'Retired API plan', state: 'closed' }]
          : undefined,
      children: taskId === 'blueprint-parent' ? [
        { id: 'blueprint-sub-1', title: 'Persistence', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-1' },
        { id: 'blueprint-sub-2', title: 'Runtime', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-2' },
        { id: 'blueprint-child', title: 'Dashboard', state: 'closed', origin_spec_version: 1, origin_sub_id: 'SUB-3' },
      ] : undefined,
      created_at: createdAt,
    },
		jobs: reviewActivity.jobs,
		events: taskId === 'blueprint-parent' ? [
      { id: 1, task_id: taskId, kind: 'blueprint.materialized', actor_id: 'system', actor_role: 'system' as const, payload: { version: 1, children_total: 3 }, at: createdAt },
      { id: 2, task_id: taskId, kind: 'blueprint.closed', actor_id: 'system', actor_role: 'system' as const, payload: {}, at: createdAt },
    ] : taskId === 'unsatisfiable' ? [
      { id: 1, task_id: taskId, kind: 'task.dependency_unsatisfiable', actor_id: 'system', actor_role: 'system' as const, payload: { depends_on_task_id: 'closed-dependency', dependency_state: 'closed' }, at: createdAt },
    ] : reviewActivity.events,
    interventions: taskId === 'spec-approval' ? [{
		id: 1,
		task_id: taskId,
		actor_id: 'operator',
		actor_role: 'human' as const,
		action: 'approve' as const,
		reason_code: 'approved',
		comment: '',
		at: '2026-07-15T11:59:00Z',
	}] : [],
    checkout_available: false,
    checkout_guidance: 'Use the assigned worktree.',
    needs_attention: taskId === 'forge-failure' || taskId === 'unsatisfiable',
    forge_failure: taskId === 'forge-failure' ? {
      category: 'forge_rate_limited',
      detail: 'merge pull request: API rate limit exceeded',
      surface: 'GitHub merge',
      at: '2026-07-15T12:02:00Z',
    } : undefined,
    spec: {
      task_id: taskId,
      version: 1,
      content: specContent,
      acceptance_count: 0,
      acceptance: [],
      decomposition: taskId === 'blueprint-parent' ? [
        { id: 'SUB-1', repo: 'conveyor', summary: 'Persistence', depends_on: [] },
        { id: 'SUB-2', repo: 'conveyor', summary: 'Runtime', depends_on: ['SUB-1'] },
        { id: 'SUB-3', repo: 'conveyor', summary: 'Dashboard', depends_on: ['SUB-2'] },
      ] : [],
      materialized_children: taskId === 'blueprint-parent' ? [
        { id: 'blueprint-sub-1', title: 'Persistence', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-1' },
        { id: 'blueprint-sub-2', title: 'Runtime', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-2' },
        { id: 'blueprint-child', title: 'Dashboard', state: 'closed', origin_spec_version: 1, origin_sub_id: 'SUB-3' },
      ] : undefined,
      approved: true,
      created_at: createdAt,
      approved_at: createdAt,
    },
		work_orders: taskId === 'unsatisfiable' ? [{
      id: 'unsatisfiable-implement-1',
      task_id: taskId,
      job_id: 'unsatisfiable-implement-1',
      stage: 'implement',
      state: 'queued',
      claimable: false,
      blocking_task_ids: ['closed-dependency'],
      unsatisfiable_task_ids: ['closed-dependency'],
      queue_entered_at: createdAt,
      queue_deadline: '2026-07-16T12:00:00Z',
      redispatch_count: 0,
      cost_usd: 0,
      tokens_in: 0,
      tokens_out: 0,
      self_reported: true,
    }] : reviewActivity.work_orders,
		verification_evidence: taskId === 'evidence' ? [
			{
				id: 'evidence-image',
				workspace: 'demo',
				name: 'proof screenshot.png',
				content_type: 'image/png',
				size_bytes: 68,
				role: 'verification_evidence' as const,
				task_id: taskId,
				download_url: '/v1/artifacts/evidence-image',
				created_at: createdAt,
			},
			{
				id: 'evidence-video',
				workspace: 'demo',
				name: 'proof recording.webm',
				content_type: 'video/webm',
				size_bytes: 128,
				role: 'verification_evidence' as const,
				task_id: taskId,
				download_url: '/v1/artifacts/evidence-video',
				created_at: createdAt,
			},
		] : [],
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
		} : taskId === 'interrupted-review-empty' ? {
			needed: true,
			review_round: 1,
			reason: 'latest review round has interrupted seats whose claims are no longer authorized',
			eligible_orders: [{ id: 'interrupted-review-empty-review-1-seat-1', review_seat: 1, last_attempt_outcome: 'expired' }],
			retained_orders: null,
		} : undefined,
		stalled: taskId === 'recovery' ? {
			needed: true,
			reason: 'automatic retry is suppressed',
			work_order: (reviewActivity.work_orders ?? [])[0],
			last_failure: 'harness exited: status 1',
		} : taskId === 'unsatisfiable' ? {
      needed: true,
      reason: 'dependency reached a terminal state without merging',
      work_order: { id: 'unsatisfiable-implement-1', stage: 'implement', state: 'queued' },
      blocking_task_ids: ['closed-dependency'],
      unsatisfiable_edge: true,
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
		} : taskId === 'queued-worker' ? {
			available: false,
			required_harnesses: ['codex'],
			reason: 'no healthy worker can serve the task\'s required harnesses',
			queue_context: 'never_started',
		} : undefined,
		merge_readiness: taskId === 'merge-unknown' ? { state: 'UNKNOWN', head_sha: 'head-1' }
			: taskId === 'merge-conflict' ? { state: 'CONFLICTING', head_sha: 'head-1', number: 12 }
			: taskId === 'merge-delayed' || taskId === 'merge-failure' ? { state: 'MERGEABLE', head_sha: 'head-1', number: 12 }
			: undefined,
  }
}

// The blueprint projection (spec §21.49): anchors left the activity feed, so
// this is where the anchor's delivery, ordered children, and title now come
// from — including the title a child's parent reference renders.
function blueprintProjection() {
  const parent = activity('blueprint-parent', false)
  return [{
    task: parent.task,
    spec: parent.spec,
    governing_version: 1,
    children: [
      { id: 'blueprint-sub-1', title: 'Persistence', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-1', repo: 'conveyor', summary: 'Persistence', depends_on: [] },
      { id: 'blueprint-sub-2', title: 'Runtime', state: 'merged', origin_spec_version: 1, origin_sub_id: 'SUB-2', repo: 'conveyor', summary: 'Runtime', depends_on: ['SUB-1'] },
      { id: 'blueprint-child', title: 'Dashboard', state: 'closed', origin_spec_version: 1, origin_sub_id: 'SUB-3', repo: 'conveyor', summary: 'Dashboard', depends_on: ['SUB-2'] },
    ],
    delivery: { state: 'completed', total: 3, merged: 2, closed: 1, open: 0 },
    serves: [],
    events: parent.events,
    artifacts: [],
  }]
}

async function mockTaskAPIs(page: Page) {
  detailRequestCounts.clear()
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
      const requestCount = (detailRequestCounts.get(taskId) ?? 0) + 1
      detailRequestCounts.set(taskId, requestCount)
      if (taskId === 'live-scroll') liveActivityRequests++
      const item = activity(taskId, taskId === 'overflowing' || taskId === 'gate', liveActivityRequests > 1 ? 19 : 18)
      if (taskId === 'blocked-refresh' && requestCount > 1) {
        item.task.blocking_task_ids = undefined
        item.task.dependencies = [{ id: 'refresh-dependency', title: 'Backend contract', state: 'merged' }]
		item.work_orders = item.work_orders?.map((order) => ({ ...order, blocking_task_ids: undefined })) ?? item.work_orders
      }
	    await route.fulfill({ json: item })
      return
    }
    if (url.pathname === '/v1/blueprints') {
      await route.fulfill({ json: blueprintProjection() })
      return
    }
		if (url.pathname === '/v1/activity') {
			const item = activity('diagnostics', false)
      const parent = activity('blueprint-parent', false)
			await route.fulfill({ json: [
        { task: item.task, latest_stage: 'review', last_event_at: createdAt, needs_attention: false, review_diagnostics: item.review_diagnostics },
        { task: parent.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: false },
      ] })
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

test('task sheet adds bottom clearance without changing full-page task spacing', async ({ page }) => {
	await page.goto('/tasks/sheet-padding')
	const sheetContent = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
	await expect(sheetContent).toHaveCSS('padding-bottom', '32px')

	await page.goto('/tasks/full-padding/full')
	await expect(page.getByRole('region', { name: 'Task content' })).toHaveCSS('padding-bottom', '0px')
})

test('new task detail tolerates a null work-order list from the API', async ({ page }) => {
	await page.goto('/tasks/no-orders/full')
	await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
	await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('task detail previews and submits a named future-only setup change', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	const nextSetup = {
		name: 'next',
		execution_settings: {
			control_plane: { triage: { model: 'control', timeout: '20m' } },
			spec: { harness: 'claude', model: 'claude-spec', model_policy: 'explicit', timeout: '30m' },
			implementation: { harness: 'claude', model: 'claude-next', model_policy: 'explicit', effort: 'high', timeout: '3h' },
			review: { execution: 'mcp', timeout: '1h', fallback_harness: 'claude' },
		},
		review: { seats: [{ harness: 'claude', model: 'claude-review', effort: 'high' }] },
		refresh_review: 'delta',
	}
	await page.route('**/v1/workspace/config*', (route) => route.fulfill({ json: { version: 1, document: { workspace: 'demo', routing: { stages: { review: {} } }, review: { seats: [] }, harnesses: [], repos: [], setups: [activity('setup-change', false).task.setup_contract, nextSetup], default_setup: 'old', execution: {} } } }))
	let submitted: Record<string, unknown> | undefined
	await page.route('**/v1/tasks/setup-change/setup*', async (route) => {
		submitted = route.request().postDataJSON()
		await route.fulfill({ json: { task: { ...activity('setup-change', false).task, setup: 'next', setup_contract: nextSetup }, review_transition: 'same_round_reconciled' } })
	})
	await page.goto('/tasks/setup-change/full')
	await expect(page.getByText('affects future work only')).toBeVisible()
	// Both route variants keep the control collapsed: the page opens on the
	// work, not on configuration.
	await page.locator('summary', { hasText: 'Change execution setup' }).click()
	await page.getByLabel('Named execution setup').selectOption('next')
	await expect(page.getByText(/After: implement claude \/ explicit \/ high \/ 3h/)).toBeVisible()
	await page.getByLabel('Setup change reason').fill('repair routing')
	await page.getByRole('button', { name: 'Change setup' }).click()
	await expect(page.getByText('Setup changed: same round reconciled.')).toBeVisible()
	expect(submitted?.setup).toBe('next')
	expect(submitted?.reason).toBe('repair routing')
	expect(String(submitted?.request_id)).not.toBe('')
})

test('task detail submits change and apply-latest setup actions without a reason', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	const nextSetup = {
		name: 'next',
		execution_settings: {
			control_plane: { triage: { model: 'control', timeout: '20m' } },
			spec: { harness: 'claude', model: 'claude-spec', model_policy: 'explicit', timeout: '30m' },
			implementation: { harness: 'claude', model: 'claude-next', model_policy: 'explicit', effort: 'high', timeout: '3h' },
			review: { execution: 'mcp', timeout: '1h', fallback_harness: 'claude' },
		},
		review: { seats: [{ harness: 'claude', model: 'claude-review', effort: 'high' }] },
		refresh_review: 'delta',
	}
	await page.route('**/v1/workspace/config*', (route) => route.fulfill({ json: { version: 1, document: { workspace: 'demo', routing: { stages: { review: {} } }, review: { seats: [] }, harnesses: [], repos: [], setups: [activity('setup-change', false).task.setup_contract, nextSetup], default_setup: 'old', execution: {} } } }))
	const submitted: Record<string, unknown>[] = []
	await page.route('**/v1/tasks/setup-change/setup*', async (route) => {
		submitted.push(route.request().postDataJSON())
		await route.fulfill({ json: { task: activity('setup-change', false).task, review_transition: 'none' } })
	})
	await page.goto('/tasks/setup-change/full')
	await page.locator('summary', { hasText: 'Change execution setup' }).click()
	const reason = page.getByLabel('Setup change reason')
	await expect(reason).toHaveAttribute('placeholder', 'Reason (optional)')

	const applyLatest = page.getByRole('button', { name: 'Apply latest setup' })
	await expect(applyLatest).toBeEnabled()
	await applyLatest.click()
	await expect.poll(() => submitted.length).toBe(1)
	expect(submitted[0]?.apply_latest).toBe(true)
	expect(submitted[0]?.reason).toBe('')

	await page.getByLabel('Named execution setup').selectOption('next')
	const changeSetup = page.getByRole('button', { name: 'Change setup' })
	await expect(changeSetup).toBeEnabled()
	await changeSetup.click()
	await expect.poll(() => submitted.length).toBe(2)
	expect(submitted[1]?.setup).toBe('next')
	expect(submitted[1]?.reason).toBe('')
})

test('task detail exposes the setup change control behind an expandable section', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	await page.route('**/v1/workspace/config*', (route) => route.fulfill({ json: { version: 1, document: { workspace: 'demo', routing: { stages: { review: {} } }, review: { seats: [] }, harnesses: [], repos: [], setups: [activity('setup-submitted', false).task.setup_contract], default_setup: 'old', execution: {} } } }))
	await page.goto('/tasks/setup-submitted')
	const summary = page.locator('summary', { hasText: 'Change execution setup' })
	await expect(summary).toBeVisible()
	await expect(page.getByLabel('Named execution setup')).not.toBeVisible()
	await summary.click()
	await expect(page.getByLabel('Named execution setup')).toBeVisible()
	// The implement attempt is submitted (delivered), not claimed: it must not
	// disable the control (spec §21.36).
	await expect(page.getByLabel('Named execution setup')).toBeEnabled()
	await expect(page.getByLabel('Setup change reason')).toBeEnabled()
})

test('a claimed attempt disables the setup change control with the specific blocker', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	await page.route('**/v1/workspace/config*', (route) => route.fulfill({ json: { version: 1, document: { workspace: 'demo', routing: { stages: { review: {} } }, review: { seats: [] }, harnesses: [], repos: [], setups: [activity('setup-claimed', false).task.setup_contract], default_setup: 'old', execution: {} } } }))
	await page.goto('/tasks/setup-claimed/full')
	await page.locator('summary', { hasText: 'Change execution setup' }).click()
	await expect(page.getByText('An attempt is claimed and executing.')).toBeVisible()
	await expect(page.getByLabel('Named execution setup')).toBeDisabled()
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
	await expect(page.getByText('No automatic retry is pending.')).toBeVisible()
	await expect(page.getByText('Resolve the primary checkout changes first.')).toHaveCount(0)
	await page.getByRole('button', { name: 'Recover work order' }).click()
	await expect.poll(() => recoveryRequest).toContain('request_id')
})

test('stalled task is labelled in the operator tray with recover and reasoned cancel controls', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	const item = activity('recovery', false)
	let cancelled = false
	await page.route('**/v1/workspaces', async (route) => {
		await route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
	})
	await page.route('**/v1/activity*', async (route) => {
		await route.fulfill({ json: [{ task: item.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: true, stalled: item.stalled }] })
	})
	await page.route('**/v1/tasks/recovery/activity*', async (route) => {
		const current = activity('recovery', false)
		await route.fulfill({
			json: cancelled
				? { ...current, task: { ...current.task, state: 'closed' }, stalled: undefined }
				: current,
		})
	})
	let cancelBody = ''
	await page.route('**/v1/tasks/recovery/close*', async (route) => {
		cancelBody = route.request().postData() ?? ''
		cancelled = true
		await route.fulfill({ json: { ...item.task, state: 'closed' } })
	})
	await page.goto('/')
	const tray = page.getByRole('region', { name: 'Needs operator' })
	await expect(tray.getByText('Stalled')).toBeVisible()
	await expect(tray.getByText('harness exited: status 1')).toBeVisible()
	await tray.getByText('Short task').click()
	await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
	await page.getByRole('button', { name: 'Cancel task' }).click()
	await page.getByPlaceholder('Why is this task being cancelled?').fill('provider setup is obsolete')
	await page.getByRole('dialog', { name: 'Cancel task' }).getByRole('button', { name: 'Cancel task' }).click()
	await expect.poll(() => cancelBody).toContain('provider setup is obsolete')
	await expect(page.getByText('Closed', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Cancel task' })).toHaveCount(0)
})

test('forge failure categories render in the needs-operator tray and task activity evidence', async ({ page }) => {
	const item = activity('forge-failure', false)
	await page.route('**/v1/activity*', async (route) => {
		await route.fulfill({
			json: [{
				task: item.task,
				latest_stage: 'merge',
				last_event_at: '2026-07-15T12:02:00Z',
				needs_attention: true,
				forge_failure: item.forge_failure,
			}],
		})
	})

	await page.goto('/')
	const tray = page.getByRole('region', { name: 'Needs operator' })
	await expect(tray.getByText('forge_rate_limited')).toBeVisible()
	await expect(tray.getByText(/GitHub merge: merge pull request/)).toBeVisible()
	await tray.getByText('Short task').click()
	await expect(page.getByText(/forge_request · create task issue: request timed out/)).toBeVisible()
	await expect(page.getByText(/forge_permission · publish review comment: resource not accessible/)).toBeVisible()
	await expect(page.getByText(/forge_rate_limited · merge pull request: API rate limit exceeded/)).toBeVisible()
})

test('checkout-blocked recovery explains the safe operator sequence before recovery', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	await page.goto('/tasks/checkout-blocked-recovery/full')

	const failure = page.getByText('The primary checkout has pre-existing changes, so Conveyor left them untouched.')
	const resolveFirst = page.getByRole('region', { name: 'Implementation paused — checkout needs attention' }).getByText('Resolve the primary checkout changes, then retry the implementation.')
	const recoveryEffect = page.getByText(/Conveyor will not clean, commit, stash, or discard them/)
	const action = page.getByRole('button', { name: 'Retry implementation' })

	await expect(failure).toBeVisible()
	await expect(resolveFirst).toBeVisible()
	await expect(recoveryEffect).toBeVisible()
	await expect(action).toBeVisible()
	await expect(action).toBeDisabled()
	await page.getByLabel('I resolved the primary checkout changes.').check()
	await expect(action).toBeEnabled()
})

test('attempt recovery keeps the later checkout blocker authoritative without a standalone attempt list', async ({ page }) => {
	await page.goto('/tasks/attempt-recovery/full')

	await expect(page.getByRole('heading', { name: 'Implementation paused — checkout needs attention' })).toBeVisible()
	await expect(page.getByRole('region', { name: 'Execution attempts' })).toHaveCount(0)
	await expect(page.getByText('The provider usage or capacity limit stopped the last attempt.')).toHaveCount(0)
	await expect(page.getByText('The primary checkout has pre-existing changes, so Conveyor left them untouched.')).toBeVisible()
	const technicalSummary = page.getByText('Show technical activity')
	const technical = technicalSummary.locator('..')
	await technicalSummary.focus()
	await expect(technicalSummary).toBeFocused()
	await technicalSummary.press('Enter')
	await expect(technical.getByText('work_order.child_failed')).toBeVisible()
	await expect(technical.getByText('work_order.lease_renewed')).toBeVisible()
	await expect(page.getByText('harness exited before completing work order')).not.toBeVisible()
	await technical.getByText('Event payload').nth(2).click()
	await expect(technical.getByText('You have reached the provider usage limit. Try again later.', { exact: false })).toBeVisible()
})

test('provider-limit retry states expose only the correct action', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	await page.goto('/tasks/usage-retry-pending/full')
	await expect(page.getByText(/Retrying in/)).toBeVisible()
	await expect(page.getByRole('button', { name: /Retry implementation|Recover work order/ })).toHaveCount(0)
	await expect(page.getByText('The provider usage limit paused the last attempt.')).toBeVisible()

	await page.goto('/tasks/usage-suppressed/full')
	await expect(page.getByText('The provider usage or capacity limit stopped the last attempt.')).toBeVisible()
	await expect(page.getByRole('button', { name: 'Retry implementation' })).toBeVisible()
	await expect(page.getByRole('region', { name: 'Execution attempts' })).toHaveCount(0)
	await expect(page.getByText('harness exited before completing work order')).not.toBeVisible()
	const technical = page.getByText('Show technical activity').locator('..')
	await technical.getByText('Show technical activity').click()
	await technical.getByText('Event payload').click()
	await expect(technical.getByText('usage limit reached', { exact: false })).toBeVisible()
})

test('a running attempt becoming paused is announced through the live region', async ({ page }) => {
	let activityRequests = 0
	let releaseStream = () => {}
	const streamReady = new Promise<void>((resolve) => { releaseStream = resolve })
	await page.route('**/v1/tasks/live-pause/activity*', async (route) => {
		activityRequests++
		const item = activity('usage-suppressed', false)
		item.task.id = 'live-pause'
		item.work_orders[0].task_id = 'live-pause'
		item.events = activityRequests === 1 ? [{ id: 1, task_id: 'live-pause', job_id: item.work_orders[0].job_id, kind: 'work_order.claimed', actor_id: 'worker', actor_role: 'runner', payload: { attempt_id: 'live-attempt' }, at: createdAt }] : item.events
		item.work_orders[0] = activityRequests === 1 ? {
			...item.work_orders[0],
			state: 'claimed',
			claimable: false,
			attempt_id: 'live-attempt',
			last_attempt_id: undefined,
			last_attempt_outcome: undefined,
			last_failure_category: undefined,
			last_failure_message: undefined,
			last_failure_detail: undefined,
			retry_suppressed: false,
		} : item.work_orders[0]
		await route.fulfill({ json: item })
	})
	await page.route('**/v1/tasks/live-pause/events/stream*', async (route) => {
		await streamReady
		await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
	})

	await page.goto('/tasks/live-pause/full')
	await expect(page.getByRole('heading', { name: 'Implementation is in progress' })).toBeVisible()
	releaseStream()
	await expect(page.getByRole('status')).toContainText('Implementation paused — provider limit reached')
	await expect(page.getByRole('status')).toContainText('Retry the implementation after the provider limit has cleared.')
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

test('interrupted review recovery tolerates legacy null empty collections', async ({ page }) => {
	await page.goto('/tasks/interrupted-review-empty/full')
	await expect(page.getByText(/interrupted-review-empty-review-1-seat-1/)).toBeVisible()
	await expect(page.getByRole('button', { name: 'Recover interrupted review round' })).toHaveCount(1)
	await expect(page.getByText('Something went wrong!')).toHaveCount(0)
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

test('work-order cards use their actual stage and technical activity exposes captured failure detail', async ({ page }) => {
	await page.goto('/tasks/stage-aware/full')
	const timeline = page.getByRole('region', { name: 'Execution event timeline' })
	await expect(timeline.getByText('Spec — waiting for an operator agent', { exact: true })).toBeVisible()
	await expect(timeline.getByText('Implementation — in progress', { exact: true })).toBeVisible()
	await expect(timeline.getByText('Review — timed out', { exact: true })).toBeVisible()
	await expect(timeline.getByText('Review — waiting for an operator agent', { exact: true })).toHaveCount(0)
	await expect(page.getByRole('region', { name: 'Execution attempts' })).toHaveCount(0)
	const technical = page.getByText('Show technical activity').locator('..')
	await technical.getByText('Show technical activity').click()
	await technical.getByText('Event payload').click()
	await expect(technical.getByText('provider rejected the configured model', { exact: false })).toBeVisible()
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

test('task body renders safe GFM in sheet and full-page headers', async ({ page }) => {
	for (const path of ['/tasks/markdown-body', '/tasks/markdown-body/full']) {
		await page.goto(path)
		await expect(page.getByRole('heading', { name: 'Structured context' })).toBeVisible()
		await expect(page.getByText('clear', { exact: true })).toHaveCSS('font-weight', /^(600|700)$/)
		await expect(page.getByRole('link', { name: 'safe link' })).toHaveAttribute('href', 'https://example.test')
		await expect(page.getByText('completed item')).toBeVisible()
		await expect(page.locator('script[data-unsafe]')).toHaveCount(0)
		await expect(page.getByText('<script data-unsafe>window.hacked = true</script>')).toBeVisible()
	}
})

test('long task body is constrained and expands accessibly in both header variants', async ({ page }) => {
	for (const path of ['/tasks/long-body', '/tasks/long-body/full']) {
		await page.goto(path)
		const expand = page.getByRole('button', { name: 'Show full description' })
		await expect(expand).toBeVisible()
		await expect(expand).toHaveAttribute('aria-expanded', 'false')
		const viewportID = await expand.getAttribute('aria-controls')
		expect(viewportID).toBeTruthy()
		const viewport = page.locator(`#${viewportID}`)
		await expect.poll(() => viewport.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
		await expect(page.locator('[data-task-body-overflow-shadow]')).toBeVisible()

		await expand.click()
		const collapse = page.getByRole('button', { name: 'Show less description' })
		await expect(collapse).toHaveAttribute('aria-expanded', 'true')
		await expect(page.locator('[data-task-body-overflow-shadow]')).toHaveCount(0)
		await expect(page.getByText('Description paragraph 30.')).toBeVisible()
	}
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
	await expect(timelineRows).toHaveCount(2)
	await expect(timelineRows.nth(0)).toContainText('Panel of 2 · unanimous to pass')
	await expect(timelineRows.nth(1)).toContainText('Pull request opened')
})

test('review card renders authorized verification evidence with accessible preview and download fallback', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator-token'))
	const authorizationHeaders: string[] = []
	let videoRequests = 0
	let allowVideoDownload = false
	const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64')
	await page.route('**/v1/artifacts/evidence-image*', async (route) => {
		authorizationHeaders.push(await route.request().headerValue('authorization') ?? '')
		await route.fulfill({ contentType: 'image/png', body: png })
	})
	await page.route('**/v1/artifacts/evidence-video*', async (route) => {
		videoRequests++
		authorizationHeaders.push(await route.request().headerValue('authorization') ?? '')
		if (!allowVideoDownload) {
			await route.fulfill({ status: 503, body: 'preview unavailable' })
			return
		}
		await route.fulfill({ contentType: 'video/webm', body: 'downloadable recording' })
	})

	await page.goto('/tasks/evidence/full')
	const reviewCard = page.getByRole('region', { name: 'Human gate' })
	await expect(reviewCard.getByRole('heading', { name: 'Verification evidence' })).toBeVisible()
	await expect(reviewCard.getByRole('img', { name: 'proof screenshot.png' })).toBeVisible()
	await expect(reviewCard.getByRole('button', { name: 'Expand proof recording.webm' })).toBeVisible()

	await reviewCard.getByRole('button', { name: 'Expand proof screenshot.png' }).click()
	const screenshotDialog = page.getByRole('dialog', { name: 'proof screenshot.png' })
	await expect(screenshotDialog.getByRole('img', { name: 'proof screenshot.png' })).toBeVisible()
	await screenshotDialog.getByRole('button', { name: 'Close preview' }).click()

	await reviewCard.getByRole('button', { name: 'Expand proof recording.webm' }).click()
	const recordingDialog = page.getByRole('dialog', { name: 'proof recording.webm' })
	await expect(recordingDialog.getByText('This evidence could not be loaded for preview. Use the authorized download instead.')).toBeVisible()
	await expect(recordingDialog.getByRole('button', { name: 'Download' })).toBeEnabled()
	const previewRequestCount = videoRequests
	allowVideoDownload = true
	const downloadPromise = page.waitForEvent('download')
	await recordingDialog.getByRole('button', { name: 'Download' }).click()
	const download = await downloadPromise
	expect(download.suggestedFilename()).toBe('proof recording.webm')
	expect(videoRequests).toBe(previewRequestCount + 1)
	expect(authorizationHeaders.length).toBeGreaterThanOrEqual(3)
	expect(authorizationHeaders.every((header) => header === 'Bearer operator-token')).toBe(true)
})

test('spec approval keeps the human marker without a duplicate versioned event', async ({ page }) => {
	await page.goto('/tasks/spec-approval/full')

	const timeline = page.getByRole('region', { name: 'Execution event timeline' })
	const timelineRows = timeline.locator('ol > li')
	await expect(timelineRows).toHaveCount(2)
	await expect(timelineRows.nth(0)).toContainText('Spec v2 drafted')
	await expect(timelineRows.nth(1)).toContainText('Approved')
	await expect(timeline.getByText(/Spec v\d+ approved/)).toHaveCount(0)
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

	// A stage that produced no narration of its own collapses to one line —
	// it carries no warning tone and never leaks the rejected payload.
	const acceptedSpec = timeline.locator('li').filter({ hasText: 'Spec completed' })
	await expect(acceptedSpec).toHaveCount(1)
	await expect(acceptedSpec.locator('article')).toHaveCount(0)
	await expect(acceptedSpec.locator('.bg-attention-dot')).toHaveCount(0)
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
	await expect(timelineRows).toHaveCount(3)
	await expect(timelineRows.nth(0)).toContainText('Panel of 2 · unanimous to pass')
	await expect(timelineRows.nth(1)).toContainText('Review claim expired without verdict submission')
	await expect(timelineRows.nth(2)).toContainText('Pull request opened')

	// The task sheet uses the same historical rendering boundary.
	await page.goto('/tasks/diagnostics')
	await expect(page.getByText('Review claimed without terminal verdict submission')).toHaveCount(0)
	await expect(page.locator('article').filter({ hasText: 'Panel of 2 · unanimous to pass' })).toHaveCount(1)
	await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
})

test('human gate renders as the event timeline tail and the page opens scrolled to it', async ({ page }) => {
	await page.goto('/tasks/gate/full')

	await expect(page.getByRole('heading', { name: 'Activity' })).toBeVisible()
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

test('worker warning is scoped to actionable task-owned work at the human gate', async ({ page }) => {
	await page.goto('/tasks/human-gate-worker-scope/full')

	await expect(page.getByRole('region', { name: 'Human gate' }).getByText('Your review, please')).toBeVisible()
	await expect(page.getByText('GitHub review publication retrying')).toBeVisible()
	await expect(page.getByRole('region', { name: 'Auto worker unavailable' })).toHaveCount(0)
	await expect(page.getByText('This queued work has never started.')).toHaveCount(0)

	await page.goto('/tasks/queued-worker/full')
	await expect(page.getByRole('region', { name: 'Auto worker unavailable' })).toBeVisible()
	await expect(page.getByText('This queued work has never started.')).toBeVisible()
})

test('parked task offers recovery instead of an invalid approval verdict', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	await page.goto('/tasks/parked/full')

	await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Approve' })).toHaveCount(0)
	await expect(page.getByText('Parked by triage — resume from the recorded recovery stage when this work is ready.')).toBeVisible()

	const recoveryRequest = page.waitForRequest((request) =>
		request.method() === 'POST' && new URL(request.url()).pathname === '/v1/tasks/parked/redispatch',
	)
	await page.getByRole('button', { name: 'Resume task' }).click()
	await recoveryRequest
})

test('merge gate renders pending readiness without offering merge', async ({ page }) => {
	await page.goto('/tasks/merge-unknown/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate.getByText('Checking merge readiness')).toBeVisible()
	await expect(gate.getByRole('button', { name: 'Readiness pending' })).toBeDisabled()
	await expect(gate.getByRole('button', { name: 'Merge pull request' })).toHaveCount(0)
})

test('merge gate fails closed when readiness is absent', async ({ page }) => {
	await page.goto('/tasks/merge-missing/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate.getByText('Checking merge readiness')).toBeVisible()
	await expect(gate.getByRole('button', { name: 'Readiness pending' })).toBeDisabled()
	await expect(gate.getByRole('button', { name: 'Merge pull request' })).toHaveCount(0)
})

test('merge gate stays pending until the post-success task and activity refreshes settle', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let merged = false
	let releaseRefresh = () => {}
	const refreshReleased = new Promise<void>((resolve) => { releaseRefresh = resolve })
	await page.route('**/v1/**', async (route) => {
		const path = new URL(route.request().url()).pathname
		if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
		if (path === '/v1/tasks/merge-delayed/merge') {
			merged = true
			return route.fulfill({ json: { id: 'merge-delayed', state: 'approved' } })
		}
		if (!merged || (path !== '/v1/tasks/merge-delayed/activity' && path !== '/v1/activity')) return route.fallback()
		await refreshReleased
		if (path === '/v1/activity') return route.fulfill({ json: [] })
		const refreshed = activity('merge-delayed', false)
		refreshed.task.state = 'done'
		await route.fulfill({ json: refreshed })
	})

	await page.goto('/tasks/merge-delayed/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	const merge = gate.getByRole('button', { name: 'Merge pull request' })
	await merge.click()
	await expect(gate.getByRole('button', { name: 'Merging…' })).toBeDisabled()
	await expect(gate.getByRole('button', { name: 'Merge pull request' })).toHaveCount(0)

	releaseRefresh()
	await expect(gate).toHaveCount(0)
})

test('failed merge restores the existing error and retry action', async ({ page }) => {
	await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
	let attempts = 0
	await page.route('**/v1/tasks/merge-failure/merge*', async (route) => {
		attempts++
		await route.fulfill({ status: 502, body: 'GitHub merge failed' })
	})

	await page.goto('/tasks/merge-failure/full')
	const gate = page.getByRole('region', { name: 'Human gate' })
	const merge = gate.getByRole('button', { name: 'Merge pull request' })
	await merge.click()
	await expect(gate.getByText('Error: GitHub merge failed')).toBeVisible()
	await expect(merge).toBeEnabled()
	await merge.click()
	await expect.poll(() => attempts).toBe(2)
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

test('new task events preserve the task sheet scroll position', async ({ page }) => {
	await page.goto('/tasks/live-scroll')

	const timeline = page.getByRole('region', { name: 'Execution event timeline' })
	const container = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
	await expect(timeline.getByText('Spec v18 drafted')).toBeVisible()
	await expect.poll(() => container.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
	await container.evaluate((element) => { element.scrollTop = 120 })
	const before = await container.evaluate((element) => element.scrollTop)
	expect(before).toBeGreaterThan(0)

	emitLiveScrollEvent()

	const newest = timeline.getByText('Spec v19 drafted')
	await expect(newest).toBeVisible()
	await expect.poll(() => container.evaluate((element) => element.scrollTop)).toBe(before)
	await expect(newest).not.toBeInViewport()
	await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
})

for (const decision of ['approve', 'redirect'] as const) {
	test(`successful ${decision} scrolls the task sheet to the refreshed timeline tail`, async ({ page }) => {
		await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator'))
		let recorded = false
		await page.route('**/v1/tasks/gate/activity*', async (route) => {
			const item = activity('gate', true)
			if (recorded) {
				item.task.state = 'running'
				item.interventions = [{
					id: 1,
					task_id: 'gate',
					actor_id: 'dashboard-operator',
					actor_role: 'human',
					action: decision,
					reason_code: decision === 'approve' ? 'approved' : 'changes_requested',
					comment: decision === 'redirect' ? 'Please revise the implementation.' : '',
					at: '2026-07-15T12:10:00Z',
				}]
			}
			await route.fulfill({ json: item })
		})
		await page.route('**/v1/tasks/gate/review*', async (route) => {
			recorded = true
			await route.fulfill({ json: { task: activity('gate', true).task, checkout_available: false, checkout_guidance: '' } })
		})

		await page.goto('/tasks/gate')
		const container = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
		await expect.poll(() => container.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
		await container.evaluate((element) => { element.scrollTop = 0 })

		const gate = page.getByRole('region', { name: 'Human gate' })
		const reviewRequest = page.waitForRequest((request) => new URL(request.url()).pathname === '/v1/tasks/gate/review')
		if (decision === 'approve') {
			await gate.getByRole('button', { name: 'Approve' }).click()
		} else {
			await gate.getByRole('button', { name: 'Request changes' }).click()
			await gate.getByLabel('Redirect feedback').fill('Please revise the implementation.')
			const submit = gate.getByRole('button', { name: 'Send feedback' })
			await expect(submit).toBeEnabled()
			await submit.click()
		}
		await reviewRequest

		const intervention = page
			.getByRole('region', { name: 'Execution event timeline' })
			.locator('ol > li')
			.filter({ hasText: decision === 'approve' ? 'Approved' : 'Requested changes' })
			.last()
		await expect(intervention).toBeVisible()
		await expect(intervention).toBeInViewport()
		await expect.poll(() => container.evaluate((element) => Math.abs(element.scrollHeight - element.clientHeight - element.scrollTop))).toBeLessThanOrEqual(1)
		await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
	})
}

test('task sheet opens scrolled to the human gate for reviewable tasks', async ({ page }) => {
	await page.goto('/tasks/gate')
	const gate = page.getByRole('region', { name: 'Human gate' })
	await expect(gate).toBeInViewport()
	await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
})

test('board activity surfaces expired-without-verdict state', async ({ page }) => {
	await page.goto('/')
	await expect(page.getByText('Verdict claim expired')).toBeVisible()
})

test('spec diagrams render best-effort and malformed Mermaid falls back to source', async ({ page }) => {
	await page.goto('/tasks/mermaid-valid/full')
	await expect(page.locator('[data-mermaid] svg')).toBeVisible()

	await page.goto('/tasks/mermaid-invalid/full')
	await expect(page.locator('code.language-mermaid')).toContainText('this is deliberately malformed')
	await expect(page.getByRole('heading', { name: 'Specification' }).first()).toBeVisible()
})

// The anchor lives at its own canonical route (§21.49), and the legacy task
// URL is a redirect into it rather than a second door.
test('blueprint and dependency details remain linked and read only', async ({ page }) => {
	await page.goto('/tasks/blueprint-parent/full')
	await expect(page).toHaveURL(/\/blueprints\/blueprint-parent$/)
	await expect(page.getByRole('heading', { name: 'Delivery' })).toBeVisible()
	await expect(page.getByText('Completed').first()).toBeVisible()
	await expect(page.getByText('2 merged · 1 closed without merging').first()).toBeVisible()
  const dashboardLinks = page.getByRole('link', { name: 'Dashboard' })
  await expect(dashboardLinks).toHaveCount(2)
  for (let index = 0; index < await dashboardLinks.count(); index++) {
    await expect(dashboardLinks.nth(index)).toHaveAttribute('href', '/tasks/blueprint-child/full')
  }
	await expect(page.getByText('3 tasks created from the blueprint')).toBeVisible()
  await expect(page.getByText('Blueprint completed — all child tasks are finished')).toBeVisible()
	await expect(page.getByRole('link', { name: 'Runtime' }).first()).toBeVisible()

	await page.goto('/tasks/blueprint-child/full')
	await expect(page.getByRole('heading', { name: 'Waiting on dependencies' })).toBeVisible()
	await expect(page.getByText('Current state · Progressing')).toBeVisible()
	await expect(page.locator('section[aria-labelledby="current-execution-title"]').getByRole('link', { name: 'Runtime · Waiting' })).toHaveAttribute('href', '/tasks/blueprint-sub-2/full')
	await expect(page.getByText('Not applicable.')).toBeVisible()
	await expect(page.getByText('Nothing — implementation starts automatically when Runtime merges.')).toBeVisible()
	await expect(page.getByText('Implementation — waiting for an operator agent', { exact: true })).toHaveCount(0)
	await expect(page.getByText('Any agent connected over MCP can claim this.', { exact: false })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
	await expect(page.getByRole('link', { name: 'Phase 6 blueprint · spec v1' })).toHaveAttribute('href', '/blueprints/blueprint-parent')
	await expect(page.getByRole('link', { name: 'Runtime · Waiting' }).last()).toHaveAttribute('href', '/tasks/blueprint-sub-2/full')
	await expect(page.getByRole('link', { name: 'Persistence · Satisfied' })).toHaveAttribute('href', '/tasks/blueprint-sub-1/full')
  await expect(page.getByText('awaiting_human')).toHaveCount(0)
  await expect(page.getByText('terminal outcome')).toHaveCount(0)
})

test('blocked detail polling clears waiting state without a reload and stays off when unblocked', async ({ page }) => {
  await page.clock.install()
  await page.goto('/tasks/blocked-refresh/full')
  await expect(page.getByRole('heading', { name: 'Waiting on dependencies' })).toBeVisible()
  expect(detailRequestCounts.get('blocked-refresh')).toBe(1)

  await page.clock.fastForward(15_100)
  await expect.poll(() => detailRequestCounts.get('blocked-refresh') ?? 0).toBeGreaterThan(1)
  await expect(page.getByRole('heading', { name: 'Waiting on dependencies' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Backend contract · Satisfied' })).toBeVisible()
  await expect(page.getByText('Implementation — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Redispatch' })).toBeVisible()

  await page.goto('/tasks/no-blockers/full')
  await expect.poll(() => detailRequestCounts.get('no-blockers') ?? 0).toBe(1)
  const initialRequests = detailRequestCounts.get('no-blockers')
  await page.clock.fastForward(30_000)
  expect(detailRequestCounts.get('no-blockers')).toBe(initialRequests)
})

test('dependency links follow the detail route variant and spec claiming stays available', async ({ page }) => {
  await page.goto('/tasks/blueprint-child')
  await expect(page.getByRole('heading', { name: 'Waiting on dependencies' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Execution event timeline' }).getByRole('link', { name: 'Runtime · Waiting' })).toHaveAttribute('href', '/tasks/blueprint-sub-2')

  await page.goto('/tasks/spec-while-blocked/full')
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByRole('heading', { name: 'Waiting on dependencies' })).toBeVisible()
  await expect(timeline.getByText('Spec — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Implementation — waiting for an operator agent', { exact: true })).toHaveCount(0)
})

test('recovery-needing work stays primary when dependencies are also unresolved', async ({ page }) => {
  await page.goto('/tasks/blocked-suppressed/full')
  const currentState = page.locator('section[aria-labelledby="current-execution-title"]')
  await expect(currentState.getByRole('heading', { name: 'Implementation paused — recovery needed' })).toBeVisible()
  await expect(currentState.getByText('Schema migration · Waiting')).toBeVisible()
  await expect(currentState.getByText(/task remains dependency-gated/i)).toBeVisible()
  await expect(page.getByText(/starts automatically/i)).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
})

test('board cards show a neutral, keyboard-reachable dependency chip using titles', async ({ page }) => {
  const item = activity('blueprint-child', false)
  await page.route('**/v1/activity*', (route) => route.fulfill({
    json: [{ task: item.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: false }],
  }))
  await page.goto('/')
  const card = page.getByRole('link', { name: /Short task/ })
  await expect(card.getByText('Waiting on dependencies')).toBeVisible()
  await card.focus()
  await expect(card.getByRole('tooltip')).toContainText('Runtime')
})

test('unsatisfiable dependency is attention-worthy and can be unlinked with an audited reason only from detail', async ({ page }) => {
  await page.addInitScript(() => sessionStorage.setItem('conveyor-token', 'operator-token'))
  let unlinked = false
  let unlinkBody: Record<string, string> | undefined
  const currentItem = () => {
    const item = activity('unsatisfiable', false)
    if (!unlinked) return item
    return {
      ...item,
      task: { ...item.task, blocking_task_ids: undefined, dependencies: [] },
      needs_attention: false,
      stalled: undefined,
      events: [
        ...item.events,
        { id: 2, task_id: 'unsatisfiable', kind: 'task.dependency_removed', actor_id: 'operator', actor_role: 'human' as const, payload: { reason: 'The retired plan is no longer required' }, at: createdAt },
      ],
    }
  }
  await page.route('**/v1/workspaces', (route) => route.fulfill({
    json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: createdAt }],
  }))
  await page.route('**/v1/activity*', (route) => {
    const item = currentItem()
    return route.fulfill({
      json: [{ task: item.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: item.needs_attention, stalled: item.stalled }],
    })
  })
  await page.route('**/v1/tasks/unsatisfiable/activity*', (route) => route.fulfill({ json: currentItem() }))
  await page.route('**/v1/tasks/unsatisfiable/dependencies/closed-dependency*', async (route) => {
    unlinkBody = route.request().postDataJSON()
    unlinked = true
    await route.fulfill({ json: { task: currentItem().task, request_id: unlinkBody?.request_id, removed: true } })
  })

  await page.goto('/')
  const card = page.getByRole('link', { name: /Short task/ })
  await expect(card.getByText('Dependency needs attention')).toBeVisible()
  await expect(card.getByRole('button', { name: /Unlink dependency/ })).toHaveCount(0)
  await card.click()

	const currentState = page.locator('section[aria-labelledby="current-execution-title"]')
  await expect(page.getByRole('region', { name: 'Dependency needs attention' }).first()).toBeVisible()
	await expect(currentState.getByRole('heading', { name: 'Dependency needs attention' })).toBeVisible()
	await expect(currentState.getByText('Retired API plan · Needs attention')).toBeVisible()
	await expect(currentState.getByText(/Unlink the dead dependency.*cancel this task/i)).toBeVisible()
	await expect(page.getByRole('heading', { name: 'Waiting on dependencies' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Cancel task' })).toBeVisible()
  await page.getByRole('button', { name: 'Unlink dependency Retired API plan' }).click()
  const remove = page.getByRole('button', { name: 'Remove dependency' })
  await expect(remove).toBeDisabled()
  await page.getByPlaceholder('Why should this dependency be removed?').fill('The retired plan is no longer required')
  await remove.click()

  await expect.poll(() => unlinkBody?.reason).toBe('The retired plan is no longer required')
  expect(unlinkBody?.request_id).toBeTruthy()
  await expect(page.getByRole('region', { name: 'Dependency needs attention' })).toHaveCount(0)
  await expect(page.getByText('Dependency removed')).toBeVisible()
})
