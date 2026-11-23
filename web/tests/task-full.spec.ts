import { expect, type Page, type Route } from '@playwright/test'
import { buildTimeline, type TimelineEntry } from '../src/lib/activity'
import type { ActivityItem, TaskReviewSeatMetadata, TaskSummaries, WorkOrder } from '../src/lib/types'
import { boundedView, type CompleteActivity, test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { expectNoNextActionOmissions } from './helpers/next-action-omissions'
import { serveNextActionsFromActivity } from './helpers/next-action-stub'

// Task detail opens on its Timeline tab; pin it so a persisted Flow choice
// can never hide the activity these cases check (component-web-task-surfaces,
// Activity tabs and the task flow view).
test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('conveyor-task-activity-view', 'timeline'))
})

const createdAt = '2026-07-15T12:00:00Z'

// The one next-action box on task detail; it carries the current execution
// state, and its supporting title has the stable `current-execution-title` ID
// (component-web-task-surfaces, Next-action card).
const nextActionBox = (page: Page) => page.getByRole('region', { name: 'Next action' })
const executionTitle = (page: Page) => nextActionBox(page).locator('#current-execution-title')
const checkpointProgress =
  'Choose whether the recovery should preserve the existing proposal or replace it with the newly confirmed lifecycle direction before implementation continues.'
const detailRequestCounts = new Map<string, number>()

// The context a task carries. The proposal card is scoped by attachment as well
// as origin, so which documents these name is what decides whether
// a task's own pending version reaches its detail at all.
type AttachedDocument = { id: string; title: string; version: number; archived?: boolean }
type ContextProposalFixture = {
  task_id: string
  target_kind: 'requirement' | 'system_design'
  target_id: string
  target_title: string
  state: 'proposed'
  source: 'planning' | 'triage' | 'operator'
  justification: string
  created_by_event_id: number
  proposed_by: string
  workspace: string
  created_at: string
  updated_at: string
}
const attachedContext: Record<
  string,
  { requirements?: AttachedDocument[]; designs?: AttachedDocument[]; proposals?: ContextProposalFixture[] }
> = {
  'attached-context': {
    requirements: [{ id: 'req-context', title: 'Confirmed product outcome', version: 3, archived: true }],
    designs: [{ id: 'design-context', title: 'Confirmed technical guidance', version: 2 }],
  },
  'design-proposal': {
    designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }],
  },
  'design-proposal-queued': {
    designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }],
  },
}

function activity(taskId: string, overflowing: boolean, liveEventCount = 18) {
  const specContent =
    taskId === 'mermaid-valid'
      ? '## Specification\n\n```mermaid\ngraph TD\n  A --> B\n```'
      : taskId === 'mermaid-invalid'
        ? '## Specification\n\n```mermaid\nthis is deliberately malformed\n```'
        : taskId === 'overflowing' || taskId === 'gate'
          ? [
              '## Specification',
              '',
              ...Array.from({ length: 60 }, (_, index) => `Scrollable specification paragraph ${index + 1}.`),
              '',
              'Regression marker at the bottom of the task content.',
            ].join('\n\n')
          : taskId === 'long-spec'
            ? [
                '## Specification',
                '',
                ...Array.from({ length: 60 }, (_, index) => `Long specification paragraph ${index + 1}.`),
                '',
                'Long spec ending marker.',
              ].join('\n\n')
            : '## Specification\n\nRegression marker at the bottom of the task content.'
  const reviewActivity =
    taskId === 'attempt-recovery' || taskId === 'decision-blocker' || taskId === 'operator-checkpoint'
      ? {
          jobs: [],
          events: [
            {
              id: 1,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.claimed',
              actor_id: 'worker-1',
              actor_role: 'runner' as const,
              payload: {
                id: 'attempt-recovery-implement-1',
                attempt_id: 'attempt-1',
                stage: 'implement',
                session_id: 'worker-1',
              },
              at: '2026-07-15T12:00:00Z',
            },
            {
              id: 2,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.lease_renewed',
              actor_id: 'worker-1',
              actor_role: 'runner' as const,
              payload: { attempt_id: 'attempt-1', lease_expires_at: '2026-07-15T12:06:00Z' },
              at: '2026-07-15T12:01:00Z',
            },
            {
              id: 3,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.child_failed',
              actor_id: 'worker-1',
              actor_role: 'runner' as const,
              payload: {
                attempt_id: 'attempt-1',
                reason: 'harness exited before completing work order',
                detail: 'You have reached the provider usage limit. Try again later.',
                failure_category: 'provider_usage_limit',
                retry_suppressed: false,
                next_retry_at: '2026-07-15T12:03:00Z',
              },
              at: '2026-07-15T12:02:00Z',
            },
            {
              id: 4,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.claimed',
              actor_id: 'worker-2',
              actor_role: 'runner' as const,
              payload: {
                id: 'attempt-recovery-implement-1',
                attempt_id: 'attempt-2',
                stage: 'implement',
                session_id: 'worker-2',
              },
              at: '2026-07-15T12:03:00Z',
            },
            {
              id: 5,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.progress_reported',
              actor_id: 'worker-2',
              actor_role: 'runner' as const,
              payload: { message: taskId === 'operator-checkpoint' ? checkpointProgress : 'Final progress report' },
              at: '2026-07-15T12:03:30Z',
            },
            {
              id: 6,
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              kind: 'work_order.released',
              actor_id: 'worker-2',
              actor_role: 'runner' as const,
              payload: {
                attempt_id: 'attempt-2',
                reason:
                  taskId === 'decision-blocker'
                    ? 'DEC-2 is proposed but not confirmed.'
                    : taskId === 'operator-checkpoint'
                      ? 'operator checkpoint reached'
                      : 'checkout_blocked_dirty_primary: primary checkout has 77 pre-existing generated-dashboard changes',
                outcome: 'released',
                retry_suppressed: true,
              },
              at: '2026-07-15T12:04:00Z',
            },
          ],
          work_orders: [
            {
              id: 'attempt-recovery-implement-1',
              task_id: taskId,
              job_id: 'attempt-recovery-implement-1',
              stage: 'implement',
              state: 'queued',
              claimable: false,
              last_attempt_id: 'attempt-2',
              last_attempt_outcome: 'released',
              last_failure_message:
                taskId === 'decision-blocker'
                  ? 'DEC-2 is proposed but not confirmed.'
                  : taskId === 'operator-checkpoint'
                    ? 'operator checkpoint reached'
                    : 'checkout_blocked_dirty_primary: primary checkout has 77 pre-existing generated-dashboard changes',
              last_failure_at: '2026-07-15T12:04:00Z',
              automatic_retry_count: 1,
              retry_suppressed: true,
              queue_entered_at: '2026-07-15T12:04:00Z',
              queue_deadline: '2026-07-16T12:04:00Z',
              updated_at: '2026-07-15T12:04:00Z',
              redispatch_count: 0,
              cost_usd: 0,
              tokens_in: 0,
              tokens_out: 0,
              usage_reported: false,
              self_reported: true,
              progress: taskId === 'operator-checkpoint' ? checkpointProgress : undefined,
              last_agent_activity_at: '2026-07-15T12:03:30Z',
              last_agent_activity_label:
                taskId === 'operator-checkpoint'
                  ? `Progress: ${checkpointProgress.slice(0, 120)}…`
                  : 'Progress: Final progress report',
            },
          ],
        }
      : taskId === 'usage-retry-pending'
        ? {
            jobs: [],
            events: [
              {
                id: 1,
                task_id: taskId,
                job_id: 'usage-retry-pending-implement-1',
                kind: 'work_order.child_failed',
                actor_id: 'worker',
                actor_role: 'runner' as const,
                payload: {
                  attempt_id: 'usage-attempt-1',
                  reason: 'harness exited before completing work order',
                  detail: 'usage limit reached',
                  failure_category: 'provider_usage_limit',
                  retry_suppressed: false,
                  next_retry_at: '2099-07-15T12:05:00Z',
                },
                at: createdAt,
              },
            ],
            work_orders: [
              {
                id: 'usage-retry-pending-implement-1',
                task_id: taskId,
                job_id: 'usage-retry-pending-implement-1',
                stage: 'implement',
                state: 'queued',
                claimable: false,
                last_attempt_id: 'usage-attempt-1',
                last_attempt_outcome: 'child_failure',
                last_failure_category: 'provider_usage_limit',
                last_failure_message: 'harness exited before completing work order',
                last_failure_detail: 'usage limit reached',
                last_failure_at: createdAt,
                automatic_retry_count: 1,
                next_retry_at: '2099-07-15T12:05:00Z',
                retry_suppressed: false,
                queue_entered_at: createdAt,
                queue_deadline: '2099-07-16T12:00:00Z',
                updated_at: createdAt,
                redispatch_count: 0,
                cost_usd: 0,
                tokens_in: 0,
                tokens_out: 0,
                usage_reported: false,
                self_reported: true,
              },
            ],
          }
        : taskId === 'usage-suppressed'
          ? {
              jobs: [],
              events: [
                {
                  id: 1,
                  task_id: taskId,
                  job_id: 'usage-suppressed-implement-1',
                  kind: 'work_order.child_failed',
                  actor_id: 'worker',
                  actor_role: 'runner' as const,
                  payload: {
                    attempt_id: 'usage-attempt-2',
                    reason: 'harness exited before completing work order',
                    detail: 'usage limit reached',
                    failure_category: 'provider_usage_limit',
                    retry_suppressed: true,
                  },
                  at: createdAt,
                },
              ],
              work_orders: [
                {
                  id: 'usage-suppressed-implement-1',
                  task_id: taskId,
                  job_id: 'usage-suppressed-implement-1',
                  stage: 'implement',
                  state: 'queued',
                  claimable: false,
                  last_attempt_id: 'usage-attempt-2',
                  last_attempt_outcome: 'child_failure',
                  last_failure_category: 'provider_usage_limit',
                  last_failure_message: 'harness exited before completing work order',
                  last_failure_detail: 'usage limit reached',
                  last_failure_at: createdAt,
                  automatic_retry_count: 3,
                  retry_suppressed: true,
                  queue_entered_at: createdAt,
                  queue_deadline: '2026-07-16T12:00:00Z',
                  updated_at: createdAt,
                  redispatch_count: 0,
                  cost_usd: 0,
                  tokens_in: 0,
                  tokens_out: 0,
                  usage_reported: false,
                  self_reported: true,
                },
              ],
            }
          : taskId === 'stage-aware'
            ? {
                jobs: [],
                events: [
                  {
                    id: 1,
                    task_id: taskId,
                    job_id: 'stage-aware-spec',
                    kind: 'work_order.child_failed',
                    actor_id: 'worker',
                    actor_role: 'runner' as const,
                    payload: {
                      reason: 'harness exited: status 1',
                      detail: 'provider rejected the configured model',
                      retry_suppressed: true,
                      suppression_reason: 'identical failure output on consecutive attempts',
                    },
                    at: createdAt,
                  },
                ],
                work_orders: [
                  {
                    id: 'stage-aware-spec',
                    task_id: taskId,
                    job_id: 'stage-aware-spec',
                    stage: 'spec',
                    state: 'queued',
                    claimable: true,
                    queue_entered_at: createdAt,
                    queue_deadline: '2026-07-16T12:00:00Z',
                    redispatch_count: 0,
                    cost_usd: 0,
                    tokens_in: 0,
                    tokens_out: 0,
                    usage_reported: false,
                    self_reported: true,
                  },
                  {
                    id: 'stage-aware-implement',
                    task_id: taskId,
                    job_id: 'stage-aware-implement',
                    stage: 'implement',
                    state: 'claimed',
                    claimable: false,
                    queue_entered_at: '2026-07-15T12:01:00Z',
                    queue_deadline: '2026-07-16T12:01:00Z',
                    redispatch_count: 0,
                    cost_usd: 0,
                    tokens_in: 0,
                    tokens_out: 0,
                    usage_reported: false,
                    self_reported: true,
                  },
                  {
                    id: 'stage-aware-review',
                    task_id: taskId,
                    job_id: 'stage-aware-review',
                    stage: 'review',
                    state: 'timed_out',
                    claimable: false,
                    queue_entered_at: '2026-07-15T12:02:00Z',
                    queue_deadline: '2026-07-16T12:02:00Z',
                    execution_deadline: '2026-07-15T12:03:00Z',
                    redispatch_count: 0,
                    cost_usd: 0,
                    tokens_in: 0,
                    tokens_out: 0,
                    usage_reported: false,
                    self_reported: true,
                  },
                ],
              }
            : taskId === 'blocked-suppressed'
              ? {
                  jobs: [],
                  events: [
                    {
                      id: 1,
                      task_id: taskId,
                      job_id: 'blocked-suppressed-implement-1',
                      kind: 'work_order.child_failed',
                      actor_id: 'worker',
                      actor_role: 'runner' as const,
                      payload: {
                        attempt_id: 'blocked-attempt-1',
                        reason: 'harness exited before completing work order',
                        detail: 'provider rejected the configured model',
                        retry_suppressed: true,
                      },
                      at: createdAt,
                    },
                  ],
                  work_orders: [
                    {
                      id: 'blocked-suppressed-implement-1',
                      task_id: taskId,
                      job_id: 'blocked-suppressed-implement-1',
                      stage: 'implement' as const,
                      state: 'queued' as const,
                      claimable: false,
                      blocking_task_ids: ['blocked-dependency'],
                      last_attempt_id: 'blocked-attempt-1',
                      last_attempt_outcome: 'child_failure' as const,
                      last_failure_message: 'harness exited before completing work order',
                      last_failure_detail: 'provider rejected the configured model',
                      last_failure_at: createdAt,
                      automatic_retry_count: 3,
                      retry_suppressed: true,
                      queue_entered_at: createdAt,
                      queue_deadline: '2026-07-16T12:00:00Z',
                      updated_at: createdAt,
                      redispatch_count: 0,
                      cost_usd: 0,
                      tokens_in: 0,
                      tokens_out: 0,
                      usage_reported: false,
                      self_reported: true,
                    },
                  ],
                }
              : taskId === 'blueprint-child' || taskId === 'blocked-refresh' || taskId === 'spec-while-blocked'
                ? {
                    jobs: [],
                    events: [],
                    work_orders: [
                      ...(taskId === 'spec-while-blocked'
                        ? [
                            {
                              id: 'spec-while-blocked-spec-1',
                              task_id: taskId,
                              job_id: 'spec-while-blocked-spec-1',
                              stage: 'spec' as const,
                              state: 'queued' as const,
                              claimable: true,
                              queue_entered_at: '2026-07-15T11:59:00Z',
                              queue_deadline: '2026-07-16T11:59:00Z',
                              redispatch_count: 0,
                              cost_usd: 0,
                              tokens_in: 0,
                              tokens_out: 0,
                              usage_reported: false,
                              self_reported: true,
                            },
                          ]
                        : []),
                      {
                        id: `${taskId}-implement-1`,
                        task_id: taskId,
                        job_id: `${taskId}-implement-1`,
                        stage: 'implement' as const,
                        state: 'queued' as const,
                        claimable: false,
                        blocking_task_ids: [taskId === 'blueprint-child' ? 'blueprint-sub-2' : 'refresh-dependency'],
                        queue_entered_at: createdAt,
                        queue_deadline: '2026-07-16T12:00:00Z',
                        redispatch_count: 0,
                        cost_usd: 0,
                        tokens_in: 0,
                        tokens_out: 0,
                        usage_reported: false,
                        self_reported: true,
                      },
                    ],
                  }
                : taskId === 'live-scroll' || taskId === 'gate'
                  ? {
                      jobs: [],
                      events: Array.from({ length: liveEventCount }, (_, index) => ({
                        id: index + 1,
                        task_id: taskId,
                        // Plan versions and dependency changes stay technical,
                        // so the filler rows are dispatch failure notes that the
                        // timeline still renders.
                        kind: 'dispatch.failed',
                        actor_id: 'system',
                        actor_role: 'system' as const,
                        payload: { error: `Live event ${index + 1}` },
                        at: `2026-07-15T12:00:${String(index).padStart(2, '0')}Z`,
                      })),
                      work_orders: [],
                    }
                  : taskId === 'reviews'
                    ? {
                        jobs: [
                          {
                            id: 'reviews-review-1-seat-1',
                            task_id: taskId,
                            stage: 'review',
                            harness: 'codex',
                            model_tier: 'gpt-review',
                            auth_mode: 'byoa',
                            runner: 'worker',
                            confinement: 'none',
                            cost_usd: 0,
                            tokens_in: 0,
                            tokens_out: 0,
                            state: 'done',
                            started_at: createdAt,
                            ended_at: '2026-07-15T12:01:00Z',
                          },
                          {
                            id: 'reviews-review-1-seat-2',
                            task_id: taskId,
                            stage: 'review',
                            harness: 'claude',
                            model_tier: 'claude-review',
                            auth_mode: 'byoa',
                            runner: 'worker',
                            confinement: 'none',
                            cost_usd: 0,
                            tokens_in: 0,
                            tokens_out: 0,
                            state: 'done',
                            started_at: '2026-07-15T12:02:00Z',
                            ended_at: '2026-07-15T12:03:00Z',
                          },
                        ],
                        events: [
                          {
                            id: 1,
                            task_id: taskId,
                            kind: 'spec.version_approved',
                            actor_id: 'operator',
                            actor_role: 'human',
                            payload: { version: 1 },
                            at: '2026-07-15T11:59:00Z',
                          },
                          {
                            id: 2,
                            task_id: taskId,
                            job_id: 'reviews-review-1-seat-1',
                            kind: 'review.completed',
                            actor_id: 'worker-1',
                            actor_role: 'runner',
                            payload: {
                              review_work_order_id: 'reviews-review-1-seat-1',
                              review_round: 1,
                              verdict: 'approve',
                              summary: 'Seat one approved',
                              feedback: 'Approved guidance remains visible.',
                              review_seat: 1,
                              reviewer_model: 'gpt-review',
                              required_effort: 'high',
                              model_enforcement: 'worker-pinned',
                            },
                            at: '2026-07-15T12:01:00Z',
                          },
                          {
                            id: 3,
                            task_id: taskId,
                            kind: 'review.completed',
                            actor_id: 'legacy-worker',
                            actor_role: 'runner',
                            payload: {
                              verdict: 'approve',
                              summary: 'Legacy duplicate',
                              feedback: 'This standalone duplicate stays hidden.',
                            },
                            at: '2026-07-15T12:01:15Z',
                          },
                          {
                            id: 4,
                            task_id: taskId,
                            kind: 'pull_request.opened',
                            actor_id: 'system',
                            actor_role: 'system',
                            payload: { url: 'https://example.test/pull/1' },
                            at: '2026-07-15T12:01:30Z',
                          },
                          // Deliberately no review_work_order_id: seat matching must fall back
                          // to the event's job id for older payloads.
                          {
                            id: 5,
                            task_id: taskId,
                            job_id: 'reviews-review-1-seat-2',
                            kind: 'review.completed',
                            actor_id: 'worker-2',
                            actor_role: 'runner',
                            payload: {
                              verdict: 'changes_requested',
                              summary: 'Seat two requested changes',
                              feedback: 'Changes guidance remains visible.',
                              review_seat: 2,
                              reviewer_model: 'claude-review',
                              required_effort: '',
                              model_enforcement: 'worker-pinned',
                            },
                            at: '2026-07-15T12:03:00Z',
                          },
                          {
                            id: 6,
                            task_id: taskId,
                            kind: 'pipeline.bounced',
                            actor_id: 'system',
                            actor_role: 'system',
                            payload: {
                              count: 1,
                              reason_code: 'changes_requested',
                              feedback: 'Changes guidance remains visible.',
                            },
                            at: '2026-07-15T12:04:00Z',
                          },
                        ],
                        work_orders: [
                          {
                            id: 'reviews-review-1-seat-1',
                            task_id: taskId,
                            job_id: 'reviews-review-1-seat-1',
                            stage: 'review',
                            state: 'completed',
                            review_round: 1,
                            review_seat: 1,
                            required_model: 'gpt-review',
                            required_harness: 'codex',
                            required_effort: 'high',
                            model_enforcement: 'worker-pinned',
                            queue_entered_at: createdAt,
                            queue_deadline: '2026-07-16T12:00:00Z',
                            redispatch_count: 0,
                            cost_usd: 0,
                            tokens_in: 0,
                            tokens_out: 0,
                            usage_reported: false,
                            self_reported: true,
                            created_at: createdAt,
                            updated_at: '2026-07-15T12:01:00Z',
                          },
                          {
                            id: 'reviews-review-1-seat-2',
                            task_id: taskId,
                            job_id: 'reviews-review-1-seat-2',
                            stage: 'review',
                            state: 'completed',
                            review_round: 1,
                            review_seat: 2,
                            required_model: 'claude-review',
                            required_harness: 'claude',
                            model_enforcement: 'worker-pinned',
                            queue_entered_at: createdAt,
                            queue_deadline: '2026-07-16T12:00:00Z',
                            redispatch_count: 0,
                            cost_usd: 0,
                            tokens_in: 0,
                            tokens_out: 0,
                            usage_reported: false,
                            self_reported: true,
                            created_at: createdAt,
                            updated_at: '2026-07-15T12:03:00Z',
                          },
                        ],
                      }
                    : taskId === 'markdown-summary'
                      ? {
                          jobs: [
                            {
                              id: 'markdown-summary-implement-1',
                              task_id: taskId,
                              stage: 'implement',
                              harness: 'codex',
                              model_tier: 'gpt-implement',
                              runner: 'worker',
                              confinement: 'none',
                              cost_usd: 0,
                              tokens_in: 0,
                              tokens_out: 0,
                              state: 'done',
                              started_at: createdAt,
                              ended_at: '2026-07-15T12:01:00Z',
                            },
                          ],
                          events: [
                            {
                              id: 1,
                              task_id: taskId,
                              job_id: 'markdown-summary-implement-1',
                              kind: 'job.summary',
                              actor_id: 'runner',
                              actor_role: 'runner',
                              payload: {
                                summary:
                                  '## Validation\n\nValidated `make build` while preserving _structured context_:\n\n- completed item\n\n<script data-summary-unsafe>window.hacked = true</script> as text.',
                              },
                              at: '2026-07-15T12:01:00Z',
                            },
                          ],
                          work_orders: [],
                        }
                      : taskId === 'output-invalid'
                        ? {
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
                              {
                                id: 'output-invalid-preferred-job',
                                task_id: taskId,
                                stage: 'triage',
                                harness: 'codex',
                                model_tier: 'gpt-triage',
                                runner: 'in-process',
                                confinement: 'none',
                                cost_usd: 0,
                                tokens_in: 0,
                                tokens_out: 0,
                                state: 'done',
                                started_at: '2026-07-15T12:05:00Z',
                                ended_at: '2026-07-15T12:05:30Z',
                              },
                              {
                                id: 'output-invalid-preferred-triage',
                                task_id: taskId,
                                stage: 'triage',
                                harness: 'codex',
                                model_tier: 'gpt-triage',
                                runner: 'in-process',
                                confinement: 'none',
                                cost_usd: 0,
                                tokens_in: 0,
                                tokens_out: 0,
                                state: 'done',
                                started_at: '2026-07-15T12:06:00Z',
                                ended_at: '2026-07-15T12:06:30Z',
                              },
                              {
                                id: 'output-invalid-preferred-review',
                                task_id: taskId,
                                stage: 'review',
                                harness: 'codex',
                                model_tier: 'gpt-review',
                                runner: 'in-process',
                                confinement: 'none',
                                cost_usd: 0,
                                tokens_in: 0,
                                tokens_out: 0,
                                state: 'done',
                                started_at: '2026-07-15T12:07:00Z',
                                ended_at: '2026-07-15T12:07:30Z',
                              },
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
                                  error:
                                    index === 0
                                      ? 'spec requires one conveyor:acceptance block'
                                      : `spec validation error ${index + 1}`,
                                  output: index === 0 ? 'PRIVATE REJECTED OUTPUT' : undefined,
                                },
                                at: `2026-07-15T12:0${index}:31Z`,
                              })),
                              {
                                id: 5,
                                task_id: taskId,
                                job_id: 'output-invalid-spec-5',
                                kind: 'spec.version_created',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { version: 1 },
                                at: '2026-07-15T12:04:31Z',
                              },
                              // A same-stage rejection for another job must not mark the accepted
                              // spec attempt as rejected.
                              {
                                id: 6,
                                task_id: taskId,
                                job_id: 'output-invalid-unrelated',
                                kind: 'spec.output_invalid',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { error: 'unrelated validation error' },
                                at: '2026-07-15T12:04:32Z',
                              },
                              {
                                id: 7,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-job',
                                kind: 'triage.output_invalid',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { error: 'superseded job error' },
                                at: '2026-07-15T12:05:31Z',
                              },
                              {
                                id: 8,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-job',
                                kind: 'job.summary',
                                actor_id: 'runner',
                                actor_role: 'runner',
                                payload: { summary: 'Harness narration wins.' },
                                at: '2026-07-15T12:05:32Z',
                              },
                              {
                                id: 9,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-triage',
                                kind: 'triage.output_invalid',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { error: 'superseded triage error' },
                                at: '2026-07-15T12:06:31Z',
                              },
                              {
                                id: 10,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-triage',
                                kind: 'triage.completed',
                                actor_id: 'runner',
                                actor_role: 'runner',
                                payload: { summary: 'Accepted triage summary wins.' },
                                at: '2026-07-15T12:06:32Z',
                              },
                              {
                                id: 11,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-review',
                                kind: 'review.output_invalid',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { error: 'superseded review error' },
                                at: '2026-07-15T12:07:31Z',
                              },
                              {
                                id: 12,
                                task_id: taskId,
                                job_id: 'output-invalid-preferred-review',
                                kind: 'review.completed',
                                actor_id: 'runner',
                                actor_role: 'runner',
                                payload: {
                                  summary: 'Accepted review summary wins.',
                                  feedback: 'Accepted review feedback wins too.',
                                },
                                at: '2026-07-15T12:07:32Z',
                              },
                              {
                                id: 13,
                                task_id: taskId,
                                job_id: 'output-invalid-spec-4',
                                kind: 'pipeline.bounce_limit',
                                actor_id: 'pipeline',
                                actor_role: 'system',
                                payload: { source: 'spec.output_invalid', max_bounces: 4 },
                                at: '2026-07-15T12:08:00Z',
                              },
                            ],
                            work_orders: [],
                          }
                        : taskId === 'diagnostics'
                          ? {
                              jobs: [
                                {
                                  id: 'diagnostics-review-1-seat-1',
                                  task_id: taskId,
                                  stage: 'review',
                                  harness: 'codex',
                                  model_tier: 'gpt-review',
                                  auth_mode: 'byoa',
                                  runner: 'worker',
                                  confinement: 'none',
                                  cost_usd: 0,
                                  tokens_in: 0,
                                  tokens_out: 0,
                                  state: 'running',
                                  started_at: '2026-07-15T12:00:00Z',
                                },
                                {
                                  id: 'diagnostics-review-1-seat-2',
                                  task_id: taskId,
                                  stage: 'review',
                                  harness: 'claude',
                                  model_tier: 'claude-review',
                                  auth_mode: 'byoa',
                                  runner: 'worker',
                                  confinement: 'none',
                                  cost_usd: 0,
                                  tokens_in: 0,
                                  tokens_out: 0,
                                  state: 'running',
                                  started_at: '2026-07-15T12:00:30Z',
                                },
                              ],
                              events: [
                                {
                                  id: 1,
                                  task_id: taskId,
                                  kind: 'spec.version_approved',
                                  actor_id: 'operator',
                                  actor_role: 'human',
                                  payload: { version: 1 },
                                  at: '2026-07-15T11:59:00Z',
                                },
                                {
                                  id: 2,
                                  task_id: taskId,
                                  kind: 'pull_request.opened',
                                  actor_id: 'system',
                                  actor_role: 'system',
                                  payload: { url: 'https://example.test/pull/1' },
                                  at: '2026-07-15T12:05:00Z',
                                },
                              ],
                              work_orders: [
                                {
                                  id: 'diagnostics-review-1-seat-1',
                                  task_id: taskId,
                                  job_id: 'diagnostics-review-1-seat-1',
                                  stage: 'review',
                                  state: 'claimed',
                                  review_round: 1,
                                  review_seat: 1,
                                  required_model: 'gpt-review',
                                  required_harness: 'codex',
                                  queue_entered_at: '2026-07-15T12:00:00Z',
                                  queue_deadline: '2026-07-16T12:00:00Z',
                                  redispatch_count: 0,
                                  cost_usd: 0,
                                  tokens_in: 0,
                                  tokens_out: 0,
                                  usage_reported: false,
                                  self_reported: true,
                                },
                                {
                                  id: 'diagnostics-review-1-seat-2',
                                  task_id: taskId,
                                  job_id: 'diagnostics-review-1-seat-2',
                                  stage: 'review',
                                  state: 'claimed',
                                  review_round: 1,
                                  review_seat: 2,
                                  required_model: 'claude-review',
                                  required_harness: 'claude',
                                  queue_entered_at: '2026-07-15T12:00:30Z',
                                  queue_deadline: '2026-07-16T12:00:30Z',
                                  redispatch_count: 0,
                                  cost_usd: 0,
                                  tokens_in: 0,
                                  tokens_out: 0,
                                  usage_reported: false,
                                  self_reported: true,
                                },
                              ],
                            }
                          : taskId === 'timeout'
                            ? {
                                jobs: [
                                  {
                                    id: 'timeout-review',
                                    task_id: taskId,
                                    stage: 'review',
                                    harness: 'claude',
                                    model_tier: 'claude-review',
                                    auth_mode: 'byoa',
                                    runner: 'worker',
                                    confinement: 'none',
                                    cost_usd: 0,
                                    tokens_in: 0,
                                    tokens_out: 0,
                                    state: 'failed',
                                    started_at: '2026-07-15T12:00:00Z',
                                    ended_at: '2026-07-15T12:30:00Z',
                                  },
                                ],
                                events: [],
                                work_orders: [
                                  {
                                    id: 'timeout-review',
                                    task_id: taskId,
                                    job_id: 'timeout-review',
                                    stage: 'review',
                                    state: 'timed_out',
                                    queue_entered_at: '2026-07-15T11:00:00Z',
                                    queue_deadline: '2026-07-16T11:00:00Z',
                                    execution_started_at: '2026-07-15T12:00:00Z',
                                    execution_deadline: '2026-07-15T12:30:00Z',
                                    updated_at: '2026-07-16T08:24:00Z',
                                    redispatch_count: 0,
                                    cost_usd: 0,
                                    tokens_in: 0,
                                    tokens_out: 0,
                                    usage_reported: false,
                                    self_reported: true,
                                  },
                                ],
                              }
                            : taskId === 'checkout-blocked-recovery'
                              ? {
                                  jobs: [
                                    {
                                      id: 'checkout-blocked-implement-1',
                                      task_id: taskId,
                                      stage: 'implement',
                                      state: 'failed',
                                      cost_usd: 0,
                                      tokens_in: 0,
                                      tokens_out: 0,
                                    },
                                  ],
                                  events: [],
                                  work_orders: [
                                    {
                                      id: 'checkout-blocked-implement-1',
                                      task_id: taskId,
                                      job_id: 'checkout-blocked-implement-1',
                                      stage: 'implement',
                                      state: 'queued',
                                      claimable: false,
                                      last_attempt_outcome: 'released',
                                      last_failure_message:
                                        'checkout_blocked_dirty_primary: shared primary checkout has pre-existing modifications in CLAUDE.md and a repository policy document; operator changes preserved',
                                      automatic_retry_count: 0,
                                      retry_suppressed: true,
                                      queue_entered_at: createdAt,
                                      queue_deadline: '2026-07-16T12:00:00Z',
                                      redispatch_count: 0,
                                      cost_usd: 0,
                                      tokens_in: 0,
                                      tokens_out: 0,
                                      usage_reported: false,
                                      self_reported: true,
                                    },
                                  ],
                                }
                              : taskId === 'recovery'
                                ? {
                                    jobs: [
                                      {
                                        id: 'recovery-review-1-seat-1',
                                        task_id: taskId,
                                        stage: 'review',
                                        state: 'pending',
                                        cost_usd: 0,
                                        tokens_in: 0,
                                        tokens_out: 0,
                                      },
                                    ],
                                    events: [],
                                    work_orders: [
                                      {
                                        id: 'recovery-review-1-seat-1',
                                        task_id: taskId,
                                        job_id: 'recovery-review-1-seat-1',
                                        stage: 'review',
                                        state: 'queued',
                                        claimable: false,
                                        last_attempt_outcome: 'child_failure',
                                        last_failure_message: 'harness exited: status 1',
                                        last_failure_exit_status: 1,
                                        last_failure_at: createdAt,
                                        automatic_retry_count: 3,
                                        retry_suppressed: true,
                                        queue_entered_at: createdAt,
                                        queue_deadline: '2026-07-16T12:00:00Z',
                                        redispatch_count: 0,
                                        cost_usd: 0,
                                        tokens_in: 0,
                                        tokens_out: 0,
                                        usage_reported: false,
                                        self_reported: true,
                                      },
                                    ],
                                  }
                                : taskId === 'interrupted-review' || taskId === 'merged-retained-review'
                                  ? {
                                      jobs: [
                                        {
                                          id: 'interrupted-review-review-1-seat-1',
                                          task_id: taskId,
                                          stage: 'review',
                                          state: taskId === 'merged-retained-review' ? 'pending' : 'done',
                                          cost_usd: 0,
                                          tokens_in: 0,
                                          tokens_out: 0,
                                        },
                                        {
                                          id: 'interrupted-review-review-1-seat-2',
                                          task_id: taskId,
                                          stage: 'review',
                                          state: 'pending',
                                          cost_usd: 0,
                                          tokens_in: 0,
                                          tokens_out: 0,
                                        },
                                      ],
                                      events: [
                                        {
                                          id: 1,
                                          task_id: taskId,
                                          job_id: 'interrupted-review-review-1-seat-1',
                                          kind: 'review.completed',
                                          actor_id: 'worker-1',
                                          actor_role: 'runner',
                                          payload: {
                                            review_work_order_id: 'interrupted-review-review-1-seat-1',
                                            verdict: 'approve',
                                            summary: 'Completed verdict',
                                            feedback: 'Retain this verdict.',
                                            review_round: 1,
                                            review_seat: 1,
                                          },
                                          at: createdAt,
                                        },
                                      ],
                                      work_orders: [
                                        {
                                          id: 'interrupted-review-review-1-seat-1',
                                          task_id: taskId,
                                          job_id: 'interrupted-review-review-1-seat-1',
                                          stage: 'review',
                                          state: taskId === 'merged-retained-review' ? 'queued' : 'completed',
                                          retry_suppressed: taskId === 'merged-retained-review' ? true : undefined,
                                          review_round: 1,
                                          review_seat: 1,
                                          queue_entered_at: createdAt,
                                          queue_deadline: '2026-07-16T12:00:00Z',
                                          redispatch_count: 0,
                                          cost_usd: 0,
                                          tokens_in: 0,
                                          tokens_out: 0,
                                          usage_reported: false,
                                          self_reported: true,
                                        },
                                        {
                                          id: 'interrupted-review-review-1-seat-2',
                                          task_id: taskId,
                                          job_id: 'interrupted-review-review-1-seat-2',
                                          stage: 'review',
                                          state: 'queued',
                                          claimable: false,
                                          review_round: 1,
                                          review_seat: 2,
                                          last_attempt_outcome: 'expired',
                                          retry_suppressed: true,
                                          required_harness: 'claude',
                                          queue_entered_at: createdAt,
                                          queue_deadline: '2026-07-16T12:00:00Z',
                                          redispatch_count: 0,
                                          cost_usd: 0,
                                          tokens_in: 0,
                                          tokens_out: 0,
                                          usage_reported: false,
                                          self_reported: true,
                                        },
                                      ],
                                    }
                                  : taskId === 'human-gate-worker-scope'
                                    ? {
                                        jobs: [
                                          {
                                            id: 'human-gate-worker-scope-review-1-seat-1',
                                            task_id: taskId,
                                            stage: 'review',
                                            state: 'done',
                                            cost_usd: 0,
                                            tokens_in: 0,
                                            tokens_out: 0,
                                            started_at: createdAt,
                                            ended_at: '2026-07-15T12:01:00Z',
                                          },
                                        ],
                                        events: [
                                          {
                                            id: 1,
                                            task_id: taskId,
                                            job_id: 'human-gate-worker-scope-review-1-seat-1',
                                            kind: 'review.completed',
                                            actor_id: 'worker-1',
                                            actor_role: 'runner',
                                            payload: {
                                              verdict: 'approve',
                                              summary: 'Review approved',
                                              feedback: 'Ready for human review.',
                                              review_round: 1,
                                              review_seat: 1,
                                            },
                                            at: '2026-07-15T12:01:00Z',
                                          },
                                          {
                                            id: 2,
                                            task_id: taskId,
                                            job_id: 'human-gate-worker-scope-review-1-seat-1',
                                            kind: 'review.publication_failed',
                                            actor_id: 'system',
                                            actor_role: 'system',
                                            payload: {
                                              review_work_order_id: 'human-gate-worker-scope-review-1-seat-1',
                                              last_error: 'GitHub unavailable',
                                            },
                                            at: '2026-07-15T12:02:00Z',
                                          },
                                        ],
                                        work_orders: [
                                          {
                                            id: 'human-gate-worker-scope-review-1-seat-1',
                                            task_id: taskId,
                                            job_id: 'human-gate-worker-scope-review-1-seat-1',
                                            stage: 'review',
                                            state: 'completed',
                                            review_round: 1,
                                            review_seat: 1,
                                            required_harness: 'codex',
                                            queue_entered_at: createdAt,
                                            queue_deadline: '2026-07-16T12:00:00Z',
                                            redispatch_count: 0,
                                            cost_usd: 0,
                                            tokens_in: 0,
                                            tokens_out: 0,
                                            usage_reported: false,
                                            self_reported: true,
                                          },
                                        ],
                                      }
                                    : taskId === 'queued-worker' || taskId === 'pull-only-worker'
                                      ? {
                                          jobs: [],
                                          events: [],
                                          work_orders: [
                                            {
                                              id: 'queued-worker-implement-1',
                                              task_id: taskId,
                                              job_id: 'queued-worker-implement-1',
                                              stage: 'implement',
                                              state: 'queued',
                                              claimable: true,
                                              required_harness: 'codex',
                                              queue_entered_at: createdAt,
                                              queue_deadline: '2026-07-16T12:00:00Z',
                                              redispatch_count: 0,
                                              cost_usd: 0,
                                              tokens_in: 0,
                                              tokens_out: 0,
                                              usage_reported: false,
                                              self_reported: true,
                                            },
                                          ],
                                        }
                                      : taskId === 'review-retry'
                                        ? {
                                            jobs: [
                                              {
                                                id: 'review-retry-review-1-seat-1',
                                                task_id: taskId,
                                                stage: 'review',
                                                state: 'done',
                                                cost_usd: 0,
                                                tokens_in: 0,
                                                tokens_out: 0,
                                              },
                                              {
                                                id: 'review-retry-review-1-seat-2',
                                                task_id: taskId,
                                                stage: 'review',
                                                state: 'failed',
                                                cost_usd: 0,
                                                tokens_in: 0,
                                                tokens_out: 0,
                                              },
                                            ],
                                            events: [
                                              {
                                                id: 1,
                                                task_id: taskId,
                                                job_id: 'review-retry-review-1-seat-1',
                                                kind: 'review.completed',
                                                actor_id: 'worker-1',
                                                actor_role: 'runner',
                                                payload: {
                                                  verdict: 'approve',
                                                  summary: 'Historical approval',
                                                  feedback: 'Round one feedback remains visible.',
                                                  review_round: 1,
                                                  review_seat: 1,
                                                },
                                                at: createdAt,
                                              },
                                            ],
                                            work_orders: [
                                              {
                                                id: 'review-retry-review-1-seat-1',
                                                task_id: taskId,
                                                job_id: 'review-retry-review-1-seat-1',
                                                stage: 'review',
                                                state: 'completed',
                                                review_round: 1,
                                                review_seat: 1,
                                                queue_entered_at: createdAt,
                                                queue_deadline: '2026-07-16T12:00:00Z',
                                                redispatch_count: 0,
                                                cost_usd: 0,
                                                tokens_in: 0,
                                                tokens_out: 0,
                                                usage_reported: false,
                                                self_reported: true,
                                              },
                                              {
                                                id: 'review-retry-review-1-seat-2',
                                                task_id: taskId,
                                                job_id: 'review-retry-review-1-seat-2',
                                                stage: 'review',
                                                state: 'timed_out',
                                                review_round: 1,
                                                review_seat: 2,
                                                queue_entered_at: createdAt,
                                                queue_deadline: '2026-07-16T12:00:00Z',
                                                execution_deadline: '2026-07-15T13:00:00Z',
                                                last_failure_message: 'review harness exhausted retries',
                                                redispatch_count: 0,
                                                cost_usd: 0,
                                                tokens_in: 0,
                                                tokens_out: 0,
                                                usage_reported: false,
                                                self_reported: true,
                                              },
                                            ],
                                          }
                                        : taskId === 'spec-approval'
                                          ? {
                                              jobs: [],
                                              events: [
                                                {
                                                  id: 1,
                                                  task_id: taskId,
                                                  kind: 'spec.version_created',
                                                  actor_id: 'pipeline',
                                                  actor_role: 'system' as const,
                                                  payload: { version: 2 },
                                                  at: '2026-07-15T11:58:00Z',
                                                },
                                                // Every backend records the approval's
                                                // intervention event with the intervention.
                                                {
                                                  id: 2,
                                                  task_id: taskId,
                                                  kind: 'intervention.approve',
                                                  actor_id: 'operator',
                                                  actor_role: 'human' as const,
                                                  payload: { reason_code: 'approved', comment: '' },
                                                  at: '2026-07-15T11:59:00Z',
                                                },
                                                {
                                                  id: 3,
                                                  task_id: taskId,
                                                  kind: 'spec.version_approved',
                                                  actor_id: 'operator',
                                                  actor_role: 'human' as const,
                                                  payload: { version: 2 },
                                                  at: '2026-07-15T11:59:00Z',
                                                },
                                              ],
                                              work_orders: [],
                                            }
                                          : taskId === 'setup-submitted'
                                            ? {
                                                jobs: [],
                                                events: [],
                                                // A delivered implement attempt awaiting review: does not block a setup
                                                // change; only claimed attempts and in-flight verdicts do.
                                                work_orders: [
                                                  {
                                                    id: 'setup-submitted-implement-1',
                                                    task_id: taskId,
                                                    job_id: 'setup-submitted-implement-1',
                                                    stage: 'implement',
                                                    state: 'submitted',
                                                    claimable: false,
                                                    queue_entered_at: createdAt,
                                                    queue_deadline: '2026-07-16T12:00:00Z',
                                                    redispatch_count: 0,
                                                    cost_usd: 0,
                                                    tokens_in: 0,
                                                    tokens_out: 0,
                                                    usage_reported: false,
                                                    self_reported: true,
                                                  },
                                                  {
                                                    id: 'setup-submitted-review-1-seat-1',
                                                    task_id: taskId,
                                                    job_id: 'setup-submitted-review-1-seat-1',
                                                    stage: 'review',
                                                    state: 'queued',
                                                    claimable: true,
                                                    review_round: 1,
                                                    review_seat: 1,
                                                    queue_entered_at: createdAt,
                                                    queue_deadline: '2026-07-16T12:00:00Z',
                                                    redispatch_count: 0,
                                                    cost_usd: 0,
                                                    tokens_in: 0,
                                                    tokens_out: 0,
                                                    usage_reported: false,
                                                    self_reported: true,
                                                  },
                                                ],
                                              }
                                            : taskId === 'setup-claimed'
                                              ? {
                                                  jobs: [],
                                                  events: [],
                                                  work_orders: [
                                                    {
                                                      id: 'setup-claimed-implement-1',
                                                      task_id: taskId,
                                                      job_id: 'setup-claimed-implement-1',
                                                      stage: 'implement',
                                                      state: 'claimed',
                                                      claimable: false,
                                                      queue_entered_at: createdAt,
                                                      queue_deadline: '2026-07-16T12:00:00Z',
                                                      redispatch_count: 0,
                                                      cost_usd: 0,
                                                      tokens_in: 0,
                                                      tokens_out: 0,
                                                      usage_reported: false,
                                                      self_reported: true,
                                                    },
                                                  ],
                                                }
                                              : taskId === 'forge-failure'
                                                ? {
                                                    jobs: [],
                                                    events: [
                                                      {
                                                        id: 1,
                                                        task_id: taskId,
                                                        kind: 'github_issue.publication_failed',
                                                        actor_id: 'system',
                                                        actor_role: 'system' as const,
                                                        payload: {
                                                          forge_error_category: 'forge_request',
                                                          last_error: 'create task issue: request timed out',
                                                        },
                                                        at: '2026-07-15T12:00:00Z',
                                                      },
                                                      {
                                                        id: 2,
                                                        task_id: taskId,
                                                        job_id: 'review-1',
                                                        kind: 'review.publication_failed',
                                                        actor_id: 'system',
                                                        actor_role: 'system' as const,
                                                        payload: {
                                                          review_work_order_id: 'review-1',
                                                          forge_error_category: 'forge_permission',
                                                          last_error: 'publish review comment: resource not accessible',
                                                        },
                                                        at: '2026-07-15T12:01:00Z',
                                                      },
                                                      {
                                                        id: 3,
                                                        task_id: taskId,
                                                        kind: 'merge.failed',
                                                        actor_id: 'system',
                                                        actor_role: 'system' as const,
                                                        payload: {
                                                          reason_code: 'forge_merge_failed',
                                                          forge_error_category: 'forge_rate_limited',
                                                          error: 'merge pull request: API rate limit exceeded',
                                                        },
                                                        at: '2026-07-15T12:02:00Z',
                                                      },
                                                    ],
                                                    work_orders: [],
                                                  }
                                                : // A task actually holding at the merge gate: the pipeline
                                                  // has pushed a reviewed head to a pull request and the
                                                  // review panel has resolved, which is the record the gate
                                                  // card reads to say what is being approved (REQ-6).
                                                  taskId === 'merge-request-changes'
                                                  ? {
                                                      jobs: [],
                                                      events: [
                                                        {
                                                          id: 1,
                                                          task_id: taskId,
                                                          job_id: 'implement-1',
                                                          kind: 'pull_request.opened',
                                                          actor_id: 'system',
                                                          actor_role: 'system' as const,
                                                          payload: {
                                                            url: 'https://github.test/acme/conveyor/pull/482',
                                                            number: 482,
                                                            repository: 'acme/conveyor',
                                                            base_sha: 'b1a5e0000000000000000000000000000000feed',
                                                            head_sha: 'ca7ca7000000000000000000000000000000beef',
                                                          },
                                                          at: '2026-07-15T12:01:00Z',
                                                        },
                                                        {
                                                          id: 2,
                                                          task_id: taskId,
                                                          job_id: 'review-1',
                                                          kind: 'review.round_completed',
                                                          actor_id: 'system',
                                                          actor_role: 'system' as const,
                                                          payload: {
                                                            review_round: 1,
                                                            verdict: 'approve',
                                                            summary: 'Both reviewers accepted the pushed branch.',
                                                            reviews: [
                                                              { review_work_order_id: 'review-1-seat-1' },
                                                              { review_work_order_id: 'review-1-seat-2' },
                                                            ],
                                                          },
                                                          at: '2026-07-15T12:02:00Z',
                                                        },
                                                      ],
                                                      work_orders: [],
                                                    }
                                                  : {
                                                      jobs: [],
                                                      events: [],
                                                      work_orders: taskId === 'no-orders' ? null : [],
                                                    }
  const reviewDiagnostics =
    taskId === 'diagnostics'
      ? [
          {
            status: 'claimed_without_verdict',
            work_order_id: 'diagnostics-review-1-seat-1',
            review_round: 1,
            review_seat: 1,
            claimed_at: '2026-07-15T12:00:00Z',
            lease_expires_at: '2026-07-15T12:15:00Z',
            reason: 'review claim is active without a successful submit_review_verdict response',
          },
          {
            status: 'claimed_without_verdict',
            work_order_id: 'diagnostics-review-1-seat-2',
            review_round: 1,
            review_seat: 2,
            claimed_at: '2026-07-15T12:00:30Z',
            lease_expires_at: '2026-07-15T12:15:30Z',
            reason: 'review claim is active without a successful submit_review_verdict response',
          },
          {
            status: 'expired_without_verdict',
            work_order_id: 'diagnostics-review-0-seat-3',
            review_round: 0,
            review_seat: 3,
            claimed_at: '2026-07-15T12:01:00Z',
            lease_expires_at: '2026-07-15T12:02:00Z',
            reason: 'review claim lease expired without terminal verdict submission',
          },
        ]
      : []
  return {
    task: {
      id: taskId,
      workspace: 'demo',
      source: 'mcp',
      title: taskId === 'blueprint-parent' ? 'Phase 6 blueprint' : overflowing ? 'Overflowing task' : 'Short task',
      body:
        taskId === 'markdown-body'
          ? '# Structured context\n\nA **clear** paragraph with [safe link](https://example.test).\n\n- first item\n- [x] completed item\n\n`inline code`\n\n<script data-unsafe>window.hacked = true</script>'
          : taskId === 'long-body'
            ? Array.from({ length: 30 }, (_, index) => `Description paragraph ${index + 1}.`).join('\n\n')
            : overflowing
              ? Array.from({ length: 80 }, (_, index) => `Description line ${index + 1}`).join('\n')
              : 'A short description.',
      class: 'bug',
      level: 'L2',
      repo: 'conveyor',
      base_branch: 'main',
      branch: `conveyor/task-${taskId}`,
      state:
        taskId === 'gate' || taskId === 'evidence' || taskId === 'human-gate-worker-scope'
          ? 'awaiting_human'
          : taskId === 'design-proposal-queued'
            ? 'queued'
            : taskId === 'parked'
              ? 'parked'
              : taskId === 'blueprint-parent' ||
                  taskId === 'blueprint-child' ||
                  taskId === 'blocked-refresh' ||
                  taskId === 'blocked-suppressed' ||
                  taskId === 'spec-while-blocked' ||
                  taskId === 'unsatisfiable'
                ? 'queued'
                : taskId === 'merged-retained-review'
                  ? 'merged'
                  : taskId === 'merge-request-changes'
                    ? 'awaiting_human'
                    : taskId.startsWith('merge-')
                      ? 'approved'
                      : 'running',
      // Merge-gate fixtures hold approved branches for an operator merge.
      merge_approval: taskId.startsWith('merge-'),
      next_stage: taskId === 'parked' ? '' : 'implement',
      recovery_stage: taskId === 'parked' ? 'triage' : taskId === 'merge-request-changes' ? 'implement' : '',
      // The head the factory judged, durable on the task itself once review
      // resolves — what the gate card names as the thing being approved.
      reviewed_head_sha: taskId === 'merge-request-changes' ? 'ca7ca7000000000000000000000000000000beef' : undefined,
      policy_contract: {
        max_bounces: 3,
        stage_timeouts: { spec: '30m', implement: '2h', review: '45m' },
        review: { seats: [{}] },
        refresh_review: 'delta',
      },
      parent_task_id: taskId === 'blueprint-child' ? 'blueprint-parent' : undefined,
      origin_spec_version: taskId === 'blueprint-child' ? 1 : undefined,
      origin_sub_id: taskId === 'blueprint-child' ? 'SUB-3' : undefined,
      github:
        taskId === 'github-fallback'
          ? {
              task_id: taskId,
              repository: 'acme/conveyor',
              spec_version: 7,
              source: 'dashboard',
              state: 'retrying' as const,
              create_state: 'reconciling' as const,
              create_attempts: 1,
              reconcile_misses: 0,
              attempts: 1,
              created_at: createdAt,
              updated_at: createdAt,
            }
          : undefined,
      blocking_task_ids:
        taskId === 'blueprint-child'
          ? ['blueprint-sub-2']
          : taskId === 'blocked-refresh'
            ? ['refresh-dependency']
            : taskId === 'blocked-suppressed'
              ? ['blocked-dependency']
              : taskId === 'spec-while-blocked'
                ? ['refresh-dependency']
                : taskId === 'unsatisfiable'
                  ? ['closed-dependency', 'live-dependency']
                  : undefined,
      dependencies:
        taskId === 'blueprint-child'
          ? [
              { id: 'blueprint-sub-2', title: 'Runtime', state: 'running' },
              { id: 'blueprint-sub-1', title: 'Persistence', state: 'merged' },
            ]
          : taskId === 'blocked-refresh'
            ? [{ id: 'refresh-dependency', title: 'Backend contract', state: 'running' }]
            : taskId === 'blocked-suppressed'
              ? [{ id: 'blocked-dependency', title: 'Schema migration', state: 'running' }]
              : taskId === 'spec-while-blocked'
                ? [{ id: 'refresh-dependency', title: 'Backend contract', state: 'running' }]
                : taskId === 'unsatisfiable'
                  ? [
                      { id: 'closed-dependency', title: 'Retired API plan', state: 'closed' },
                      { id: 'live-dependency', title: 'Active schema work', state: 'running' },
                    ]
                  : undefined,
      children:
        taskId === 'blueprint-parent'
          ? [
              {
                id: 'blueprint-sub-1',
                title: 'Persistence',
                state: 'merged',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-1',
              },
              {
                id: 'blueprint-sub-2',
                title: 'Runtime',
                state: 'merged',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-2',
              },
              {
                id: 'blueprint-child',
                title: 'Dashboard',
                state: 'closed',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-3',
              },
            ]
          : undefined,
      created_at: createdAt,
      context: attachedContext[taskId],
    },
    jobs: reviewActivity.jobs,
    events:
      taskId === 'blueprint-parent'
        ? [
            {
              id: 1,
              task_id: taskId,
              kind: 'blueprint.materialized',
              actor_id: 'system',
              actor_role: 'system' as const,
              payload: { version: 1, children_total: 3 },
              at: createdAt,
            },
            {
              id: 2,
              task_id: taskId,
              kind: 'blueprint.closed',
              actor_id: 'system',
              actor_role: 'system' as const,
              payload: {},
              at: createdAt,
            },
          ]
        : taskId === 'unsatisfiable'
          ? [
              {
                id: 1,
                task_id: taskId,
                kind: 'task.dependency_unsatisfiable',
                actor_id: 'system',
                actor_role: 'system' as const,
                payload: { depends_on_task_id: 'closed-dependency', dependency_state: 'closed' },
                at: createdAt,
              },
            ]
          : reviewActivity.events,
    interventions:
      taskId === 'spec-approval'
        ? [
            {
              id: 1,
              task_id: taskId,
              actor_id: 'operator',
              actor_role: 'human' as const,
              action: 'approve' as const,
              reason_code: 'approved',
              comment: '',
              at: '2026-07-15T11:59:00Z',
            },
          ]
        : [],
    checkout_command: taskId.startsWith('work-on-this-') ? `conveyor run ${taskId}` : undefined,
    checkout_available: taskId.startsWith('work-on-this-'),
    checkout_guidance: taskId.startsWith('work-on-this-') ? '' : 'Use the assigned worktree.',
    needs_attention: taskId === 'forge-failure' || taskId === 'unsatisfiable',
    at_merge_gate: taskId === 'merge-request-changes',
    forge_failure:
      taskId === 'forge-failure'
        ? {
            category: 'forge_rate_limited',
            detail: 'merge pull request: API rate limit exceeded',
            surface: 'GitHub merge',
            at: '2026-07-15T12:02:00Z',
          }
        : undefined,
    spec: {
      task_id: taskId,
      version: 1,
      content: specContent,
      acceptance_count: 0,
      acceptance: [],
      decomposition:
        taskId === 'blueprint-parent'
          ? [
              { id: 'SUB-1', repo: 'conveyor', summary: 'Persistence', depends_on: [] },
              { id: 'SUB-2', repo: 'conveyor', summary: 'Runtime', depends_on: ['SUB-1'] },
              { id: 'SUB-3', repo: 'conveyor', summary: 'Dashboard', depends_on: ['SUB-2'] },
            ]
          : [],
      materialized_children:
        taskId === 'blueprint-parent'
          ? [
              {
                id: 'blueprint-sub-1',
                title: 'Persistence',
                state: 'merged',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-1',
              },
              {
                id: 'blueprint-sub-2',
                title: 'Runtime',
                state: 'merged',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-2',
              },
              {
                id: 'blueprint-child',
                title: 'Dashboard',
                state: 'closed',
                origin_spec_version: 1,
                origin_sub_id: 'SUB-3',
              },
            ]
          : undefined,
      approved: true,
      created_at: createdAt,
      approved_at: createdAt,
    },
    work_orders:
      taskId === 'unsatisfiable'
        ? [
            {
              id: 'unsatisfiable-implement-1',
              task_id: taskId,
              job_id: 'unsatisfiable-implement-1',
              stage: 'implement',
              state: 'queued',
              claimable: false,
              blocking_task_ids: ['closed-dependency', 'live-dependency'],
              unsatisfiable_task_ids: ['closed-dependency'],
              queue_entered_at: createdAt,
              queue_deadline: '2026-07-16T12:00:00Z',
              redispatch_count: 0,
              cost_usd: 0,
              tokens_in: 0,
              tokens_out: 0,
              usage_reported: false,
              self_reported: true,
            },
          ]
        : reviewActivity.work_orders,
    verification_evidence:
      taskId === 'evidence'
        ? [
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
          ]
        : [],
    lineage_graph:
      taskId === 'full-header-id'
        ? {
            roots: [{ type: 'task', id: taskId, label: 'Short task' }],
            nodes: [
              { type: 'task', id: taskId, label: 'Short task' },
              { type: 'work_order', id: `${taskId}-implement-1`, label: 'implement work order for full-header-id' },
            ],
            links: [
              {
                workspace: 'demo',
                src_type: 'task',
                src_id: taskId,
                dst_type: 'work_order',
                dst_id: `${taskId}-implement-1`,
                kind: 'dispatches',
                created_by_event_id: 1,
                created_at: createdAt,
              },
            ],
            truncated: false,
            omitted_nodes: 0,
            omitted_links: 0,
            budget: { max_depth: 3, max_nodes: 32, max_links: 128 },
          }
        : undefined,
    review_diagnostics: reviewDiagnostics,
    review_recovery:
      taskId === 'review-retry'
        ? {
            needed: true,
            prior_round: 1,
            reason: 'latest review round is terminal after a reviewer timed out',
            timed_out_orders: reviewActivity.work_orders?.filter((order) => order.state === 'timed_out') ?? [],
            inconsistent_orders: [],
          }
        : taskId === 'review-wedge'
          ? {
              needed: true,
              prior_round: 2,
              reason:
                'latest review round is non-progressing because a completed seat has a contradictory failed child outcome',
              timed_out_orders: [],
              inconsistent_orders: [
                {
                  id: 'review-wedge-review-2-seat-1',
                  task_id: taskId,
                  job_id: 'review-wedge-review-2-seat-1',
                  stage: 'review',
                  state: 'completed',
                  claimable: false,
                  review_round: 2,
                  review_seat: 1,
                  last_attempt_outcome: 'child_failure',
                  retry_suppressed: true,
                  queue_entered_at: createdAt,
                  queue_deadline: createdAt,
                  redispatch_count: 0,
                  cost_usd: 0,
                  tokens_in: 0,
                  tokens_out: 0,
                  usage_reported: false,
                  self_reported: true,
                },
              ],
            }
          : undefined,
    interrupted_review_recovery:
      taskId === 'interrupted-review'
        ? {
            needed: true,
            review_round: 1,
            reason: 'latest review round has interrupted seats whose claims are no longer authorized',
            eligible_orders: reviewActivity.work_orders?.filter((order) => order.state === 'queued') ?? [],
            retained_orders: reviewActivity.work_orders?.filter((order) => order.state === 'completed') ?? [],
          }
        : taskId === 'interrupted-review-empty'
          ? {
              needed: true,
              review_round: 1,
              reason: 'latest review round has interrupted seats whose claims are no longer authorized',
              eligible_orders: [
                { id: 'interrupted-review-empty-review-1-seat-1', review_seat: 1, last_attempt_outcome: 'expired' },
              ],
              retained_orders: null,
            }
          : undefined,
    stalled:
      taskId === 'recovery'
        ? {
            needed: true,
            reason: 'automatic retry is suppressed',
            work_order: (reviewActivity.work_orders ?? [])[0],
            last_failure: 'harness exited: status 1',
          }
        : taskId === 'unsatisfiable'
          ? {
              needed: true,
              reason: 'dependency reached a terminal state without merging',
              work_order: { id: 'unsatisfiable-implement-1', stage: 'implement', state: 'queued' },
              blocking_task_ids: ['closed-dependency'],
              unsatisfiable_edge: true,
            }
          : undefined,
    merge_readiness:
      taskId === 'merge-unknown'
        ? { state: 'UNKNOWN', head_sha: 'head-1' }
        : taskId === 'merge-conflict'
          ? { state: 'CONFLICTING', head_sha: 'head-1', number: 12 }
          : taskId === 'merge-delayed' || taskId === 'merge-failure'
            ? { state: 'MERGEABLE', head_sha: 'head-1', number: 12 }
            : undefined,
  }
}

// The blueprint projection: anchors left the activity feed, so
// this is where the anchor's delivery, ordered children, and title now come
// from — including the title a child's parent reference renders.
function blueprintProjection() {
  const parent = activity('blueprint-parent', false)
  return [
    {
      task: parent.task,
      spec: parent.spec,
      governing_version: 1,
      children: [
        {
          id: 'blueprint-sub-1',
          title: 'Persistence',
          state: 'merged',
          origin_spec_version: 1,
          origin_sub_id: 'SUB-1',
          repo: 'conveyor',
          summary: 'Persistence',
          depends_on: [],
        },
        {
          id: 'blueprint-sub-2',
          title: 'Runtime',
          state: 'merged',
          origin_spec_version: 1,
          origin_sub_id: 'SUB-2',
          repo: 'conveyor',
          summary: 'Runtime',
          depends_on: ['SUB-1'],
        },
        {
          id: 'blueprint-child',
          title: 'Dashboard',
          state: 'closed',
          origin_spec_version: 1,
          origin_sub_id: 'SUB-3',
          repo: 'conveyor',
          summary: 'Dashboard',
          depends_on: ['SUB-2'],
        },
      ],
      delivery: { state: 'completed', total: 3, merged: 2, closed: 1, open: 0 },
      serves: [],
      events: parent.events,
      artifacts: [],
    },
  ]
}

async function mockTaskAPIs(page: Page) {
  await serveNextActionsFromActivity(page)
  detailRequestCounts.clear()
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') {
      await route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
      return
    }
    if (url.pathname === '/v1/me') {
      await route.fulfill({ json: callerIdentity({ id: 'usr_operator', role: 'operator' }, route.request()) })
      return
    }
    const taskMatch = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/activity$/)
    if (taskMatch) {
      const taskId = decodeURIComponent(taskMatch[1])
      const requestCount = (detailRequestCounts.get(taskId) ?? 0) + 1
      detailRequestCounts.set(taskId, requestCount)
      const item = activity(taskId, taskId === 'overflowing' || taskId === 'gate')
      if (taskId === 'blocked-refresh' && requestCount > 1) {
        item.task.blocking_task_ids = undefined
        item.task.dependencies = [{ id: 'refresh-dependency', title: 'Backend contract', state: 'merged' }]
        item.work_orders =
          item.work_orders?.map((order) => ({ ...order, blocking_task_ids: undefined })) ?? item.work_orders
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
      await route.fulfill({
        json: [
          {
            task: item.task,
            latest_stage: 'review',
            last_event_at: createdAt,
            needs_attention: false,
            review_diagnostics: item.review_diagnostics,
          },
          { task: parent.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: false },
        ],
      })
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
test('task detail headers show the task name while routes and API lookup keep using the task ID', async ({ page }) => {
  const fullTaskID = 'full-header-id'
  const fullActivity = page.waitForRequest(
    (request) => new URL(request.url()).pathname === `/v1/tasks/${fullTaskID}/activity`,
  )
  await page.goto(`/w/demo/tasks/${fullTaskID}/full`)
  await fullActivity
  // The full route's header is a breadcrumb naming the task ID; the title
  // reads once, in the reading column.
  const fullHeader = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to Tasks' }) })
  await expect(fullHeader.getByRole('navigation', { name: 'Breadcrumb' })).toHaveText(`Tasks/${fullTaskID}`)
  await expect(fullHeader).not.toContainText('Short task')
  await expect(page.getByRole('heading', { name: 'Short task', level: 1 })).toHaveCount(1)
  // Relationships live in the explorer panel now, not in a card on the page;
  // the detail surfaces carry the affordance only.
  await expect(page.getByRole('button', { name: 'Knowledge explorer' })).toBeVisible()
  await expect(page.getByText('Trace planning to delivery evidence')).toHaveCount(0)
  expect(new URL(page.url()).pathname).toBe(`/w/demo/tasks/${fullTaskID}/full`)

  const sheetTaskID = 'sheet-header-id'
  const sheetActivity = page.waitForRequest(
    (request) => new URL(request.url()).pathname === `/v1/tasks/${sheetTaskID}/activity`,
  )
  await page.goto(`/w/demo/tasks/${sheetTaskID}`)
  await sheetActivity
  const sheetHeader = page.locator('header').filter({ has: page.getByRole('button', { name: 'Close panel' }) })
  await expect(sheetHeader).toContainText('Short task')
  await expect(sheetHeader).not.toContainText(sheetTaskID)
  expect(new URL(page.url()).pathname).toBe(`/w/demo/tasks/${sheetTaskID}`)
})

test('recovery authorization refusal renders the API message instead of its JSON envelope', async ({ page }) => {
  await page.route('**/v1/work-orders/*/recover*', (route) =>
    route.fulfill({
      status: 404,
      json: {
        error: 'workspace_not_found',
        message: 'This workspace is unavailable or you cannot recover work in it.',
      },
    }),
  )

  await page.goto('/w/demo/tasks/recovery/full')
  const action = page.getByRole('button', { name: 'Recover work order' })
  await expect(action).toBeEnabled()
  await action.click()
  await expect(page.getByText(/This workspace is unavailable or you cannot recover work in it\./)).toBeVisible()
  await expect(page.getByText(/\{"error":"workspace_not_found"/)).toHaveCount(0)
})

test('checkpoint recovery requires and submits operator direction', async ({ page }) => {
  let recoveryRequest = ''
  await page.route('**/v1/work-orders/*/recover*', async (route) => {
    recoveryRequest = route.request().postData() ?? ''
    await route.fulfill({ json: { id: 'operator-checkpoint-implement-1', state: 'queued', claimable: true } })
  })
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  // A checkpoint without a decision request keeps the released wording
  // (req-260820-394cac AC-1.3).
  await expect(executionTitle(page)).toHaveText('Implementation paused — recovery needed')
  await expect(page.getByText(/A decision is required before this work can continue/)).toBeVisible()
  await expect(page.getByText(/Recovery without direction will repeat the checkpoint/)).toBeVisible()
  const checkpointMessage = page.getByText('Agent checkpoint message').locator('..')
  await expect(checkpointMessage).toBeVisible()
  await expect(checkpointMessage.getByText(checkpointProgress, { exact: true })).toBeVisible()
  await expect(page.getByText('Attached context: None')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Attach context' })).toBeVisible()
  const action = page.getByRole('button', { name: 'Recover work order' })
  await expect(action).toBeDisabled()
  await page.getByLabel('Operator direction').fill('Proceed with the accepted amendment.')
  await expect(action).toBeEnabled()
  await action.click()
  await expect.poll(() => recoveryRequest).not.toBe('')
  await expect.poll(() => JSON.parse(recoveryRequest).direction).toBe('Proceed with the accepted amendment.')
})

// A released checkpoint order's report reads in its recovery card. The
// timeline adds no separate agent activity note for it, and its lifecycle
// note stays (component-web-task-surfaces invariant 72; req-operator-next-action
// AC-7.3).
test('task timeline adds no agent activity note and the checkpoint card keeps the report', async ({ page }) => {
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const checkpointMessage = page.getByText('Agent checkpoint message').locator('..')
  await expect(checkpointMessage.getByText(checkpointProgress, { exact: true })).toBeVisible()
  const timeline = page.getByLabel('Execution event timeline')
  await expect(timeline.locator('li').first()).toBeVisible()
  await expect(timeline.getByText(/Agent activity/)).toHaveCount(0)
  await expect(timeline.getByText('Show full progress report')).toHaveCount(0)
  await expect(page.getByText(checkpointProgress, { exact: true })).toHaveCount(1)
})

// Each live progress report prints inside its own card, once: a claimed risk
// order, a claimed order without a started job, a running Implementation with
// its own summary, a running Verify job, and a deliberating review seat. No
// card prints the claimant, agent, and model line, and no timeline note
// repeats a report (component-web-task-surfaces invariants 72 and 73;
// DEC-138(5); req-risk-based-review AC-5.6; req-operator-next-action AC-7.3).
const liveTask = 'live-progress'
const liveAt = (minute: number) => `2026-07-15T12:${String(minute).padStart(2, '0')}:00Z`
const liveReports = {
  risk: 'I read the plan and the matched path rules.\n\nNext, I will rate the plan version.',
  plan: 'I am drafting the plan from the attached designs.',
  implement: 'Round 1 fixes are pushed. Next, I will wait for exact-head CI.',
  verify: 'The verifier is running the selected kits against the head.',
  review: 'I am reading the diff against the governing design.',
  stale: 'An old report from an order that lost its lease.',
  rated: 'A report from the risk agent before it rated the head.',
}
const liveRatedReason = 'Only the dashboard copy changes.'
const liveHead = 'c'.repeat(40)
const liveBase = 'b'.repeat(40)

function liveJob(id: string, stage: string, state: string, startedMinute?: number) {
  return {
    id,
    task_id: liveTask,
    stage,
    harness: 'external-mcp',
    model_tier: 'operator-owned',
    runner: 'external',
    confinement: 'none',
    tokens_in: 0,
    tokens_out: 0,
    state,
    started_at: startedMinute === undefined ? undefined : liveAt(startedMinute),
    ended_at: state === 'done' ? liveAt((startedMinute ?? 0) + 1) : undefined,
  }
}

function liveOrder(id: string, stage: string, state: string, extra: Partial<WorkOrder> = {}): WorkOrder {
  return {
    id,
    task_id: liveTask,
    job_id: id,
    stage,
    state,
    claimable: false,
    queue_entered_at: liveAt(1),
    updated_at: liveAt(2),
    redispatch_count: 0,
    tokens_in: 0,
    tokens_out: 0,
    usage_reported: false,
    self_reported: true,
    claimed_by: 'run:usr_operator',
    agent: 'claude-code',
    model: 'claude-opus-5-5',
    last_agent_activity_at: liveAt(5),
    last_agent_activity_label: 'Progress: a short report',
    ...extra,
  } as WorkOrder
}

function liveProgressItem(options: { riskProgress?: string } = {}): ActivityItem {
  const item = activity(liveTask, false) as unknown as CompleteActivity
  item.task = { ...item.task, state: 'running' as ActivityItem['task']['state'] }
  item.events = []
  item.jobs = [
    liveJob(`${liveTask}-risk-1`, 'risk', 'running', 3),
    liveJob(`${liveTask}-risk-3`, 'risk', 'done', 3),
    liveJob(`${liveTask}-implement-1`, 'implement', 'running', 4),
    liveJob(`${liveTask}-review-1-seat-1`, 'review', 'running', 6),
  ] as unknown as ActivityItem['jobs']
  item.work_orders = [
    liveOrder(`${liveTask}-risk-1`, 'risk', 'claimed', {
      progress: options.riskProgress ?? liveReports.risk,
      execution_started_at: liveAt(3),
      risk_subject: { plan_version: 2 },
    }),
    liveOrder(`${liveTask}-risk-2`, 'risk', 'claimed', {
      execution_started_at: liveAt(3),
      risk_subject: { head_sha: liveHead, base_sha: liveBase },
    }),
    liveOrder(`${liveTask}-risk-3`, 'risk', 'completed', {
      progress: liveReports.rated,
      risk_subject: { head_sha: liveHead, base_sha: liveBase },
    }),
    liveOrder(`${liveTask}-risk-4`, 'risk', 'stale', {
      progress: liveReports.stale,
      risk_subject: { plan_version: 1 },
    }),
    liveOrder(`${liveTask}-risk-5`, 'risk', 'timed_out', {
      progress: liveReports.stale,
      execution_deadline: liveAt(9),
      risk_subject: { plan_version: 1 },
    }),
    liveOrder(`${liveTask}-spec-1`, 'spec', 'claimed', {
      progress: `conveyor run mode: self-claimed\n${liveReports.plan}`,
    }),
    liveOrder(`${liveTask}-implement-1`, 'implement', 'claimed', { progress: liveReports.implement }),
    liveOrder(`${liveTask}-review-1-seat-1`, 'review', 'claimed', {
      review_round: 1,
      review_seat: 1,
      progress: liveReports.review,
    }),
  ]
  item.risk = {
    mode: 'record',
    tier: 'low',
    source: 'risk_agent',
    reason: liveRatedReason,
    required_agent_reviews: 1,
    required_human_approvals: 0,
    calculated_agent_reviews: 1,
    calculated_human_approvals: 0,
    recorded_approvals: 0,
    author_approval_needed: false,
    ratings: [
      {
        seq: 1,
        key: `risk_agent:${liveTask}-risk-3`,
        source: 'risk_agent',
        tier: 'low',
        head_sha: liveHead,
        base_sha: liveBase,
        reason: liveRatedReason,
        matched_rules: [],
        risk_work_order_id: `${liveTask}-risk-3`,
        risk_model: 'claude-sonnet-5-5',
        created_at: liveAt(4),
      } as NonNullable<ActivityItem['risk']>['ratings'][number],
    ],
  }
  const bounded = boundedView(item, liveTask)
  bounded.summaries = {
    ...bounded.summaries,
    job_results: {
      [`${liveTask}-implement-1`]: { summary: { text: 'The harness narrated the attempt.' } },
    } as TaskSummaries['job_results'],
  }
  return bounded
}

// The timeline builder adds no separate agent activity note for any order,
// whatever its stage, state, or last activity label. Each live report stays
// in its own card, and lifecycle notes from events stay.
function liveTimeline(extra: Partial<ActivityItem>): TimelineEntry[] {
  const item = activity(liveTask, false) as unknown as ActivityItem
  return buildTimeline({
    ...item,
    jobs: [],
    events: [],
    work_orders: [],
    interventions: [],
    risk: undefined,
    summaries: {},
    ...extra,
  } as ActivityItem)
}

const agentActivityNotes = (entries: TimelineEntry[]) =>
  entries.filter(
    (entry) => entry.type === 'note' && (entry.title.startsWith('Agent activity') || entry.key.startsWith('agent-')),
  )

test('no order adds a separate agent activity note in any stage or state', () => {
  const labels = ['Progress: wiring the timeline', 'Branch pushed', 'Work order claimed', 'Claim lease renewed']
  const states = ['queued', 'claimed', 'stale', 'timed_out', 'submitted', 'completed', 'cancelled']
  for (const stage of ['spec', 'implement', 'verify', 'review', 'risk']) {
    for (const state of states) {
      for (const label of labels) {
        for (const withJob of [false, true]) {
          const id = `${liveTask}-${stage}-1`
          const entries = liveTimeline({
            jobs: withJob
              ? ([liveJob(id, stage, state === 'claimed' ? 'running' : 'done', 3)] as unknown as ActivityItem['jobs'])
              : [],
            work_orders: [
              liveOrder(id, stage, state, {
                review_round: stage === 'review' ? 1 : undefined,
                review_seat: stage === 'review' ? 1 : undefined,
                last_agent_activity_label: label,
                progress: liveReports.implement,
              }),
            ],
          })
          expect(agentActivityNotes(entries), `${stage} ${state} ${label} ${withJob}`).toEqual([])
        }
      }
    }
  }
  // An event note still renders beside a live order.
  const parked = liveTimeline({
    work_orders: [liveOrder(`${liveTask}-implement-1`, 'implement', 'claimed')],
    events: [
      {
        id: 1,
        task_id: liveTask,
        kind: 'task.state_changed',
        actor_id: 'system',
        actor_role: 'system',
        payload: { to: 'parked', command: 'triage.park' },
        at: liveAt(8),
      },
    ] as unknown as ActivityItem['events'],
  })
  expect(parked.filter((entry) => entry.type === 'note').map((entry) => 'title' in entry && entry.title)).toEqual([
    'Parked',
  ])
})

test('the timeline builder keeps each live report in its own card, once', () => {
  const id = `${liveTask}-implement-1`
  const narration = { job_results: { [id]: { summary: { text: 'The harness narrated the attempt.' } } } }
  const jobEntry = (entries: TimelineEntry[]) => entries.find((entry) => entry.type === 'job')
  const orderEntryOf = (entries: TimelineEntry[]) => entries.find((entry) => entry.type === 'order')
  const running = [liveJob(id, 'implement', 'running', 3)] as unknown as ActivityItem['jobs']
  const report = liveOrder(id, 'implement', 'claimed', { progress: liveReports.implement })
  // A generic summary is replaced by the report, which then is not repeated.
  const generic = jobEntry(liveTimeline({ jobs: running, work_orders: [report] }))
  expect(generic).toMatchObject({ summary: liveReports.implement })
  expect(generic).not.toHaveProperty('progress', expect.anything())
  // A distinct summary keeps its text, and the report prints beside it.
  expect(jobEntry(liveTimeline({ jobs: running, work_orders: [report], summaries: narration }))).toMatchObject({
    summary: 'The harness narrated the attempt.',
    progress: liveReports.implement,
  })
  // A Verify card shows no summary, so it always takes the report.
  const verifyId = `${liveTask}-verify-1`
  expect(
    jobEntry(
      liveTimeline({
        jobs: [liveJob(verifyId, 'verify', 'running', 3)] as unknown as ActivityItem['jobs'],
        work_orders: [liveOrder(verifyId, 'verify', 'claimed', { progress: liveReports.verify })],
      }),
    ),
  ).toMatchObject({ progress: liveReports.verify })
  // Only a running job under a claimed order takes the report.
  for (const [orderState, jobState] of [
    ['submitted', 'running'],
    ['claimed', 'done'],
  ] as const) {
    const entry = jobEntry(
      liveTimeline({
        jobs: [liveJob(id, 'implement', jobState, 3)] as unknown as ActivityItem['jobs'],
        work_orders: [{ ...report, state: orderState }],
        summaries: narration,
      }),
    )
    expect(entry, `${orderState} ${jobState}`).not.toHaveProperty('progress', expect.anything())
  }
  // A claimed order without a started job, or with a pending one, shows the
  // report as its detail. A queued order adds no entry.
  expect(orderEntryOf(liveTimeline({ work_orders: [report] }))).toMatchObject({ detail: liveReports.implement })
  const pending = [liveJob(id, 'implement', 'pending')] as unknown as ActivityItem['jobs']
  expect(orderEntryOf(liveTimeline({ jobs: pending, work_orders: [report] }))).toMatchObject({
    detail: liveReports.implement,
  })
  expect(liveTimeline({ work_orders: [{ ...report, state: 'queued' }] })).toEqual([])
  // A blank or run-mode-only report is no report, and run-mode lines drop out.
  for (const progress of ['conveyor run mode: self-claimed', '  \n ', undefined]) {
    const order = { ...report, progress }
    expect(orderEntryOf(liveTimeline({ work_orders: [order] })), String(progress)).not.toHaveProperty(
      'detail',
      expect.anything(),
    )
    expect(
      jobEntry(liveTimeline({ jobs: running, work_orders: [order], summaries: narration })),
      String(progress),
    ).not.toHaveProperty('progress', expect.anything())
  }
  expect(
    orderEntryOf(
      liveTimeline({ work_orders: [{ ...report, progress: `conveyor run mode: self-claimed\n${liveReports.plan}` }] }),
    ),
  ).toMatchObject({ detail: liveReports.plan })
  // A panel absorbs a seat's order in every status, so no other entry shows
  // its report.
  const seatId = `${liveTask}-review-1-seat-1`
  for (const [state, jobState] of [
    ['claimed', 'running'],
    ['queued', 'running'],
    ['stale', 'running'],
    ['timed_out', 'running'],
    ['claimed', 'failed'],
  ] as const) {
    const entries = liveTimeline({
      jobs: [liveJob(seatId, 'review', jobState, 3)] as unknown as ActivityItem['jobs'],
      work_orders: [
        liveOrder(seatId, 'review', state, { review_round: 1, review_seat: 1, progress: liveReports.review }),
      ],
    })
    expect(
      entries.map((entry) => entry.type),
      `${state} ${jobState}`,
    ).toEqual(['panel'])
  }
})

const liveRiskCard = (page: Page, n: number) =>
  page.locator(`li[data-order-stage="risk"][data-order-id="${liveTask}-risk-${n}"] article[data-risk-order-card]`)

test('live progress reads once inside each card and no card prints the claimant line', async ({ page }) => {
  await page.route(`**/v1/tasks/${liveTask}/activity*`, (route) => route.fulfill({ json: liveProgressItem() }))
  await page.goto(`/w/demo/tasks/${liveTask}/full`)
  const timeline = page.getByLabel('Execution event timeline')

  // A claimed unrated risk order's report is its body, prose like a job's.
  const claimed = liveRiskCard(page, 1)
  const paragraphs = claimed.locator('[data-risk-order-body="progress"] p')
  await expect(paragraphs).toHaveText(liveReports.risk.split('\n\n'))
  await expect(claimed.locator('header')).toContainText('In progress')
  await expect(claimed.locator('footer[data-job-footer] [data-job-model]')).toContainText('claude-opus-5-5')
  // Without a report, the claimed card has no body and the header keeps its
  // bottom padding.
  const empty = liveRiskCard(page, 2)
  await expect(empty.locator('header')).toContainText('In progress')
  await expect(empty.locator('[data-risk-order-body]')).toHaveCount(0)
  await expect(empty.locator('header')).toHaveClass(/pb-3/)
  await expect(empty.locator('header [data-risk-order-time]')).toBeVisible()
  // A rated order keeps its reason, and an expired order keeps its state
  // detail, even when the order reported progress.
  await expect(liveRiskCard(page, 3).locator('[data-risk-order-body="reason"]')).toHaveText(liveRatedReason)
  await expect(liveRiskCard(page, 4).locator('[data-risk-order-body="detail"]')).toContainText(
    'No agent claimed it before queue retention elapsed.',
  )
  await expect(liveRiskCard(page, 5).locator('[data-risk-order-body="detail"]')).toContainText(
    'The execution deadline elapsed before the agent finished',
  )
  await expect(timeline.getByText(liveReports.rated)).toHaveCount(0)
  await expect(timeline.getByText(liveReports.stale)).toHaveCount(0)

  // A claimed order without a started job shows its filtered report.
  const plan = timeline.locator(`li[data-order-id="${liveTask}-spec-1"]`)
  await expect(plan.locator('[data-order-detail]')).toHaveText(liveReports.plan)
  await expect(timeline.getByText(/conveyor run mode/i)).toHaveCount(0)

  // A running Implementation keeps its own summary and shows the report
  // beside it, inside the same card.
  const implementation = timeline.locator('article[data-implementation-entry]')
  await expect(implementation).toContainText('The harness narrated the attempt.')
  await expect(implementation.locator('[data-job-progress]')).toContainText(liveReports.implement)

  // A deliberating seat keeps its report in its row.
  await expect(timeline.locator('[data-seat-progress]')).toHaveText(liveReports.review)

  // Each report reads once, and no note or card prints the claimant line.
  for (const report of [liveReports.plan, liveReports.implement, liveReports.review])
    await expect(timeline.getByText(report, { exact: true })).toHaveCount(1)
  await expect(timeline.getByText(/run:usr_operator/)).toHaveCount(0)
  await expect(timeline.getByText(/claude-code · claude-opus-5-5/)).toHaveCount(0)
  await expect(timeline.getByText(/Agent activity/)).toHaveCount(0)
})

test('a claimed risk card with a blank or run-mode-only report has no body', async ({ page }) => {
  for (const riskProgress of ['  \n ', 'conveyor run mode: self-claimed']) {
    await page.route(`**/v1/tasks/${liveTask}/activity*`, (route) =>
      route.fulfill({ json: liveProgressItem({ riskProgress }) }),
    )
    await page.goto(`/w/demo/tasks/${liveTask}/full`)
    const claimed = liveRiskCard(page, 1)
    await expect(claimed.locator('header')).toContainText('In progress')
    await expect(claimed.locator('[data-risk-order-body]')).toHaveCount(0)
    await expect(claimed.locator('header')).toHaveClass(/pb-3/)
    await page.unroute(`**/v1/tasks/${liveTask}/activity*`)
  }
})

test('a running Verify card shows its live report beside its evidence', async ({ page }) => {
  const verifyId = `${liveTask}-verify-1`
  const item = liveProgressItem()
  item.jobs = [liveJob(verifyId, 'verify', 'running', 7)] as unknown as ActivityItem['jobs']
  item.work_orders = [liveOrder(verifyId, 'verify', 'claimed', { progress: liveReports.verify })]
  item.risk = undefined
  item.summaries = { ...item.summaries, job_results: {} }
  await page.route(`**/v1/tasks/${liveTask}/activity*`, (route) => route.fulfill({ json: item }))
  await page.route(`**/v1/tasks/${liveTask}/verification*`, (route) =>
    route.fulfill({
      json: { head_sha: '', current_context_id: '', contexts: { items: [], next_cursor: '' }, overview: {} },
    }),
  )
  await page.goto(`/w/demo/tasks/${liveTask}/full`)
  const card = page.getByRole('article', { name: 'Verification' })
  await expect(card.locator('[data-job-progress]')).toContainText(liveReports.verify)
  const timeline = page.getByLabel('Execution event timeline')
  await expect(timeline.getByText(liveReports.verify, { exact: true })).toHaveCount(1)
  await expect(timeline.getByText(/Agent activity/)).toHaveCount(0)
})

// Visual record for the pull request: a claimed risk card with a progress
// report and one without, a claimed order without a job, and a running
// Implementation with its live report, at 375 px and a desktop width. The task scratch
// directory named by CONVEYOR_SCREENSHOT_DIR keeps a local copy, and the
// Playwright report carries each attachment.
for (const width of [375, 1280]) {
  test(`claimed risk card screenshots at ${width} px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 1000 })
    await page.route(`**/v1/tasks/${liveTask}/activity*`, (route) => route.fulfill({ json: liveProgressItem() }))
    await page.goto(`/w/demo/tasks/${liveTask}/full`)
    await expect(liveRiskCard(page, 1).locator('[data-risk-order-body="progress"]')).toBeVisible()
    await page.evaluate(() => document.fonts.ready)
    if (width === 375) expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375)
    const shots = process.env.CONVEYOR_SCREENSHOT_DIR
    for (const [selector, name] of [
      [`li[data-order-id="${liveTask}-risk-1"]`, 'risk-card-claimed-progress'],
      [`li[data-order-id="${liveTask}-risk-2"]`, 'risk-card-claimed-empty'],
      [`li[data-order-id="${liveTask}-spec-1"]`, 'jobless-order-progress'],
      ['li:has(> article[data-implementation-entry])', 'implementation-live-progress'],
    ] as const) {
      const item = page.locator(selector)
      const size = await item.evaluate((element) => [element.scrollWidth, element.clientWidth])
      expect(size[0]).toBeLessThanOrEqual(size[1])
      const file = `${name}-${width}.png`
      const image = await item.screenshot({ path: shots ? `${shots}/${file}` : undefined })
      await testInfo.attach(file, { body: image, contentType: 'image/png' })
    }
  })
}

// The checkpoint shape of task 261015-3721c6: seven citations across four
// System Design documents, each with one pending task-authored proposal
// (component-web-task-surfaces, Implementation checkpoints).
const sevenCitationDocuments = [
  {
    id: 'component-http-api',
    title: 'HTTP API',
    cited: 43,
    proposal: 44,
    sections: ['Bounded detail view', 'Task operations'],
  },
  {
    id: 'component-web-task-surfaces',
    title: 'Web task surfaces',
    cited: 7,
    proposal: 8,
    sections: ['Task detail', 'History pages', 'Audit disclosures'],
  },
  { id: 'component-web-dashboard', title: 'Web dashboard', cited: 25, proposal: 26, sections: ['Route tree'] },
  { id: 'component-mcp-investigation-reads', title: 'MCP investigation reads', cited: 3, proposal: 4, sections: [''] },
]
const sevenCitationDecision =
  'Confirm or dismiss component-http-api v44, component-web-task-surfaces v8, component-web-dashboard v26, and component-mcp-investigation-reads v4, then recover this order.'

function sevenCitationCheckpoint(confirmed: Set<string>) {
  return {
    decision_request: sevenCitationDecision,
    class: 'authority_conflict',
    citations: sevenCitationDocuments.flatMap((document) =>
      document.sections.map((section) => ({
        document_id: document.id,
        document_kind: 'system_design' as const,
        document_title: document.title,
        cited_version: document.cited,
        ...(section ? { statement_or_section_id: section } : {}),
        current_confirmed_version: confirmed.has(document.id) ? document.proposal : document.cited,
        newer_confirmed: confirmed.has(document.id),
        pending_proposals: confirmed.has(document.id)
          ? []
          : [
              {
                id: document.id,
                title: document.title,
                tier: 'system_design' as const,
                version: document.proposal,
                origin_type: 'task' as const,
                origin_id: 'operator-checkpoint',
                proposed_at: createdAt,
              },
            ],
      })),
    ),
  }
}

async function routeSevenCitationCheckpoint(page: Page, confirmed: Set<string>) {
  const confirms: Array<{ path: string; ifMatch?: string }> = []
  const recoveries: Array<Record<string, unknown>> = []
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    const item = activity('operator-checkpoint', false)
    const order = item.work_orders?.[0]
    if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
    order.checkpoint = sevenCitationCheckpoint(confirmed)
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/system-designs/*/versions/*/confirm**', async (route) => {
    const path = new URL(route.request().url()).pathname
    confirms.push({ path, ifMatch: route.request().headers()['if-match'] })
    const id = path.split('/')[3]
    confirmed.add(id)
    await route.fulfill({ json: {} })
  })
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', async (route) => {
    recoveries.push(route.request().postDataJSON() as Record<string, unknown>)
    await route.fulfill({ json: { id: 'attempt-recovery-implement-1', state: 'queued', claimable: true } })
  })
  return { confirms, recoveries }
}

test('an authority-conflict checkpoint states the decision request and groups seven citations into four documents', async ({
  page,
}) => {
  const confirmed = new Set<string>()
  const { confirms, recoveries } = await routeSevenCitationCheckpoint(page, confirmed)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')

  // The current state is the server's label and the agent's decision request;
  // the server's question stays off the dashboard (req-operator-next-action
  // AC-1.6, AC-1.9). Nothing reads as a failure that needs recovery.
  const banner = nextActionBox(page)
  await expect(nextActionBox(page).getByText('Cited revisions need a decision', { exact: true })).toBeVisible()
  await expect(banner.getByText(sevenCitationDecision, { exact: true })).toBeVisible()
  await expectNoNextActionOmissions(banner, {
    sentences: ['Will you confirm or dismiss the cited revisions, then recover the order with direction?'],
  })
  await expect(page.getByText(/recovery needed/i)).toHaveCount(0)
  await expect(page.getByText(/try again/i)).toHaveCount(0)

  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByText(sevenCitationDecision, { exact: true })).toBeVisible()
  await expect(card.getByRole('button', { name: 'Confirm 4 revisions & resume' })).toBeVisible()
  const review = card.getByRole('button', { name: 'Review' })
  await expect(review).toHaveAttribute('aria-expanded', 'false')
  await review.click()
  await expect(review).toHaveAttribute('aria-expanded', 'true')
  await expect(
    card.getByText(
      'Confirm or dismiss the pending revisions of HTTP API, Web task surfaces, Web dashboard, and MCP investigation reads. That decision settles the conflict; recover the order with direction after it.',
    ),
  ).toBeVisible()
  await expect(card.getByText(/Recovery without direction will repeat the checkpoint/)).toHaveCount(0)

  // One block per document keeps every cited section, and each pending
  // proposal gets one Confirm and one Review-or-dismiss action.
  const blocks = card.getByRole('list', { name: /^Citations of / })
  await expect(blocks).toHaveCount(4)
  await expect(blocks.getByRole('listitem')).toHaveCount(7)
  for (const document of sevenCitationDocuments) {
    const citations = card.getByRole('list', { name: `Citations of ${document.title}` })
    await expect(citations.getByRole('listitem')).toHaveCount(document.sections.length)
    for (const section of document.sections) {
      await expect(
        citations.getByText(`Cited v${document.cited} · ${section || 'document-level reference'}`, { exact: true }),
      ).toBeVisible()
    }
    await expect(card.getByRole('link', { name: document.title, exact: true })).toHaveCount(1)
    await expect(card.getByRole('button', { name: `Confirm v${document.proposal}`, exact: true })).toHaveCount(1)
    await expect(card.getByRole('link', { name: `Review or dismiss v${document.proposal}`, exact: true })).toHaveCount(
      1,
    )
  }
  await expect(card.getByRole('button', { name: /^Confirm v/ })).toHaveCount(4)
  await expect(card.getByText(/^Still confirmed at v/)).toHaveCount(4)

  // A partial advancement pre-fills nothing.
  const direction = page.getByLabel('Operator direction')
  await card.getByRole('button', { name: 'Confirm v44' }).click()
  await expect(card.getByText('Now confirmed at v44')).toBeVisible()
  await expect(card.getByRole('button', { name: /^Confirm v/ })).toHaveCount(3)
  await expect(direction).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeDisabled()

  for (const version of [8, 26, 4]) {
    await card.getByRole('button', { name: `Confirm v${version}`, exact: true }).click()
    await expect(card.getByText(`Now confirmed at v${version}`, { exact: true })).toBeVisible()
  }
  expect(confirms.map((confirm) => confirm.path)).toEqual([
    '/v1/system-designs/component-http-api/versions/44/confirm',
    '/v1/system-designs/component-web-task-surfaces/versions/8/confirm',
    '/v1/system-designs/component-web-dashboard/versions/26/confirm',
    '/v1/system-designs/component-mcp-investigation-reads/versions/4/confirm',
  ])
  expect(confirms.map((confirm) => confirm.ifMatch)).toEqual(['"43"', '"7"', '"25"', '"3"'])

  // Every document advanced: the direction names each one once, in block order.
  await expect(direction).toHaveValue(
    'Continue under the newly confirmed authority: component-http-api v44, component-web-task-surfaces v8, component-web-dashboard v26, component-mcp-investigation-reads v4.',
  )
  await direction.fill('Continue under the confirmed revisions.')
  await page.getByRole('button', { name: 'Recover work order' }).click()
  await expect.poll(() => recoveries.at(-1)?.direction).toBe('Continue under the confirmed revisions.')
})

test('task detail links attached product and design context', async ({ page }) => {
  await page.goto('/w/demo/tasks/attached-context/full')
  const context = page.getByRole('region', { name: 'Attached context' })
  await expect(context.getByRole('link', { name: 'Confirmed product outcome' })).toHaveAttribute(
    'href',
    /requirements.*requirement=req-context/,
  )
  await expect(context.getByRole('link', { name: 'Confirmed technical guidance' })).toHaveAttribute(
    'href',
    /system-design.*document=design-context/,
  )
  await expect(context).toContainText('v3')
  await expect(context).toContainText('v2')
  await expect(context.getByText('Archived', { exact: true })).toBeVisible()
})

test('review card renders authorized verification evidence with accessible preview and download fallback', async ({
  page,
}) => {
  const authorizationHeaders: string[] = []
  let videoRequests = 0
  let allowVideoDownload = false
  const png = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
    'base64',
  )
  await page.route('**/v1/artifacts/evidence-image*', async (route) => {
    authorizationHeaders.push((await route.request().headerValue('authorization')) ?? '')
    await route.fulfill({ contentType: 'image/png', body: png })
  })
  await page.route('**/v1/artifacts/evidence-video*', async (route) => {
    videoRequests++
    authorizationHeaders.push((await route.request().headerValue('authorization')) ?? '')
    if (!allowVideoDownload) {
      await route.fulfill({ status: 503, body: 'preview unavailable' })
      return
    }
    await route.fulfill({ contentType: 'video/webm', body: 'downloadable recording' })
  })

  await page.goto('/w/demo/tasks/evidence/full')
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
  await expect(
    recordingDialog.getByText('This evidence could not be loaded for preview. Use the authorized download instead.'),
  ).toBeVisible()
  await expect(recordingDialog.getByRole('button', { name: 'Download' })).toBeEnabled()
  const previewRequestCount = videoRequests
  allowVideoDownload = true
  const downloadPromise = page.waitForEvent('download')
  await recordingDialog.getByRole('button', { name: 'Download' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe('proof recording.webm')
  expect(videoRequests).toBe(previewRequestCount + 1)
  expect(authorizationHeaders.length).toBeGreaterThanOrEqual(3)
  expect(authorizationHeaders.every((header) => header === '')).toBe(true)
})

// The plan version and its approval read as one plan-cycle entry that names
// the approved version, with no separate approval row
// (component-web-task-surfaces invariant 68; req-260809-a40c75 AC-4.2).
test('spec approval reads as one plan cycle that names the approved version', async ({ page }) => {
  await page.goto('/w/demo/tasks/spec-approval/full')

  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const timelineRows = timeline.locator(':scope > ol > li')
  await expect(timelineRows).toHaveCount(1)
  await expect(timelineRows.nth(0)).toContainText('Plan v2 ready')
  await expect(timelineRows.nth(0)).toContainText('Approved plan v2')
  await expect(timeline.getByText('Plan v2 drafted')).toHaveCount(0)
  await expect(timeline.getByText(/Spec v\d+ approved/)).toHaveCount(0)
})

// The gate as the backend leaves it: the request event, the claim released in
// the same transaction, and the implement order sitting queued with retry
// suppressed.
function planRevisionGateActivity(taskId: string) {
  const item = activity('gate', false)
  item.task.id = taskId
  item.events = [
    {
      id: 91,
      task_id: taskId,
      job_id: `${taskId}-implement-1`,
      kind: 'work_order.plan_revision_requested',
      actor_id: 'worker-1',
      actor_role: 'agent',
      payload: {
        work_order_id: `${taskId}-implement-1`,
        attempt_id: 'attempt-9',
        rationale: 'A dependency changed the public handler contract.',
        plan_version: 7,
      },
      at: '2026-07-15T12:10:00Z',
    },
    {
      id: 92,
      task_id: taskId,
      job_id: `${taskId}-implement-1`,
      kind: 'work_order.released',
      actor_id: 'worker-1',
      actor_role: 'agent',
      payload: { attempt_id: 'attempt-9', reason: 'plan revision requested', outcome: 'released' },
      at: '2026-07-15T12:10:00Z',
    },
  ]
  item.work_orders = [
    {
      id: `${taskId}-implement-1`,
      task_id: taskId,
      job_id: `${taskId}-implement-1`,
      stage: 'implement',
      state: 'queued',
      claimable: false,
      last_attempt_id: 'attempt-9',
      last_attempt_outcome: 'released',
      last_failure_message: 'plan revision requested',
      last_failure_at: '2026-07-15T12:10:00Z',
      automatic_retry_count: 0,
      retry_suppressed: true,
      queue_entered_at: '2026-07-15T12:10:00Z',
      queue_deadline: '2026-07-16T12:10:00Z',
      updated_at: '2026-07-15T12:10:00Z',
      redispatch_count: 0,
      cost_usd: 0,
      tokens_in: 0,
      tokens_out: 0,
      usage_reported: false,
      self_reported: true,
    },
  ]
  return item
}

test('the revision chain reads as one plan cycle: request, decision, and the plan version it produced', async ({
  page,
}) => {
  const taskId = 'plan-revision-history'
  await page.route(`**/v1/tasks/${taskId}/activity*`, async (route) => {
    const item = planRevisionGateActivity(taskId)
    // Decided and re-planned: approval cancels the contested implement order,
    // the task is running again, and only the recorded history remains.
    item.task.state = 'running'
    item.work_orders = []
    item.events = [
      ...item.events,
      {
        id: 93,
        task_id: taskId,
        kind: 'intervention.redirect',
        actor_id: 'operator',
        actor_role: 'human',
        payload: { reason_code: 'plan-revision-approved', comment: 'Re-plan around the new handler contract.' },
        at: '2026-07-15T12:20:00Z',
      },
      {
        id: 94,
        task_id: taskId,
        kind: 'spec.version_created',
        actor_id: 'agent',
        actor_role: 'agent',
        payload: { version: 8 },
        at: '2026-07-15T12:30:00Z',
      },
    ]
    item.interventions = [
      {
        id: 7,
        task_id: taskId,
        actor_id: 'operator',
        actor_role: 'human',
        action: 'redirect',
        reason_code: 'plan-revision-approved',
        comment: 'Re-plan around the new handler contract.',
        at: '2026-07-15T12:20:00Z',
      },
    ]
    // The revised plan re-enters the ordinary approval gate, so the panel
    // still shows it as the latest version, awaiting approval.
    item.spec = { ...item.spec, version: 8, approved: false, approved_at: undefined }
    await route.fulfill({ json: item })
  })

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })

  // One plan-cycle entry holds the request, the decision, and the version it
  // produced, each with its time (req-260809-a40c75 AC-4.1; DEC-130(1)).
  const cycle = timeline.getByRole('article', { name: 'Plan revision v7 → v8' })
  await expect(cycle).toBeVisible()
  // This fixture renders no decision panel below the timeline, so the waiting
  // cycle uses the neutral wording.
  await expect(cycle.getByText('Waiting for a decision', { exact: true })).toBeVisible()
  const steps = cycle.locator('[data-plan-cycle-step]')
  await expect(steps).toHaveCount(3)
  await expect(steps.nth(0)).toContainText('Agent asked to revise plan v7')
  await expect(steps.nth(0)).toContainText('A dependency changed the public handler contract.')
  await expect(steps.nth(1)).toContainText('Allowed a revision — back to planning')
  await expect(steps.nth(1)).toContainText('Re-plan around the new handler contract.')
  await expect(steps.nth(2)).toContainText('Plan v8 ready')
  await expect(cycle.locator('time')).toHaveCount(3)
  await expect(cycle.getByText('Waiting for an operator decision')).toBeVisible()

  // No separate request note or decision card, and the release the request
  // rode in on is not repeated.
  await expect(timeline.getByText('The agent asked to revise plan v7')).toHaveCount(0)
  await expect(timeline.getByText('Plan revision approved — returning to planning')).toHaveCount(0)
  await expect(timeline.getByText('Work-order claim released')).toHaveCount(0)
  // The wire vocabulary stays on the wire.
  await expect(timeline.getByText('plan-revision-approved')).toHaveCount(0)
  await expect(timeline.getByText('Requested changes')).toHaveCount(0)

  const plan = page.getByRole('region', { name: 'Execution plan' })
  await expect(plan.getByText('v8', { exact: true })).toBeVisible()
  await expect(plan.getByText('Awaiting approval')).toBeVisible()
})

test('merge gate stays pending until the post-success task and activity refreshes settle', async ({ page }) => {
  let merged = false
  let releaseRefresh = () => {}
  const refreshReleased = new Promise<void>((resolve) => {
    releaseRefresh = resolve
  })
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

  await page.goto('/w/demo/tasks/merge-delayed/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  const merge = gate.getByRole('button', { name: 'Merge', exact: true })
  // Merge opens its confirm (component-web-task-surfaces invariant 45); the
  // confirm posts the act.
  await merge.click()
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  await dialog.locator('[data-confirm-merge]').click()
  // The accepted merge closes the confirm, and Merge stays disabled until the
  // refreshes land.
  await expect(dialog).toHaveCount(0)
  await expect(merge).toBeDisabled()

  releaseRefresh()
  await expect(gate).toHaveCount(0)
})

test('failed merge restores the existing error and retry action', async ({ page }) => {
  let attempts = 0
  await page.route('**/v1/tasks/merge-failure/merge*', async (route) => {
    attempts++
    await route.fulfill({ status: 502, body: 'GitHub merge failed' })
  })

  await page.goto('/w/demo/tasks/merge-failure/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await gate.getByRole('button', { name: 'Merge', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Confirm merge' })
  const confirm = dialog.locator('[data-confirm-merge]')
  await confirm.click()
  await expect(dialog.getByText('GitHub merge failed')).toBeVisible()
  await expect(confirm).toBeEnabled()
  await confirm.click()
  await expect.poll(() => attempts).toBe(2)
})

// REQ-6: the gate is where a person reviews what their run produced, so it
// states what is being approved — the head the factory judged, the pull request
// it was pushed to, and the verdict that resolved — from the record the
// pipeline already wrote.
test('the merge gate names the reviewed head, pull request, and factory verdict', async ({ page }) => {
  await page.goto('/w/demo/tasks/merge-request-changes/full')

  const card = page.getByRole('region', { name: 'What you are approving' })
  await expect(card.getByText('ca7ca700', { exact: true })).toBeVisible()
  // The full SHA stays available without spending a line on it.
  await expect(card.getByText('ca7ca700', { exact: true })).toHaveAttribute(
    'title',
    'ca7ca7000000000000000000000000000000beef',
  )
  // The base and head bound a commit range, and the card says exactly that.
  // Nothing in the browser contract carries a diff stat or changed-file list,
  // so no wording here may imply the card summarises the change itself (REQ-6).
  const range = card.getByText('b1a5e000 … ca7ca700')
  await expect(range).toBeVisible()
  await expect(range).toHaveAttribute(
    'title',
    'b1a5e0000000000000000000000000000000feed … ca7ca7000000000000000000000000000000beef',
  )
  await expect(card.getByText('Commit range')).toBeVisible()
  await expect(card.getByText(/diff|files changed|insertions|deletions|everything since/i)).toHaveCount(0)

  const pullRequest = card.getByRole('link', { name: /acme\/conveyor#482/ })
  await expect(pullRequest).toHaveAttribute('href', 'https://github.test/acme/conveyor/pull/482')
  await expect(card.getByText('2 factory reviewers approved this')).toBeVisible()
  await expect(card.getByText('Both reviewers accepted the pushed branch.')).toBeVisible()
  await expect(card.getByText('conveyor/task-merge-request-changes')).toBeVisible()

  // Presentation only: the approve affordance the gate already offered is
  // untouched beside it.
  await expect(page.getByRole('region', { name: 'Human gate' }).getByRole('button', { name: 'Approve' })).toBeEnabled()
})

test('merge gate sends user feedback through request changes', async ({ page }) => {
  let feedback = ''
  await page.route('**/v1/tasks/merge-request-changes/request-changes*', async (route) => {
    feedback = ((await route.request().postDataJSON()) as { feedback: string }).feedback
    await route.fulfill({ json: { task: activity('merge-request-changes', false).task, feedback } })
  })

  await page.goto('/w/demo/tasks/merge-request-changes/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await gate.getByRole('button', { name: 'Request changes' }).click()
  // The composer will not send until something is written.
  await expect(gate.getByRole('button', { name: 'Request changes' }).last()).toBeDisabled()

  await gate.getByLabel('Changes you want').fill('Keep this exact feedback.')
  await expect(gate.getByRole('button', { name: 'Request changes' }).last()).toBeEnabled()
  await gate.getByRole('button', { name: 'Request changes' }).last().click()

  await expect.poll(() => feedback).toBe('Keep this exact feedback.')
})

// The workspace's own co-members, which is all the assignment picker may ever
// offer: there is no user directory to search (AC-3.2).
const assignmentMembers = [
  {
    workspace_id: 'demo',
    user_id: 'usr_ada',
    email: 'ada@example.test',
    display_name: 'Ada Owner',
    role: 'operator' as const,
    created_at: createdAt,
  },
  {
    workspace_id: 'demo',
    user_id: 'usr_bo',
    email: 'bo@example.test',
    display_name: 'Bo Member',
    role: 'contributor' as const,
    created_at: createdAt,
  },
]

/**
 * Mount task detail with the reads the assignment control depends on, holding
 * the assignee server-side so a set or clear comes back through the projection
 * rather than from browser state.
 *
 * `operator` decides only the role the server reports for the caller, which is
 * the real discriminator: the surface asks who it is rather than guessing.
 */
async function routeAssignment(page: Page, options: { operator: boolean }) {
  const state: { assignee?: (typeof assignmentMembers)[number]; events: TaskEventFixture[] } = { events: [] }
  // The app shell reconciles the stored selection against this enumeration, and
  // the membership reads are workspace-scoped, so the selection has to survive.
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/me**', (route) =>
    route.fulfill({
      json: callerIdentity(
        {
          id: 'usr_ada',
          email: 'ada@example.test',
          display_name: 'Ada Owner',
          role: options.operator ? 'operator' : 'contributor',
        },
        route.request(),
      ),
    }),
  )
  await page.route('**/v1/workspaces/demo/members**', (route) => route.fulfill({ json: assignmentMembers }))
  await page.route('**/v1/tasks/assignable/assignee**', async (route) => {
    const body = JSON.parse(route.request().postData() ?? '{}') as { assignee_user_id?: string }
    const userID = (body.assignee_user_id ?? '').trim()
    state.assignee = assignmentMembers.find((member) => member.user_id === userID)
    state.events.push({
      id: 900 + state.events.length,
      task_id: 'assignable',
      kind: state.assignee ? 'task.assignee.set' : 'task.assignee.cleared',
      actor_id: 'usr_ada',
      actor_role: 'human',
      payload: { assignee_user_id: userID },
      at: '2026-07-15T13:00:00Z',
    })
    await route.fulfill({ json: {} })
  })
  await page.route('**/v1/tasks/assignable/activity**', async (route) => {
    const item = activity('assignable', false)
    item.task.assignee = state.assignee
      ? {
          user_id: state.assignee.user_id,
          email: state.assignee.email,
          display_name: state.assignee.display_name,
        }
      : undefined
    item.events = [...item.events, ...state.events]
    await route.fulfill({ json: item })
  })
}

type TaskEventFixture = {
  id: number
  task_id: string
  kind: string
  actor_id: string
  actor_role: 'human'
  payload: Record<string, unknown>
  at: string
}

// req-task-lifecycle-and-queue AC-2.4/AC-2.5: an operator routes the task by
// picking a workspace co-member, and the audited outcome reads back in the
// timeline like every other operator act. Assignment constrains who may claim
// it and nothing else (AC-2.3, AC-2.7; DEC-125(4)).
test('an operator assigns a task from the member picker and the timeline records it', async ({ page }) => {
  await routeAssignment(page, { operator: true })

  await page.goto('/w/demo/tasks/assignable/full')
  await page.getByRole('button', { name: 'Task actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Assign', exact: true })).toBeVisible()
  // Nothing claims the task yet, so no chip stands in for an assignment.
  await expect(page.getByTitle(/^Assigned to/)).toHaveCount(0)

  await page.getByRole('menuitem', { name: 'Assign', exact: true }).click()
  const picker = page.getByRole('listbox', { name: 'Workspace members' })
  await expect(picker.getByRole('option', { name: 'Ada Owner' })).toBeVisible()
  await picker.getByRole('option', { name: 'Bo Member' }).click()

  // The header fact and the control both follow the server's answer.
  await expect(page.getByTitle('Assigned to Bo Member — bo@example.test · usr_bo')).toBeVisible()
  await page.getByRole('button', { name: 'Task actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Reassign', exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Activity' })).toContainText('Assigned to Bo Member')

  // Clearing restores the unassigned presentation rather than leaving a blank.
  await page.getByRole('menuitem', { name: 'Reassign', exact: true }).click()
  await page.getByRole('button', { name: 'Clear assignee' }).click()
  await page.getByRole('button', { name: 'Task actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Assign', exact: true })).toBeVisible()
  await expect(page.getByTitle(/^Assigned to/)).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Activity' })).toContainText('Assignee cleared')
})

// ---- Review seat minimum (feature-review-seat-minimum RS-7, RS-3) ----------

type SeatFixtureOrder = {
  round: number
  seat: number
  model: string
  verdict?: 'approve' | 'changes_requested'
  id?: string
}

function seatOrder(taskId: string, order: SeatFixtureOrder, index: number): WorkOrder {
  const id = order.id ?? `${taskId}-review-${order.round}-seat-${order.seat}`
  const at = new Date(Date.parse('2026-07-15T12:00:00Z') + order.round * 3_600_000 + index * 1_000).toISOString()
  return {
    id,
    task_id: taskId,
    job_id: id,
    stage: 'review',
    state: order.verdict ? 'completed' : 'claimed',
    review_round: order.round,
    review_seat: order.seat,
    required_model: order.model,
    queue_entered_at: at,
    queue_deadline: '2026-07-20T12:00:00Z',
    redispatch_count: 0,
    cost_usd: 0,
    tokens_in: 0,
    tokens_out: 0,
    usage_reported: false,
    self_reported: true,
  } as WorkOrder
}

function seatMeta(
  round: number,
  count: number,
  source: TaskReviewSeatMetadata['seat_count_source'],
  extra: Partial<TaskReviewSeatMetadata> = {},
): TaskReviewSeatMetadata {
  return { review_round: round, seat_count: count, seat_count_source: source, minimum_seat_count: 2, ...extra }
}

// A bounded task-detail response as the server sends it, with the review seat
// summaries stated directly (internal/httpapi/task_detail_review_seats.go).
function seatPanelItem(
  taskId: string,
  options: {
    orders: SeatFixtureOrder[]
    summaries: Partial<TaskSummaries>
    state?: string
    spec?: { review_seats?: number | null; approved?: boolean }
  },
): ActivityItem {
  const item = activity(taskId, false) as unknown as CompleteActivity
  item.task = { ...item.task, state: (options.state ?? 'running') as ActivityItem['task']['state'] }
  item.jobs = []
  item.events = []
  item.work_orders = options.orders.map((order, index) => seatOrder(taskId, order, index))
  if (item.spec)
    item.spec = { ...item.spec, approved: options.spec?.approved ?? true, review_seats: options.spec?.review_seats }
  const bounded = boundedView(item, taskId)
  const byOrder: NonNullable<NonNullable<TaskSummaries['review_results']>['by_order']> = {}
  for (const order of item.work_orders ?? []) {
    const verdict = options.orders.find(
      (candidate, index) => seatOrder(taskId, candidate, index).id === order.id,
    )?.verdict
    if (verdict) byOrder[order.id] = { verdict, summary: 'Seat verdict.', feedback: '', at: order.queue_entered_at }
  }
  bounded.summaries = {
    ...bounded.summaries,
    review_results: { by_order: byOrder, by_job: {} },
    ...options.summaries,
  }
  return bounded
}

const seatHistory = {
  orders: [
    { round: 1, seat: 1, model: 'r1-model', verdict: 'changes_requested' as const },
    { round: 1, seat: 2, model: 'r1-model', verdict: 'changes_requested' as const },
    { round: 2, seat: 1, model: 'r2-model', verdict: 'approve' as const },
    { round: 2, seat: 2, model: 'r2-model' },
    { round: 2, seat: 3, model: 'r2-model' },
  ],
  rounds: {
    '1': {
      event_id: '41',
      verdict: 'changes_requested' as const,
      summary: 'Round 1 settled.',
      at: '2026-07-15T13:30:00Z',
    },
  },
}

function openRoundItem(
  taskId: string,
  overrides: { state?: string; seatsInRound2?: number; superseded?: boolean } = {},
) {
  const extra = overrides.seatsInRound2 ?? 3
  const orders = [
    ...seatHistory.orders.filter((order) => order.round === 1),
    ...Array.from({ length: extra }, (_, index) => ({
      round: 2,
      seat: index + 1,
      model: 'r2-model',
      verdict: index === 0 ? ('approve' as const) : undefined,
    })),
  ]
  const round2 = seatMeta(2, extra, 'carried', {
    carried_from_round: 1,
    planned_seat_count: 3,
    superseded: overrides.superseded,
  })
  return seatPanelItem(taskId, {
    orders,
    state: overrides.state,
    summaries: {
      review_round: { ...round2 },
      review_round_seats: {
        by_round: { '1': seatMeta(1, 2, 'minimum', { planned_seat_count: 3 }), '2': round2 },
        by_order: {},
      },
      review_seat_policy: { minimum_seat_count: 2, planned_seat_count: 3 },
      review_rounds: { by_round: seatHistory.rounds, by_order: {} },
    },
  })
}

const roundPanel = (page: Page, model: string) => page.locator('article').filter({ hasText: model })

test('review seat header shows each source and approved planned count', async ({ page }) => {
  const taskId = 'seat-sources'
  const orders: SeatFixtureOrder[] = [
    { round: 1, seat: 1, model: 'declared-model', verdict: 'changes_requested' },
    { round: 2, seat: 1, model: 'planned-model', verdict: 'changes_requested' },
    { round: 4, seat: 1, model: 'carried-model', verdict: 'changes_requested' },
    { round: 5, seat: 1, model: 'minimum-model' },
    { round: 5, seat: 2, model: 'minimum-model' },
  ]
  const item = seatPanelItem(taskId, {
    orders,
    summaries: {
      review_round: seatMeta(5, 2, 'minimum', { planned_seat_count: 3 }),
      review_round_seats: {
        by_round: {
          '1': seatMeta(1, 1, 'declared', { planned_seat_count: 3 }),
          '2': seatMeta(2, 1, 'planned', { planned_seat_count: 3 }),
          '4': seatMeta(4, 1, 'carried', { carried_from_round: 2, planned_seat_count: 3 }),
          '5': seatMeta(5, 2, 'minimum', { planned_seat_count: 3 }),
        },
        by_order: {},
      },
      review_seat_policy: { minimum_seat_count: 2, planned_seat_count: 3 },
    },
  })
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => route.fulfill({ json: item }))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  for (const [model, header, title] of [
    ['declared-model', 'Panel of 1 · minimum 2', 'Seat count declared at submission · Plan: 3 seats'],
    ['planned-model', 'Panel of 1 · minimum 2', 'Seat count planned · Plan: 3 seats'],
    // Round 3 never existed: the tooltip names round 2, not an assumed R-1.
    ['carried-model', 'Panel of 1 · minimum 2', 'Seat count carried from round 2 · Plan: 3 seats'],
    ['minimum-model', 'Panel of 2 · minimum 2', 'Seat count is the workspace minimum · Plan: 3 seats'],
  ] as const) {
    const panel = roundPanel(page, model)
    await expect(panel.getByTitle(title, { exact: true })).toHaveText(header)
  }
  // A one-seat panel states no unanimity; a larger panel keeps it beside the header.
  await expect(roundPanel(page, 'declared-model').getByText('unanimous to pass')).toHaveCount(0)
  await expect(roundPanel(page, 'minimum-model').getByText('· unanimous to pass')).toBeVisible()
  // Source machine codes never reach the page.
  await expect(page.getByText(/^(declared|planned|carried|minimum)$/)).toHaveCount(0)
})

test('review seat addition is visible only for the current unsettled round and operate_gates', async ({ page }) => {
  const addButton = page.getByRole('button', { name: 'Add reviewer seat' })
  await page.route('**/v1/tasks/seat-open/activity*', (route) => route.fulfill({ json: openRoundItem('seat-open') }))
  await page.goto('/w/demo/tasks/seat-open/full')
  await expect(roundPanel(page, 'r2-model').getByRole('button', { name: 'Add reviewer seat' })).toBeVisible()
  await expect(addButton).toHaveCount(1)
  await expect(roundPanel(page, 'r1-model').getByRole('button', { name: 'Add reviewer seat' })).toHaveCount(0)

  // A merged task, a superseded latest round, and a settled latest round offer no addition.
  for (const [taskId, item] of [
    ['seat-merged', openRoundItem('seat-merged', { state: 'merged' })],
    ['seat-superseded', openRoundItem('seat-superseded', { superseded: true })],
  ] as const) {
    await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => route.fulfill({ json: item }))
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    await expect(roundPanel(page, 'r2-model')).toContainText('Panel of 3 · minimum 2')
    await expect(addButton).toHaveCount(0)
  }
  const settled = openRoundItem('seat-settled')
  settled.summaries = {
    ...settled.summaries,
    review_rounds: {
      by_round: { ...seatHistory.rounds, '2': { event_id: '60', verdict: 'approve', at: '2026-07-15T15:00:00Z' } },
      by_order: {},
    },
  }
  await page.route('**/v1/tasks/seat-settled/activity*', (route) => route.fulfill({ json: settled }))
  await page.goto('/w/demo/tasks/seat-settled/full')
  await expect(roundPanel(page, 'r2-model').locator('[data-review-verdict]')).toHaveText('Approved')
  await expect(addButton).toHaveCount(0)

  // Without the served operate_gates capability the control never renders.
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_contributor', role: 'contributor' }, route.request()) }),
  )
  await page.goto('/w/demo/tasks/seat-open/full')
  await expect(roundPanel(page, 'r2-model')).toContainText('Panel of 3 · minimum 2')
  await expect(addButton).toHaveCount(0)
})

// A task whose frozen risk mode is record shows its tier, the tier's source
// and reason, and its counts in the task facts, and each review seat shows the
// tier it stated (req-risk-based-review REQ-11, AC-11.2, AC-5.3). An off task
// shows none of them, and an unrated task says so.
function riskPanelItem(taskId: string, risk?: ActivityItem['risk']) {
  const item = seatPanelItem(taskId, {
    orders: [
      { round: 1, seat: 1, model: 'risk-seat-model', verdict: 'changes_requested' },
      { round: 1, seat: 2, model: 'risk-seat-model', verdict: 'changes_requested' },
    ],
    summaries: {
      review_round_seats: { by_round: { '1': seatMeta(1, 2, 'minimum') }, by_order: {} },
      review_seat_policy: { minimum_seat_count: 2 },
    },
  })
  const byOrder = item.summaries?.review_results?.by_order ?? {}
  const [first, second] = item.work_orders.map((order) => order.id)
  if (risk) {
    byOrder[first] = { ...byOrder[first], risk_tier: 'low', risk_reason: 'One handler changes and its tests cover it.' }
    byOrder[second] = {
      ...byOrder[second],
      risk_tier: 'medium',
      risk_reason: 'The verdict transaction gains a write on every store.',
    }
  }
  item.risk = risk
  return item
}

const recordedRisk: NonNullable<ActivityItem['risk']> = {
  mode: 'record',
  tier: 'medium',
  source: 'reviewer',
  reason: 'The verdict transaction gains a write on every store.',
  head: 'a'.repeat(40),
  required_agent_reviews: 2,
  required_human_approvals: 0,
  calculated_agent_reviews: 2,
  calculated_human_approvals: 1,
  recorded_approvals: 0,
  author_approval_needed: false,
  ratings: [],
}

test('task facts and review seats show the recorded risk tier', async ({ page }) => {
  const taskId = 'risk-recorded'
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) =>
    route.fulfill({ json: riskPanelItem(taskId, recordedRisk) }),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  // The fact shows the tier chip alone. Behind it, in order: the source,
  // "Recorded only", the reason, and the needs (component-web-task-surfaces
  // invariant 66; DEC-127(2)).
  const fact = page.locator('[data-task-risk]')
  await expect(fact.locator('[data-risk-tier]')).toHaveText('Medium')
  await expect(fact).toHaveText('Medium')
  await expect(fact.locator('[data-task-risk-details]')).toHaveCount(0)
  const chip = fact.getByRole('button')
  await expect(chip).toHaveAccessibleName(
    `Risk tier Medium. From a reviewer. Recorded only. ${recordedRisk.reason} Needs 2 agent reviews and no human approval.`,
  )
  await chip.hover()
  const details = fact.locator('[data-task-risk-details]')
  await expect(details).toBeVisible()
  expect(await details.locator(':scope > span').allInnerTexts()).toEqual([
    'From a reviewer\nRecorded only',
    recordedRisk.reason ?? '',
    'Needs 2 agent reviews and no human approval',
  ])
  await expect(details.locator('[data-task-risk-reason]')).toHaveText(recordedRisk.reason ?? '')
  await expect(details.locator('[data-task-risk-needs]')).toHaveText('Needs 2 agent reviews and no human approval')
  await expect(details.locator('[data-task-risk-approvals]')).toHaveCount(0)
  await page.mouse.move(0, 0)
  await expect(details).toHaveCount(0)
  await chip.focus()
  await expect(fact.locator('[data-task-risk-reason]')).toHaveText(recordedRisk.reason ?? '')
  await page.keyboard.press('Escape')
  await expect(fact.locator('[data-task-risk-details]')).toHaveCount(0)
  const panel = roundPanel(page, 'risk-seat-model')
  await expect(panel.locator('[data-seat-risk-tier="low"]')).toContainText('Low')
  await expect(panel.locator('[data-seat-risk-tier="medium"]')).toHaveAttribute(
    'title',
    'The verdict transaction gains a write on every store.',
  )
})
