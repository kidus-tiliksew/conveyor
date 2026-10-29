import { readFileSync } from 'node:fs'
import { expect, type Locator, type Page, type Route } from '@playwright/test'
import type { ActivityItem, TaskReviewSeatMetadata, TaskSummaries, WorkOrder } from '../src/lib/types'
import { boundedView, type CompleteActivity, test } from './helpers/bounded-detail'
import { callerIdentity } from './helpers/caller-identity'
import { serveNextActionsFromActivity } from './helpers/next-action-stub'
import { waitForSheetSettled } from './helpers/sheet'
import { activityFrame, controlTaskStreams, readyFrame } from './helpers/task-streams'

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

async function showAllBoardTasks(page: Page) {
  await page.addInitScript(() =>
    localStorage.setItem('conveyor-task-filters:board:demo', JSON.stringify({ created: 'any' })),
  )
}
// The compact next action the server puts on a stalled activity item, for
// board fixtures built by hand.
const stalledNextAction = {
  kind: 'stalled',
  label: 'Stalled',
  actor: 'operator',
  judgment: true,
  summary: 'This order cannot be claimed.',
  deadline: null,
}
const checkpointProgress =
  'Choose whether the recovery should preserve the existing proposal or replace it with the newly confirmed lifecycle direction before implementation continues.'
let emitLiveScrollEvent = () => {}
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
  'proposal-only': {
    designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }],
  },
  // Carries a different document than the one it proposed against, so its own
  // pending version stays on the document's attention surface.
  'unattached-proposal': {
    designs: [{ id: 'design-delivery', title: 'Delivery tiers', version: 4 }],
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
                        // Plan versions stay technical, so the filler rows are
                        // dependency notes that the timeline still renders.
                        kind: 'task.dependency_removed',
                        actor_id: 'system',
                        actor_role: 'system' as const,
                        payload: { reason: `Live event ${index + 1}` },
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
                                                {
                                                  id: 2,
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
    pending_authority: taskId === 'design-proposal' || taskId === 'design-proposal-queued',
    proposal_claim_waiting: taskId === 'design-proposal' || taskId === 'design-proposal-queued',
    waiting_proposals:
      taskId === 'design-proposal' || taskId === 'design-proposal-queued'
        ? [{ tier: 'system_design', id: 'design-lifecycle', version: 2 }]
        : undefined,
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
    needs_attention: taskId === 'forge-failure' || taskId === 'unsatisfiable' || taskId === 'design-proposal',
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
    worker_status:
      taskId === 'interrupted-review'
        ? {
            available: false,
            required_harnesses: ['claude'],
            reason: "no healthy worker can serve the task's required harnesses; last heartbeat was 2m ago",
            last_heartbeat_at: createdAt,
            last_heartbeat_age: '2m0s',
            queue_context: 'interrupted',
          }
        : taskId === 'null-worker-status'
          ? {
              available: false,
              required_harnesses: null,
              reason: "no healthy worker can serve the task's required harnesses",
              queue_context: 'never_started',
            }
          : taskId === 'queued-worker'
            ? {
                available: false,
                required_harnesses: ['codex'],
                reason: "no healthy worker can serve the task's required harnesses",
                queue_context: 'never_started',
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
  let liveActivityRequests = 0
  let releaseLiveStream = () => {}
  const liveStreamRelease = new Promise<void>((resolve) => {
    releaseLiveStream = resolve
  })
  emitLiveScrollEvent = releaseLiveStream
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
      if (taskId === 'live-scroll') liveActivityRequests++
      const item = activity(taskId, taskId === 'overflowing' || taskId === 'gate', liveActivityRequests > 1 ? 19 : 18)
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
  const fullActivity = page.waitForRequest(
    (request) => new URL(request.url()).pathname === `/v1/tasks/${fullTaskID}/activity`,
  )
  await page.goto(`/w/demo/tasks/${fullTaskID}/full`)
  await fullActivity
  // The full route's header is a breadcrumb naming the task ID; the title
  // reads once, in the reading column.
  const fullHeader = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to board' }) })
  await expect(fullHeader.getByRole('navigation', { name: 'Breadcrumb' })).toHaveText(`Board/${fullTaskID}`)
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

test('task metadata presents stored spec versions as plan versions', async ({ page }) => {
  await page.goto('/w/demo/tasks/github-fallback/full')
  await expect(page.getByText('retrying · plan v7')).toBeVisible()
  await expect(page.getByText('retrying · spec v7')).toHaveCount(0)
})

// The agent prompt the checkout group copies: the title and the conveyor-work
// instruction, one blank line apart. The task body is never part of it.
function expectedAgentPrompt(origin: string, taskID: string, title: string) {
  const instruction = `Use the conveyor-work skill to work Conveyor task ${taskID} in workspace \`demo\` on ${origin}.`
  return [title, instruction].join('\n\n')
}

// Serves one task's detail with overridden task fields or checkout projection.
async function routeCheckoutTask(
  page: Page,
  taskID: string,
  overrides: { title?: string; body?: string; checkout_command?: string },
) {
  const { checkout_command, ...task } = overrides
  await page.route(
    (url) => url.pathname === `/v1/tasks/${taskID}/activity`,
    (route) => {
      const item = activity(taskID, false)
      return route.fulfill({
        json: {
          ...item,
          task: { ...item.task, ...task },
          ...('checkout_command' in overrides ? { checkout_command } : {}),
        },
      })
    },
  )
}

// The agent prompt row is the region named "Agent prompt".
function checkoutGroup(scope: Page | Locator) {
  return scope.getByRole('region', { name: 'Agent prompt' })
}

// The row's toggle reveals the complete prompt below it.
async function expandPrompt(group: Locator) {
  const toggle = group.getByRole('button', { name: 'Show agent prompt' })
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await expect(group.locator('code')).toBeHidden()
  await toggle.click()
  await expect(group.getByRole('button', { name: 'Hide agent prompt' })).toHaveAttribute('aria-expanded', 'true')
  await expect(group.locator('code')).toBeVisible()
}

// The visible prompt is the group's code element; its raw text keeps the
// prompt's newlines, which Playwright's text matchers would normalize.
function promptText(group: Locator) {
  return group.locator('code').evaluate((node) => node.textContent)
}

// Measures the agent prompt row, its one-line preview, and the expanded prompt
// against the row's parent and the viewport.
async function checkoutBounds(group: Locator) {
  return group.evaluate((node) => {
    const element = node as HTMLElement
    const code = element.querySelector('code')!
    const row = element.firstElementChild as HTMLElement
    const preview = element.querySelector('[data-agent-prompt-preview]') as HTMLElement
    const parent = element.parentElement!.getBoundingClientRect()
    const box = element.getBoundingClientRect()
    const style = getComputedStyle(code)
    return {
      width: box.width,
      left: box.left,
      right: box.right,
      parentLeft: parent.left,
      parentRight: parent.right,
      parentWidth: parent.width,
      previewLines: Math.round(
        preview.getBoundingClientRect().height / Number.parseFloat(getComputedStyle(preview).lineHeight),
      ),
      viewportWidth: document.documentElement.clientWidth,
      overflows: [element, row, code].some((part) => part.scrollWidth > part.clientWidth),
      pageOverflows: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      // Lines count the whole prompt, including any part scrolled out of view.
      lines: Math.round(
        (code.scrollHeight - Number.parseFloat(style.paddingTop) - Number.parseFloat(style.paddingBottom)) /
          Number.parseFloat(style.lineHeight),
      ),
      scrollable: code.scrollHeight > code.clientHeight,
      whiteSpace: style.whiteSpace,
    }
  })
}

// Asserts the group fits its parent and the viewport with no horizontal
// overflow, and that the copy button stays inside the viewport.
async function expectCheckoutFits(group: Locator) {
  await expect.poll(async () => (await checkoutBounds(group)).overflows).toBe(false)
  const bounds = await checkoutBounds(group)
  expect(bounds.pageOverflows).toBe(false)
  expect(bounds.left).toBeGreaterThanOrEqual(bounds.parentLeft - 0.5)
  expect(bounds.right).toBeLessThanOrEqual(bounds.parentRight + 0.5)
  expect(bounds.right).toBeLessThanOrEqual(bounds.viewportWidth + 0.5)
  const copy = group.getByRole('button', { name: 'Copy agent prompt' })
  await expect(copy).toBeVisible()
  const copyBox = (await copy.boundingBox())!
  expect(copyBox.x + copyBox.width).toBeLessThanOrEqual(bounds.right + 0.5)
  return bounds
}

test('Agent prompt shows the title and instruction and copies the same text', async ({ page, context }) => {
  const taskID = 'work-on-this-with-a-deliberately-long-responsive-identifier'
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])

  await page.goto(`/w/demo/tasks/${taskID}/full`)
  const origin = await page.evaluate(() => window.location.origin)
  const expectedPrompt = expectedAgentPrompt(origin, taskID, 'Short task')
  const group = checkoutGroup(page)
  await expect(group).toBeVisible()
  expect(await promptText(group)).toBe(expectedPrompt)
  expect(await promptText(group)).not.toContain('A short description.')
  await expect(group.locator('code')).not.toHaveAttribute('title')
  // Collapsed, the row previews the prompt on one line.
  await expect(group.locator('[data-agent-prompt-preview]')).toContainText('Use the conveyor-work skill')
  expect((await checkoutBounds(group)).previewLines).toBe(1)

  // Expanded, the prompt keeps its blank line: two paragraphs and one blank
  // line render as at least three lines.
  await expandPrompt(group)
  const bounds = await checkoutBounds(group)
  expect(bounds.whiteSpace).toBe('pre-wrap')
  expect(bounds.lines).toBeGreaterThanOrEqual(3)

  // The Terminal tab and the tab structure are gone from the prompt group;
  // the only tabs on the page are the activity's Timeline and Flow.
  await expect(group.getByRole('tablist')).toHaveCount(0)
  await expect(group.getByRole('tab')).toHaveCount(0)
  await expect(group.getByRole('tabpanel')).toHaveCount(0)
  await expect(page.getByRole('tab')).toHaveText(['Timeline', 'Flow'])
  await expect(page.getByText(`conveyor run ${taskID}`, { exact: false })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Copy task run command' })).toHaveCount(0)

  // The row spans its parent and never widens it.
  const roomy = await expectCheckoutFits(group)
  expect(roomy.width).toBeCloseTo(roomy.parentWidth, 0)
  await page.setViewportSize({ width: 1920, height: 720 })
  const wider = await expectCheckoutFits(group)
  expect(wider.width).toBeCloseTo(wider.parentWidth, 0)
  await page.setViewportSize({ width: 1280, height: 720 })

  await group.getByRole('button', { name: 'Copy agent prompt' }).click()
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(expectedPrompt)
  expect(await promptText(group)).toBe(expectedPrompt)
})

// A long title with an unbroken token and a long task ID must wrap at phone
// widths on both the full page and the task sheet. The task's long Markdown
// body stays out of the prompt.
const longToken = `https://example.test/${'unbroken-segment-'.repeat(8)}end`
const longTitle = `Replace the checkout presentation ${'with a deliberately long task title '.repeat(4).trim()} ${longToken}`
const bodyMarker = 'body-marker-never-in-the-prompt'
const longBody = [
  '## Context',
  '',
  `The task body carries ${bodyMarker} and the original report.`,
  '',
  '1. First step with `code` and **bold** text.',
  '2. Second step:',
  '   - a nested item keeps its indentation;',
  '   - another nested item.',
  '',
  ...Array.from(
    { length: 6 },
    (_, index) =>
      `Paragraph ${index + 1}. ${'Every sentence of this body stays in the description. '.repeat(3).trim()}`,
  ).flatMap((paragraph) => [paragraph, '']),
  'Final paragraph ends here.',
].join('\n')

for (const route of ['full', 'sheet'] as const) {
  test(`Agent prompt leaves out the body and wraps without overflow on the ${route} task view`, async ({
    page,
    context,
  }) => {
    const taskID = `work-on-this-long-prompt-${route}-${'x'.repeat(48)}`
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])
    await routeCheckoutTask(page, taskID, { title: longTitle, body: longBody })

    await page.goto(route === 'full' ? `/w/demo/tasks/${taskID}/full` : `/w/demo/tasks/${taskID}`)
    const origin = await page.evaluate(() => window.location.origin)
    const expectedPrompt = expectedAgentPrompt(origin, taskID, longTitle)
    const sheet = page.getByRole('dialog', { name: 'Task detail' })
    const scope = route === 'full' ? page : sheet
    const group = checkoutGroup(scope)
    await expect(group).toBeVisible()
    expect(await promptText(group)).toBe(expectedPrompt)
    expect(await promptText(group)).not.toContain(bodyMarker)
    // The task's description still shows the body the prompt leaves out.
    await expect(scope.getByText(bodyMarker, { exact: false })).toBeVisible()
    // The sheet's slide moves the group and its copy button between reads.
    if (route === 'sheet') await waitForSheetSettled(sheet)

    // Collapsed, the row fits on one line at every width.
    for (const width of [1280, 390, 320]) {
      await page.setViewportSize({ width, height: 720 })
      if (route === 'sheet') await waitForSheetSettled(sheet)
      expect((await expectCheckoutFits(group)).previewLines).toBe(1)
    }
    await page.setViewportSize({ width: 1280, height: 720 })
    if (route === 'sheet') await waitForSheetSettled(sheet)

    // Expanded, the prompt grows with its text and has no inner scroll.
    await expandPrompt(group)
    const desktop = await expectCheckoutFits(group)
    expect(desktop.width).toBeLessThanOrEqual(desktop.parentWidth + 0.5)
    expect(desktop.scrollable).toBe(false)

    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 720 })
      if (route === 'sheet') await waitForSheetSettled(sheet)
      const phone = await expectCheckoutFits(group)
      // The sheet narrows at phone width, so the prompt takes more lines. The
      // full route's prompt sits in a 20rem inspector from lg up, narrower
      // than a phone's single column, so there it only has to keep wrapping.
      if (route === 'sheet') expect(phone.lines).toBeGreaterThan(desktop.lines)
      else expect(phone.lines).toBeGreaterThanOrEqual(3)
      expect(phone.scrollable).toBe(false)
      expect(await promptText(group)).toBe(expectedPrompt)
    }

    await group.getByRole('button', { name: 'Copy agent prompt' }).click()
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(expectedPrompt)
  })
}

for (const theme of ['light', 'dark'] as const) {
  test(`Agent prompt keeps the prompt readable in the ${theme} theme`, async ({ page }) => {
    await page.addInitScript((choice) => localStorage.setItem('conveyor-theme', choice), theme)
    await page.goto('/w/demo/tasks/work-on-this-theme/full')
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    const prompt = checkoutGroup(page).locator('code')
    await expect(prompt).toContainText('Short task')
    const colors = await prompt.evaluate((node) => ({
      text: getComputedStyle(node).color,
      background: getComputedStyle(node.closest('.bg-surface')!).backgroundColor,
    }))
    expect(colors.text).not.toBe(colors.background)
  })
}

test('Agent prompt shows the agent prompt for an available checkout without a command', async ({ page }) => {
  const taskID = 'work-on-this-no-command'
  await routeCheckoutTask(page, taskID, { checkout_command: undefined })
  await page.goto(`/w/demo/tasks/${taskID}/full`)
  const origin = await page.evaluate(() => window.location.origin)
  const group = checkoutGroup(page)
  await expect(group).toBeVisible()
  expect(await promptText(group)).toBe(expectedAgentPrompt(origin, taskID, 'Short task'))
  await expect(page.getByRole('button', { name: 'Copy agent prompt' })).toBeVisible()
})

test('Agent prompt names the address workspace without a stored selection', async ({ page }) => {
  const taskID = 'work-on-this-x'
  // Two workspaces and no stored selection: the address alone names the
  // workspace (req-workspace-addresses AC-1.3), so the prompt names it.
  await page.addInitScript(() => localStorage.removeItem('conveyor-workspace'))
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({
      json: [
        { id: 'demo', name: 'Demo' },
        { id: 'other', name: 'Other' },
      ],
    }),
  )
  await routeCheckoutTask(page, taskID, { checkout_command: undefined })

  await page.goto(`/w/demo/tasks/${taskID}/full`)
  const origin = await page.evaluate(() => window.location.origin)
  const group = checkoutGroup(page)
  await expect(group).toBeVisible()
  expect(await promptText(group)).toBe(expectedAgentPrompt(origin, taskID, 'Short task'))
})

test('Work on this guidance shows the guidance without an agent prompt', async ({ page }) => {
  await page.goto('/w/demo/tasks/guidance-only/full')
  await expect(page.getByText('Use the assigned worktree.')).toBeVisible()
  await expect(page.getByText('Work on this locally', { exact: true })).toBeVisible()
  await expect(checkoutGroup(page)).toHaveCount(0)
  // The only tablist is the activity's Timeline and Flow; the guidance has none.
  await expect(page.getByRole('tablist')).toHaveCount(1)
  await expect(page.getByRole('tablist')).toHaveAccessibleName('Task activity view')
  await expect(page.getByRole('button', { name: 'Copy agent prompt' })).toHaveCount(0)
  await expect(page.getByText('Use the conveyor-work skill', { exact: false })).toHaveCount(0)
})

test('task sheet adds bottom clearance without changing full-page task spacing', async ({ page }) => {
  await page.goto('/w/demo/tasks/sheet-padding')
  const sheetContent = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
  await expect(sheetContent).toHaveCSS('padding-bottom', '32px')

  await page.goto('/w/demo/tasks/full-padding/full')
  await expect(page.getByRole('region', { name: 'Task content' })).toHaveCSS('padding-bottom', '0px')
})

test('new task detail tolerates a null work-order list from the API', async ({ page }) => {
  await page.goto('/w/demo/tasks/no-orders/full')
  await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
  await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('task detail omits the frozen policy projection and retired controls', async ({ page }) => {
  for (const path of ['/w/demo/tasks/policy-projection', '/w/demo/tasks/policy-projection/full']) {
    await page.goto(path)

    await expect(page.getByText('Repository', { exact: true })).toBeVisible()
    await expect(page.getByText('Branch', { exact: true })).toBeVisible()
    await expect(page.getByText('Created', { exact: true })).toBeVisible()
    await expect(page.getByText('Assignee', { exact: true })).toBeVisible()
    await expect(page.getByText('Policy', { exact: true })).toHaveCount(0)
    await expect(page.getByText('Plan 30m · Implement 2h · Review 45m · 1 review seat · 3 review rounds')).toHaveCount(
      0,
    )
    await expect(page.getByText(/Change execution setup/i)).toHaveCount(0)
    await expect(page.getByText(/^Setup$/)).toHaveCount(0)
  }
})
test('a claimed attempt exposes reasoned operator preemption with the renewal grace bound', async ({ page }) => {
  let preemptBody = ''
  let preemptKey = ''
  await page.route('**/v1/work-orders/setup-claimed-implement-1/preempt*', async (route) => {
    preemptBody = route.request().postData() ?? ''
    preemptKey = route.request().headers()['x-idempotency-key'] ?? ''
    await route.fulfill({
      json: {
        request_id: preemptKey,
        work_order: { id: 'setup-claimed-implement-1', state: 'queued' },
        revoked_attempt_id: 'attempt-1',
        revoked_session_id: 'session-1',
        revoked_worker_id: 'worker-1',
        grace_bound: 'one renewal interval',
      },
    })
  })
  await page.goto('/w/demo/tasks/setup-claimed/full')
  // At rest it is one quiet trigger in the next-action box — never an entry or
  // tail card inside the Activity event list.
  const trigger = page.getByRole('button', { name: 'Stop attempt' })
  await expect(trigger).toBeVisible()
  await expect(trigger).toHaveCount(1)
  await expect(nextActionBox(page).getByRole('button', { name: 'Stop attempt' })).toBeVisible()
  await expect(
    page.locator('section[aria-label="Execution event timeline"] > ol').getByRole('button', { name: /Stop/ }),
  ).toHaveCount(0)
  await expect(page.getByLabel('Reason for stopping this attempt')).toBeHidden()

  await trigger.click()
  await expect(page.getByText(/stops within about a minute/)).toBeVisible()
  const action = page.getByRole('button', { name: 'Stop the attempt' })
  await expect(action).toBeDisabled()
  await page.getByLabel('Reason for stopping this attempt').fill('Switch to the repaired setup')
  await expect(action).toBeEnabled()
  await action.click()
  await expect.poll(() => preemptBody).toContain('Switch to the repaired setup')
  expect(JSON.parse(preemptBody).request_id).toBe(preemptKey)
  expect(preemptKey).not.toBe('')
  await expect(page.getByRole('button', { name: 'Stopped' })).toBeDisabled()
})

test('preemption is rendered as attributed activity rather than a failure', async ({ page }) => {
  const item = activity('preempt-history', false)
  item.events.push({
    id: 999,
    task_id: item.task.id,
    job_id: 'preempt-history-implement-1',
    kind: 'work_order.preempted',
    actor_id: 'operator-kidus',
    actor_role: 'human',
    payload: { reason: 'Replace the setup contract', attempt_id: 'attempt-1', request_id: 'request-1' },
    at: '2026-07-15T12:05:00Z',
  })
  await page.route('**/v1/tasks/preempt-history/activity*', (route) => route.fulfill({ json: item }))
  await page.goto('/w/demo/tasks/preempt-history/full')
  await expect(page.getByText('Work-order attempt preempted by operator')).toBeVisible()
  await expect(page.getByText(/Replace the setup contract · stops within one renewal interval/)).toBeVisible()
})

test('task detail tolerates null required harnesses from a legacy worker status', async ({ page }) => {
  await page.goto('/w/demo/tasks/null-worker-status/full')
  await expect(page.getByText('No healthy enrolled worker can serve this queued work')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Enrolled worker unavailable' })).not.toHaveClass(/bg-attention-soft/)
  await expect(page.getByText('Required harnesses: not yet routed.')).toBeVisible()
  await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('suppressed worker order exposes failure state and audited recovery action', async ({ page }) => {
  let recoveryRequest = ''
  await page.route('**/v1/work-orders/*/recover*', async (route) => {
    recoveryRequest = route.request().postData() ?? ''
    await route.fulfill({ json: { id: 'recovery-review-1-seat-1', state: 'queued', claimable: true } })
  })
  await page.goto('/w/demo/tasks/recovery/full')
  await expect(page.getByText(/harness exited: status 1/)).toBeVisible()
  await expect(page.getByText('No automatic retry is pending.')).toBeVisible()
  await expect(page.getByText('Resolve the primary checkout changes first.')).toHaveCount(0)
  await expect(page.getByText('You can add an optional instruction for the next attempt.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Attach context' })).toBeVisible()
  await page.getByLabel('Operator direction').fill('  Retry with the approved dashboard approach.  ')
  await page.getByRole('button', { name: 'Recover work order' }).click()
  await expect.poll(() => recoveryRequest).not.toBe('')
  await expect.poll(() => JSON.parse(recoveryRequest).direction).toBe('Retry with the approved dashboard approach.')
})

test('ordinary recovery permits an empty operator direction', async ({ page }) => {
  let recoveryRequest = ''
  await page.route('**/v1/work-orders/*/recover*', async (route) => {
    recoveryRequest = route.request().postData() ?? ''
    await route.fulfill({ json: { id: 'recovery-review-1-seat-1', state: 'queued', claimable: true } })
  })
  await page.goto('/w/demo/tasks/recovery/full')
  await expect(page.getByLabel('Operator direction')).toHaveValue('')
  const action = page.getByRole('button', { name: 'Recover work order' })
  await expect(action).toBeEnabled()
  await action.click()
  await expect.poll(() => recoveryRequest).not.toBe('')
  expect(JSON.parse(recoveryRequest)).not.toHaveProperty('direction')
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

test('checkpoint recovery keeps the generic copy when no progress report exists', async ({ page }) => {
  const item = activity('operator-checkpoint', false)
  item.work_orders = item.work_orders?.map((order) => ({ ...order, progress: '' }))
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(page.getByText(/A decision is required before this work can continue/)).toBeVisible()
  await expect(page.getByText('Agent checkpoint message')).toHaveCount(0)
})

test('task timeline expands the complete agent progress report', async ({ page }) => {
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const timeline = page.getByLabel('Execution event timeline')
  await expect(timeline.getByText(/Agent activity — Progress:/)).toBeVisible()
  const disclosure = timeline.getByText('Show full progress report')
  await expect(disclosure).toBeVisible()
  await disclosure.click()
  await expect(disclosure.locator('..').getByText(checkpointProgress, { exact: true })).toBeVisible()
})

test('checkpoint recovery summarizes attached requirement and design context', async ({ page }) => {
  const item = activity('operator-checkpoint', false)
  item.task.context = {
    requirements: [{ id: 'req-1', title: 'Confirmed requirement', version: 2 }],
    designs: [{ id: 'design-1', title: 'Confirmed design', version: 3 }],
  }
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(page.getByText(/Attached context: 1 requirement\(s\), 1 design document\(s\)/)).toBeVisible()
})

test('structured authority checkpoint leads with the decision and resolves through cited proposals', async ({
  page,
}) => {
  const item = activity('operator-checkpoint', false)
  item.task.context = {
    designs: [
      { id: 'design-checkpoint', title: 'Checkpoint design', version: 1 },
      { id: 'design-uncited', title: 'Uncited design', version: 1 },
      { id: 'req-checkpoint', title: 'Requirement identity design', version: 1 },
    ],
  }
  const order = item.work_orders?.[0] as unknown as {
    checkpoint: {
      decision_request: string
      class: string
      citations: Array<{
        document_id: string
        document_kind: string
        document_title: string
        cited_version: number
        statement_or_section_id?: string
        current_confirmed_version: number
        newer_confirmed: boolean
        pending_proposals: Array<{
          id: string
          title: string
          tier: string
          version: number
          origin_type: string
          proposed_at: string
        }>
      }>
    }
  }
  order.checkpoint = {
    decision_request: 'Confirm the design revision and direct the next implementation attempt.',
    class: 'authority_conflict',
    citations: [
      {
        document_id: 'req-checkpoint',
        document_kind: 'requirement',
        document_title: 'Checkpoint requirement',
        cited_version: 1,
        statement_or_section_id: 'AC-2.2',
        current_confirmed_version: 2,
        newer_confirmed: true,
        pending_proposals: [],
      },
      {
        document_id: 'design-checkpoint',
        document_kind: 'system_design',
        document_title: 'Checkpoint design',
        cited_version: 1,
        current_confirmed_version: 1,
        newer_confirmed: false,
        pending_proposals: [
          {
            id: 'design-checkpoint',
            title: 'Checkpoint design',
            tier: 'system_design',
            version: 2,
            origin_type: 'operator',
            proposed_at: createdAt,
          },
        ],
      },
    ],
  }
  let resolved = false
  let confirmHeaders: Record<string, string> = {}
  let recoveryBody: Record<string, unknown> | undefined
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    if (resolved) {
      order.checkpoint.citations[1].current_confirmed_version = 2
      order.checkpoint.citations[1].newer_confirmed = true
      order.checkpoint.citations[1].pending_proposals = []
    }
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({
      json: [
        designProposalSummary('operator-checkpoint', 'design-checkpoint', 'Checkpoint design', 2),
        designProposalSummary('operator-checkpoint', 'design-uncited', 'Uncited design', 3),
        designProposalSummary('operator-checkpoint', 'req-checkpoint', 'Requirement identity design', 4),
      ],
    }),
  )
  // The inbox lists pending proposals from the server's next-actions listing,
  // so the checkpoint's "Review or dismiss" path lands on this card.
  await page.route('**/v1/next-actions*', (route) =>
    route.fulfill({
      json: {
        operator_decisions: 1,
        items: [
          {
            band: 'decisions',
            kind: 'document_proposal',
            proposal: {
              id: 'design-checkpoint',
              title: 'Checkpoint design',
              tier: 'system_design',
              version: 2,
              origin_type: 'operator',
              proposed_at: createdAt,
              age_seconds: 60,
            },
          },
        ],
        acts: [],
      },
    }),
  )
  await page.route('**/v1/system-designs/design-checkpoint/versions/2/confirm**', async (route) => {
    confirmHeaders = route.request().headers()
    resolved = true
    await route.fulfill({ json: {} })
  })
  await page.route('**/v1/pending-proposals*', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            id: 'design-checkpoint',
            title: 'Checkpoint design',
            tier: 'system_design',
            version: 2,
            origin_type: 'session',
            origin_id: 'local-operator-session',
            proposed_at: createdAt,
            age_seconds: 60,
          },
        ],
        attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
      },
    })
  })
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', async (route) => {
    recoveryBody = route.request().postDataJSON() as Record<string, unknown>
    await route.fulfill({ json: { id: 'operator-checkpoint-implement-2', state: 'queued', claimable: true } })
  })

  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByText(order.checkpoint.decision_request, { exact: true })).toBeVisible()
  // One cited document is already answered, so the row offers Respond rather
  // than confirm-and-resume (component-web-task-surfaces).
  await expect(card.getByRole('button', { name: /& resume$/ })).toHaveCount(0)
  await expect(card.getByRole('button', { name: 'Respond' })).toBeVisible()
  await expect(card.getByText(checkpointProgress, { exact: true })).not.toBeVisible()
  await card.getByRole('button', { name: 'Review' }).click()
  await card.getByText('Show full progress report').click()
  await expect(card.getByText(checkpointProgress, { exact: true })).toBeVisible()
  await expect(card.getByRole('link', { name: 'Checkpoint requirement' })).toHaveAttribute(
    'href',
    /requirements.*requirement=req-checkpoint.*#ac-2\.2/,
  )
  await expect(card.getByRole('link', { name: 'Checkpoint design' })).toHaveAttribute(
    'href',
    /system-design.*document=design-checkpoint/,
  )
  await expect(card.getByText('Now confirmed at v2')).toBeVisible()
  await expect(card.getByText('Still confirmed at v1')).toBeVisible()
  await expect(
    card.getByText('No revision is waiting for a decision. Author one if the conflict remains.'),
  ).toBeVisible()
  await expect(card.getByText('A revision is waiting for your decision.')).toBeVisible()
  const standalone = page.getByRole('region', { name: 'System Design proposals from this task' })
  await expect(standalone).toHaveCount(1)
  await expect(standalone).not.toHaveClass(/bg-attention-soft/)
  await expect(standalone.getByRole('link', { name: 'Checkpoint design' })).toHaveCount(0)
  await expect(standalone.getByRole('link', { name: 'Uncited design' })).toHaveCount(1)
  await expect(standalone.getByRole('link', { name: 'Requirement identity design' })).toHaveCount(1)
  await expect(card.getByRole('button', { name: 'Confirm v2', exact: true })).toHaveCount(1)
  await expect(page.getByRole('button', { name: 'Confirm version 2' })).toHaveCount(0)
  await expect(standalone.getByRole('button', { name: 'Confirm version 3' })).toHaveCount(1)
  await expect(standalone.getByRole('button', { name: 'Confirm version 4' })).toHaveCount(1)
  const dismissPath = card.getByRole('link', { name: 'Review or dismiss v2' })
  await expect(dismissPath).toHaveAttribute('href', /\/inbox\?.*document=design-checkpoint.*tier=system_design/)
  await dismissPath.click()
  await expect(page.getByText('Showing System Design proposals for design-checkpoint.')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Checkpoint design' })).toBeVisible()
  await page.goBack()
  await card.getByRole('button', { name: 'Review' }).click()
  await card.getByRole('button', { name: 'Confirm v2', exact: true }).click()
  await expect.poll(() => resolved).toBe(true)
  expect(confirmHeaders.authorization).toBeUndefined()
  expect(confirmHeaders['if-match']).toBe('"1"')

  const direction = page.getByLabel('Operator direction')
  await expect(direction).toHaveValue(
    'Continue under the newly confirmed authority: req-checkpoint v2, design-checkpoint v2.',
  )
  await direction.fill('Proceed using the confirmed checkpoint documents.')
  await page.getByRole('button', { name: 'Recover work order' }).click()
  await expect.poll(() => recoveryBody?.direction).toBe('Proceed using the confirmed checkpoint documents.')
})

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
    sections: ['Task detail composition', 'History pages', 'Audit disclosures'],
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

test('an authority-conflict checkpoint states the server question and groups seven citations into four documents', async ({
  page,
}) => {
  const confirmed = new Set<string>()
  const { confirms, recoveries } = await routeSevenCitationCheckpoint(page, confirmed)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')

  // The current state is the server's label, the agent's decision request, and
  // the server's question; nothing reads as a failure that needs recovery.
  const banner = nextActionBox(page)
  await expect(executionTitle(page)).toHaveText('Cited revisions need a decision')
  await expect(banner.getByText(sevenCitationDecision, { exact: true })).toBeVisible()
  await expect(
    banner.getByText('Will you confirm or dismiss the cited revisions, then recover the order with direction?'),
  ).toBeVisible()
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

test('an authority-conflict pre-fill never overwrites operator direction', async ({ page }) => {
  const confirmed = new Set(sevenCitationDocuments.slice(1).map((document) => document.id))
  await routeSevenCitationCheckpoint(page, confirmed)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  const direction = page.getByLabel('Operator direction')
  await card.getByRole('button', { name: 'Review' }).click()
  await expect(card.getByRole('button', { name: /^Confirm v/ })).toHaveCount(1)
  await direction.fill('Keep the operator wording.')
  await card.getByRole('button', { name: 'Confirm v44' }).click()
  await expect(card.getByText('Now confirmed at v44')).toBeVisible()
  await expect(card.getByRole('button', { name: /^Confirm v/ })).toHaveCount(0)
  await expect(direction).toHaveValue('Keep the operator wording.')
})

test('an authority conflict without proposals asks for authorship', async ({ page }) => {
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    const item = activity('operator-checkpoint', false)
    const order = item.work_orders?.[0]
    if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
    order.checkpoint = {
      decision_request: 'Author a revision of the cited design, then recover this order.',
      class: 'authority_conflict',
      citations: [
        {
          document_id: 'component-web-dashboard',
          document_kind: 'system_design',
          document_title: 'Web dashboard',
          cited_version: 25,
          statement_or_section_id: 'Route tree',
          current_confirmed_version: 25,
          newer_confirmed: false,
          pending_proposals: [],
        },
      ],
    }
    await route.fulfill({ json: item })
  })
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(executionTitle(page)).toHaveText('Cited documents need a revision')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByRole('button', { name: /& resume$/ })).toHaveCount(0)
  await card.getByRole('button', { name: 'Respond' }).click()
  await expect(
    card.getByText(
      'No cited document has a pending revision. Author a revision of Web dashboard, then recover the order with direction.',
    ),
  ).toBeVisible()
  await expect(card.getByRole('button', { name: /^Confirm v/ })).toHaveCount(0)
  await expect(
    card.getByText('No revision is waiting for a decision. Author one if the conflict remains.'),
  ).toBeVisible()
})

// After an operator recovery the order is queued again, keeps its checkpoint
// and failure message for history, and the server states ordinary queued
// work. The page follows that statement: no decision or recovery is offered
// for the answered question (req-operator-next-action AC-1.4, AC-1.6, AC-1.7;
// component-work-orders, Next action).
test('a resumed checkpoint order follows the queued server statement', async ({ page }) => {
  let resumed = false
  const checkpoint = {
    decision_request: 'Author a revision of the cited design, then recover this order.',
    class: 'authority_conflict',
    citations: [
      {
        document_id: 'component-web-dashboard',
        document_kind: 'system_design',
        document_title: 'Web dashboard',
        cited_version: 25,
        statement_or_section_id: 'Route tree',
        current_confirmed_version: 26,
        newer_confirmed: true,
        pending_proposals: [],
      },
    ],
  }
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    const item = activity('operator-checkpoint', false)
    const order = item.work_orders?.[0]
    if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
    order.checkpoint = checkpoint
    if (resumed) {
      Object.assign(order, {
        last_attempt_outcome: undefined,
        retry_suppressed: false,
        claimable: true,
        redispatch_count: 1,
        operator_direction: 'Continue under the newly confirmed authority: component-web-dashboard v26.',
      })
      item.next_action = {
        kind: 'queued',
        label: 'Queued',
        actor: 'agent',
        judgment: false,
        summary: 'An order is waiting to be claimed.',
        deadline: null,
        choices: [],
        current_order: {
          id: order.id,
          stage: order.stage,
          state: 'queued',
          queue_deadline: null,
          execution_deadline: null,
          lease_expires_at: null,
        },
        details: {},
      }
    }
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(executionTitle(page)).toHaveText('Cited documents need a revision')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByText('Cited documents need a revision', { exact: true })).toBeVisible()
  await card.getByRole('button', { name: 'Review' }).click()
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()

  resumed = true
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(nextActionBox(page).getByRole('heading', { name: 'An order is waiting to be claimed.' })).toBeVisible()
  await expect(nextActionBox(page).getByText('Queued', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Checkpoint decision and recovery' })).toHaveCount(0)
  await expect(page.getByText('Decision needed', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
  await expect(nextActionBox(page).getByText('Answer the agent?')).toHaveCount(0)
})

test('a generic decision request keeps the generic direction guidance', async ({ page }) => {
  let recoveryBody: Record<string, unknown> | undefined
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    const item = activity('operator-checkpoint', false)
    const order = item.work_orders?.[0]
    if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
    order.checkpoint = { decision_request: 'Should the migration keep the legacy column?' }
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', async (route) => {
    recoveryBody = route.request().postDataJSON() as Record<string, unknown>
    await route.fulfill({ json: { id: 'attempt-recovery-implement-1', state: 'queued', claimable: true } })
  })
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const banner = nextActionBox(page)
  await expect(executionTitle(page)).toHaveText('Agent question')
  await expect(banner.getByText('Should the migration keep the legacy column?', { exact: true })).toBeVisible()
  await expect(banner.getByText('Answer the agent?')).toBeVisible()
  await expect(page.getByText(/recovery needed/i)).toHaveCount(0)
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByRole('button', { name: /& resume$/ })).toHaveCount(0)
  await card.getByRole('button', { name: 'Respond' }).click()
  await expect(page.getByLabel('Operator direction')).toBeFocused()
  await expect(
    card.getByText(
      'A decision is required before this work can continue. Recovery without direction will repeat the checkpoint.',
    ),
  ).toBeVisible()
  const recover = page.getByRole('button', { name: 'Recover work order' })
  await expect(recover).toBeDisabled()
  await page.getByLabel('Operator direction').fill('Keep the legacy column.')
  await recover.click()
  await expect.poll(() => recoveryBody?.direction).toBe('Keep the legacy column.')
})

test('structured authority checkpoint omits the standalone card when every proposal is cited', async ({ page }) => {
  const item = activity('operator-checkpoint', false)
  item.task.context = {
    designs: [{ id: 'design-checkpoint', title: 'Checkpoint design', version: 1 }],
  }
  const order = item.work_orders?.[0]
  if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
  order.checkpoint = {
    decision_request: 'Confirm the cited design before implementation continues.',
    class: 'authority_conflict',
    citations: [
      {
        document_id: 'design-checkpoint',
        document_kind: 'system_design',
        document_title: 'Checkpoint design',
        cited_version: 1,
        current_confirmed_version: 1,
        newer_confirmed: false,
        pending_proposals: [
          {
            id: 'design-checkpoint',
            title: 'Checkpoint design',
            tier: 'system_design',
            version: 2,
            origin_type: 'task',
            proposed_at: createdAt,
          },
        ],
      },
    ],
  }
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({
      json: [designProposalSummary('operator-checkpoint', 'design-checkpoint', 'Checkpoint design', 2)],
    }),
  )

  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await expect(page.getByRole('region', { name: 'Checkpoint decision and recovery' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Confirm v2' })).toHaveCount(1)
  await expect(page.getByRole('region', { name: 'System Design proposals from this task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Confirm version 2' })).toHaveCount(0)
})

// Confirm and resume (component-web-task-surfaces, Implementation checkpoints;
// req-260820-394cac AC-1.2, AC-2.6, AC-2.7): a System Design document with two
// pending versions and a requirement with one, cited in that order.
type BulkCheckpointState = {
  current: Record<string, number>
  pending: Record<string, number[]>
  failOn?: string
  ignore?: Set<string>
  failActivity?: boolean
}

function bulkCheckpoint(state: BulkCheckpointState) {
  const documents = [
    {
      id: 'component-alpha',
      kind: 'system_design' as const,
      title: 'Alpha design',
      cited: 3,
      sections: ['Intake', 'Output'],
    },
    { id: 'req-beta', kind: 'requirement' as const, title: 'Beta requirement', cited: 1, sections: ['AC-1.1'] },
  ]
  return {
    decision_request:
      'Confirm or dismiss component-alpha v4 and v5 and req-beta v2, which replace the intake rules the approved plan conflicts with, then let the order resume under them without a separate recovery act.',
    class: 'authority_conflict',
    citations: documents.flatMap((document) =>
      document.sections.map((section) => ({
        document_id: document.id,
        document_kind: document.kind,
        document_title: document.title,
        cited_version: document.cited,
        statement_or_section_id: section,
        current_confirmed_version: state.current[document.id],
        newer_confirmed: state.current[document.id] > document.cited,
        pending_proposals: state.pending[document.id].map((version) => ({
          id: document.id,
          title: document.title,
          tier: document.kind,
          version,
          origin_type: 'task' as const,
          origin_id: 'operator-checkpoint',
          proposed_at: createdAt,
        })),
      })),
    ),
  }
}

async function routeBulkCheckpoint(page: Page, state: BulkCheckpointState) {
  const confirms: Array<{ path: string; ifMatch?: string }> = []
  const recoveries: Array<Record<string, unknown>> = []
  let activityReads = 0
  await page.route('**/v1/tasks/operator-checkpoint/activity*', async (route) => {
    activityReads++
    if (state.failActivity) {
      await route.fulfill({ status: 503, json: { error: 'unavailable', message: 'Task activity is unavailable.' } })
      return
    }
    const item = activity('operator-checkpoint', false)
    const order = item.work_orders?.[0]
    if (!order) throw new Error('Expected the checkpoint fixture to include a work order.')
    order.checkpoint = bulkCheckpoint(state)
    await route.fulfill({ json: item })
  })
  const confirm = async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    const [, , , id, , version] = path.split('/')
    confirms.push({ path, ifMatch: route.request().headers()['if-match'] })
    if (state.failOn === `${id}:${version}`) {
      await route.fulfill({
        status: 409,
        json: { error: 'version_conflict', message: `${id} moved before v${version}.` },
      })
      return
    }
    if (!state.ignore?.has(id)) {
      state.current[id] = Number(version)
      state.pending[id] = state.pending[id].filter((pending) => pending !== Number(version))
    }
    await route.fulfill({ json: {} })
  }
  await page.route('**/v1/system-designs/*/versions/*/confirm**', confirm)
  await page.route('**/v1/requirements/*/versions/*/confirm**', confirm)
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', async (route) => {
    recoveries.push(route.request().postDataJSON() as Record<string, unknown>)
    await route.fulfill({ json: { id: 'attempt-recovery-implement-1', state: 'queued', claimable: true } })
  })
  return { confirms, recoveries, activityReads: () => activityReads }
}

function freshBulkState(): BulkCheckpointState {
  return { current: { 'component-alpha': 3, 'req-beta': 1 }, pending: { 'component-alpha': [5, 4], 'req-beta': [2] } }
}

test('a decision checkpoint renders one row with the server label, a clamped request, Review, and one primary act', async ({
  page,
}) => {
  await routeBulkCheckpoint(page, freshBulkState())
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByText('Cited revisions need a decision', { exact: true })).toBeVisible()
  const request = card.getByTestId('checkpoint-decision-request')
  await expect(request).toHaveClass(/line-clamp-2/)
  await expect(card.getByRole('button', { name: 'Confirm 3 revisions & resume' })).toBeEnabled()
  await expect(card.getByRole('button', { name: 'Respond' })).toHaveCount(0)
  const review = card.getByRole('button', { name: 'Review' })
  const drawer = page.locator(`#${await review.getAttribute('aria-controls')}`)
  await expect(review).toHaveAttribute('aria-expanded', 'false')
  await expect(drawer).toBeHidden()
  await expect(page.getByLabel('Operator direction')).toBeHidden()
  await expect(card).not.toHaveClass(/bg-attention-soft/)

  // Expanding shows the whole request in place, the guidance once, and the
  // recovery form; typed direction survives closing and reopening.
  await review.click()
  await expect(drawer).toBeVisible()
  await expect(request).not.toHaveClass(/line-clamp-2/)
  await expect(card.getByText(/That decision settles the conflict/)).toHaveCount(1)
  await expect(card.getByRole('list', { name: /^Citations of / })).toHaveCount(2)
  await expect(card.getByRole('button', { name: /^Confirm v\d+$/ })).toHaveCount(3)
  await page.getByLabel('Operator direction').fill('Hold the intake rules as written.')
  await review.click()
  await expect(drawer).toBeHidden()
  await review.click()
  await expect(page.getByLabel('Operator direction')).toHaveValue('Hold the intake rules as written.')
})

test('confirm and resume confirms every cited proposal in ascending order and never recovers', async ({ page }) => {
  const { confirms, recoveries } = await routeBulkCheckpoint(page, freshBulkState())
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await card.getByRole('button', { name: 'Confirm 3 revisions & resume' }).click()

  // The refreshed order is still released until the server's clock tick, and
  // every citation is answered, so the row keeps the resuming status.
  await expect(card.getByRole('status')).toHaveText('Confirmed — resuming')
  expect(confirms).toEqual([
    { path: '/v1/system-designs/component-alpha/versions/4/confirm', ifMatch: '"3"' },
    { path: '/v1/system-designs/component-alpha/versions/5/confirm', ifMatch: '"4"' },
    { path: '/v1/requirements/req-beta/versions/2/confirm', ifMatch: '"1"' },
  ])
  await expect(card.getByText('Now confirmed at v5', { exact: true })).toHaveCount(1)
  await expect(card.getByRole('button', { name: 'Review' })).toHaveAttribute('aria-expanded', 'false')
  await expect(card.getByRole('button', { name: /& resume$/ })).toHaveCount(0)
  expect(recoveries).toEqual([])
})

test('confirm and resume opens the recovery drawer when the refreshed checkpoint is not answered', async ({ page }) => {
  // The requirement confirmation succeeds but the refreshed citation still
  // carries its pending proposal: an AC-2.7 case the operator recovers.
  const state = { ...freshBulkState(), ignore: new Set(['req-beta']) }
  const { recoveries } = await routeBulkCheckpoint(page, state)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await card.getByRole('button', { name: 'Confirm 3 revisions & resume' }).click()
  await expect(card.getByRole('button', { name: 'Review' })).toHaveAttribute('aria-expanded', 'true')
  await expect(page.getByLabel('Operator direction')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
  await expect(card.getByText('Confirmed — resuming')).toHaveCount(0)
  expect(recoveries).toEqual([])
})

test('a failed confirm and resume stops, reports in the row, and retries only the remaining proposals', async ({
  page,
}) => {
  const state: BulkCheckpointState = { ...freshBulkState(), failOn: 'component-alpha:5' }
  const { confirms, recoveries } = await routeBulkCheckpoint(page, state)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await card.getByRole('button', { name: 'Confirm 3 revisions & resume' }).click()
  await expect(card.getByRole('alert')).toContainText('This design changed while you were reviewing it.')
  expect(confirms.map((confirm) => confirm.path)).toEqual([
    '/v1/system-designs/component-alpha/versions/4/confirm',
    '/v1/system-designs/component-alpha/versions/5/confirm',
  ])
  await expect(card.getByText('Confirmed — resuming')).toHaveCount(0)

  // The refreshed state names the remaining proposals and the current
  // confirmed version; v4 is never confirmed twice.
  state.failOn = undefined
  const retry = card.getByRole('button', { name: 'Confirm 2 revisions & resume' })
  await expect(retry).toBeEnabled()
  await card.getByRole('button', { name: 'Review' }).click()
  await expect(page.getByLabel('Operator direction')).toBeVisible()
  await retry.click()
  await expect(card.getByRole('status')).toHaveText('Confirmed — resuming')
  expect(confirms.slice(2)).toEqual([
    { path: '/v1/system-designs/component-alpha/versions/5/confirm', ifMatch: '"4"' },
    { path: '/v1/requirements/req-beta/versions/2/confirm', ifMatch: '"1"' },
  ])
  expect(recoveries).toEqual([])
})

// Review round 1 (seat 1): a failed task-detail read after a partial
// confirmation keeps the stale projection mounted. Confirmation stays disabled
// until a read succeeds, so v4 is never confirmed twice.
test('a failed refresh after partial confirmation keeps confirmation disabled until a successful read', async ({
  page,
}) => {
  const state: BulkCheckpointState = { ...freshBulkState(), failOn: 'component-alpha:5' }
  const { confirms, recoveries } = await routeBulkCheckpoint(page, state)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  const stale = card.getByRole('button', { name: 'Confirm 3 revisions & resume' })
  await expect(stale).toBeEnabled()
  state.failActivity = true
  await stale.click()

  const refreshFailure = card.getByRole('alert').filter({ hasText: 'Could not refresh this task' })
  await expect(refreshFailure).toBeVisible()
  await expect(card.getByRole('alert').filter({ hasText: 'This design changed' })).toBeVisible()
  expect(confirms.map((confirm) => confirm.path)).toEqual([
    '/v1/system-designs/component-alpha/versions/4/confirm',
    '/v1/system-designs/component-alpha/versions/5/confirm',
  ])
  await expect(stale).toBeDisabled()
  await card.getByRole('button', { name: 'Review' }).click()
  for (const version of [4, 5, 2]) {
    await expect(card.getByRole('button', { name: `Confirm v${version}`, exact: true })).toBeDisabled()
  }
  // Manual recovery stays available while the projection is stale.
  await page.getByLabel('Operator direction').fill('Continue with the confirmed intake rules.')
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeEnabled()
  await stale.click({ force: true })
  expect(confirms).toHaveLength(2)

  // A successful read restores the controls from the refreshed versions.
  state.failActivity = false
  state.failOn = undefined
  await refreshFailure.getByRole('button', { name: 'Refresh' }).click()
  await expect(refreshFailure).toHaveCount(0)
  const retry = card.getByRole('button', { name: 'Confirm 2 revisions & resume' })
  await expect(retry).toBeEnabled()
  await retry.click()
  await expect(card.getByRole('status')).toHaveText('Confirmed — resuming')
  expect(confirms.slice(2)).toEqual([
    { path: '/v1/system-designs/component-alpha/versions/5/confirm', ifMatch: '"4"' },
    { path: '/v1/requirements/req-beta/versions/2/confirm', ifMatch: '"1"' },
  ])
  expect(confirms.filter((confirm) => confirm.path.endsWith('/versions/4/confirm'))).toHaveLength(1)
  expect(recoveries).toEqual([])
})

test('confirm and resume requires document confirmation capability', async ({ page }) => {
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_maintainer', role: 'maintainer' }, route.request()) }),
  )
  await routeBulkCheckpoint(page, freshBulkState())
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByRole('button', { name: 'Confirm 3 revisions & resume' })).toBeDisabled()
  await expect(card.getByText('Document confirmation capability is required.').first()).toBeVisible()
})

test('Show changes reads the proposal only after it opens and renders the bounded diff', async ({ page }) => {
  await routeBulkCheckpoint(page, freshBulkState())
  let documentReads = 0
  await page.route(
    (url) => url.pathname === '/v1/system-designs/component-alpha',
    async (route) => {
      documentReads++
      const version = (n: number, content: string, confirmed: boolean) => ({
        document_id: 'component-alpha',
        version: n,
        content,
        governs: [],
        origin: 'implementation_deliberation',
        confirmed,
        dismissed: false,
      })
      await route.fulfill({
        json: {
          document: { id: 'component-alpha', title: 'Alpha design', current_version: 3 },
          current_version: version(3, '# Alpha\n\nIntake keeps the recorded media type.', true),
          pending_versions: [
            version(4, '# Alpha\n\nIntake sends textual documents as text/plain.', false),
            version(5, '# Alpha\n\nIntake sends textual documents as text/plain.\n\nOutput is redacted.', false),
          ],
          versions: [version(3, '# Alpha\n\nIntake keeps the recorded media type.', true)],
          lineage: [],
          lineage_total: 0,
          lineage_snapshot_id: 0,
          drift: [],
        },
      })
    },
  )
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await card.getByRole('button', { name: 'Review' }).click()
  await expect(card.getByText('Show changes in v4')).toBeVisible()
  expect(documentReads).toBe(0)
  await card.getByText('Show changes in v4').click()
  await expect(card.getByText('Confirmed v3', { exact: true })).toBeVisible()
  await expect(card.getByText('Proposed v4', { exact: true })).toBeVisible()
  await expect(card.getByText('Intake sends textual documents as text/plain.')).toBeVisible()
  expect(documentReads).toBeGreaterThan(0)
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
})

test('the decision row and its open drawer stay inside a 390 px viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await routeBulkCheckpoint(page, freshBulkState())
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Checkpoint decision and recovery' })
  await expect(card.getByRole('button', { name: 'Confirm 3 revisions & resume' })).toBeVisible()
  const overflow = () =>
    page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(await overflow()).toBeLessThanOrEqual(0)
  await card.getByRole('button', { name: 'Review' }).click()
  await expect(page.getByLabel('Operator direction')).toBeVisible()
  expect(await overflow()).toBeLessThanOrEqual(0)
})

test('stalled task is labelled on its board card with recover and reasoned cancel controls', async ({ page }) => {
  await showAllBoardTasks(page)
  const item = activity('recovery', false)
  item.task.assignee = {
    user_id: 'usr-assigned',
    email: 'assigned@example.test',
    display_name: 'Assigned User',
  }
  let cancelled = false
  await page.route('**/v1/workspaces', async (route) => {
    await route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
  })
  await page.route('**/v1/activity*', async (route) => {
    await route.fulfill({
      json: [
        {
          task: item.task,
          latest_stage: 'implement',
          last_event_at: createdAt,
          needs_attention: true,
          stalled: item.stalled,
          next_action: stalledNextAction,
        },
      ],
    })
  })
  await page.route('**/v1/tasks/recovery/activity*', async (route) => {
    const current = activity('recovery', false)
    await route.fulfill({
      json: cancelled ? { ...current, task: { ...current.task, state: 'closed' }, stalled: undefined } : current,
    })
  })
  let cancelBody = ''
  await page.route('**/v1/tasks/recovery/close*', async (route) => {
    cancelBody = route.request().postData() ?? ''
    cancelled = true
    await route.fulfill({ json: { ...item.task, state: 'closed' } })
  })
  await page.goto('/')
  // An operator item stays in its stage column, styled as waiting on a person.
  const card = page.getByRole('region', { name: 'Implement' }).getByRole('link', { name: /Short task/ })
  await expect(card).toHaveAttribute('data-operator', 'true')
  await expect(card.getByText('Stalled')).toBeVisible()
  // The board card carries the shared assignee chip: the name is what shows,
  // and the account identifiers behind it stay in the tooltip.
  const assignee = card.getByTitle('Assigned to Assigned User — assigned@example.test · usr-assigned')
  await expect(assignee).toBeVisible()
  await expect(assignee).toContainText('Assigned User')
  await expect(assignee).toContainText('AU')
  await expect(card.getByText('harness exited: status 1')).toBeVisible()
  await card.click()
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
  await page.getByRole('button', { name: 'Task actions' }).click()
  await page.getByRole('menuitem', { name: 'Cancel task' }).click()
  await page.getByPlaceholder('Why is this task being cancelled?').fill('provider setup is obsolete')
  await page.getByRole('dialog', { name: 'Cancel task' }).getByRole('button', { name: 'Cancel task' }).click()
  await expect.poll(() => cancelBody).toContain('provider setup is obsolete')
  // Scoped to the sheet: the board's shared state filter behind it lists every
  // task state as an option, including this one.
  await expect(page.getByRole('dialog', { name: 'Task detail' }).getByText('Closed', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Cancel task' })).toHaveCount(0)
})

// A stale or timed-out order with a later non-cancelled same-stage successor is
// historical: the store refuses its recovery (store.WorkOrderRecoverySupersessionError),
// so the timeline must not offer it (req-task-lifecycle-and-queue REQ-6 AC-6.2).
const supersessionTaskId = 'superseded-recovery'

type SupersessionOrder = {
  id: string
  stage: 'spec' | 'implement'
  state: 'queued' | 'stale' | 'timed_out' | 'submitted' | 'completed' | 'cancelled'
  created_at?: string
  updated_at?: string
}

function supersessionActivity(orders: SupersessionOrder[], nextStage: 'spec' | 'implement' = 'spec') {
  const base = activity(supersessionTaskId, false)
  const workOrders = orders.map((order) => ({
    task_id: supersessionTaskId,
    job_id: order.id,
    claimable: order.state === 'queued',
    queue_entered_at: order.created_at ?? createdAt,
    queue_deadline: '2026-10-02T00:00:00Z',
    redispatch_count: 0,
    cost_usd: 0,
    tokens_in: 0,
    tokens_out: 0,
    usage_reported: false,
    self_reported: true,
    ...order,
    updated_at: order.updated_at ?? order.created_at,
  }))
  return {
    ...base,
    task: { ...base.task, state: 'queued', next_stage: nextStage },
    work_orders: workOrders,
    stalled: {
      needed: true,
      reason: 'work order went stale before it was claimed',
      work_order: workOrders.find((order) => order.state === 'stale' || order.state === 'timed_out'),
      last_failure: 'queue retention elapsed before an agent claimed the order',
    },
  }
}

async function routeSupersession(page: Page, orders: SupersessionOrder[], nextStage?: 'spec' | 'implement') {
  const recovered: string[] = []
  await page.route(`**/v1/tasks/${supersessionTaskId}/activity*`, (route) =>
    route.fulfill({ json: supersessionActivity(orders, nextStage) }),
  )
  await page.route('**/v1/work-orders/*/recover*', async (route) => {
    recovered.push(decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2) ?? ''))
    await route.fulfill({ json: { id: 'recovered', state: 'queued', claimable: true } })
  })
  return recovered
}

// The observed deployment shape: two plan orders went stale, then a third was
// queued and is claimable with no attempt yet.
const observedSupersession: SupersessionOrder[] = [
  { id: `${supersessionTaskId}-spec-1`, stage: 'spec', state: 'stale', created_at: '2026-09-28T09:00:00Z' },
  { id: `${supersessionTaskId}-spec-2`, stage: 'spec', state: 'stale', created_at: '2026-09-29T10:00:00.5Z' },
  { id: `${supersessionTaskId}-spec-3`, stage: 'spec', state: 'queued', created_at: '2026-09-30T12:26:00.123456Z' },
]

async function expectSupersededStaleHistory(scope: Page | Locator) {
  await expect(scope.getByText('Plan — waiting for an operator agent')).toBeVisible()
  await expect(scope.getByText('Plan — went stale in the queue')).toHaveCount(2)
  await expect(scope.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  await expect(scope.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
  await expect(scope.getByText('Plan paused — recovery needed')).toHaveCount(0)
}

test('superseded stale plan orders offer Redispatch for the queued successor instead of recovery', async ({ page }) => {
  const recovered = await routeSupersession(page, observedSupersession)
  await page.goto(`/w/demo/tasks/${supersessionTaskId}/full`)
  await expectSupersededStaleHistory(page)
  expect(recovered).toEqual([])
})

test('the board keeps a superseded task stalled while its sheet offers only Redispatch', async ({ page }) => {
  await showAllBoardTasks(page)
  const item = supersessionActivity(observedSupersession)
  await routeSupersession(page, observedSupersession)
  await page.route('**/v1/activity*', (route) =>
    route.fulfill({
      json: [
        {
          task: item.task,
          latest_stage: 'spec',
          last_event_at: createdAt,
          needs_attention: true,
          stalled: item.stalled,
          next_action: stalledNextAction,
        },
      ],
    }),
  )
  await page.goto('/')
  const card = page.getByRole('region', { name: 'Plan' }).getByRole('link', { name: /Short task/ })
  await expect(card).toHaveAttribute('data-operator', 'true')
  await expect(card.getByText('Stalled')).toBeVisible()
  await card.click()
  await expectSupersededStaleHistory(page.getByRole('dialog', { name: 'Task detail' }))
})

// A pending proposal is decided in the Inbox; the board does not reroute the
// task that authored it, so it stays in its stage column either way.
test('pending authority keeps a live task in its stage column', async ({ page }) => {
  await showAllBoardTasks(page)
  const item = activity('design-proposal', false)
  await page.route('**/v1/activity*', async (route) => {
    await route.fulfill({
      json: [
        {
          task: { ...item.task, title: 'Proposal-gated task', next_stage: 'review' },
          latest_stage: 'review',
          last_event_at: createdAt,
          needs_attention: true,
          pending_authority: true,
        },
        {
          task: { ...item.task, id: 'resolved-terminal', title: 'Completed proposal task', state: 'merged' },
          latest_stage: 'review',
          last_event_at: createdAt,
          needs_attention: true,
          pending_authority: true,
        },
      ],
    })
  })

  await page.goto('/')
  const review = page.getByRole('region', { name: 'Review' })
  await expect(review.getByText('Proposal-gated task')).toBeVisible()
  await expect(review.getByText('Completed proposal task')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Completed' }).getByText('Completed proposal task')).toBeVisible()
})

test('forge failure categories render on the board card and in task activity evidence', async ({ page }) => {
  await showAllBoardTasks(page)
  const item = activity('forge-failure', false)
  await page.route('**/v1/activity*', async (route) => {
    await route.fulfill({
      json: [
        {
          task: item.task,
          latest_stage: 'merge',
          last_event_at: '2026-07-15T12:02:00Z',
          needs_attention: true,
          forge_failure: item.forge_failure,
        },
      ],
    })
  })

  await page.goto('/')
  const card = page.getByRole('link', { name: /Short task/ })
  await expect(card.getByText('forge_rate_limited')).toBeVisible()
  await expect(card.getByText(/GitHub merge: merge pull request/)).toBeVisible()
  await card.click()
  await expect(page.getByText(/forge_request · create task issue: request timed out/)).toBeVisible()
  await expect(page.getByText(/forge_permission · publish review comment: resource not accessible/)).toBeVisible()
  await expect(page.getByText(/forge_rate_limited · merge pull request: API rate limit exceeded/)).toBeVisible()
})

test('checkout-blocked recovery explains the safe operator sequence before recovery', async ({ page }) => {
  await page.goto('/w/demo/tasks/checkout-blocked-recovery/full')

  const failure = page.getByText('The primary checkout has pre-existing changes, so Conveyor left them untouched.')
  const resolveFirst = nextActionBox(page).getByText(
    'Resolve the primary checkout changes, then retry the implementation.',
  )
  const recoveryEffect = page.getByText(/Conveyor will not clean, commit, stash, or discard them/)
  const action = page.getByRole('button', { name: 'Retry implementation' })

  await expect(failure).toBeVisible()
  await expect(executionTitle(page)).toHaveText('Implementation paused — checkout needs attention')
  await expect(resolveFirst).toBeVisible()
  await expect(recoveryEffect).toBeVisible()
  await expect(action).toBeVisible()
  await expect(action).toBeDisabled()
  await page.getByLabel('I resolved the primary checkout changes.').check()
  await expect(action).toBeEnabled()
})

test('attempt recovery keeps the later checkout blocker authoritative without a standalone attempt list', async ({
  page,
}) => {
  await page.goto('/w/demo/tasks/attempt-recovery/full')

  await expect(executionTitle(page)).toHaveText('Implementation paused — checkout needs attention')
  await expect(page.getByRole('region', { name: 'Execution attempts' })).toHaveCount(0)
  await expect(page.getByText('The provider usage or capacity limit stopped the last attempt.')).toHaveCount(0)
  await expect(
    page.getByText('The primary checkout has pre-existing changes, so Conveyor left them untouched.'),
  ).toBeVisible()
  const technicalSummary = page.getByText('Show technical activity')
  const technical = technicalSummary.locator('..')
  await technicalSummary.focus()
  await expect(technicalSummary).toBeFocused()
  await technicalSummary.press('Enter')
  await expect(technical.getByText('work_order.child_failed')).toBeVisible()
  await expect(technical.getByText('work_order.lease_renewed')).toBeVisible()
  await expect(page.getByText('harness exited before completing work order')).not.toBeVisible()
  await technical.getByText('Event payload').nth(2).click()
  await expect(
    technical.getByText('You have reached the provider usage limit. Try again later.', { exact: false }),
  ).toBeVisible()
})

test('provider-limit retry states expose only the correct action', async ({ page }) => {
  await page.goto('/w/demo/tasks/usage-retry-pending/full')
  await expect(page.getByText(/Retrying in/)).toBeVisible()
  await expect(page.getByRole('button', { name: /Retry implementation|Recover work order/ })).toHaveCount(0)
  await expect(page.getByText('The provider usage limit paused the last attempt.')).toBeVisible()

  await page.goto('/w/demo/tasks/usage-suppressed/full')
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
  const streamReady = new Promise<void>((resolve) => {
    releaseStream = resolve
  })
  await page.route('**/v1/tasks/live-pause/activity*', async (route) => {
    activityRequests++
    const item = activity('usage-suppressed', false)
    item.task.id = 'live-pause'
    item.work_orders[0].task_id = 'live-pause'
    item.events =
      activityRequests === 1
        ? [
            {
              id: 1,
              task_id: 'live-pause',
              job_id: item.work_orders[0].job_id,
              kind: 'work_order.claimed',
              actor_id: 'worker',
              actor_role: 'runner',
              payload: { attempt_id: 'live-attempt' },
              at: createdAt,
            },
          ]
        : item.events
    item.work_orders[0] =
      activityRequests === 1
        ? {
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
          }
        : item.work_orders[0]
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/tasks/live-pause/events/stream*', async (route) => {
    await streamReady
    await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
  })

  await page.goto('/w/demo/tasks/live-pause/full')
  await expect(nextActionBox(page).getByText('Implementation · Progressing')).toBeVisible()
  await expect(executionTitle(page)).toHaveCount(0)
  releaseStream()
  await expect(page.getByRole('status')).toContainText('Implementation paused — provider limit reached')
  await expect(page.getByRole('status')).toContainText('Retry the implementation after the provider limit has cleared.')
})

test('client machine activity is collapsed, refreshes, and disappears after termination', async ({ page }) => {
  let activityRequests = 0
  await page.route('**/v1/tasks/attempt-observability/activity*', async (route) => {
    activityRequests++
    const item = activity('usage-suppressed', false)
    item.task.id = 'attempt-observability'
    item.work_orders[0] = {
      ...item.work_orders[0],
      task_id: 'attempt-observability',
      state: activityRequests < 3 ? 'claimed' : 'completed',
      claimable: false,
      attempt_id: 'attempt-live',
      last_attempt_id: activityRequests < 3 ? undefined : 'attempt-live',
      activity_snapshot:
        activityRequests < 3
          ? {
              attempt_id: 'attempt-live',
              content:
                activityRequests === 1
                  ? `building dashboard\n${Array.from({ length: 20 }, (_, index) => `first output ${index}`).join('\n')}\nfirst tail`
                  : `running tests\n${Array.from({ length: 20 }, (_, index) => `latest output ${index}`).join('\n')}\nlatest tail`,
              captured_at: activityRequests === 1 ? '2026-07-15T12:01:00Z' : '2026-07-15T12:02:00Z',
            }
          : undefined,
    }
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/attempt-observability/full')
  const activityRegion = page.getByRole('region', { name: 'Client machine activity' })
  const disclosure = activityRegion.locator('details')
  const toggle = activityRegion.locator('summary')
  const output = activityRegion.locator('pre')
  await expect(page.getByRole('region', { name: 'Recent activity' })).toHaveCount(0)
  await expect(activityRegion.getByRole('heading', { name: 'Client machine activity' })).toBeVisible()
  await expect(disclosure).not.toHaveAttribute('open', '')
  await expect(output).not.toBeVisible()

  await toggle.focus()
  await toggle.press('Enter')
  await expect(disclosure).toHaveAttribute('open', '')
  await expect(output).toContainText('first tail')
  await expect(activityRegion.getByText(/Captured/)).toBeVisible()
  await expect.poll(() => output.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)

  await page.reload()
  await expect(disclosure).not.toHaveAttribute('open', '')
  await expect(output).not.toBeVisible()
  await toggle.click()
  await expect(disclosure).toHaveAttribute('open', '')
  await expect(output).toContainText('latest tail')
  await expect(output).not.toContainText('first tail')
  await expect.poll(() => output.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)

  await page.reload()
  await expect(activityRegion).toHaveCount(0)
})

// A task whose first work order ended two attempts: one truncated capture with
// text and one empty capture. The fake server sends their metadata only and
// serves each capture's content from this fixture (tests/helpers/bounded-detail).
function transcriptActivity(taskId: string, title: string) {
  const item = activity('timeout', false)
  item.task = { ...item.task, id: taskId, title }
  item.jobs = item.jobs.map((job) => ({ ...job, task_id: taskId }))
  item.work_orders[0] = {
    ...item.work_orders[0],
    task_id: taskId,
    transcript_captures: [
      {
        attempt_id: 'attempt-ended',
        content: `captured transcript tail marker ${taskId}`,
        termination_reason: 'execution deadline elapsed',
        truncated: true,
        captured_at: '2026-07-15T12:30:00Z',
      },
      {
        attempt_id: 'attempt-empty',
        content: '',
        termination_reason: 'harness exited',
        truncated: false,
        captured_at: '2026-07-15T12:40:00Z',
      },
    ],
  }
  return item
}

// Every transcript content read the page starts, and a gate that can fail or
// hold the next one (component-web-task-surfaces, Transcript disclosures).
async function routeTranscriptReads(page: Page) {
  const requests: string[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname.includes('/transcript-captures/')) requests.push(`${url.pathname}${url.search}`)
  })
  const gate = { fail: false, hold: undefined as Promise<void> | undefined }
  await page.route(/\/transcript-captures\//, async (route) => {
    if (gate.fail) {
      gate.fail = false
      await route.fulfill({ status: 500, contentType: 'text/plain', body: 'internal server error\n' })
      return
    }
    if (gate.hold) await gate.hold
    // The page may have abandoned the read while it was held.
    await route.fallback().catch(() => {})
  })
  return { requests, gate }
}

test('terminated attempt transcripts load only when their disclosure opens', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  const { requests, gate } = await routeTranscriptReads(page)
  let detailReads = 0
  await page.route('**/v1/tasks/attempt-transcript/activity*', async (route) => {
    detailReads++
    await route.fulfill({ json: transcriptActivity('attempt-transcript', 'Transcript task') })
  })
  const orderId = transcriptActivity('attempt-transcript', 'Transcript task').work_orders[0].id
  const contentPath = (attempt: string) =>
    `/v1/tasks/attempt-transcript/work-orders/${orderId}/transcript-captures/${attempt}?workspace_id=demo`

  await page.goto('/w/demo/tasks/attempt-transcript/full')
  const ended = page.locator('details').filter({ hasText: 'Attempt attempt-ended transcript' })
  const empty = page.locator('details').filter({ hasText: 'Attempt attempt-empty transcript' })
  await expect(ended).toBeVisible()
  await expect(empty).toBeVisible()
  await expect(page.getByText('captured transcript tail marker')).toHaveCount(0)

  // A closed disclosure sends no read, through a detail refetch and focus.
  await expect.poll(() => streams.countFor('attempt-transcript')).toBeGreaterThan(0)
  await streams.open()
  await streams.send(activityFrame)
  await expect.poll(() => detailReads).toBe(2)
  await page.evaluate(() => {
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
  })
  await expect(ended).toBeVisible()
  expect(requests).toEqual([])

  // Opening reads only that attempt in the explicit workspace; the metadata
  // lines render with it.
  await ended.locator('summary').click()
  await expect(ended.getByText('captured transcript tail marker attempt-transcript')).toBeVisible()
  await expect(ended.getByText('Ended: execution deadline elapsed')).toBeVisible()
  await expect(ended.getByText('This transcript was truncated; the most recent output is shown.')).toBeVisible()
  expect(requests).toEqual([contentPath('attempt-ended')])

  // Empty content says so, without a truncation notice.
  await empty.locator('summary').click()
  await expect(empty.getByText('No transcript output was captured.')).toBeVisible()
  await expect(empty.getByText('Ended: harness exited')).toBeVisible()
  await expect(empty.getByText('This transcript was truncated', { exact: false })).toHaveCount(0)
  expect(requests).toEqual([contentPath('attempt-ended'), contentPath('attempt-empty')])

  // Closing and reopening reads again; a failure offers Retry.
  await ended.locator('summary').click()
  await expect(ended.getByText('captured transcript tail marker attempt-transcript')).toHaveCount(0)
  gate.fail = true
  await ended.locator('summary').click()
  await expect(ended.getByRole('alert')).toContainText('Could not load the transcript')
  await ended.getByRole('button', { name: 'Retry' }).click()
  await expect(ended.getByText('captured transcript tail marker attempt-transcript')).toBeVisible()
  expect(requests.filter((path) => path === contentPath('attempt-ended'))).toHaveLength(3)

  // A task with no captures renders no disclosure.
  await page.goto('/w/demo/tasks/timeout/full')
  await expect(page.getByText(/Attempt .* transcript/)).toHaveCount(0)
})

test('a task change during a pending transcript read drops the late response', async ({ page }) => {
  const { requests, gate } = await routeTranscriptReads(page)
  await page.route(/\/v1\/tasks\/transcript-owner-[ab]\/activity/, async (route) => {
    const taskId = new URL(route.request().url()).pathname.split('/')[3]
    await route.fulfill({
      json:
        taskId === 'transcript-owner-a'
          ? transcriptActivity(taskId, 'First transcript owner')
          : {
              ...activity('timeout', false),
              task: { ...activity('timeout', false).task, id: taskId, title: 'Second transcript owner' },
            },
    })
  })
  await page.route('**/v1/activity*', (route) => {
    const first = transcriptActivity('transcript-owner-a', 'First transcript owner')
    const second = activity('timeout', false)
    return route.fulfill({
      json: [
        { task: first.task, latest_stage: 'implement', last_event_at: '2026-07-15T12:02:00Z', needs_attention: false },
        {
          task: { ...second.task, id: 'transcript-owner-b', title: 'Second transcript owner' },
          latest_stage: 'implement',
          last_event_at: '2026-07-15T12:01:00Z',
          needs_attention: false,
        },
      ],
    })
  })
  // Navigation follows the Board page, so the move happens on the Board sheet.
  await page.goto('/w/demo/tasks/transcript-owner-a')
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  const ended = sheet.locator('details').filter({ hasText: 'Attempt attempt-ended transcript' })
  await expect(ended).toBeVisible()
  expect(requests).toEqual([])
  const orderId = transcriptActivity('transcript-owner-a', 'First transcript owner').work_orders[0].id
  await ended.locator('summary').click()
  await expect(ended.getByText('captured transcript tail marker transcript-owner-a')).toBeVisible()
  await expect(ended.getByText('This transcript was truncated; the most recent output is shown.')).toBeVisible()
  expect(requests).toEqual([
    `/v1/tasks/transcript-owner-a/work-orders/${orderId}/transcript-captures/attempt-ended?workspace_id=demo`,
  ])
  await ended.locator('summary').click()

  // Reopen with the read held, then move to the next task before it answers.
  let release = () => {}
  gate.hold = new Promise<void>((resolve) => {
    release = resolve
  })
  await ended.locator('summary').click()
  await expect(ended.getByRole('status')).toHaveText('Loading transcript…')
  await expect.poll(() => requests.length).toBe(2)

  await sheet.locator('header').first().getByRole('link', { name: 'Next task' }).click()
  await expect(sheet.locator('header').first()).toContainText('Second transcript owner')
  release()
  // The held response for the first task never renders on the second.
  await expect(sheet.getByText('captured transcript tail marker')).toHaveCount(0)
  await expect(sheet.getByText(/Attempt .* transcript/)).toHaveCount(0)
  expect(requests).toHaveLength(2)
})

test('timed-out review round exposes a reasoned full-round retry and preserves history', async ({ page }) => {
  let retryRequest = ''
  await page.route('**/v1/tasks/*/review-round/retry*', async (route) => {
    retryRequest = route.request().postData() ?? ''
    await route.fulfill({
      json: {
        request_id: 'retry-1',
        task_id: 'review-retry',
        prior_round: 1,
        new_round: 2,
        pr_head: 'abc',
        work_orders: [{ id: 'seat-1' }, { id: 'seat-2' }],
      },
    })
  })
  await page.goto('/w/demo/tasks/review-retry/full')
  await expect(page.getByText('Review round 1 needs operator attention')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Review round recovery' })).not.toHaveClass(/bg-attention-soft/)
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

test('contradictory completed review round surfaces a needs-operator retry card', async ({ page }) => {
  await page.goto('/w/demo/tasks/review-wedge/full')
  await expect(page.getByText('Review round 2 needs operator attention')).toBeVisible()
  await expect(page.getByText(/completed verdict with contradictory child_failure outcome/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry review round' })).toBeDisabled()
})

test('interrupted review round offers one same-round recovery and retains completed verdicts', async ({ page }) => {
  let recoveryRequest = ''
  await page.route('**/v1/tasks/*/review-round/recover*', async (route) => {
    recoveryRequest = route.request().postData() ?? ''
    await route.fulfill({
      json: {
        request_id: 'recover-1',
        task_id: 'interrupted-review',
        review_round: 1,
        recovered_orders: [{ id: 'seat-2' }],
        retained_orders: [{ id: 'seat-1' }],
      },
    })
  })
  await page.goto('/w/demo/tasks/interrupted-review/full')
  await expect(page.getByRole('region', { name: 'Interrupted review round recovery' })).not.toHaveClass(
    /bg-attention-soft/,
  )
  await expect(page.getByText('No healthy enrolled worker can serve this queued work')).toBeVisible()
  await expect(page.getByText(/This work was interrupted/)).toBeVisible()
  await expect(page.getByText(/completed verdict retained/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
  const action = page.getByRole('button', { name: 'Recover interrupted review round' })
  await expect(action).toHaveCount(1)
  await action.click()
  await expect.poll(() => recoveryRequest).toContain('request_id')
  await expect(page.getByText(/Recovered 1 interrupted seat; 1 completed verdict retained/)).toBeVisible()
})

test('merged task retains historical approval without interrupted review recovery', async ({ page }) => {
  await page.goto('/w/demo/tasks/merged-retained-review/full')
  await expect(page.getByText('Retain this verdict.')).toBeVisible()
  await expect(page.getByTitle('Completed verdict')).toHaveText('Approved')
  await expect(page.getByRole('button', { name: 'Recover interrupted review round' })).toHaveCount(0)
})

test('interrupted review recovery tolerates legacy null empty collections', async ({ page }) => {
  await page.goto('/w/demo/tasks/interrupted-review-empty/full')
  await expect(page.getByText(/interrupted-review-empty-review-1-seat-1/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Recover interrupted review round' })).toHaveCount(1)
  await expect(page.getByText('Something went wrong!')).toHaveCount(0)
})

test('timeout timeline and duration use the execution deadline', async ({ page }) => {
  await page.goto('/w/demo/tasks/timeout/full')
  const expectedDeadline = await page.evaluate(() =>
    new Date('2026-07-15T12:30:00Z').toLocaleString('en', {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    }),
  )
  const timeoutEntry = page.locator('li').filter({ hasText: 'Review — timed out' })
  await expect(timeoutEntry.getByText(expectedDeadline)).toBeVisible()
  await expect(page.locator('article').filter({ hasText: 'claude-review' }).getByText('30m 00s')).toBeVisible()
})

test('work-order cards use their actual stage and technical activity exposes captured failure detail', async ({
  page,
}) => {
  await page.goto('/w/demo/tasks/stage-aware/full')
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Plan — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Implementation — in progress', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Review — timed out', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Review — waiting for an operator agent', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Execution attempts' })).toHaveCount(0)
  const technical = page.getByText('Show technical activity').locator('..')
  await technical.getByText('Show technical activity').click()
  await technical.getByText('Event payload').click()
  await expect(technical.getByText('provider rejected the configured model', { exact: false })).toBeVisible()
})

for (const [modelName, iconFile] of [
  ['gpt-implement-with-an-intentionally-long-model-name', 'openai'],
  ['claude-sonnet-4', 'claude-color'],
  ['gemini-2.5-pro', 'gemini-color'],
  ['grok-4', 'grok'],
  ['opencode-go/glm-5.3-flash', 'zhipu-color'],
  ['OpenCode-Go/GLM-5.3-FLASH', 'zhipu-color'],
  ['glm-5', 'zhipu-color'],
  ['chatglm3', 'zhipu-color'],
  ['zhipu/model', 'zhipu-color'],
  ['unknown/model', undefined],
  ['unknown/notglm-5', undefined],
  ['unknown/glmodel', undefined],
] as const) {
  test(`implementation job cards preserve provider logo and narrow header meta for ${modelName}`, async ({ page }) => {
    await page.setViewportSize({ width: 480, height: 720 })
    await page.route('**/v1/tasks/implementation-card/activity*', async (route) => {
      const item = activity('stage-aware', false)
      item.task.id = 'implementation-card'
      item.jobs = [
        {
          id: 'implementation-card-implement-1',
          task_id: 'implementation-card',
          stage: 'implement',
          harness: 'codex',
          model_tier: modelName,
          runner: 'worker',
          confinement: 'none',
          cost_usd: 0,
          tokens_in: 0,
          tokens_out: 0,
          state: 'done',
          started_at: createdAt,
          ended_at: '2026-07-15T12:01:00Z',
        },
      ]
      item.work_orders = [
        {
          ...item.work_orders[1],
          id: 'implementation-card-implement-1',
          task_id: 'implementation-card',
          job_id: 'implementation-card-implement-1',
          state: 'completed',
          required_model: modelName,
        },
      ]
      item.events = [
        {
          id: 1,
          task_id: 'implementation-card',
          job_id: 'implementation-card-implement-1',
          kind: 'job.summary',
          actor_id: 'worker',
          actor_role: 'runner',
          payload: { summary: 'Implementation completed without reported usage.' },
          at: '2026-07-15T12:01:00Z',
        },
      ]
      await route.fulfill({ json: item })
    })

    await page.goto('/w/demo/tasks/implementation-card/full')
    // Model and duration sit in the entry's header line; the entry has no
    // footer (component-web-task-surfaces, Work-order timeline entries).
    const card = page.locator('article').filter({ hasText: 'Implementation completed without reported usage.' })
    const header = card.locator('[data-job-header]')
    const duration = header.locator('[data-job-duration]')
    const model = duration.locator('xpath=preceding-sibling::span[1]')

    await expect(card.locator('footer')).toHaveCount(0)
    await expect(header.getByText('Implementation', { exact: true })).toBeVisible()
    await expect(duration).toHaveText('1m 00s')
    await expect(model.getByText(modelName, { exact: true })).toBeVisible()
    await expect(model.getByText(modelName, { exact: true })).toHaveAttribute('title', modelName)
    if (iconFile) {
      const asset = readFileSync(
        new URL(`../node_modules/@lobehub/icons-static-svg/icons/${iconFile}.svg`, import.meta.url),
        'utf8',
      )
      const expectedPaths = Array.from(asset.matchAll(/\sd="([^"]+)"/g), (match) => match[1])
      const logo = model.locator('span[aria-hidden="true"]')
      await expect(logo).toBeVisible()
      await expect(logo.locator('svg')).toHaveCount(1)
      expect(await logo.locator('path').evaluateAll((paths) => paths.map((path) => path.getAttribute('d')))).toEqual(
        expectedPaths,
      )
      await expect(model.locator('.lucide-cpu')).toHaveCount(0)
    } else {
      await expect(model.locator('.lucide-cpu')).toBeVisible()
      await expect(model.locator('.lucide-cpu')).toHaveAttribute('aria-hidden', 'true')
      await expect(model.locator('span[aria-hidden="true"]')).toHaveCount(0)
    }
    const unavailableUsage = header.getByText('Usage unavailable', { exact: true })
    await expect(unavailableUsage).toHaveCount(1)
    await expect(unavailableUsage).toHaveAttribute('role', 'tooltip')
    await expect(unavailableUsage).toHaveCSS('opacity', '0')

    // Model, then duration, inside the header line with nothing overflowing.
    const layout = await header.evaluate((element) => {
      const headerBox = element.getBoundingClientRect()
      const durationBox = element.querySelector('[data-job-duration]')!.getBoundingClientRect()
      const modelBox = (
        element.querySelector('[data-job-duration]')!.previousElementSibling as HTMLElement
      ).getBoundingClientRect()
      return {
        ordered: modelBox.right <= durationBox.left + 1,
        inside: modelBox.left >= headerBox.left - 0.5 && durationBox.right <= headerBox.right + 0.5,
        overflows: element.scrollWidth > element.clientWidth,
      }
    })
    expect(layout.ordered).toBe(true)
    expect(layout.inside).toBe(true)
    expect(layout.overflows).toBe(false)
  })
}

test('task detail aggregates reported work-order usage and distinguishes reported zero from unavailable', async ({
  page,
}) => {
  await page.route('**/v1/tasks/usage-telemetry/activity*', async (route) => {
    const item = activity('stage-aware', false)
    item.task.id = 'usage-telemetry'
    item.work_orders = item.work_orders.map((order, index) => ({
      ...order,
      task_id: 'usage-telemetry',
      usage_reported: index < 2,
      self_reported: index === 0,
      tokens_in: index === 0 ? 1200 : 0,
      tokens_out: index === 0 ? 300 : 0,
      cost_usd: 1.25,
    }))
    item.events = [
      {
        id: 1,
        task_id: 'usage-telemetry',
        job_id: item.work_orders[0].job_id,
        kind: 'work_order.usage_reported',
        actor_id: 'spec-agent',
        actor_role: 'runner',
        payload: {
          work_order_id: item.work_orders[0].id,
          tokens_in: 1200,
          tokens_out: 300,
          cost_usd: 1.25,
        },
        at: createdAt,
      },
      {
        id: 2,
        task_id: 'usage-telemetry',
        job_id: item.work_orders[1].job_id,
        kind: 'work_order.usage_reported',
        actor_id: 'implement-agent',
        actor_role: 'runner',
        payload: { work_order_id: item.work_orders[1].id, tokens_in: 0, tokens_out: 0, cost_usd: 0 },
        at: '2026-07-15T12:01:00Z',
      },
    ]
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/usage-telemetry/full')
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('1.2k in / 300 out · self-reported')).toBeVisible()
  await expect(timeline.getByText('0 in / 0 out', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Usage unavailable')).toBeVisible()
  await expect(timeline.getByText(/\$\d/)).toHaveCount(0)
  await expect(timeline.getByText('worker-reported', { exact: false })).toHaveCount(0)
  const reviewPanel = timeline.locator('article').filter({ hasText: 'Panel of' })
  await expect(reviewPanel.getByText(/\$\d/)).toHaveCount(0)
  await expect(timeline.getByRole('tooltip').filter({ hasText: /\$\d/ })).toHaveCount(0)
  await expect(timeline.getByRole('tooltip').filter({ hasText: 'worker-reported' })).toHaveCount(0)

  const technical = page.getByText('Show technical activity').locator('..')
  await technical.getByText('Show technical activity').click()
  await expect(technical.getByRole('region', { name: 'Task usage telemetry' })).toContainText(
    '1.2k in / 300 out across 2 reported work orders · 1 order unavailable',
  )
  await expect(technical.getByRole('list', { name: 'Work-order usage' })).toContainText('0 in / 0 out')
  await expect(technical.getByRole('list', { name: 'Work-order usage' })).toContainText('Usage unavailable')
  await expect(technical.getByText(/\$\d/)).toHaveCount(0)
  await expect(technical.getByText('worker-reported', { exact: false })).toHaveCount(0)
})

test('in-process jobs retain provider usage when the matching work order has no telemetry report', async ({ page }) => {
  await page.route('**/v1/tasks/provider-usage/activity*', async (route) => {
    const item = activity('stage-aware', false)
    item.task.id = 'provider-usage'
    item.jobs = [
      {
        id: 'provider-usage-spec',
        task_id: 'provider-usage',
        stage: 'spec',
        harness: 'codex',
        model_tier: 'gpt-spec',
        runner: 'in-process',
        confinement: 'none',
        cost_usd: 0,
        tokens_in: 17,
        tokens_out: 3,
        state: 'done',
        started_at: createdAt,
        ended_at: '2026-07-15T12:01:00Z',
      },
    ]
    item.work_orders = [
      {
        ...item.work_orders[0],
        id: 'provider-usage-spec',
        task_id: 'provider-usage',
        job_id: 'provider-usage-spec',
        state: 'completed',
      },
    ]
    item.events = [
      {
        id: 1,
        task_id: 'provider-usage',
        job_id: 'provider-usage-spec',
        kind: 'job.summary',
        actor_id: 'runner',
        actor_role: 'runner',
        payload: { summary: 'Provider usage remains attached to this in-process attempt.' },
        at: '2026-07-15T12:01:00Z',
      },
    ]
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/provider-usage/full')
  const job = page.locator('article').filter({ hasText: 'Provider usage remains attached' })
  await expect(job).toContainText('gpt-spec')
  await expect(job.getByRole('tooltip')).toContainText('17 in / 3 out · provider-reported')
  await expect(job).not.toContainText('Usage unavailable')
  await expect(job).not.toContainText('$0.00')
})

test('overflowing full-screen task content scrolls from top to bottom', async ({ page }) => {
  await page.goto('/w/demo/tasks/overflowing/full')

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

  // The plan previews six lines on the full route too; open it so the marker
  // at its end is part of the scroll.
  await page.getByRole('button', { name: 'Show full plan' }).click()
  await content.hover()
  await page.mouse.wheel(0, 500)
  await expect.poll(() => content.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)

  await content.focus()
  await page.keyboard.press('End')
  await expect(marker).toBeInViewport()
  await expect
    .poll(() => content.evaluate((element) => element.scrollTop + element.clientHeight))
    .toBeGreaterThanOrEqual(await content.evaluate((element) => element.scrollHeight - 1))
})

test('short full-screen task content does not create a nested vertical scrollbar', async ({ page }) => {
  await page.goto('/w/demo/tasks/short/full')

  const content = page.getByRole('region', { name: 'Task content' })
  await expect(content).toBeVisible()
  const dimensions = await content.evaluate((element) => ({
    clientHeight: element.clientHeight,
    scrollHeight: element.scrollHeight,
  }))
  expect(dimensions.scrollHeight).toBeLessThanOrEqual(dimensions.clientHeight)
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

test('task context attachment keeps archived documents out of the picker', async ({ page }) => {
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const requirementRequest = page.waitForRequest((request) => new URL(request.url()).pathname === '/v1/requirements')
  const designRequest = page.waitForRequest((request) => new URL(request.url()).pathname === '/v1/system-designs')
  await page.getByRole('button', { name: 'Attach context' }).click()
  const [requirements, designs] = await Promise.all([requirementRequest, designRequest])
  expect(new URL(requirements.url()).searchParams.has('include_archived')).toBe(false)
  expect(new URL(designs.url()).searchParams.has('include_archived')).toBe(false)
  const dialog = page.getByRole('dialog', { name: 'Manage task context' })
  await dialog.getByRole('combobox', { name: 'Search context' }).click()
  await expect(dialog.getByText('No confirmed documents yet.')).toBeVisible()
  await expect(dialog.getByText(/Archived requirement|Archived design/)).toHaveCount(0)
})

type ContextChange = {
  add: { requirement_ids?: string[]; system_design_ids?: string[] }
  remove: { requirement_ids?: string[]; system_design_ids?: string[] }
}

async function mockContextManagement(
  page: Page,
  options: { archived?: boolean; state?: string; failure?: boolean } = {},
) {
  const item = activity('operator-checkpoint', false)
  item.task.state = options.state ?? 'running'
  item.task.context = {
    requirements: [{ id: 'req-attached', title: 'Attached requirement', version: 3, archived: options.archived }],
    designs: [{ id: 'design-attached', title: 'Attached design', version: 2, archived: options.archived }],
  }
  const changes: ContextChange[] = []
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.route('**/v1/requirements?*', (route) =>
    route.fulfill({
      json: [
        ...(!options.archived
          ? [
              {
                requirement: { id: 'req-attached', title: 'Attached requirement', archived: false },
                current_version: { version: 3 },
              },
            ]
          : []),
        { requirement: { id: 'req-new', title: 'New requirement', archived: false }, current_version: { version: 1 } },
        {
          requirement: { id: 'req-archived', title: 'Archived picker requirement', archived: true },
          current_version: { version: 1 },
        },
      ],
    }),
  )
  await page.route('**/v1/system-designs?*', (route) =>
    route.fulfill({
      json: [
        ...(!options.archived
          ? [
              {
                document: { id: 'design-attached', title: 'Attached design', archived: false },
                current_version: { version: 2 },
              },
            ]
          : []),
        { document: { id: 'design-new', title: 'New design', archived: false }, current_version: { version: 1 } },
        {
          document: { id: 'design-archived', title: 'Archived picker design', archived: true },
          current_version: { version: 1 },
        },
      ],
    }),
  )
  await page.route('**/v1/tasks/operator-checkpoint/context?*', async (route) => {
    const change = route.request().postDataJSON() as ContextChange
    changes.push(change)
    if (options.failure) {
      await route.fulfill({
        status: 409,
        json: { error: 'task_terminal', message: 'Task is terminal; context cannot be changed.' },
      })
      return
    }
    item.task.context = {
      requirements: [
        ...(item.task.context?.requirements ?? []).filter((doc) => !change.remove.requirement_ids?.includes(doc.id)),
        ...(change.add.requirement_ids ?? []).map((id) => ({ id, title: 'New requirement', version: 1 })),
      ],
      designs: [
        ...(item.task.context?.designs ?? []).filter((doc) => !change.remove.system_design_ids?.includes(doc.id)),
        ...(change.add.system_design_ids ?? []).map((id) => ({ id, title: 'New design', version: 1 })),
      ],
    }
    await route.fulfill({ json: item.task.context })
  })
  return { changes }
}

for (const archived of [false, true]) {
  test(`task context dialog removes both document tiers${archived ? ' including archived attachments' : ''}`, async ({
    page,
  }) => {
    const { changes } = await mockContextManagement(page, { archived })
    await page.goto('/w/demo/tasks/operator-checkpoint/full')
    await page.getByRole('button', { name: 'Attach context', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Manage task context' })
    await expect(dialog.getByRole('button', { name: 'Attach selected context' })).toBeDisabled()
    if (archived) await expect(dialog.getByText('Archived', { exact: true })).toHaveCount(2)
    await dialog.getByRole('combobox').click()
    await expect(dialog.getByRole('option', { name: /Archived picker/ })).toHaveCount(0)
    await dialog.getByRole('button', { name: 'Remove context Attached requirement', exact: true }).click()
    await dialog.getByRole('button', { name: 'Remove context Attached design', exact: true }).click()
    await dialog.getByRole('button', { name: 'Save context', exact: true }).click()
    await expect(dialog).toHaveCount(0)
    expect(changes).toEqual([
      {
        add: { requirement_ids: [], system_design_ids: [] },
        remove: { requirement_ids: ['req-attached'], system_design_ids: ['design-attached'] },
      },
    ])
    await expect(page.getByRole('region', { name: 'Attached context' })).toHaveCount(0)
  })
}

test('task context dialog saves disjoint additions and removals', async ({ page }) => {
  const { changes } = await mockContextManagement(page)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await page.getByRole('button', { name: 'Attach context', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Manage task context' })
  await dialog.getByRole('button', { name: 'Remove context Attached requirement', exact: true }).click()
  await dialog.getByRole('button', { name: 'Remove context Attached design', exact: true }).click()
  await dialog.getByRole('combobox').click()
  await dialog.getByRole('option', { name: /New requirement/ }).click()
  await dialog.getByRole('option', { name: /New design/ }).click()
  await dialog.getByRole('combobox').press('Escape')
  await dialog.getByRole('button', { name: 'Save context', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(changes).toEqual([
    {
      add: { requirement_ids: ['req-new'], system_design_ids: ['design-new'] },
      remove: { requirement_ids: ['req-attached'], system_design_ids: ['design-attached'] },
    },
  ])
  const card = page.getByRole('region', { name: 'Attached context' })
  await expect(card.getByRole('link', { name: 'New requirement' })).toBeVisible()
  await expect(card.getByRole('link', { name: 'New design' })).toBeVisible()
  await expect(card.getByRole('link', { name: /Attached/ })).toHaveCount(0)
})

test('task context dialog cancels removals by keeping or reselecting attached documents', async ({ page }) => {
  const { changes } = await mockContextManagement(page)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await page.getByRole('button', { name: 'Attach context', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Manage task context' })
  for (const title of ['Attached requirement', 'Attached design']) {
    await dialog.getByRole('button', { name: `Remove context ${title}`, exact: true }).click()
    await dialog.getByRole('button', { name: `Keep context ${title}`, exact: true }).click()
    await expect(dialog.getByRole('button', { name: 'Attach selected context' })).toBeDisabled()
    await dialog.getByRole('button', { name: `Remove context ${title}`, exact: true }).click()
    await dialog.getByRole('combobox').click()
    await dialog.getByRole('option', { name: new RegExp(title) }).click()
    await dialog.getByRole('combobox').press('Escape')
    await expect(dialog.getByRole('button', { name: 'Attach selected context' })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: `Remove context ${title}`, exact: true })).toBeVisible()
  }
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  expect(changes).toEqual([])
})

test('task context dialog still saves additions alone', async ({ page }) => {
  const { changes } = await mockContextManagement(page)
  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  await page.getByRole('button', { name: 'Attach context', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Manage task context' })
  await dialog.getByRole('combobox').click()
  await dialog.getByRole('option', { name: /New requirement/ }).click()
  await dialog.getByRole('combobox').press('Escape')
  await dialog.getByRole('button', { name: 'Attach selected context' }).click()
  await expect(dialog).toHaveCount(0)
  expect(changes).toEqual([
    {
      add: { requirement_ids: ['req-new'], system_design_ids: [] },
      remove: { requirement_ids: [], system_design_ids: [] },
    },
  ])
  await expect(
    page.getByRole('region', { name: 'Attached context' }).getByRole('link', { name: 'New requirement' }),
  ).toBeVisible()
})

for (const surface of ['full', 'sheet']) {
  for (const archived of [false, true]) {
    test(`task context card confirms removal of both tiers on ${surface}${archived ? ' when archived' : ''}`, async ({
      page,
    }) => {
      await page.setViewportSize({ width: 1100, height: 900 })
      const { changes } = await mockContextManagement(page, { archived })
      await page.goto(`/w/demo/tasks/operator-checkpoint${surface === 'full' ? '/full' : ''}`)
      const card = page.getByRole('region', { name: 'Attached context' })
      for (const [title, version] of [
        ['Attached requirement', 3],
        ['Attached design', 2],
      ] as const) {
        await card.getByRole('button', { name: `Remove context ${title}`, exact: true }).click()
        const dialog = page.getByRole('dialog', { name: 'Remove task context' })
        await expect(dialog).toContainText(`${title} · v${version}`)
        const count = changes.length
        await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
        expect(changes).toHaveLength(count)
        await expect(card.getByRole('link', { name: title })).toBeVisible()
        await card.getByRole('button', { name: `Remove context ${title}`, exact: true }).click()
        await dialog.getByRole('button', { name: 'Remove context', exact: true }).click()
        await expect(dialog).toHaveCount(0)
        await expect(card.getByRole('link', { name: title })).toHaveCount(0)
      }
      expect(changes).toEqual([
        { add: {}, remove: { requirement_ids: ['req-attached'] } },
        { add: {}, remove: { system_design_ids: ['design-attached'] } },
      ])
    })
  }
}

for (const state of ['running', 'merged', 'closed']) {
  for (const surface of ['full', 'sheet']) {
    test(`task context removal is hidden on ${surface} for ${state === 'running' ? 'a viewer' : state + ' tasks'}`, async ({
      page,
    }) => {
      await mockContextManagement(page, { state })
      if (state === 'running')
        await page.route('**/v1/me**', (route) =>
          route.fulfill({ json: callerIdentity({ id: 'usr_viewer', role: 'viewer' }, route.request()) }),
        )
      await page.goto(`/w/demo/tasks/operator-checkpoint${surface === 'full' ? '/full' : ''}`)
      await expect(page.getByRole('region', { name: 'Attached context' })).toBeVisible()
      await expect(page.getByRole('button', { name: /^Remove context/ })).toHaveCount(0)
      // A terminal task has no current order (server next action), so no recovery card offers it.
      await expect(page.getByRole('button', { name: 'Attach context', exact: true })).toHaveCount(0)
    })
  }
}

for (const surface of ['dialog', 'card']) {
  test(`task context ${surface} preserves attachments and reports a refused removal`, async ({ page }) => {
    const { changes } = await mockContextManagement(page, { failure: true })
    await page.goto('/w/demo/tasks/operator-checkpoint/full')
    const card = page.getByRole('region', { name: 'Attached context' })
    if (surface === 'dialog') {
      await page.getByRole('button', { name: 'Attach context', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: 'Manage task context' })
      await dialog.getByRole('button', { name: 'Remove context Attached requirement', exact: true }).click()
      await dialog.getByRole('button', { name: 'Save context', exact: true }).click()
    } else {
      await card.getByRole('button', { name: 'Remove context Attached requirement', exact: true }).click()
      await page.getByRole('dialog').getByRole('button', { name: 'Remove context', exact: true }).click()
    }
    await expect(page.getByRole('dialog')).toContainText('Task is terminal; context cannot be changed.')
    await expect(card.getByRole('link', { name: 'Attached requirement' })).toBeVisible()
    expect(changes).toHaveLength(1)
  })
}

test('task context suggestions show justification and become attachments or disappear when resolved', async ({
  page,
}) => {
  const taskId = 'context-suggestions'
  let requirementConfirmed = false
  let requirementPending = true
  let designPending = true
  const proposal = (
    target_kind: ContextProposalFixture['target_kind'],
    target_id: string,
    target_title: string,
    source: ContextProposalFixture['source'],
    justification: string,
  ): ContextProposalFixture => ({
    task_id: taskId,
    target_kind,
    target_id,
    target_title,
    state: 'proposed',
    source,
    justification,
    created_by_event_id: 42,
    proposed_by: 'system',
    workspace: 'demo',
    created_at: createdAt,
    updated_at: createdAt,
  })

  await page.route('**/v1/tasks/context-suggestions/activity*', (route) => {
    const item = activity(taskId, false)
    item.task.context = {
      requirements: requirementConfirmed ? [{ id: 'req-intake', title: 'Task intake and triage', version: 4 }] : [],
      designs: [],
      proposals: [
        ...(requirementPending
          ? [
              proposal(
                'requirement',
                'req-intake',
                'Task intake and triage',
                'triage',
                'The task explicitly changes operator intake behavior.',
              ),
            ]
          : []),
        ...(designPending
          ? [
              proposal(
                'system_design',
                'design-web-dashboard',
                'Web dashboard',
                'planning',
                'The dashboard interaction inventory governs this surface.',
              ),
            ]
          : []),
      ],
    }
    return route.fulfill({ json: item })
  })
  await page.route('**/v1/tasks/context-suggestions/context/proposals/requirement/req-intake/confirm*', (route) => {
    expect(route.request().headers().authorization).toBeUndefined()
    requirementPending = false
    requirementConfirmed = true
    return route.fulfill({ json: {} })
  })
  await page.route(
    '**/v1/tasks/context-suggestions/context/proposals/system_design/design-web-dashboard/dismiss*',
    (route) => {
      expect(route.request().headers().authorization).toBeUndefined()
      designPending = false
      return route.fulfill({ json: {} })
    },
  )

  await page.goto(`/w/demo/tasks/${taskId}`)
  const sheetSuggestions = page.getByRole('region', { name: 'Suggested context' })
  await expect(sheetSuggestions).toContainText('Task intake and triage')
  await expect(sheetSuggestions).toContainText('The dashboard interaction inventory governs this surface.')

  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const suggestions = page.getByRole('region', { name: 'Suggested context' })
  await expect(suggestions).toContainText('Task intake and triage')
  await expect(suggestions).toContainText('From task intake')
  await expect(suggestions).toContainText('The task explicitly changes operator intake behavior.')
  await expect(suggestions).toContainText('The dashboard interaction inventory governs this surface.')

  // Suggested context only informs; the next-action card's server choices
  // decide each proposal, once (component-web-task-surfaces, Next-action card).
  await expect(suggestions.getByRole('button')).toHaveCount(0)
  const nextAction = page.getByRole('region', { name: 'Next action' })
  const choices = nextAction.locator('[data-next-action-choices]')
  await expect(choices.getByRole('button')).toHaveCount(4)
  for (const button of await choices.getByRole('button').all()) await expect(button).toBeEnabled()
  await expect(nextAction).not.toContainText('cannot send this choice')

  const confirm = page.waitForRequest(
    (request) =>
      request.method() === 'POST' &&
      new URL(request.url()).pathname === `/v1/tasks/${taskId}/context/proposals/requirement/req-intake/confirm`,
  )
  await choices.getByRole('button', { name: 'Confirm context requirement req-intake' }).click()
  expect((await confirm).postData()).toBeNull()
  const attached = page.getByRole('region', { name: 'Attached context' })
  await expect(attached.getByRole('link', { name: 'Task intake and triage' })).toBeVisible()
  await expect(suggestions).not.toContainText('The task explicitly changes operator intake behavior.')

  await choices.getByRole('button', { name: 'Dismiss context system_design design-web-dashboard' }).click()
  await expect(suggestions).toHaveCount(0)
  await expect(nextAction).not.toContainText('Context proposal')
})

test('task context suggestions remain visible without decision controls for non-operators', async ({ page }) => {
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_viewer', role: 'contributor' }, route.request()) }),
  )
  await page.route('**/v1/tasks/context-suggestion-viewer/activity*', (route) => {
    const item = activity('context-suggestion-viewer', false)
    item.task.context = {
      proposals: [
        {
          task_id: 'context-suggestion-viewer',
          target_kind: 'requirement',
          target_id: 'req-intake',
          target_title: 'Task intake and triage',
          state: 'proposed',
          source: 'operator',
          justification: 'Useful context remains readable to collaborators.',
          created_by_event_id: 43,
          proposed_by: 'usr_operator',
          workspace: 'demo',
          created_at: createdAt,
          updated_at: createdAt,
        },
      ],
    }
    return route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/context-suggestion-viewer/full')
  const suggestions = page.getByRole('region', { name: 'Suggested context' })
  await expect(suggestions).toContainText('Useful context remains readable to collaborators.')
  await expect(suggestions.getByRole('button', { name: 'Attach context' })).toHaveCount(0)
  await expect(suggestions.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('task sheet bounds an overflowing spec and expands and collapses it accessibly', async ({ page }) => {
  await page.goto('/w/demo/tasks/long-spec')

  const toggle = page.getByRole('button', { name: 'Show full plan' })
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
  await expect(page.getByRole('button', { name: 'Show full plan' })).toHaveAttribute('aria-expanded', 'false')
  await expect(page.locator('[data-spec-overflow-shadow]')).toBeVisible()
  await expect.poll(() => viewport.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
})

test('task body renders safe GFM in sheet and full-page headers', async ({ page }) => {
  for (const path of ['/w/demo/tasks/markdown-body', '/w/demo/tasks/markdown-body/full']) {
    await page.goto(path)
    await expect(page.getByRole('heading', { name: 'Structured context' })).toBeVisible()
    await expect(page.getByText('clear', { exact: true })).toHaveCSS('font-weight', /^(600|700)$/)
    await expect(page.getByRole('link', { name: 'safe link' })).toHaveAttribute('href', 'https://example.test')
    await expect(page.getByText('completed item')).toBeVisible()
    await expect(page.locator('script[data-unsafe]')).toHaveCount(0)
    await expect(page.getByText('<script data-unsafe>window.hacked = true</script>')).toBeVisible()
  }
})

test('activity job summaries render inline code without enabling raw HTML', async ({ page }) => {
  await page.goto('/w/demo/tasks/markdown-summary/full')

  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const summary = timeline.locator('article').filter({ has: page.getByRole('heading', { name: 'Validation' }) })
  await expect(summary).toHaveCount(1)
  await expect(summary.locator('.markdown code')).toHaveText('make build')
  await expect(summary.getByRole('heading', { name: 'Validation' })).toBeVisible()
  await expect(summary.getByText('structured context', { exact: true })).toHaveCSS('font-style', 'italic')
  await expect(summary.getByRole('listitem')).toHaveText('completed item')
  await expect(summary).toContainText('<script data-summary-unsafe>window.hacked = true</script> as text.')
  await expect(summary).not.toContainText('`')
  await expect(summary.locator('script[data-summary-unsafe]')).toHaveCount(0)
})

test('long task body is constrained and expands accessibly in both header variants', async ({ page }) => {
  await showAllBoardTasks(page)
  for (const path of ['/w/demo/tasks/long-body', '/w/demo/tasks/long-body/full']) {
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

test('a short side-view spec stays unbounded and the full route previews a long spec', async ({ page }) => {
  await page.goto('/w/demo/tasks/short')
  await expect(page.getByRole('button', { name: /Show (full plan|less)/ })).toHaveCount(0)
  await expect(page.locator('[data-spec-overflow-shadow]')).toHaveCount(0)

  // The full route's reading column shows the same six-line preview as the
  // sheet; opening it reveals the whole plan (component-web-task-surfaces,
  // Task detail composition).
  await page.goto('/w/demo/tasks/long-spec/full')
  const toggle = page.getByRole('button', { name: 'Show full plan' })
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await expect(page.locator('[data-spec-overflow-shadow]')).toBeVisible()
  await toggle.click()
  await expect(page.getByRole('button', { name: 'Show less' })).toHaveAttribute('aria-expanded', 'true')
  await expect(page.locator('[data-spec-overflow-shadow]')).toHaveCount(0)
  const marker = page.getByText('Long spec ending marker.')
  await marker.scrollIntoViewIfNeeded()
  await expect(marker).toBeVisible()
})

test('review panel replaces duplicate review and bounce activity notes', async ({ page }) => {
  await page.goto('/w/demo/tasks/reviews/full')

  // One panel card, not one card per seat.
  const panel = page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })
  await expect(panel).toHaveCount(1)

  // Both seats resolve their verdicts — including the seat whose
  // review.completed event carries no review_work_order_id.
  await expect(panel.getByText('Approved', { exact: true })).toBeVisible()
  await expect(panel.getByText('Changes', { exact: true })).toBeVisible()
  await expect(panel.getByText('pinned', { exact: true })).toHaveCount(0)
  await expect(panel.getByText('2 of 2 verdicts in')).toBeVisible()

  // Feedback from every seat stays visible, attributed in the merged notes.
  await expect(panel.getByText('Approved guidance remains visible.')).toBeVisible()
  await expect(panel.getByText('Changes guidance remains visible.')).toBeVisible()

  // The per-seat audit events fold into the panel — no duplicate rows.
  await expect(page.getByText(/^Independent review:/)).toHaveCount(0)
  await expect(page.getByText('Bounced back to implement (bounce 1)', { exact: true })).toHaveCount(0)

  const timelineRows = page.getByRole('region', { name: 'Execution event timeline' }).locator('ol > li')
  await expect(timelineRows).toHaveCount(2)
  await expect(timelineRows.nth(0)).toContainText(/Panel of 2\b.*unanimous to pass/)
  await expect(timelineRows.nth(1)).toContainText('Pull request opened')
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

// The plan section shows each version, so neither versioned plan event adds a
// timeline row (component-web-task-surfaces, Task detail composition).
test('spec approval keeps the human marker without versioned plan events', async ({ page }) => {
  await page.goto('/w/demo/tasks/spec-approval/full')

  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const timelineRows = timeline.locator('ol > li')
  await expect(timelineRows).toHaveCount(1)
  await expect(timelineRows.nth(0)).toContainText('Approved')
  await expect(timeline.getByText('Plan v2 drafted')).toHaveCount(0)
  await expect(timeline.getByText(/Spec v\d+ approved/)).toHaveCount(0)
})

test('output-validation rejections show job-specific errors with warning tone and preserve accepted narration', async ({
  page,
}) => {
  await page.goto('/w/demo/tasks/output-invalid/full')

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

  // A plan stage that produced no narration of its own renders no row: the
  // plan section tells it, and the rejected payload never leaks.
  await expect(timeline.locator('li').filter({ hasText: 'Plan completed' })).toHaveCount(0)
  await expect(page.getByText('PRIVATE REJECTED OUTPUT')).toHaveCount(0)

  await expect(timeline.getByText('Harness narration wins.', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Accepted triage summary wins.', { exact: true })).toBeVisible()
  await expect(
    timeline.getByText(/Accepted review summary wins\.[\s\S]*Accepted review feedback wins too\./),
  ).toBeVisible()
  await expect(timeline.getByText(/superseded (job|triage|review) error/)).toHaveCount(0)

  await expect(timeline.getByText('Review check-in — paused after the configured rounds', { exact: true })).toHaveCount(
    1,
  )
  await expect(timeline.getByText('Source: spec output validation · maximum 4 bounces', { exact: true })).toBeVisible()
})

test('active review claim diagnostics stay in the review panel instead of standalone history rows', async ({
  page,
}) => {
  await page.goto('/w/demo/tasks/diagnostics/full')

  await expect(page.getByText('Review claimed without terminal verdict submission')).toHaveCount(0)
  await expect(
    page.getByText(/review claim is active without a successful submit_review_verdict response/),
  ).toHaveCount(0)
  const panel = page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })
  await expect(panel).toHaveCount(1)
  await expect(panel.getByText('Deliberating')).toHaveCount(2)
  await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
  await expect(page.getByText(/seat 3 · diagnostics-review-0-seat-3 · review claim lease expired/)).toBeVisible()

  const timelineRows = page.getByRole('region', { name: 'Execution event timeline' }).locator('ol > li')
  await expect(timelineRows).toHaveCount(3)
  await expect(timelineRows.nth(0)).toContainText(/Panel of 2\b.*unanimous to pass/)
  await expect(timelineRows.nth(1)).toContainText('Review claim expired without verdict submission')
  await expect(timelineRows.nth(2)).toContainText('Pull request opened')
  // The claimed attempt's stop affordance lives once in the next-action box,
  // never as a row in the event list.
  await expect(timelineRows.getByRole('button', { name: /Stop/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Stop attempt' })).toHaveCount(1)
  await expect(nextActionBox(page).getByRole('button', { name: 'Stop attempt' })).toBeVisible()

  // The task sheet uses the same historical rendering boundary.
  await page.goto('/w/demo/tasks/diagnostics')
  await expect(page.getByText('Review claimed without terminal verdict submission')).toHaveCount(0)
  await expect(page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })).toHaveCount(1)
  await expect(page.getByText('Review claim expired without verdict submission')).toBeVisible()
})

test('review panel hides generated metadata while preserving reported zero usage and meaningful progress', async ({
  page,
}) => {
  await page.route('**/v1/tasks/review-panel-cleanup/activity*', async (route) => {
    const item = activity('diagnostics', false)
    item.task.id = 'review-panel-cleanup'
    item.jobs = item.jobs?.map((job) => ({ ...job, task_id: 'review-panel-cleanup' }))
    item.events = item.events.map((event) => ({ ...event, task_id: 'review-panel-cleanup' }))
    item.work_orders = item.work_orders?.map((order, index) => ({
      ...order,
      task_id: 'review-panel-cleanup',
      model_enforcement: 'self-reported',
      usage_reported: index === 1,
      tokens_in: 0,
      tokens_out: 0,
      progress:
        index === 0
          ? 'conveyor run mode: confirmed-per-stage'
          : 'conveyor run mode: auto-chained\nChecking requirement citations.',
    }))
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/review-panel-cleanup/full')
  const panel = page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })

  await expect(panel).toContainText('gpt-review')
  await expect(panel).toContainText('claude-review')
  await expect(panel.getByText('Deliberating')).toHaveCount(2)
  await expect(panel.getByText('0 in / 0 out', { exact: true }).last()).toBeVisible()
  await expect(panel.getByText('Checking requirement citations.', { exact: true })).toBeVisible()
  await expect(panel.getByText(/conveyor run mode/i)).toHaveCount(0)
  await expect(panel.getByText(/self-reported/i)).toHaveCount(0)
  await expect(panel.getByText('Usage unavailable')).toHaveCount(0)
  await expect(panel.getByRole('tooltip').filter({ hasText: 'Usage unavailable' })).toHaveCount(0)
})

test('external Cursor Auto claims show the Cursor logo in the job header and review seat without rewriting auto', async ({
  page,
}) => {
  const iconPaths = (iconFile: string) =>
    Array.from(
      readFileSync(
        new URL(`../node_modules/@lobehub/icons-static-svg/icons/${iconFile}.svg`, import.meta.url),
        'utf8',
      ).matchAll(/\sd="([^"]+)"/g),
      (match) => match[1],
    )
  const logoPaths = (chip: Locator) =>
    chip
      .locator('span[aria-hidden="true"]')
      .locator('path')
      .evaluateAll((paths) => paths.map((path) => path.getAttribute('d')))

  await page.route('**/v1/tasks/cursor-auto-implement/activity*', async (route) => {
    const item = activity('stage-aware', false)
    item.task.id = 'cursor-auto-implement'
    item.jobs = [
      {
        id: 'cursor-auto-implement-implement-1',
        task_id: 'cursor-auto-implement',
        stage: 'implement',
        harness: 'external-mcp',
        model_tier: 'operator-owned',
        runner: 'external',
        confinement: 'none',
        cost_usd: 0,
        tokens_in: 0,
        tokens_out: 0,
        state: 'done',
        started_at: createdAt,
        ended_at: '2026-07-15T12:01:00Z',
      },
    ]
    item.work_orders = [
      {
        ...item.work_orders[1],
        id: 'cursor-auto-implement-implement-1',
        task_id: 'cursor-auto-implement',
        job_id: 'cursor-auto-implement-implement-1',
        state: 'completed',
        agent: 'cursor',
        model: 'auto',
        model_enforcement: 'self-reported',
      },
    ]
    item.events = [
      {
        id: 1,
        task_id: 'cursor-auto-implement',
        job_id: 'cursor-auto-implement-implement-1',
        kind: 'job.summary',
        actor_id: 'agent',
        actor_role: 'agent',
        payload: { summary: 'Cursor implemented the change.' },
        at: '2026-07-15T12:01:00Z',
      },
    ]
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/cursor-auto-implement/full')
  const footerChip = page
    .locator('article')
    .filter({ hasText: 'Cursor implemented the change.' })
    .locator('[data-job-duration]')
    .locator('xpath=preceding-sibling::span[1]')
  await expect(footerChip.getByText('auto', { exact: true })).toBeVisible()
  await expect(footerChip.getByText('auto', { exact: true })).toHaveAttribute('title', 'auto')
  expect(await logoPaths(footerChip)).toEqual(iconPaths('cursor'))
  await expect(footerChip.locator('.lucide-cpu')).toHaveCount(0)
  await expect(footerChip.getByRole('tooltip')).toContainText('via Cursor')

  await page.route('**/v1/tasks/cursor-auto-review/activity*', async (route) => {
    const item = activity('diagnostics', false)
    item.task.id = 'cursor-auto-review'
    item.jobs = item.jobs?.map((job) => ({ ...job, task_id: 'cursor-auto-review' }))
    item.events = item.events.map((event) => ({ ...event, task_id: 'cursor-auto-review' }))
    item.work_orders = item.work_orders?.map((order, index) => ({
      ...order,
      task_id: 'cursor-auto-review',
      agent: 'cursor',
      model: index === 0 ? 'auto' : undefined,
    }))
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/cursor-auto-review/full')
  const panel = page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })
  const autoSeat = panel.locator('span.group\\/model').filter({ hasText: 'auto' })
  await expect(autoSeat.getByText('auto', { exact: true })).toBeVisible()
  expect(await logoPaths(autoSeat)).toEqual(iconPaths('cursor'))
  await expect(autoSeat.locator('.lucide-cpu')).toHaveCount(0)
  await expect(autoSeat.getByRole('tooltip')).toContainText('via Cursor')

  // A provider-matched model keeps its provider logo whatever the harness.
  const claudeSeat = panel.locator('span.group\\/model').filter({ hasText: 'claude-review' })
  expect(await logoPaths(claudeSeat)).toEqual(iconPaths('claude-color'))
  await expect(claudeSeat.getByText(/via /)).toHaveCount(0)
})

test('human gate renders as the event timeline tail and the page opens scrolled to it', async ({ page }) => {
  await page.goto('/w/demo/tasks/gate/full')

  await expect(page.getByRole('heading', { name: 'Activity' })).toBeVisible()
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate).toHaveCount(1)
  await expect(gate.getByRole('heading', { name: 'Plan approval' })).toBeVisible()

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

for (const decision of [
  {
    name: 'approve revision',
    open: 'Approve revision',
    input: 'Revision direction',
    comment: 'Keep the API contract byte-compatible.',
    confirm: 'Send to planning',
    action: 'redirect',
    reasonCode: 'plan-revision-approved',
  },
  {
    name: 'decline with direction',
    open: 'Decline with direction',
    input: 'Implementation direction',
    comment: 'Continue against plan v7 and preserve the handler shape.',
    confirm: 'Retry implementation',
    action: 'redirect',
    reasonCode: 'plan-revision-declined',
  },
  {
    name: 'reject',
    open: 'Reject task',
    input: 'Rejection note',
    comment: 'The requested outcome is no longer wanted.',
    confirm: 'Reject task',
    action: 'reject',
    reasonCode: 'plan-revision-rejected',
  },
] as const) {
  test(`plan revision gate renders request context and submits ${decision.name}`, async ({ page }) => {
    const taskId = `plan-revision-${decision.action}-${decision.reasonCode}`
    let activityRequests = 0
    await page.route(`**/v1/tasks/${taskId}/activity*`, async (route) => {
      activityRequests += 1
      await route.fulfill({ json: planRevisionGateActivity(taskId) })
    })

    let requestBody: Record<string, unknown> | undefined
    await page.route(`**/v1/tasks/${taskId}/review*`, async (route) => {
      requestBody = route.request().postDataJSON() as Record<string, unknown>
      await route.fulfill({
        json: { task: activity('gate', false).task, checkout_available: false, checkout_guidance: '' },
      })
    })

    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const gate = page.getByRole('region', { name: 'Human gate' })
    await expect(gate.getByText('Plan revision requested')).toBeVisible()
    await expect(gate.getByText('v7', { exact: true })).toBeVisible()
    await expect(gate.getByText('A dependency changed the public handler contract.')).toBeVisible()
    // The decision card is the only actionable pause surface. Its decline
    // path owns the implementation retry, so generic recovery cannot bypass
    // the pending operator decision.
    await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
    await expect(page.getByLabel('Operator direction')).toHaveCount(0)
    const decided = activityRequests

    await gate.getByRole('button', { name: decision.open }).first().click()
    const input = gate.getByLabel(decision.input)
    if (decision.reasonCode === 'plan-revision-declined') {
      await expect(gate.getByRole('button', { name: decision.confirm })).toBeDisabled()
    }
    // The operator's direction reaches the backend trimmed — a textarea's
    // incidental whitespace is not operator instruction.
    await input.fill(`  ${decision.comment}\n`)
    await gate.getByRole('button', { name: decision.confirm }).last().click()

    await expect
      .poll(() => requestBody)
      .toEqual({
        action: decision.action,
        reason_code: decision.reasonCode,
        comment: decision.comment,
      })
    await expect.poll(() => activityRequests).toBeGreaterThan(decided)
  })
}

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

test('the revision chain reads as request, decision, and the plan version it produced', async ({ page }) => {
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
  const rows = timeline.locator('ol > li')

  await expect(timeline.getByText('The agent asked to revise plan v7')).toBeVisible()
  await expect(timeline.getByText('A dependency changed the public handler contract.')).toBeVisible()
  await expect(timeline.getByText('Plan revision approved — returning to planning')).toBeVisible()
  await expect(timeline.getByText('Re-plan around the new handler contract.')).toBeVisible()
  // The new version shows in the plan section, not as a timeline row.
  await expect(timeline.getByText('Plan v8 drafted')).toHaveCount(0)

  // Two distinct entries in the order they happened (AC-4.1) — and the
  // release the request rode in on is not repeated as a third.
  await expect(rows.nth(0)).toContainText('The agent asked to revise plan v7')
  await expect(rows.nth(1)).toContainText('Plan revision approved')
  await expect(timeline.getByText('Work-order claim released')).toHaveCount(0)
  // The wire vocabulary stays on the wire.
  await expect(timeline.getByText('plan-revision-approved')).toHaveCount(0)
  await expect(timeline.getByText('Requested changes')).toHaveCount(0)

  const plan = page.getByRole('region', { name: 'Execution plan' })
  await expect(plan.getByText('v8', { exact: true })).toBeVisible()
  await expect(plan.getByText('Awaiting approval')).toBeVisible()
})

test('worker warning is scoped to actionable task-owned work at the human gate', async ({ page }) => {
  await page.goto('/w/demo/tasks/human-gate-worker-scope/full')

  await expect(
    page.getByRole('region', { name: 'Human gate' }).getByRole('heading', { name: 'Plan approval' }),
  ).toBeVisible()
  await expect(page.getByText('GitHub review publication failed')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Enrolled worker unavailable' })).toHaveCount(0)
  await expect(page.getByText('This queued work has never started.')).toHaveCount(0)

  await page.goto('/w/demo/tasks/queued-worker/full')
  await expect(page.getByRole('region', { name: 'Enrolled worker unavailable' })).toBeVisible()
  await expect(page.getByText('This queued work has never started.')).toBeVisible()
})

test('pull-only queued work waits for an operator agent without a worker alarm or route chip', async ({ page }) => {
  await page.goto('/w/demo/tasks/pull-only-worker/full')

  await expect(page.getByText('Implementation — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Enrolled worker unavailable' })).toHaveCount(0)
  await expect(page.getByText('Needs a route', { exact: true })).toHaveCount(0)
})

test('parked task offers recovery instead of an invalid approval verdict', async ({ page }) => {
  await page.goto('/w/demo/tasks/parked/full')

  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Approve' })).toHaveCount(0)
  await expect(
    page.getByText('Parked by triage — resume from the recorded recovery stage when this work is ready.'),
  ).toBeVisible()

  const recoveryRequest = page.waitForRequest(
    (request) => request.method() === 'POST' && new URL(request.url()).pathname === '/v1/tasks/parked/redispatch',
  )
  await page.getByRole('button', { name: 'Resume task' }).click()
  await recoveryRequest
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
  await merge.click()
  await expect(gate.getByRole('button', { name: 'Recording…' })).toBeDisabled()
  await expect(gate.getByRole('button', { name: 'Merge', exact: true })).toHaveCount(0)

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
  const merge = gate.getByRole('button', { name: 'Merge', exact: true })
  await merge.click()
  await expect(gate.getByText('GitHub merge failed')).toBeVisible()
  await expect(merge).toBeEnabled()
  await merge.click()
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

test('contributor assignee can request merge-gate changes without gate approval controls', async ({ page }) => {
  await page.route('**/v1/me**', (route) =>
    route.fulfill({
      json: callerIdentity(
        {
          id: 'usr_contributor',
          email: 'contributor@example.test',
          display_name: 'Connie Contributor',
          role: 'contributor',
        },
        route.request(),
      ),
    }),
  )
  await page.route('**/v1/tasks/merge-request-changes/activity*', (route) => {
    const item = activity('merge-request-changes', false)
    item.task.assignee = {
      user_id: 'usr_contributor',
      email: 'contributor@example.test',
      display_name: 'Connie Contributor',
    }
    return route.fulfill({ json: item })
  })
  let feedback = ''
  await page.route('**/v1/tasks/merge-request-changes/request-changes*', async (route) => {
    feedback = ((await route.request().postDataJSON()) as { feedback: string }).feedback
    await route.fulfill({ json: { task: activity('merge-request-changes', false).task, feedback } })
  })

  await page.goto('/w/demo/tasks/merge-request-changes/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate.getByRole('button', { name: 'Approve' })).toHaveCount(0)
  await expect(gate.getByRole('button', { name: 'Merge', exact: true })).toHaveCount(0)
  await gate.getByRole('button', { name: 'Request changes' }).click()
  await gate.getByLabel('Changes you want').fill('Please cover the contributor path.')
  await gate.getByRole('button', { name: 'Request changes' }).last().click()
  await expect.poll(() => feedback).toBe('Please cover the contributor path.')
})

test("non-assignee contributor cannot request changes at another user's merge gate", async ({ page }) => {
  await page.route('**/v1/me**', (route) =>
    route.fulfill({
      json: callerIdentity(
        {
          id: 'usr_contributor',
          email: 'contributor@example.test',
          display_name: 'Connie Contributor',
          role: 'contributor',
        },
        route.request(),
      ),
    }),
  )
  await page.route('**/v1/tasks/merge-request-changes/activity*', (route) => {
    const item = activity('merge-request-changes', false)
    item.task.assignee = {
      user_id: 'usr_other',
      email: 'other@example.test',
      display_name: 'Other Contributor',
    }
    return route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/merge-request-changes/full')
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Request changes' })).toHaveCount(0)
})

// The return is a recorded decision, so it reads back in the timeline in the
// same plain language it was sent in — never as the wire reason code (REQ-6).
test('the recorded return reads back in the timeline with its feedback', async ({ page }) => {
  let returned = false
  await page.route('**/v1/tasks/merge-request-changes/activity*', async (route) => {
    const item = activity('merge-request-changes', false)
    if (returned) {
      item.interventions = [
        {
          id: 7,
          task_id: 'merge-request-changes',
          actor_id: 'usr-member',
          actor_role: 'user' as const,
          action: 'redirect' as const,
          reason_code: 'user-request-changes',
          comment: 'Keep this exact feedback.',
          at: '2026-07-15T12:05:00Z',
        },
      ]
    }
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/tasks/merge-request-changes/request-changes*', async (route) => {
    returned = true
    await route.fulfill({ json: { task: activity('merge-request-changes', false).task, feedback: 'ok' } })
  })

  await page.goto('/w/demo/tasks/merge-request-changes/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await gate.getByRole('button', { name: 'Request changes' }).click()
  await gate.getByLabel('Changes you want').fill('Keep this exact feedback.')
  await gate.getByRole('button', { name: 'Request changes' }).last().click()

  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(timeline.getByText('Sent back with your feedback')).toBeVisible()
  await expect(timeline.getByText('Keep this exact feedback.')).toBeVisible()
  await expect(timeline.getByText('This feedback goes to the next implementation run word for word.')).toBeVisible()
  // The wire reason code never reaches the story.
  await expect(timeline.getByText('user-request-changes')).toHaveCount(0)
})

test('new task events preserve the task sheet scroll position', async ({ page }) => {
  await page.goto('/w/demo/tasks/live-scroll')

  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  const container = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
  await expect(timeline.getByText('Live event 18', { exact: true })).toBeVisible()
  await expect.poll(() => container.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
  await container.evaluate((element) => {
    element.scrollTop = 120
  })
  const before = await container.evaluate((element) => element.scrollTop)
  expect(before).toBeGreaterThan(0)

  emitLiveScrollEvent()

  const newest = timeline.getByText('Live event 19', { exact: true })
  await expect(newest).toBeVisible()
  await expect.poll(() => container.evaluate((element) => element.scrollTop)).toBe(before)
  await expect(newest).not.toBeInViewport()
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
})

for (const decision of ['approve', 'redirect'] as const) {
  test(`successful ${decision} scrolls the task sheet to the refreshed timeline tail`, async ({ page }) => {
    let recorded = false
    await page.route('**/v1/tasks/gate/activity*', async (route) => {
      const item = activity('gate', true)
      if (recorded) {
        item.task.state = 'running'
        item.interventions = [
          {
            id: 1,
            task_id: 'gate',
            actor_id: 'dashboard-operator',
            actor_role: 'human',
            action: decision,
            reason_code: decision === 'approve' ? 'approved' : 'changes_requested',
            comment: decision === 'redirect' ? 'Please revise the implementation.' : '',
            at: '2026-07-15T12:10:00Z',
          },
        ]
      }
      await route.fulfill({ json: item })
    })
    await page.route('**/v1/tasks/gate/review*', async (route) => {
      recorded = true
      await route.fulfill({
        json: { task: activity('gate', true).task, checkout_available: false, checkout_guidance: '' },
      })
    })

    await page.goto('/w/demo/tasks/gate')
    const container = page.getByRole('dialog', { name: 'Task detail' }).locator('.overflow-y-auto')
    await expect.poll(() => container.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
    await container.evaluate((element) => {
      element.scrollTop = 0
    })

    const gate = page.getByRole('region', { name: 'Human gate' })
    const reviewRequest = page.waitForRequest((request) => new URL(request.url()).pathname === '/v1/tasks/gate/review')
    if (decision === 'approve') {
      await gate.getByRole('button', { name: 'Approve' }).click()
    } else {
      await gate.getByRole('button', { name: 'Redirect' }).click()
      await gate.getByLabel('Guidance').fill('Please revise the implementation.')
      const submit = gate.getByRole('button', { name: 'Redirect' }).last()
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
    await expect
      .poll(() =>
        container.evaluate((element) => Math.abs(element.scrollHeight - element.clientHeight - element.scrollTop)),
      )
      .toBeLessThanOrEqual(1)
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
  })
}

test('task sheet opens scrolled to the human gate for reviewable tasks', async ({ page }) => {
  await page.goto('/w/demo/tasks/gate')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect(gate).toBeInViewport()
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollTop)).toBe(0)
})

test('board activity surfaces expired-without-verdict state', async ({ page }) => {
  await showAllBoardTasks(page)
  await page.goto('/')
  await expect(page.getByText('Verdict claim expired')).toBeVisible()
})

test('shared Markdown renders plan diagrams responsively in both themes and falls back to source', async ({ page }) => {
  await page.setViewportSize({ width: 720, height: 900 })
  await page.goto('/w/demo/tasks/mermaid-valid/full')
  const diagram = page.locator('[data-mermaid]')
  await expect(diagram.locator('svg')).toBeVisible()
  await expect(diagram).toHaveCSS('overflow-x', 'auto')
  for (const theme of ['light', 'dark']) {
    // At this width the theme control sits in the navigation drawer.
    await page.getByRole('button', { name: 'Open navigation' }).click()
    await page.getByRole('dialog', { name: 'Navigation' }).getByLabel('Theme').selectOption(theme)
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog', { name: 'Navigation' })).toBeHidden()
    await expect(diagram.locator('svg')).toBeVisible()
    await expect(diagram).toBeInViewport()
  }

  await page.goto('/w/demo/tasks/mermaid-invalid/full')
  await expect(page.locator('code.language-mermaid')).toContainText('this is deliberately malformed')
  await expect(page.getByRole('heading', { name: 'Execution plan' }).first()).toBeVisible()
})

// The anchor lives at its own canonical route (component-web-task-surfaces), and the legacy task
// URL is a redirect into it rather than a second door.
test('blueprint and dependency details remain linked and read only', async ({ page }) => {
  await page.goto('/w/demo/tasks/blueprint-parent/full')
  await expect(page).toHaveURL(/\/blueprints\/blueprint-parent$/)
  await expect(page.getByRole('heading', { name: 'Delivery' })).toBeVisible()
  await expect(page.getByText('Completed').first()).toBeVisible()
  await expect(page.getByText('2 merged · 1 closed without merging').first()).toBeVisible()
  const dashboardLinks = page.getByRole('link', { name: 'Dashboard' })
  await expect(dashboardLinks).toHaveCount(2)
  for (let index = 0; index < (await dashboardLinks.count()); index++) {
    await expect(dashboardLinks.nth(index)).toHaveAttribute('href', '/w/demo/tasks/blueprint-child/full')
  }
  await expect(page.getByText('3 tasks created from the blueprint')).toBeVisible()
  await expect(page.getByText('Blueprint completed — all child tasks are finished')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Runtime' }).first()).toBeVisible()

  await page.goto('/w/demo/tasks/blueprint-child/full')
  const box = nextActionBox(page)
  await expect(box.getByRole('heading', { name: 'This task is waiting on another task.' })).toBeVisible()
  await expect(box.getByText('Implementation · Progressing')).toBeVisible()
  await expect(box.getByRole('link', { name: 'Runtime · Waiting' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/blueprint-sub-2/full',
  )
  await expect(box.getByText('Not applicable.')).toBeVisible()
  // The summary is the box's one headline; the derived "Waiting on
  // dependencies" title states the same fact and is not printed (DEC-61).
  await expect(page.getByText('This task is waiting on another task.', { exact: true })).toHaveCount(1)
  await expect(box.getByText('Waiting on dependencies', { exact: true })).toHaveCount(0)
  await expect(executionTitle(page)).toHaveCount(0)
  await expect(page.getByText('What to do next')).toHaveCount(0)
  await expect(page.getByText('Implementation — waiting for an operator agent', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Any agent connected over MCP can claim this.', { exact: false })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Phase 6 blueprint · plan v1' })).toHaveAttribute(
    'href',
    '/w/demo/blueprints/blueprint-parent',
  )
  await expect(page.getByRole('link', { name: 'Runtime · Waiting' }).last()).toHaveAttribute(
    'href',
    '/w/demo/tasks/blueprint-sub-2/full',
  )
  await expect(page.getByRole('link', { name: 'Persistence · Satisfied' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/blueprint-sub-1/full',
  )
  await expect(page.getByText('awaiting_human')).toHaveCount(0)
  await expect(page.getByText('terminal outcome')).toHaveCount(0)
})

test('blueprint history describes a missing stored spec as a missing plan version', async ({ page }) => {
  const [blueprint] = blueprintProjection()
  await page.route('**/v1/blueprints**', (route) =>
    route.fulfill({
      json: [
        {
          ...blueprint,
          spec: undefined,
          governing_version: undefined,
        },
      ],
    }),
  )

  await page.goto('/w/demo/blueprints/blueprint-parent')
  await expect(page.getByText('This blueprint has no recorded plan version.')).toBeVisible()
  await expect(page.getByText('This blueprint has no recorded spec version.')).toHaveCount(0)
})

test('blocked detail polling clears waiting state without a reload and stays off when unblocked', async ({ page }) => {
  await page.clock.install()
  await page.goto('/w/demo/tasks/blocked-refresh/full')
  await expect(
    nextActionBox(page).getByRole('heading', { name: 'This task is waiting on another task.' }),
  ).toBeVisible()
  expect(detailRequestCounts.get('blocked-refresh')).toBe(1)

  await page.clock.fastForward(15_100)
  await expect.poll(() => detailRequestCounts.get('blocked-refresh') ?? 0).toBeGreaterThan(1)
  await expect(nextActionBox(page).getByRole('heading', { name: 'This task is waiting on another task.' })).toHaveCount(
    0,
  )
  await expect(page.getByRole('link', { name: 'Backend contract · Satisfied' })).toBeVisible()
  await expect(page.getByText('Implementation — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Redispatch' })).toBeVisible()

  await page.goto('/w/demo/tasks/no-blockers/full')
  await expect.poll(() => detailRequestCounts.get('no-blockers') ?? 0).toBe(1)
  const initialRequests = detailRequestCounts.get('no-blockers')
  await page.clock.fastForward(30_000)
  expect(detailRequestCounts.get('no-blockers')).toBe(initialRequests)
})

test('dependency links follow the detail route variant and spec claiming stays available', async ({ page }) => {
  await page.goto('/w/demo/tasks/blueprint-child')
  await expect(
    nextActionBox(page).getByRole('heading', { name: 'This task is waiting on another task.' }),
  ).toBeVisible()
  await expect(nextActionBox(page).getByRole('link', { name: 'Runtime · Waiting' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/blueprint-sub-2',
  )

  await page.goto('/w/demo/tasks/spec-while-blocked/full')
  const timeline = page.getByRole('region', { name: 'Execution event timeline' })
  await expect(
    nextActionBox(page).getByRole('heading', { name: 'This task is waiting on another task.' }),
  ).toBeVisible()
  await expect(timeline.getByText('Plan — waiting for an operator agent', { exact: true })).toBeVisible()
  await expect(timeline.getByText('Implementation — waiting for an operator agent', { exact: true })).toHaveCount(0)
})

test('a pending decision blocker links directly to its System Design decision card', async ({ page }) => {
  await page.goto('/w/demo/tasks/decision-blocker/full')
  const blocker = nextActionBox(page)
  const decision = blocker.getByRole('link', { name: 'Open DEC-2 in System Design' })
  await expect(decision).toHaveAttribute('href', '/w/demo/system-design#decision-dec-2')
  await decision.click()
  await expect(page).toHaveURL(/\/system-design#decision-dec-2$/)
})

test('recovery-needing work stays primary when dependencies are also unresolved', async ({ page }) => {
  await page.goto('/w/demo/tasks/blocked-suppressed/full')
  const currentState = nextActionBox(page)
  await expect(executionTitle(page)).toHaveText('Implementation paused — recovery needed')
  await expect(currentState.getByText('Schema migration · Waiting')).toBeVisible()
  await expect(currentState.getByText(/task remains dependency-gated/i)).toBeVisible()
  await expect(page.getByText(/starts automatically/i)).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Recover work order' })).toBeVisible()
  // The box offers Redispatch only as the server's own choice; its execution
  // state adds none.
  await expect(currentState.locator('dl').getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
  await expect(currentState.getByRole('button', { name: 'Redispatch' })).toHaveCount(
    await currentState.locator('[data-next-action-choices]').getByRole('button', { name: 'Redispatch' }).count(),
  )
})

test('board cards show a neutral, keyboard-reachable dependency chip using titles', async ({ page }) => {
  await showAllBoardTasks(page)
  const item = activity('blueprint-child', false)
  await page.route('**/v1/activity*', (route) =>
    route.fulfill({
      json: [{ task: item.task, latest_stage: 'implement', last_event_at: createdAt, needs_attention: false }],
    }),
  )
  await page.goto('/')
  const card = page.getByRole('link', { name: /Short task/ })
  // The dependency mark replaces the chip (req-operator-next-action AC-3.4).
  const mark = card.locator('[data-dependency-mark="waiting"]')
  await expect(mark).toBeVisible()
  await card.focus()
  await expect(mark.locator('[role="tooltip"]')).toContainText('Runtime')
})

test('unsatisfiable dependency is attention-worthy and can be unlinked with an audited reason only from detail', async ({
  page,
}) => {
  await showAllBoardTasks(page)
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
        {
          id: 2,
          task_id: 'unsatisfiable',
          kind: 'task.dependency_removed',
          actor_id: 'operator',
          actor_role: 'human' as const,
          payload: { reason: 'The retired plan is no longer required' },
          at: createdAt,
        },
      ],
    }
  }
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({
      json: [{ id: 'demo', name: 'Demo', config_version: 1, created_at: createdAt }],
    }),
  )
  await page.route('**/v1/activity*', (route) => {
    const item = currentItem()
    return route.fulfill({
      json: [
        {
          task: item.task,
          latest_stage: 'implement',
          last_event_at: createdAt,
          needs_attention: item.needs_attention,
          stalled: item.stalled,
        },
      ],
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
  await expect(card.locator('[data-dependency-mark="unsatisfiable"]')).toHaveAttribute(
    'aria-label',
    /closed without merging/,
  )
  await expect(card.getByRole('button', { name: /Unlink dependency/ })).toHaveCount(0)
  await card.click()

  const currentState = nextActionBox(page)
  await expect(page.getByRole('region', { name: 'Dependency needs attention' }).first()).toBeVisible()
  // The summary states the unsatisfied edge, so the derived "Dependency needs
  // attention" title is not repeated in the box (DEC-61).
  await expect(currentState.getByRole('heading', { name: 'A dependency ended without merging.' })).toBeVisible()
  await expect(executionTitle(page)).toHaveCount(0)
  await expect(currentState.getByText('Implementation · Paused — needs attention')).toBeVisible()
  await expect(currentState.getByText('Retired API plan · Needs attention')).toBeVisible()
  await expect(currentState.getByText('Active schema work · Waiting')).toBeVisible()
  await expect(currentState.getByText(/Unlink the dead dependency.*cancel this task/i)).toBeVisible()
  await expect(page.getByRole('heading', { name: 'This task is waiting on another task.' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Recover work order' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Redispatch' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Task actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Cancel task' })).toBeVisible()
  await page.keyboard.press('Escape')
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

test('task header links an open dependency with one stable audited request', async ({ page }) => {
  let linked = false
  let requestCount = 0
  let linkBody: Record<string, string> | undefined
  const currentItem = () => {
    const item = activity('full-header-id', false)
    if (linked) {
      item.task.dependencies = [
        ...(item.task.dependencies ?? []),
        { id: 'candidate-open', title: 'Candidate API', state: 'running' as const },
      ]
      item.task.blocking_task_ids = ['candidate-open']
    }
    return item
  }
  await page.route('**/v1/tasks/full-header-id/activity*', (route) => route.fulfill({ json: currentItem() }))
  await page.route(/\/v1\/tasks(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: [
        currentItem().task,
        { ...currentItem().task, id: 'candidate-open', title: 'Candidate API', state: 'running' },
        { ...currentItem().task, id: 'candidate-closed', title: 'Closed candidate', state: 'closed' },
      ],
    }),
  )
  await page.route('**/v1/tasks/full-header-id/dependencies*', async (route) => {
    requestCount++
    linkBody = route.request().postDataJSON()
    linked = true
    await route.fulfill({ json: { task: currentItem().task, request_id: linkBody?.request_id, added: true } })
  })

  await page.goto('/w/demo/tasks/full-header-id')
  await page.getByRole('button', { name: 'Task actions' }).click()
  await page.getByRole('menuitem', { name: 'Link dependency' }).click()
  await page.getByPlaceholder('Task title or ID').fill('candidate')
  await expect(page.getByRole('button', { name: /Candidate API/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Closed candidate/ })).toHaveCount(0)
  await page.getByRole('button', { name: /Candidate API/ }).click()
  await page.getByPlaceholder('Why should this task wait for the selected task?').fill('Ship the API first')
  await page.getByRole('button', { name: 'Add dependency' }).click()

  await expect.poll(() => requestCount).toBe(1)
  expect(linkBody).toMatchObject({ depends_on_task_id: 'candidate-open', reason: 'Ship the API first' })
  expect(linkBody?.request_id).toBeTruthy()
  await expect(page.getByRole('link', { name: 'Candidate API · Waiting' })).toBeVisible()
})

// The origin-task System Design proposal card. The decision a
// task raised is confirmable from that task, without leaving for the document.
function designCollection(originTaskId: string, resolved: boolean) {
  return [designProposalSummary(originTaskId, 'design-lifecycle', 'Work-order lifecycle', 2, resolved)]
}

function designProposalSummary(
  originTaskId: string,
  documentId: string,
  title: string,
  proposalVersion: number,
  resolved = false,
) {
  const confirmedVersion = {
    document_id: documentId,
    version: 1,
    content: '# Lifecycle\n\nThe service owns lifecycle authority.',
    governs: [{ repository: 'conveyor', paths: ['internal/workorder/**'] }],
    origin: 'operator',
    confirmed: true,
    dismissed: false,
    workspace: 'demo',
    created_at: '2026-08-05T08:00:00Z',
  }
  const proposal = {
    ...confirmedVersion,
    version: proposalVersion,
    content: '# Lifecycle\n\nThe service owns lifecycle authority and recovery direction.',
    origin: 'implementation_deliberation',
    origin_task_id: originTaskId,
    confirmed: resolved,
    dismissed: false,
    created_at: '2026-08-05T09:00:00Z',
  }
  return {
    document: {
      id: documentId,
      slug: documentId,
      title,
      category: 'Architecture',
      current_version: resolved ? proposalVersion : 1,
      workspace: 'demo',
      created_at: '2026-08-05T08:00:00Z',
      updated_at: '2026-08-05T09:00:00Z',
    },
    current_version: resolved ? proposal : confirmedVersion,
    // Confirmation is what empties this list; the card reads no other state.
    pending_versions: resolved ? [] : [proposal],
    versions: [confirmedVersion, proposal],
    lineage: [],
    drift: [],
  }
}

test('checkpoint proposal confirmation completes before recovery with an audited direction', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  const item = activity('operator-checkpoint', false)
  item.task.context = {
    designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }],
  }
  const calls: string[] = []
  let confirmHeaders: Record<string, string> = {}
  let recoveryBody: Record<string, unknown> | undefined
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('operator-checkpoint', false) }),
  )
  await page.route('**/v1/system-designs/design-lifecycle/versions/2/confirm**', async (route) => {
    calls.push('confirm')
    confirmHeaders = route.request().headers()
    await route.fulfill({ json: {} })
  })
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', async (route) => {
    calls.push('recover')
    recoveryBody = route.request().postDataJSON() as Record<string, unknown>
    await route.fulfill({ json: { id: 'operator-checkpoint-implement-1', state: 'queued', claimable: true } })
  })

  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Confirm proposals and recover work order' })
  await expect(card).toHaveCount(1)
  await expect(card.getByText(checkpointProgress, { exact: true })).toBeVisible()
  await expect(card.getByText('Version 2 proposed by this task')).toBeVisible()
  await expect(card.getByRole('link', { name: 'Review or dismiss proposals' })).toHaveAttribute(
    'href',
    /\/inbox\?.*task=operator-checkpoint/,
  )
  await expect(page.getByLabel('Operator direction')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'System Design proposals from this task' })).toHaveCount(0)

  await card.getByRole('button', { name: 'Confirm v2 and recover' }).click()
  await expect.poll(() => calls).toEqual(['confirm', 'recover'])
  expect(confirmHeaders.authorization).toBeUndefined()
  expect(confirmHeaders['if-match']).toBe('"1"')
  expect(recoveryBody?.direction).toBe('Operator confirmed design-lifecycle v2.')
  expect(recoveryBody?.request_id).toBeTruthy()
})

test('checkpoint proposal confirmation failure is visible and does not recover', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  const item = activity('operator-checkpoint', false)
  item.task.context = {
    designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }],
  }
  let recoveryCalls = 0
  await page.route('**/v1/tasks/operator-checkpoint/activity*', (route) => route.fulfill({ json: item }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('operator-checkpoint', false) }),
  )
  await page.route('**/v1/system-designs/design-lifecycle/versions/2/confirm**', (route) =>
    route.fulfill({ status: 500, json: { message: 'Confirmation rejected by operator policy.' } }),
  )
  await page.route('**/v1/work-orders/attempt-recovery-implement-1/recover*', (route) => {
    recoveryCalls++
    return route.fulfill({ json: {} })
  })

  await page.goto('/w/demo/tasks/operator-checkpoint/full')
  const card = page.getByRole('region', { name: 'Confirm proposals and recover work order' })
  await card.getByRole('button', { name: 'Confirm v2 and recover' }).click()
  await expect(card.getByText('Confirmation rejected by operator policy.')).toBeVisible()
  expect(recoveryCalls).toBe(0)
})

test("a task's own System Design proposal is confirmable from its detail and clears in place", async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  let confirmed = false
  let pendingAuthority = true
  let confirmHeaders: Record<string, string> = {}
  let releaseActivityEvent = () => {}
  const activityEvent = new Promise<void>((resolve) => {
    releaseActivityEvent = resolve
  })
  await page.route('**/v1/tasks/design-proposal/activity*', (route) => {
    const item = activity('design-proposal', false)
    return route.fulfill({
      json: { ...item, pending_authority: pendingAuthority, proposal_claim_waiting: pendingAuthority },
    })
  })
  await page.route('**/v1/tasks/design-proposal/events/stream*', async (route) => {
    await activityEvent
    await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
  })
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('design-proposal', confirmed) }),
  )
  await page.route('**/v1/system-designs/design-lifecycle/versions/2/confirm**', async (route) => {
    confirmHeaders = route.request().headers()
    confirmed = true
    pendingAuthority = false
    await route.fulfill({ json: {} })
    releaseActivityEvent()
  })

  await page.goto('/w/demo/tasks/design-proposal/full')
  const card = page.getByRole('region', { name: 'Verification and review are waiting on a document decision' })
  await expect(card).toHaveCount(1)
  await expect(card).toContainText('Verification and review cannot be claimed until you confirm or dismiss')
  await expect(card.getByRole('list', { name: 'Proposals verification and review are waiting on' })).toContainText(
    'System Design design-lifecycle v2',
  )
  await expect(card).toContainText('System Design update proposed')
  await expect(card).toContainText('Version 2 proposed by this task')
  await expect(card.getByRole('link', { name: 'Confirm or dismiss the proposal' })).toHaveAttribute(
    'href',
    /\/inbox\?.*task=design-proposal/,
  )
  await expect(card.getByRole('link', { name: 'Work-order lifecycle' })).toHaveAttribute(
    'href',
    /system-design.*document=design-lifecycle/,
  )

  // A confirm that clears the card without a full-page reload keeps this marker.
  await page.evaluate(() => ((window as unknown as { noReload?: boolean }).noReload = true))
  await card.getByRole('button', { name: 'Confirm version 2' }).click()
  await expect(card).toHaveCount(0)
  await expect(page.getByText('Verification and review are waiting on a System Design decision')).toHaveCount(0)
  expect(await page.evaluate(() => (window as unknown as { noReload?: boolean }).noReload)).toBe(true)
  expect(confirmHeaders.authorization).toBeUndefined()
  expect(confirmHeaders['if-match']).toBe('"1"')

  // The same composition carries it on the sheet route, not just the full page.
  confirmed = false
  pendingAuthority = true
  await page.goto('/w/demo/tasks/design-proposal')
  await expect(
    page
      .getByRole('region', { name: 'Verification and review are waiting on a document decision' })
      .getByRole('button', {
        name: 'Confirm version 2',
      }),
  ).toBeVisible()
})

test('maintainer sees a pending task System Design proposal without an enabled Confirm action', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_maintainer', role: 'maintainer' }, route.request()) }),
  )
  await page.route('**/v1/tasks/design-proposal/activity*', (route) => {
    const item = activity('design-proposal', false)
    return route.fulfill({ json: { ...item, pending_authority: true } })
  })
  await page.route('**/v1/tasks/design-proposal/events/stream*', (route) => route.fulfill({ status: 204 }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('design-proposal', false) }),
  )

  await page.goto('/w/demo/tasks/design-proposal/full')
  const card = page.getByRole('region', { name: 'Verification and review are waiting on a document decision' })
  await expect(card).toHaveCount(1)
  await expect(card).toContainText('System Design update proposed')
  await expect(card.getByRole('button', { name: 'Confirm version 2' })).toBeDisabled()
  await expect(card.getByText('Document confirmation capability is required.')).toBeVisible()
})

test('task detail renders no proposal card for a pending version another task raised', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: designCollection('other-task', false) }))

  await page.goto('/w/demo/tasks/design-proposal/full')
  await expect(page.getByRole('region', { name: 'Activity' })).toBeVisible()
  await expect(
    page.getByRole('region', { name: 'Verification and review are waiting on a document decision' }),
  ).toContainText('Confirm or dismiss the proposal')
  await expect(page.getByRole('region', { name: 'System Design proposals from this task' })).toHaveCount(0)
  await expect(page.getByText('System Design update proposed')).toHaveCount(0)
})

test('task detail renders a proposal-only card without the review-gate headline', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('proposal-only', false) }),
  )

  await page.goto('/w/demo/tasks/proposal-only/full')
  const card = page.getByRole('region', { name: 'System Design proposals from this task' })
  await expect(card).toContainText('System Design update proposed')
  await expect(card.getByRole('button', { name: 'Confirm version 2' })).toBeVisible()
  await expect(card).not.toContainText('Verification and review are waiting on a System Design decision')
})

function pendingQueue(taskId: string, items: Array<{ id: string; tier: string; version?: number; title?: string }>) {
  const queued = [
    ...items.map((item) => ({
      title: 'Pending document',
      ...item,
      origin_type: 'task',
      origin_id: taskId,
      proposed_at: createdAt,
      age_seconds: 1,
    })),
    // Neither another task nor a non-task origin changes this task's notice.
    ...['task', 'operator'].map((origin_type) => ({
      id: `unrelated-${origin_type}`,
      title: 'Unrelated proposal',
      tier: 'decision',
      origin_type,
      origin_id: origin_type === 'task' ? 'other-task' : taskId,
      proposed_at: createdAt,
      age_seconds: 1,
    })),
  ]
  return {
    items: queued,
    attention: { task_count: 1, pending_proposal_count: queued.length, total: queued.length + 1 },
  }
}

function queuedOrder(taskId: string, stage: 'implement' | 'verify' | 'review') {
  return {
    id: `${taskId}-${stage}-1`,
    task_id: taskId,
    stage,
    state: 'queued',
    ...(stage === 'review' ? { review_seat: 1 } : {}),
    claimable: stage === 'implement',
    created_at: createdAt,
    updated_at: createdAt,
  }
}

// req-260810-70ce2f REQ-1 and req-260810-23b69f AC-2.1: the wait copy is read
// from the server's waiting_proposals, never from the tiers the task authored.
for (const withDesignCard of [false, true]) {
  for (const { tiers, noun } of [
    { tiers: ['requirement'], noun: 'requirement' },
    { tiers: ['system_design'], noun: 'System Design' },
    { tiers: ['requirement', 'system_design'], noun: 'document' },
    { tiers: ['future_kind'], noun: 'document' },
    { tiers: ['system_design', 'future_kind'], noun: 'document' },
    { tiers: [], noun: 'document' },
  ]) {
    test(`verification and review wait copy uses ${noun} for ${tiers.join('+') || 'missing kinds'} with design card ${withDesignCard}`, async ({
      page,
    }) => {
      const taskId = 'design-proposal-queued'
      const blockers = tiers.map((tier, index) => ({ tier, id: `pending-${index}`, version: 2 }))
      await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
      await page.route('**/v1/system-designs**', (route) =>
        route.fulfill({ json: withDesignCard ? designCollection(taskId, false) : [] }),
      )
      await page.route('**/v1/pending-proposals*', (route) => route.fulfill({ json: pendingQueue(taskId, blockers) }))
      await page.route('**/v1/tasks/design-proposal-queued/activity*', (route) =>
        route.fulfill({
          json: {
            ...activity(taskId, false),
            proposal_claim_waiting: true,
            waiting_proposals: blockers,
            work_orders: [queuedOrder(taskId, 'review')],
          },
        }),
      )
      await page.goto(`/w/demo/tasks/${taskId}/full`)
      const card = page.getByRole('region', { name: 'Verification and review are waiting on a document decision' })
      await expect(
        card.getByText(`Verification and review are waiting on a ${noun} decision`, { exact: true }),
      ).toBeVisible()
      const proposal = tiers.length > 1 ? 'proposals' : 'proposal'
      await expect(card).toContainText(
        `Verification and review cannot be claimed until you confirm or dismiss the task's pending ${proposal}.`,
      )
      await expect(card.getByRole('link', { name: `Confirm or dismiss the ${proposal}`, exact: true })).toHaveAttribute(
        'href',
        /\/inbox\?.*task=design-proposal-queued/,
      )
      await expect(card.getByText('System Design update proposed')).toHaveCount(withDesignCard ? 1 : 0)
      await expect(card.getByRole('list', { name: 'Proposals review will not include' })).toHaveCount(0)
      await expect(page.getByRole('button', { name: 'Redispatch', exact: true })).toHaveCount(0)
      await expect(page.getByText('Queued — re-enqueue if dispatch stalled.')).toHaveCount(0)
    })
  }
}

// A task-authored decision proposal is listed and flagged, but it never holds
// verification or review, so no surface may say a claim is blocked
// (req-260810-23b69f AC-2.1, AC-2.2; REQ-3).
for (const stage of ['verify', 'review'] as const) {
  test(`a pending decision proposal says review will not include it with a queued ${stage} order`, async ({ page }) => {
    const taskId = 'design-proposal-queued'
    await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
    await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: [] }))
    await page.route('**/v1/pending-proposals*', (route) =>
      route.fulfill({
        json: pendingQueue(taskId, [{ id: 'DEC-71', tier: 'decision', title: 'Keep the gate narrow.' }]),
      }),
    )
    await page.route('**/v1/tasks/design-proposal-queued/activity*', (route) =>
      route.fulfill({
        json: {
          ...activity(taskId, false),
          needs_attention: true,
          pending_authority: true,
          proposal_claim_waiting: false,
          waiting_proposals: undefined,
          work_orders: [queuedOrder(taskId, stage)],
        },
      }),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const card = page.getByRole('region', { name: 'Pending proposals from this task' })
    await expect(
      card.getByText('Review will not include this pending decision proposal', { exact: true }),
    ).toBeVisible()
    await expect(card).toContainText('Verification and review are not waiting on it.')
    await expect(card.getByRole('list', { name: 'Proposals review will not include' })).toContainText(
      'Decision DEC-71 — Keep the gate narrow.',
    )
    await expect(card).not.toContainText('Unrelated proposal')
    await expect(card.getByRole('link', { name: 'Confirm or dismiss the proposal', exact: true })).toHaveAttribute(
      'href',
      /\/inbox\?.*task=design-proposal-queued/,
    )
    await expect(page.getByText(/cannot be claimed/)).toHaveCount(0)
    await expect(page.getByText(/(Review is|are) waiting on/)).toHaveCount(0)
    await expect(
      page.getByRole('region', { name: 'Verification and review are waiting on a document decision' }),
    ).toHaveCount(0)
    // The decision is an attention signal, not a claim wait, so recovery stays available.
    await expect(page.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
  })
}

test('a mixed proposal notice attributes the wait only to the requirement', async ({ page }) => {
  const taskId = 'design-proposal-queued'
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/system-designs**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/pending-proposals*', (route) =>
    route.fulfill({
      json: pendingQueue(taskId, [
        { id: 'req-claims', tier: 'requirement', version: 3, title: 'Claim waits' },
        { id: 'DEC-72', tier: 'decision', title: 'Record the dashboard wording.' },
      ]),
    }),
  )
  await page.route('**/v1/tasks/design-proposal-queued/activity*', (route) =>
    route.fulfill({
      json: {
        ...activity(taskId, false),
        proposal_claim_waiting: true,
        waiting_proposals: [{ tier: 'requirement', id: 'req-claims', version: 3 }],
        work_orders: [queuedOrder(taskId, 'verify')],
      },
    }),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const card = page.getByRole('region', { name: 'Verification and review are waiting on a document decision' })
  await expect(
    card.getByText('Verification and review are waiting on a requirement decision', { exact: true }),
  ).toBeVisible()
  const blockers = card.getByRole('list', { name: 'Proposals verification and review are waiting on' })
  await expect(blockers).toContainText('Requirement req-claims v3 — Claim waits')
  await expect(blockers).not.toContainText('DEC-72')
  const signalOnly = card.getByRole('list', { name: 'Proposals review will not include' })
  await expect(signalOnly).toContainText('Decision DEC-72 — Record the dashboard wording.')
  await expect(signalOnly).not.toContainText('req-claims')
  await expect(card).toContainText('Review will not include this pending decision proposal')
  await expect(card).toContainText('Verification and review are not waiting on it.')
  await expect(card.getByRole('link', { name: 'Confirm or dismiss the proposals', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Redispatch', exact: true })).toHaveCount(0)
})

for (const { waiting, stage } of [
  { waiting: false, stage: 'review' },
  { waiting: false, stage: 'verify' },
  { waiting: true, stage: 'implement' },
] as const) {
  test(`redispatch remains available for queued ${stage} with claim waiting ${waiting}`, async ({ page }) => {
    const taskId = 'design-proposal-queued'
    await page.route('**/v1/tasks/design-proposal-queued/activity*', (route) =>
      route.fulfill({
        json: {
          ...activity(taskId, false),
          // The attention signal stays on: only the claim wait suppresses recovery.
          pending_authority: true,
          proposal_claim_waiting: waiting,
          waiting_proposals: waiting ? [{ tier: 'system_design', id: 'design-lifecycle', version: 2 }] : undefined,
          work_orders: [{ ...queuedOrder(taskId, stage), claimable: true }],
        },
      }),
    )
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    await expect(page.getByRole('button', { name: 'Redispatch', exact: true })).toBeVisible()
    await expect(page.getByText('Queued — re-enqueue if dispatch stalled.')).toBeVisible()
    if (!waiting)
      await expect(
        page.getByRole('region', { name: 'Verification and review are waiting on a document decision' }),
      ).toHaveCount(0)
  })
}

// The read is the workspace-wide collection, so origin alone would carry a
// proposal onto a task that does not hold the document. Attachment is the other
// half of the own-proposal scope: 'unattached-proposal' raised this pending version,
// but carries a different document, so the decision stays on the document's own
// attention surface.
test('task detail renders no proposal card for a document the task does not carry', async ({ page }) => {
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/system-designs**', (route) =>
    route.fulfill({ json: designCollection('unattached-proposal', false) }),
  )

  await page.goto('/w/demo/tasks/unattached-proposal/full')
  await expect(page.getByRole('region', { name: 'Activity' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'System Design proposals from this task' })).toHaveCount(0)
  await expect(page.getByText('System Design update proposed')).toHaveCount(0)
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
// it and nothing else (AC-2.3, AC-2.7; DEC-74(4)).
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

test('assignment history resolves a prior assignee from the workspace roster', async ({ page }) => {
  await routeAssignment(page, { operator: true })
  await page.route('**/v1/tasks/assignable/activity**', async (route) => {
    const item = activity('assignable', false)
    item.task.assignee = { user_id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Owner' }
    item.events = [
      {
        id: 901,
        task_id: 'assignable',
        kind: 'task.assignee.set',
        actor_id: 'usr_ada',
        actor_role: 'human',
        payload: { assignee_user_id: 'usr_bo' },
        at: '2026-07-15T12:00:00Z',
      },
    ]
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/assignable/full')
  await expect(page.getByRole('region', { name: 'Activity' })).toContainText('Assigned to Bo Member')
  await expect(page.getByRole('region', { name: 'Activity' })).not.toContainText('Assigned to usr_bo')
})

// AC-1.2/AC-2.2: setting an assignee is an operator act. A member reads who
// holds the task and is offered no way to change it.
test('a non-operator sees the assignee but no set or clear control', async ({ page }) => {
  await routeAssignment(page, { operator: false })
  await page.route('**/v1/tasks/assignable/activity**', async (route) => {
    const item = activity('assignable', false)
    item.task.assignee = { user_id: 'usr_bo', email: 'bo@example.test', display_name: 'Bo Member' }
    await route.fulfill({ json: item })
  })

  await page.goto('/w/demo/tasks/assignable/full')
  await expect(page.getByTitle('Assigned to Bo Member — bo@example.test · usr_bo')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Task actions' })).toHaveCount(0)
  await expect(page.getByRole('menuitem')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Reassign' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Clear assignee' })).toHaveCount(0)
})

// AC-4.1/AC-4.3: the store refuses a claim on an assigned task by naming the
// assignee's raw account ID. That sentence is written for an agent's error
// channel, so the surface says who holds the task instead.
test('a claim refusal names the assignee rather than showing the transport sentence', async ({ page }) => {
  await showAllBoardTasks(page)
  await page.route('**/v1/workspaces', (route) => route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] }))
  await page.route('**/v1/workspaces/demo/members**', (route) => route.fulfill({ json: assignmentMembers }))
  await page.route('**/v1/activity*', (route) =>
    route.fulfill({
      json: [
        {
          task: {
            ...activity('refused', false).task,
            // The task has since been reassigned; the refusal still names the
            // member who held it when the failed claim was recorded.
            assignee: { user_id: 'usr_ada', email: 'ada@example.test', display_name: 'Ada Owner' },
          },
          latest_stage: 'implement',
          last_event_at: createdAt,
          needs_attention: true,
          stalled: {
            needed: true,
            reason: 'dispatch is failing repeatedly',
            last_failure: 'task refused is assigned to usr_bo; only that assignee may claim its work orders',
          },
        },
      ],
    }),
  )

  await page.goto('/')
  const card = page.getByRole('region', { name: 'Implement' }).getByRole('link', { name: /Short task/ })
  await expect(card).toContainText('Assigned to Bo Member — only they can pick this up.')
  await expect(card).not.toContainText('only that assignee may claim its work orders')
  await expect(card).not.toContainText('usr_bo')
})

for (const withNotes of [true, false]) {
  test(`review card renders optional operator notes: ${withNotes}`, async ({ page }) => {
    await page.route('**/v1/tasks/reviews/activity*', (route) =>
      route.fulfill({
        json: {
          ...activity('reviews', false),
          ...(withNotes
            ? {
                operator_notes: [
                  {
                    document_id: 'req-intent',
                    tier: 'requirement',
                    version: 2,
                    note: 'Preserve <original> intent.\nKeep the scope.',
                    dismissed_at: createdAt,
                  },
                  {
                    document_id: 'design-owner',
                    tier: 'system_design',
                    version: 4,
                    note: 'Correct the owner.',
                    dismissed_at: createdAt,
                  },
                ],
              }
            : {}),
        },
      }),
    )
    await page.goto('/w/demo/tasks/reviews/full')
    const panel = page.locator('article').filter({ hasText: /Panel of 2\b.*unanimous to pass/ })
    await expect(panel).toBeVisible()
    const notes = panel.getByRole('region', { name: "Operator notes on this task's proposals" })
    if (withNotes) {
      await expect(notes).toContainText('Requirement req-intent · v2')
      await expect(notes).toContainText('System Design design-owner · v4')
      await expect(notes).toContainText("Operator's reason: Preserve <original> intent.")
      await expect(notes).toContainText('Keep the scope.')
      await expect(notes).toContainText("Operator's reason: Correct the owner.")
    } else await expect(notes).toHaveCount(0)
  })
}

function restartFixture(taskId = 'restart-source') {
  const base = activity(taskId, false)
  return {
    ...base,
    task: { ...base.task, state: 'running' },
    events: [
      {
        id: 1,
        task_id: taskId,
        kind: 'pull_request.opened',
        payload: { url: 'https://github.com/example/conveyor/pull/42', number: 42 },
        at: createdAt,
      },
    ],
    work_orders: ['claimed', 'queued', 'completed', 'cancelled'].map((state, index) => ({
      id: `${taskId}-implement-${index}`,
      task_id: taskId,
      job_id: `${taskId}-implement-${index}`,
      stage: 'implement',
      state,
      claimable: false,
      queue_entered_at: createdAt,
      created_at: createdAt,
      updated_at: createdAt,
    })),
  }
}

const restartProposal = {
  id: 'req-restart',
  title: 'Restart lifecycle',
  tier: 'requirement',
  version: 7,
  origin_type: 'task',
  origin_id: 'restart-source',
  proposed_at: createdAt,
  age_seconds: 10,
}

async function mockRestart(page: Page, options: { role?: string; proposals?: (typeof restartProposal)[] } = {}) {
  await page.route('**/v1/me**', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'usr_restart', role: options.role ?? 'operator' }, route.request()) }),
  )
  await page.route('**/v1/tasks/restart-source/activity*', (route) => route.fulfill({ json: restartFixture() }))
  await page.route('**/v1/pending-proposals*', (route) =>
    route.fulfill({
      json: {
        items: options.proposals ?? [restartProposal],
        attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
      },
    }),
  )
}

async function openRestart(page: Page) {
  await page.goto('/w/demo/tasks/restart-source/full')
  await page.getByRole('button', { name: 'Task actions' }).click()
  await page.getByRole('menuitem', { name: 'Start over', exact: true }).click()
  return page.getByRole('dialog', { name: 'Start over', exact: true })
}

test('start over previews orders, task-owned proposal versions and the PR, then navigates with one idempotent request', async ({
  page,
}) => {
  await mockRestart(page, {
    proposals: [
      restartProposal,
      { ...restartProposal, id: 'unrelated-task', title: 'Other task proposal', origin_id: 'another-task' },
      { ...restartProposal, id: 'unrelated-origin', title: 'Operator proposal', origin_type: 'operator' },
      { ...restartProposal, id: 'unrelated-tier', title: 'Decision proposal', tier: 'decision' },
    ],
  })
  const requests: Array<{ body: Record<string, unknown>; headers: Record<string, string> }> = []
  const successor = {
    ...restartFixture('restart-next'),
    task: {
      ...restartFixture('restart-next').task,
      supersedes: 'restart-source',
      intake_operator_direction: 'Start-over reason: Wrong approach\nOperator note: Keep the confirmed pins.',
    },
  }
  await page.route('**/v1/tasks/restart-next/activity*', (route) => route.fulfill({ json: successor }))
  await page.route('**/v1/tasks/restart-source/restart*', async (route) => {
    requests.push({ body: route.request().postDataJSON(), headers: route.request().headers() })
    if (requests.length === 1) return route.abort('failed')
    await route.fulfill({
      status: 201,
      json: {
        task: { ...restartFixture().task, state: 'closed', superseded_by: 'restart-next' },
        successor: successor.task,
        created: false,
      },
    })
  })
  const dialog = await openRestart(page)
  await expect(dialog.getByText('2 non-terminal work orders will be cancelled.')).toBeVisible()
  await expect(
    dialog.getByText('1 pending document proposal from this task will be dismissed with your note:'),
  ).toBeVisible()
  await expect(dialog.getByText('Restart lifecycle v7')).toBeVisible()
  await expect(dialog.getByText(/Other task proposal|Operator proposal|Decision proposal/)).toHaveCount(0)
  await expect(dialog.getByRole('link')).toHaveAttribute('href', 'https://github.com/example/conveyor/pull/42')
  await expect(dialog.getByText(/new task with a new branch/)).toBeVisible()
  await dialog.getByLabel('Reason', { exact: false }).fill('  Wrong approach  ')
  await dialog.getByLabel('What went wrong', { exact: false }).fill('Keep the confirmed pins.')
  await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
  await expect(page).toHaveURL(/\/tasks\/restart-next\/full$/)
  await expect(page.getByRole('status').filter({ hasText: 'Started over as restart-next' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'restart-source', exact: true })).toHaveAttribute(
    'href',
    '/w/demo/tasks/restart-source/full',
  )
  await expect(page.getByRole('region', { name: 'Operator direction', exact: true })).toContainText(
    'Keep the confirmed pins.',
  )
  expect(requests).toHaveLength(2)
  expect(requests[0].body).toEqual(requests[1].body)
  expect(requests[0].body).toMatchObject({
    reason: 'Wrong approach',
    note: 'Keep the confirmed pins.',
    request_id: expect.any(String),
  })
  expect(requests[0].headers['x-idempotency-key']).toBe(requests[0].body.request_id)
  expect(requests[1].headers['x-idempotency-key']).toBe(requests[0].body.request_id)
  expect(requests[0].headers['x-conveyor-csrf']).toBe('1')
})

for (const role of ['viewer', 'executor', 'contributor']) {
  test(`start over is hidden without operate_gates for ${role}`, async ({ page }) => {
    await mockRestart(page, { role })
    await page.goto('/w/demo/tasks/restart-source/full')
    await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Task actions' })).toHaveCount(0)
    await expect(page.getByRole('menuitem')).toHaveCount(0)
  })
}
for (const state of ['closed', 'merged']) {
  test(`start over is hidden for terminal ${state} tasks`, async ({ page }) => {
    await mockRestart(page)
    const item = restartFixture()
    item.task.state = state
    await page.route('**/v1/tasks/restart-source/activity*', (route) => route.fulfill({ json: item }))
    await page.goto('/w/demo/tasks/restart-source/full')
    await expect(page.getByRole('heading', { name: 'Short task' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Task actions' })).toHaveCount(0)
    await expect(page.getByRole('menuitem')).toHaveCount(0)
  })
}

test('start over enforces reason and note limits including Unicode character counts', async ({ page }) => {
  await mockRestart(page)
  const dialog = await openRestart(page)
  const submit = dialog.getByRole('button', { name: 'Start over', exact: true })
  const reason = dialog.getByLabel('Reason', { exact: false })
  const note = dialog.getByLabel('What went wrong', { exact: false })
  await expect(submit).toBeDisabled()
  await reason.fill('   ')
  await expect(submit).toBeDisabled()
  await reason.fill('a'.repeat(201))
  await expect(dialog.getByText('Reason must be at most 200 characters.')).toBeVisible()
  await expect(submit).toBeDisabled()
  await reason.fill('😀'.repeat(200))
  await expect(submit).toBeEnabled()
  await note.fill('n'.repeat(2001))
  await expect(dialog.getByText('2001 / 2000 characters')).toBeVisible()
  await expect(submit).toBeDisabled()
  await note.fill('😀'.repeat(2000))
  await expect(dialog.getByText('2000 / 2000 characters')).toBeVisible()
  await expect(submit).toBeEnabled()
})

for (const tier of ['requirement', 'system_design']) {
  test(`start over requires confirm_documents for task-owned ${tier} proposals`, async ({ page }) => {
    await mockRestart(page, { role: 'maintainer', proposals: [{ ...restartProposal, tier }] })
    const dialog = await openRestart(page)
    await dialog.getByLabel('Reason', { exact: false }).fill('Try again')
    await expect(dialog.getByRole('alert')).toContainText('An operator with confirm_documents')
    await expect(dialog.getByRole('button', { name: 'Start over', exact: true })).toBeDisabled()
  })
}

test('start over permits a maintainer when proposals belong only to other tasks', async ({ page }) => {
  await mockRestart(page, { role: 'maintainer', proposals: [{ ...restartProposal, origin_id: 'another-task' }] })
  const dialog = await openRestart(page)
  await dialog.getByLabel('Reason', { exact: false }).fill('Try again')
  await expect(dialog.getByRole('button', { name: 'Start over', exact: true })).toBeEnabled()
})

test('start over blocks loading and failed proposal reads and can retry the preview', async ({ page }) => {
  await mockRestart(page)
  let release = () => {}
  const response = new Promise<void>((resolve) => {
    release = resolve
  })
  let failing = true
  await page.route('**/v1/pending-proposals*', async (route) => {
    await response
    if (failing) return route.fulfill({ status: 503, body: 'Unavailable' })
    return route.fulfill({ json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } } })
  })
  const dialog = await openRestart(page)
  await dialog.getByLabel('Reason', { exact: false }).fill('Try again')
  await expect(dialog.getByText('Loading pending proposals…')).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Start over', exact: true })).toBeDisabled()
  release()
  await expect(dialog.getByText('Pending proposals could not be loaded. Retry before starting over.')).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Start over', exact: true })).toBeDisabled()
  failing = false
  await dialog.getByRole('button', { name: 'Retry preview' }).click()
  await expect(dialog.getByRole('button', { name: 'Start over', exact: true })).toBeEnabled()
})

for (const [status, code, message] of [
  [403, 'start_over_confirm_documents', 'task has pending proposals: confirm_documents capability is required'],
  [409, 'start_over_request_conflict', 'request_id was already used with a different reason or note'],
  [409, 'task_terminal', 'terminal tasks cannot start over; create a task with POST /v1/tasks'],
] as const) {
  test(`start over preserves server refusal: ${message}`, async ({ page }) => {
    await mockRestart(page)
    let calls = 0
    await page.route('**/v1/tasks/restart-source/restart*', (route) => {
      calls++
      return route.fulfill({ status, json: { error: code, message } })
    })
    const dialog = await openRestart(page)
    await dialog.getByLabel('Reason', { exact: false }).fill('Try again')
    await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
    await expect(dialog.getByRole('alert')).toHaveText(message)
    expect(calls).toBe(1)
    await expect(page).toHaveURL(/restart-source\/full$/)
  })
}

test('start over shows the restart request ID without putting it in the message', async ({ page }) => {
  await mockRestart(page)
  await page.route('**/v1/tasks/restart-source/restart?*', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback()
    await route.fulfill({
      status: 500,
      headers: { 'X-Request-Id': 'req-restart-500' },
      json: { error: 'internal_error', message: 'internal server error' },
    })
  })
  const dialog = await openRestart(page)
  await dialog.getByLabel('Reason', { exact: false }).fill('Try again')
  await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText('internal server error')
  await expect(dialog.getByTestId('task-restart-request-id')).toHaveText('Request ID: req-restart-500')
  await expect(dialog.getByRole('alert')).not.toContainText('req-restart-500')
})

for (const [state, message] of [
  ['closed', 'Pull request closed.'],
  ['retrying', 'Pull request closure will be retried.'],
  ['failed', 'Pull request closure failed; the restart remains complete.'],
  ['queued', 'Pull request closure is queued.'],
  ['skipped', 'Pull request closure was skipped.'],
]) {
  test(`start over link survives recorded PR close outcome ${state}`, async ({ page }) => {
    await mockRestart(page)
    const base = restartFixture()
    await page.route('**/v1/tasks/restart-source/activity*', (route) =>
      route.fulfill({
        json: {
          ...base,
          task: {
            ...base.task,
            state: 'closed',
            superseded_by: 'restart-next',
            pull_request_close_state: state,
            pull_request_close: {
              state,
              ...(state === 'failed' || state === 'retrying' ? { last_error: 'Forge unavailable' } : {}),
            },
          },
        },
      }),
    )
    await page.goto('/w/demo/tasks/restart-source/full')
    await expect(page.getByRole('link', { name: 'Started over as restart-next' })).toHaveAttribute(
      'href',
      '/w/demo/tasks/restart-next/full',
    )
    await expect(page.getByText(message, { exact: state !== 'failed' && state !== 'retrying' })).toBeVisible()
    if (state === 'failed' || state === 'retrying') await expect(page.getByText(/Forge unavailable/)).toBeVisible()
  })
}

for (const empty of [undefined, null, '']) {
  test(`start over omits absent restart facts (${String(empty)})`, async ({ page }) => {
    await mockRestart(page)
    const base = restartFixture()
    await page.route('**/v1/tasks/restart-source/activity*', (route) =>
      route.fulfill({
        json: {
          ...base,
          work_orders: empty === '' ? [] : empty,
          events: [],
          task: {
            ...base.task,
            supersedes: empty,
            superseded_by: empty,
            intake_operator_direction: empty,
            pull_request_close_state: empty,
            pull_request_close: empty === '' ? { state: '' } : empty,
          },
        },
      }),
    )
    const dialog = await openRestart(page)
    await expect(page.getByText(/Started over as|Restarted from|Pull request closed\./)).toHaveCount(0)
    await expect(page.getByRole('region', { name: 'Operator direction', exact: true })).toHaveCount(0)
    await expect(dialog.getByText(/non-terminal work orders|Pull request/)).toHaveCount(0)
  })
}

test('start over board cards mark both sides and sheet links use sheet routes', async ({ page }) => {
  await mockRestart(page)
  await showAllBoardTasks(page)
  const retired = {
    ...restartFixture(),
    task: { ...restartFixture().task, title: 'Retired restart task', state: 'closed', superseded_by: 'restart-next' },
  }
  const successor = {
    ...restartFixture('restart-next'),
    task: { ...restartFixture('restart-next').task, title: 'Successor restart task', supersedes: 'restart-source' },
  }
  await page.route('**/v1/activity*', (route) => route.fulfill({ json: [retired, successor] }))
  await page.route('**/v1/tasks/restart-source/activity*', (route) => route.fulfill({ json: retired }))
  await page.route('**/v1/tasks/restart-next/activity*', (route) => route.fulfill({ json: successor }))
  await page.goto('/')
  await expect(page.getByRole('link').filter({ hasText: 'Retired restart task' })).toContainText('restarted')
  await expect(page.getByRole('link').filter({ hasText: 'Successor restart task' })).toContainText('restarted')
  await page.getByRole('link').filter({ hasText: 'Retired restart task' }).click()
  await expect(page.getByRole('link', { name: 'Started over as restart-next' })).toHaveAttribute(
    'href',
    '/w/demo/tasks/restart-next',
  )
  await page.getByRole('link', { name: 'Started over as restart-next' }).click()
  await expect(page.getByRole('link', { name: 'restart-source', exact: true })).toHaveAttribute(
    'href',
    '/w/demo/tasks/restart-source',
  )
})

test('start over manual retry preserves the request after automatic network retries fail', async ({ page }) => {
  await mockRestart(page, { proposals: [] })
  const bodies: Record<string, unknown>[] = []
  await page.route('**/v1/tasks/restart-source/restart*', (route) => {
    bodies.push(route.request().postDataJSON())
    return route.abort('failed')
  })
  const dialog = await openRestart(page)
  await dialog.getByLabel('Reason', { exact: false }).fill('Keep this request')
  await dialog.getByRole('button', { name: 'Start over', exact: true }).click()
  await expect(dialog.getByRole('alert')).toBeVisible()
  expect(bodies).toHaveLength(2)
  await expect(dialog.getByLabel('Reason', { exact: false })).toBeDisabled()
  await dialog.getByRole('button', { name: 'Retry start over' }).click()
  await expect.poll(() => bodies.length).toBe(4)
  for (const body of bodies) expect(body).toEqual(bodies[0])
  expect(bodies[0]).not.toHaveProperty('note')
})

test('start over counts recoverable and submitted orders but excludes cancelled and completed orders', async ({
  page,
}) => {
  await mockRestart(page)
  const base = restartFixture()
  await page.route('**/v1/tasks/restart-source/activity*', (route) =>
    route.fulfill({
      json: {
        ...base,
        work_orders: ['stale', 'timed_out', 'submitted', 'completed', 'cancelled'].map((state, index) => ({
          ...base.work_orders[0],
          id: `order-${index}`,
          state,
        })),
      },
    }),
  )
  const dialog = await openRestart(page)
  await expect(dialog.getByText('3 non-terminal work orders will be cancelled.')).toBeVisible()
})

for (const variant of ['sheet', 'full']) {
  test(`task actions menu supports keyboard navigation and dismissal in ${variant}`, async ({ page }) => {
    await page.goto(`/w/demo/tasks/full-header-id${variant === 'full' ? '/full' : ''}`)
    const trigger = page.getByRole('button', { name: 'Task actions' })
    const menu = page.getByRole('menu', { name: 'Task actions' })
    // The trigger is the quiet overflow icon: named for assistive tech and the
    // tooltip, never as visible text that competes with the title.
    await expect(trigger).toHaveAttribute('aria-label', 'Task actions')
    await expect(trigger).toHaveText('')
    // A task carries no hold, so no Hold or Release action sits beside the
    // menu or inside it (DEC-74(3)).
    await expect(page.getByRole('button', { name: /^(Hold|Release)$/ })).toHaveCount(0)
    await expect(menu).toHaveCount(0)
    await trigger.focus()
    await page.keyboard.press('ArrowDown')
    await expect(menu.getByRole('menuitem')).toHaveText(['Link dependency', 'Assign', 'Cancel task', 'Start over'])
    await expect(menu.getByRole('menuitem', { name: 'Link dependency' })).toBeFocused()
    await page.keyboard.press('End')
    await expect(menu.getByRole('menuitem', { name: 'Start over' })).toBeFocused()
    await page.keyboard.press('ArrowDown')
    await expect(menu.getByRole('menuitem', { name: 'Link dependency' })).toBeFocused()
    await page.keyboard.press('ArrowUp')
    await expect(menu.getByRole('menuitem', { name: 'Start over' })).toBeFocused()
    await page.keyboard.press('Home')
    await page.keyboard.press('Space')
    const link = page.getByRole('dialog', { name: 'Link dependency', exact: true })
    await expect(link).toBeVisible()
    await expect(menu).toHaveCount(0)
    await link.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(link).toHaveCount(0)
    await trigger.focus()
    await page.keyboard.press('ArrowUp')
    await expect(menu.getByRole('menuitem', { name: 'Start over' })).toBeFocused()
    await page.keyboard.press('Escape')
    await expect(trigger).toBeFocused()
    await expect(menu).toHaveCount(0)
    await trigger.click()
    await page.keyboard.press('Tab')
    await expect(menu).toHaveCount(0)
    await trigger.click()
    await page.getByRole('heading', { name: 'Short task', exact: true }).click()
    await expect(menu).toHaveCount(0)
    if (variant === 'sheet') await expect(page.getByRole('dialog', { name: 'Task detail', exact: true })).toBeVisible()
  })

  test(`no hold action, badge, or request renders in ${variant} even for a stale hold`, async ({ page }) => {
    // A response from before the retirement may still carry hold: true; the
    // header ignores it and offers no reservation (DEC-74(3)).
    let holdRequests = 0
    await page.route('**/v1/tasks/full-header-id/activity*', (route) => {
      const item = activity('full-header-id', false)
      return route.fulfill({ json: { ...item, task: { ...item.task, hold: true } } })
    })
    await page.route('**/v1/tasks/full-header-id/hold*', (route) => {
      holdRequests++
      return route.fulfill({ status: 410, json: { error: 'task_hold_retired' } })
    })
    await page.goto(`/w/demo/tasks/full-header-id${variant === 'full' ? '/full' : ''}`)
    await expect(page.getByRole('heading', { name: 'Short task', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: /^(Hold|Release)$/ })).toHaveCount(0)
    await expect(page.getByText('Held', { exact: true })).toHaveCount(0)
    await page.getByRole('button', { name: 'Task actions' }).click()
    await expect(page.getByRole('menu', { name: 'Task actions' }).getByRole('menuitem')).toHaveText([
      'Link dependency',
      'Assign',
      'Cancel task',
      'Start over',
    ])
    expect(holdRequests).toBe(0)
  })
}

// Mirrors component-http-api's lightweight record shape, with the full bodies
// available only from the selected audit route.
test('lightweight audit disclosures load on demand, retry, and stay out of SSE refresh', async ({ page }) => {
  const item = activity('audit-detail', false)
  const event = {
    id: 482,
    task_id: 'audit-detail',
    kind: 'review.output_invalid',
    actor_id: 'runner',
    actor_role: 'runner',
    at: createdAt,
    payload: { reason: 'invalid output' },
    audit_available: true,
  }
  const order = {
    id: 'audit-review-7',
    task_id: 'audit-detail',
    job_id: 'audit-review-7',
    stage: 'review',
    state: 'completed',
    created_at: createdAt,
    tokens_in: 0,
    tokens_out: 0,
    audit_available: true,
  }
  let details = 0
  const requests: string[] = []
  let releaseEvent = () => {}
  let releaseStream = () => {}
  const eventGate = new Promise<void>((resolve) => {
    releaseEvent = resolve
  })
  const streamGate = new Promise<void>((resolve) => {
    releaseStream = resolve
  })
  await page.route('**/v1/tasks/audit-detail/activity**', (route) => {
    details++
    return route.fulfill({
      json: {
        ...item,
        events: [event],
        work_orders: [order],
        task: {
          ...item.task,
          dependencies: [{ id: 'dependency', title: 'Required backend', state: 'running' }],
          blocking_task_ids: ['dependency'],
        },
      },
    })
  })
  await page.route('**/v1/tasks/audit-detail/events/stream**', async (route) => {
    await streamGate
    await route.fulfill({ contentType: 'text/event-stream', body: 'event: activity\ndata: {}\n\n' })
  })
  await page.route('**/v1/tasks/audit-detail/audit/**', async (route) => {
    const url = new URL(route.request().url())
    expect(url.searchParams.get('workspace_id')).toBe('demo')
    requests.push(url.pathname)
    if (url.pathname.endsWith('/event/482')) {
      await eventGate
      if (requests.filter((path) => path.endsWith('/event/482')).length === 1)
        return route.fulfill({ status: 404, body: 'Not found' })
      return route.fulfill({
        json: {
          kind: 'event',
          event: {
            ...event,
            payload: {
              governance_snapshot: { designs: [{ content: 'Complete historical authority' }] },
              evidence: '/v1/artifacts/proof',
              output: 'REJECTED-OUTPUT-MUST-STAY-HIDDEN',
            },
          },
        },
      })
    }
    expect(url.pathname).toBe('/v1/tasks/audit-detail/audit/work-order/audit-review-7')
    return route.fulfill({
      json: {
        kind: 'work-order',
        work_order: { ...order, governance_snapshot: { designs: [{ content: 'Complete current authority' }] } },
      },
    })
  })
  const started = Date.now()
  await page.goto('/w/demo/tasks/audit-detail/full')
  await expect(page.getByText('Regression marker at the bottom of the task content.')).toBeVisible()
  await expect(page.getByRole('link', { name: /Required backend/ })).toBeVisible()
  console.log(`seeded lightweight browser render: ${Date.now() - started} ms; mocked response, no live speed claim`)
  expect(requests).toEqual([])
  await page.getByText('Show technical activity').click()
  expect(requests).toEqual([])
  await expect(page.getByText('Event 482', { exact: true })).toBeVisible()
  await page.getByText('Event payload', { exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Loading audit detail' })).toBeVisible()
  releaseEvent()
  await expect(page.getByRole('alert').filter({ hasText: 'Could not load audit detail' })).toBeVisible()
  expect(requests).toHaveLength(1)
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.locator('pre').filter({ hasText: 'Complete historical authority' })).toBeVisible()
  await expect(page.locator('pre').filter({ hasText: '/v1/artifacts/proof' })).toBeVisible()
  await expect(page.getByText('REJECTED-OUTPUT-MUST-STAY-HIDDEN')).toHaveCount(0)
  await page.getByText('Work-order audit audit-review-7 (current record)', { exact: true }).click()
  await expect(page.locator('pre').filter({ hasText: 'Complete current authority' })).toBeVisible()
  expect(requests).toHaveLength(3)
  releaseStream()
  await expect.poll(() => details).toBeGreaterThan(1)
  expect(requests).toHaveLength(3)
  // Reopening fetches the mutable order afresh; it is not frozen in cache.
  await page.getByText('Work-order audit audit-review-7 (current record)', { exact: true }).click()
  await page.getByText('Work-order audit audit-review-7 (current record)', { exact: true }).click()
  await expect.poll(() => requests.length).toBe(4)
})

test('seeded full and lightweight task payloads record comparable render observations', async ({ page }) => {
  const snapshot = { designs: [{ content: 's'.repeat(500_000) }] }
  const base = activity('audit-measurement', false)
  const events = Array.from({ length: 482 }, (_, index) => ({
    id: index + 1,
    task_id: 'audit-measurement',
    kind: 'work_order.updated',
    actor_id: 'runner',
    actor_role: 'runner',
    at: createdAt,
    payload: { message: 'Operational progress', ...(index >= 479 ? { governance_snapshot: snapshot } : {}) },
  }))
  const orders = Array.from({ length: 7 }, (_, index) => ({
    id: `audit-order-${index}`,
    task_id: 'audit-measurement',
    job_id: `audit-order-${index}`,
    stage: 'review',
    state: 'completed',
    created_at: createdAt,
    tokens_in: 0,
    tokens_out: 0,
    ...(index === 6 ? { governance_snapshot: snapshot } : {}),
  }))
  const full = { ...base, events, work_orders: orders }
  const project = (record: object) => {
    let omitted = false
    const data = JSON.parse(
      JSON.stringify(record, (key, value) => {
        if (key === 'governance_snapshot' || key === 'served_requirement_snapshot') {
          omitted = true
          return undefined
        }
        return value
      }),
    )
    return omitted ? { ...data, audit_available: true } : data
  }
  const lightweight = { ...base, events: events.map(project), work_orders: orders.map(project) }
  let selected = JSON.stringify(full)
  let audits = 0
  await page.route('**/v1/tasks/audit-measurement/activity**', (route) =>
    route.fulfill({ contentType: 'application/json', body: selected }),
  )
  await page.route('**/v1/tasks/audit-measurement/audit/**', (route) => {
    audits++
    return route.fulfill({ status: 500 })
  })
  // Warm the same route before both measured navigations.
  await page.goto('/w/demo/tasks/audit-measurement/full')
  await expect(page.getByText('Regression marker at the bottom of the task content.')).toBeVisible()
  for (const [label, data] of [
    ['full', full],
    ['lightweight', lightweight],
  ] as const) {
    selected = JSON.stringify(data)
    const start = Date.now()
    await page.goto('/w/demo/tasks/audit-measurement/full')
    await expect(page.getByText('Regression marker at the bottom of the task content.')).toBeVisible()
    console.log(
      `seeded ${label}: ${Buffer.byteLength(selected)} uncompressed bytes; warm navigation-to-plan ${Date.now() - start} ms (mocked API, same browser/client)`,
    )
    expect(audits).toBe(0)
  }
  expect(Buffer.byteLength(JSON.stringify(lightweight))).toBeLessThan(Buffer.byteLength(JSON.stringify(full)) * 0.15)
})

const creatorCases = [
  { name: 'member', role: 'human', id: 'user:usr_ada', label: 'Ada Owner' },
  { name: 'unprefixed member', role: 'human', id: 'usr_ada', label: 'Ada Owner' },
  { name: 'email fallback', role: 'human', id: 'user:usr_email', label: 'email@example.test' },
  { name: 'ID fallback', role: 'human', id: 'user:usr_id', label: 'usr_id' },
  { name: 'former member', role: 'human', id: 'user:usr_former', label: 'User · usr_former' },
  { name: 'agent', role: 'agent', id: 'agent:usr_ada', label: 'Agent · usr_ada' },
  { name: 'runner', role: 'runner', id: 'worker:worker-1', label: 'Runner · worker-1' },
  { name: 'system', role: 'system', id: 'conveyor', label: 'System · conveyor' },
  { name: 'missing event', role: 'human', id: 'user:usr_ada', label: 'Unknown', missing: true },
  { name: 'missing identifier', role: 'human', id: '', label: 'Unknown' },
  { name: 'empty prefixed identifier', role: 'human', id: 'user:', label: 'Unknown' },
  { name: 'unknown actor class', role: 'unrecognized', id: 'usr_ada', label: 'Unknown' },
]

for (const surface of ['full', 'board sheet']) {
  for (const creator of creatorCases) {
    test(`Created by shows ${creator.name} on the ${surface}`, async ({ page }) => {
      await page.route('**/v1/workspaces/demo/members**', (route) =>
        route.fulfill({
          json: [
            ...assignmentMembers,
            { ...assignmentMembers[0], user_id: 'usr_email', display_name: '', email: 'email@example.test' },
            { ...assignmentMembers[0], user_id: 'usr_id', display_name: '', email: '' },
          ],
        }),
      )
      const item = activity('creator', false)
      const event = {
        id: 1,
        task_id: item.task.id,
        kind: 'task.created',
        actor_role: creator.role,
        actor_id: creator.id,
        payload: {},
        at: createdAt,
      }
      await page.route('**/v1/tasks/creator/activity*', (route) =>
        route.fulfill({
          json: {
            ...item,
            events: [
              { ...event, id: 2, kind: 'task.updated', actor_role: 'human', actor_id: 'user:usr_bo' },
              ...(creator.missing ? [] : [event]),
              { ...event, id: 3, kind: 'task.created_elsewhere', actor_role: 'human', actor_id: 'user:usr_bo' },
            ],
          },
        }),
      )
      await page.goto(`/w/demo/tasks/creator${surface === 'full' ? '/full' : ''}`)
      const sheet = page.getByRole('dialog', { name: 'Task detail' })
      // The creator reads "by <creator>" after the Created time
      // (component-web-task-surfaces, Task detail composition).
      const label = page.locator('dt').filter({ hasText: /^Created$/ })
      const value = label.locator('xpath=following-sibling::dd[1]')
      const creatorName = value.locator('[data-task-creator] > span').first()
      await expect(label).toHaveCount(1)
      await expect(value.locator('[data-task-creator]')).toContainText('by ')
      await expect(creatorName).toHaveText(creator.label)
      if (creator.label !== 'Unknown') {
        await expect(creatorName).toHaveAttribute('title', new RegExp(creator.id))
        await expect(value.locator('.sr-only')).toContainText(creator.id)
      }
      await expect(label.locator('xpath=preceding-sibling::dt[1]')).toHaveText('Assignee')
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 850 })
        if (surface === 'board sheet') await waitForSheetSettled(sheet)
        await expect
          .poll(() =>
            label.evaluate((element) => {
              const repo = element.parentElement!.querySelector('dt')!.getBoundingClientRect()
              const creator = element.getBoundingClientRect()
              return creator.y > repo.y && Math.abs(creator.x - repo.x) < 1
            }),
          )
          .toBe(true)
        await expect
          .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
          .toBe(true)
      }
    })
  }
}

test('Created by preserves a long historical identity when the workspace member read fails', async ({ page }) => {
  const identifier = `user:usr_${'historical'.repeat(30)}`
  await page.route('**/v1/workspaces/demo/members**', (route) => route.fulfill({ status: 500, json: {} }))
  const item = activity('creator-long', false)
  await page.route('**/v1/tasks/creator-long/activity*', (route) =>
    route.fulfill({
      json: {
        ...item,
        events: [{ id: 1, kind: 'task.created', actor_role: 'human', actor_id: identifier, at: createdAt }],
      },
    }),
  )
  await page.setViewportSize({ width: 390, height: 850 })
  await page.goto('/w/demo/tasks/creator-long/full')
  const value = page
    .locator('dt')
    .filter({ hasText: /^Created$/ })
    .locator('xpath=following-sibling::dd[1]')
  const creatorName = value.locator('[data-task-creator] > span').first()
  await expect(creatorName).toHaveText(`User · ${identifier.slice(5)}`)
  await expect(creatorName).toHaveAttribute('title', `User · ${identifier}`)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

const failedTriageReason =
  'Responses API (api.openai.com) image preparation failed for model "gpt-5": artifact image-1 (image/png): content does not match its declared image media type'

function failedTriageFixture(reason = 'Responses API request failed: upstream service unavailable') {
  const taskId = 'failed-triage'
  return {
    task: {
      id: taskId,
      workspace: 'demo',
      title: 'Recover triage image preparation',
      body: 'Inspect the supplied image.',
      source: 'mcp',
      class: 'bug',
      level: '',
      repo: 'conveyor',
      base_branch: 'main',
      branch: 'conveyor/task-failed-triage',
      state: 'awaiting_human',
      next_stage: '',
      recovery_stage: 'triage',
      spec_approval: true,
      merge_approval: true,
      policy_version: 1,
      created_at: createdAt,
    },
    jobs: [
      {
        id: 'failed-triage-triage-1',
        task_id: taskId,
        stage: 'triage',
        state: 'failed',
        harness: 'openai-responses',
        model_tier: 'gpt-5',
        runner: 'in-process',
        confinement: 'control-plane',
        tokens_in: 0,
        tokens_out: 0,
        started_at: createdAt,
        ended_at: '2026-07-15T12:00:01Z',
      },
    ],
    events: [
      {
        id: 461070,
        task_id: taskId,
        job_id: 'failed-triage-triage-1',
        kind: 'pipeline.dispatched',
        actor_id: 'system',
        actor_role: 'system',
        payload: { stage: 'triage', execution: 'in_process' },
        at: createdAt,
      },
      {
        id: 461071,
        task_id: taskId,
        job_id: 'failed-triage-triage-1',
        kind: 'job.failed',
        actor_id: 'system',
        actor_role: 'system',
        payload: {
          error: reason,
          diagnostic: {
            phase: reason === failedTriageReason ? 'attachment_validation' : 'response',
            provider: 'openai_responses',
            endpoint: 'api.openai.com',
            model: 'gpt-5',
            attachment_count: 1,
            retryable: false,
          },
        },
        at: '2026-07-15T12:00:01Z',
      },
    ],
    work_orders: [],
    interventions: [],
    checkout_available: false,
    checkout_guidance: '',
    needs_attention: true,
    at_merge_gate: false,
  }
}

for (const surface of ['full', 'sheet', 'permalink']) {
  test(`failed triage ${surface} shows the recorded image failure without approval`, async ({ page }, testInfo) => {
    const item = failedTriageFixture(failedTriageReason)
    await page.route('**/v1/tasks/failed-triage/activity*', (route) => route.fulfill({ json: item }))
    await page.goto(
      surface === 'full'
        ? '/w/demo/tasks/failed-triage/full'
        : surface === 'sheet'
          ? '/w/demo/tasks/failed-triage'
          : '/w/demo/tasks?task=failed-triage',
    )
    const recovery = page.getByRole('region', { name: 'Triage recovery' })
    await expect(recovery).toContainText(failedTriageReason)
    await expect(recovery).toContainText('Repair or replace the identified attachment')
    await expect(recovery.getByRole('button', { name: 'Retry triage' })).toBeDisabled()
    await expect(page.getByRole('button', { name: 'Task status: Triage failed', exact: true })).toBeVisible()
    await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
    await expect(page.getByText('Your review, please')).toHaveCount(0)
    // The gate's choices are the server's job_failure choices (DEC-61), shown
    // on the next-action card; the recovery card offers no approval of its own.
    await expect(recovery.getByRole('button', { name: 'Approve', exact: true })).toHaveCount(0)
    await expect(page.getByText('Factory review approved this')).toHaveCount(0)
    await recovery.scrollIntoViewIfNeeded()
    await testInfo.attach(`failed-triage-${surface}`, { body: await page.screenshot(), contentType: 'image/png' })
  })
}

for (const [reason, guidance] of [
  [
    'triage text/history budget exhausted: 2000000 bytes exceeds 1000000-byte limit; reduce the identified attachment or task context before retrying',
    'Reduce the identified attachment or task context',
  ],
  ['task context is invalid: missing confirmed document', 'Repair the task context'],
]) {
  test(`failed triage requires repair for ${guidance}`, async ({ page }) => {
    await page.route('**/v1/tasks/failed-triage/activity*', (route) =>
      route.fulfill({ json: failedTriageFixture(reason) }),
    )
    await page.goto('/w/demo/tasks/failed-triage/full')
    const recovery = page.getByRole('region', { name: 'Triage recovery' })
    await expect(recovery).toContainText(reason)
    await expect(recovery).toContainText(guidance)
    await expect(recovery.getByRole('button', { name: 'Retry triage' })).toBeDisabled()
  })
}

test('failed triage retry waits for response and every projection refresh and prevents duplicate clicks', async ({
  page,
}, testInfo) => {
  const item = failedTriageFixture()
  let accepted = false
  let requests = 0
  let releaseRequest = () => {}
  let releaseRefresh = () => {}
  const requestGate = new Promise<void>((resolve) => {
    releaseRequest = resolve
  })
  const refreshGate = new Promise<void>((resolve) => {
    releaseRefresh = resolve
  })
  const refreshed = new Set<string>()
  await page.route('**/v1/tasks/failed-triage/activity*', async (route) => {
    if (accepted) refreshed.add('detail')
    await route.fulfill({
      json: accepted
        ? { ...item, task: { ...item.task, state: 'queued', next_stage: 'triage' }, needs_attention: false }
        : item,
    })
  })
  await page.route('**/v1/activity*', async (route) => {
    if (accepted) {
      refreshed.add('activity')
      await refreshGate
    }
    await route.fulfill({
      json: [
        {
          task: accepted ? { ...item.task, state: 'queued', next_stage: 'triage' } : item.task,
          latest_stage: 'triage',
          needs_attention: !accepted,
          last_event_at: createdAt,
        },
      ],
    })
  })
  await page.route('**/v1/tasks/failed-triage/review*', async (route) => {
    requests++
    expect(route.request().method()).toBe('POST')
    expect(new URL(route.request().url()).searchParams.get('workspace_id')).toBe('demo')
    expect(route.request().headers()['x-conveyor-csrf']).toBe('1')
    expect(route.request().postDataJSON()).toEqual({
      action: 'redirect',
      reason_code: 'changes-requested',
      comment: expect.stringContaining('Retry triage'),
    })
    await requestGate
    accepted = true
    await route.fulfill({ json: { task: { ...item.task, state: 'queued', next_stage: 'triage' } } })
  })
  await page.goto('/w/demo/tasks?task=failed-triage')
  const recovery = page.getByRole('region', { name: 'Triage recovery' })
  const retry = recovery.getByRole('button')
  await retry.evaluate((button: HTMLButtonElement) => {
    button.click()
    button.click()
  })
  await expect.poll(() => requests).toBe(1)
  await expect(retry).toBeDisabled()
  releaseRequest()
  await expect.poll(() => refreshed.has('activity')).toBe(true)
  await expect(recovery).toBeVisible()
  await expect(retry).toHaveText('Checking recovery…')
  await expect(retry).toBeDisabled()
  await testInfo.attach('failed-triage-pending-refresh', { body: await page.screenshot(), contentType: 'image/png' })
  releaseRefresh()
  await expect(recovery).toHaveCount(0)
  await expect.poll(() => refreshed.has('detail')).toBe(true)
  await expect(page.getByRole('button', { name: 'Task status: Queued', exact: true })).toBeVisible()
  expect(requests).toBe(1)
  await page.reload()
  await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
})

for (const status of [500, 409]) {
  test(`failed triage retry preserves failure after HTTP ${status}`, async ({ page }, testInfo) => {
    const reason = status === 409 ? 'task is not at a human gate' : 'recovery service unavailable'
    await page.route('**/v1/tasks/failed-triage/activity*', (route) => route.fulfill({ json: failedTriageFixture() }))
    await page.route('**/v1/tasks/failed-triage/review*', (route) => route.fulfill({ status, body: reason }))
    await page.goto('/w/demo/tasks/failed-triage/full')
    const recovery = page.getByRole('region', { name: 'Triage recovery' })
    await recovery.getByRole('button', { name: 'Retry triage' }).click()
    await expect(recovery.getByRole('alert')).toContainText(reason)
    await expect(recovery.getByRole('button', { name: 'Retry triage' })).toBeEnabled()
    await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
    await testInfo.attach(`failed-triage-error-${status}`, { body: await page.screenshot(), contentType: 'image/png' })
  })
}

test('failed triage accepted retry with stale detail refreshes status without resubmission', async ({ page }) => {
  let requests = 0
  let detailReads = 0
  await page.route('**/v1/tasks/failed-triage/activity*', (route) => {
    detailReads++
    return route.fulfill({ json: failedTriageFixture() })
  })
  await page.route('**/v1/tasks/failed-triage/review*', (route) => {
    requests++
    return route.fulfill({ json: {} })
  })
  await page.goto('/w/demo/tasks/failed-triage/full')
  const recovery = page.getByRole('region', { name: 'Triage recovery' })
  await recovery.getByRole('button', { name: 'Retry triage' }).click()
  await expect(recovery.getByRole('alert')).toContainText('recovery is not yet confirmed')
  const previousReads = detailReads
  await recovery.getByRole('button', { name: 'Refresh status' }).click()
  await expect.poll(() => detailReads).toBeGreaterThan(previousReads)
  expect(requests).toBe(1)
})

test('failed triage capability refusal keeps the reason visible without a retry action', async ({ page }) => {
  await page.route('**/v1/me*', (route) =>
    route.fulfill({ json: callerIdentity({ id: 'reader', role: 'viewer' }, route.request()) }),
  )
  await page.route('**/v1/tasks/failed-triage/activity*', (route) => route.fulfill({ json: failedTriageFixture() }))
  await page.goto('/w/demo/tasks/failed-triage/full')
  const recovery = page.getByRole('region', { name: 'Triage recovery' })
  await expect(recovery).toContainText('upstream service unavailable')
  await expect(recovery).toContainText('An operator with permission')
  await expect(recovery.getByRole('button')).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Human gate' })).toHaveCount(0)
})

for (const gate of ['spec', 'merge']) {
  test(`failed triage history does not replace the later ${gate} gate`, async ({ page }, testInfo) => {
    const item = failedTriageFixture()
    const stage = gate === 'spec' ? 'spec' : 'review'
    item.task.next_stage = gate === 'spec' ? 'implement' : ''
    item.task.recovery_stage = 'implement'
    item.jobs.push({
      ...item.jobs[0],
      id: `later-${stage}`,
      stage,
      state: 'done',
      started_at: '2026-07-15T13:00:00Z',
      ended_at: '2026-07-15T13:01:00Z',
    })
    await page.route('**/v1/tasks/failed-triage/activity*', (route) =>
      route.fulfill({
        json: {
          ...item,
          at_merge_gate: gate === 'merge',
          spec: {
            task_id: item.task.id,
            version: 1,
            content: '## Approved work plan',
            approved: gate === 'merge',
            created_at: createdAt,
            acceptance: [],
            decomposition: [],
          },
        },
      }),
    )
    await page.goto('/w/demo/tasks/failed-triage/full')
    await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
    const humanGate = page.getByRole('region', { name: 'Human gate' })
    await expect(humanGate).toBeVisible()
    await expect(humanGate.getByRole('button', { name: 'Approve', exact: true })).toBeVisible()
    await testInfo.attach(`preserved-${gate}-gate`, { body: await page.screenshot(), contentType: 'image/png' })
  })
}

for (const change of ['missing evidence', 'new triage attempt', 'later intervention', 'later dispatch']) {
  test(`failed triage predicate refuses ${change}`, async ({ page }) => {
    const item = failedTriageFixture()
    if (change === 'missing evidence') {
      item.events = []
      item.jobs[0].state = 'running'
    }
    if (change === 'new triage attempt')
      item.jobs.push({ ...item.jobs[0], id: 'triage-2', state: 'running', started_at: '2026-07-15T13:00:00Z' })
    if (change.startsWith('later'))
      item.events.push({
        ...item.events[0],
        id: 461073,
        kind: change === 'later intervention' ? 'intervention.redirect' : 'pipeline.dispatched',
        at: '2026-07-15T13:00:00Z',
      })
    await page.route('**/v1/tasks/failed-triage/activity*', (route) => route.fulfill({ json: item }))
    await page.goto('/w/demo/tasks/failed-triage/full')
    await expect(page.getByRole('region', { name: 'Human gate' })).toBeVisible()
    await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
  })
}

test('failed triage retry refreshes the Board projection before clearing the sheet', async ({ page }) => {
  const item = failedTriageFixture()
  let accepted = false
  let boardRefreshed = false
  await page.route('**/v1/tasks/failed-triage/activity*', (route) =>
    route.fulfill({
      json: {
        ...item,
        task: accepted ? { ...item.task, state: 'queued', next_stage: 'triage' } : item.task,
      },
    }),
  )
  await page.route('**/v1/activity*', (route) => {
    if (accepted) boardRefreshed = true
    return route.fulfill({
      json: [
        {
          task: accepted ? { ...item.task, state: 'queued', next_stage: 'triage' } : item.task,
          latest_stage: 'triage',
          needs_attention: !accepted,
          last_event_at: createdAt,
        },
      ],
    })
  })
  await page.route('**/v1/tasks/failed-triage/review*', (route) => {
    accepted = true
    return route.fulfill({ json: {} })
  })
  await page.goto('/w/demo/tasks/failed-triage')
  await page.getByRole('region', { name: 'Triage recovery' }).getByRole('button', { name: 'Retry triage' }).click()
  await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
  expect(boardRefreshed).toBe(true)
  await page.getByRole('button', { name: 'Close panel' }).click()
  await expect(page.getByRole('link').filter({ hasText: 'Recover triage image preparation' })).not.toContainText(
    'Triage failed',
  )
})

test('failed triage accepted retry exposes refresh failure and only retries the read', async ({ page }) => {
  let accepted = false
  let readsFail = true
  let submissions = 0
  const item = failedTriageFixture()
  await page.route('**/v1/tasks/failed-triage/activity*', (route) => {
    if (accepted && readsFail) return route.fulfill({ status: 503, body: 'Task refresh unavailable' })
    return route.fulfill({
      json: accepted ? { ...item, task: { ...item.task, state: 'queued', next_stage: 'triage' } } : item,
    })
  })
  await page.route('**/v1/tasks/failed-triage/review*', (route) => {
    accepted = true
    submissions++
    return route.fulfill({ json: {} })
  })
  await page.goto('/w/demo/tasks/failed-triage/full')
  const recovery = page.getByRole('region', { name: 'Triage recovery' })
  await recovery.getByRole('button', { name: 'Retry triage' }).click()
  await expect(recovery.getByRole('alert')).toContainText('Task refresh unavailable')
  readsFail = false
  await recovery.getByRole('button', { name: 'Refresh status' }).click()
  await expect(recovery).toHaveCount(0)
  expect(submissions).toBe(1)
})

test('failed triage predicate tolerates absent job evidence', async ({ page }) => {
  const item = failedTriageFixture()
  await page.route('**/v1/tasks/failed-triage/activity*', (route) =>
    route.fulfill({ json: { ...item, jobs: [], events: [] } }),
  )
  await page.goto('/w/demo/tasks/failed-triage/full')
  await expect(page.getByRole('region', { name: 'Human gate' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
})

test('failed triage predicate orders fractional-second execution timestamps chronologically', async ({ page }) => {
  const item = failedTriageFixture()
  item.jobs.push({ ...item.jobs[0], id: 'new-triage', state: 'running', started_at: '2026-07-15T12:00:00.500Z' })
  await page.route('**/v1/tasks/failed-triage/activity*', (route) => route.fulfill({ json: item }))
  await page.goto('/w/demo/tasks/failed-triage/full')
  await expect(page.getByRole('region', { name: 'Human gate' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Triage recovery' })).toHaveCount(0)
})

// Stream reconnection and the notify stream's ready refresh
// (component-web-task-surfaces, Live updates) run against the controlled
// `fetch` in helpers/task-streams.ts.

// Freeze the installed clock at a moment with no stream timer pending.
async function pauseClock(page: Page) {
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
}

// The full route names its task once, in the reading column's heading; the
// route header is a breadcrumb (component-web-task-surfaces, Task detail
// composition).
function fullTaskHeader(page: Page) {
  return page.getByRole('region', { name: 'Task content' }).getByRole('heading', { level: 1 })
}

// A ready frame refreshes only entries past their stale window
// (component-web-task-surfaces, Live updates), so each ready frame below
// arrives after the detail's five-second window.
test('every task stream connection refreshes stale task detail once through its ready frame', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  let detailRequests = 0
  let activityRequests = 0
  await page.route('**/v1/tasks/stream-gap/activity*', async (route) => {
    detailRequests++
    const item = activity('stream-gap', false)
    // Detail that changed while the stream was down.
    item.task.title = detailRequests <= 2 ? 'Before the stream gap' : 'After the stream gap'
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/activity*', async (route) => {
    activityRequests++
    await route.fallback()
  })
  await page.clock.install({ time: new Date('2026-07-15T12:30:00Z') })
  await page.goto('/w/demo/tasks/stream-gap/full')
  await expect(fullTaskHeader(page)).toContainText('Before the stream gap')
  await expect.poll(() => streams.countFor('stream-gap')).toBeGreaterThan(0)

  // Opening a connection refreshes nothing until the server's ready frame,
  // which arrives after the server read the history fingerprint.
  await streams.open()
  await pauseClock(page)
  const connections = await streams.count()
  const feedRequests = activityRequests
  await page.clock.runFor(5_000)
  expect(detailRequests).toBe(1)
  await streams.send(readyFrame)
  await page.clock.runFor(249)
  expect(detailRequests).toBe(1)
  await page.clock.runFor(1)
  await expect.poll(() => detailRequests).toBe(2)
  // The full route mounts no Board page, so no activity entry is active and
  // the refresh sends no activity read.
  expect(activityRequests).toBe(feedRequests)

  // A proxy idle timeout or restart ends the response cleanly.
  await streams.close()
  await page.clock.runFor(999)
  expect(await streams.count()).toBe(connections)
  await page.clock.runFor(1)
  await expect.poll(() => streams.count()).toBe(connections + 1)
  expect((await streams.urls()).at(-1)).toContain('/v1/tasks/stream-gap/events/stream?mode=notify&workspace_id=demo')
  expect(detailRequests).toBe(2)

  // The reconnect's ready frame refreshes the now-stale detail once, covering
  // the outage.
  await streams.open()
  await page.clock.runFor(5_000)
  expect(detailRequests).toBe(2)
  await streams.send(readyFrame)
  await page.clock.runFor(250)
  await expect.poll(() => detailRequests).toBe(3)
  expect(activityRequests).toBe(feedRequests)
  await page.clock.resume()
  await expect(fullTaskHeader(page)).toContainText('After the stream gap')
  expect(detailRequests).toBe(3)
})

test('task stream retries errors, non-OK and bodyless responses with bounded backoff', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  let detailRequests = 0
  await page.route('**/v1/tasks/stream-retry/activity*', async (route) => {
    detailRequests++
    await route.fulfill({ json: activity('stream-retry', false) })
  })
  await page.clock.install({ time: new Date('2026-07-15T12:30:00Z') })
  await page.goto('/w/demo/tasks/stream-retry/full')
  await expect(fullTaskHeader(page)).toContainText('Short task')
  await expect.poll(() => streams.countFor('stream-retry')).toBeGreaterThan(0)
  await pauseClock(page)

  // Each ending is followed by exactly one new connection after its delay.
  const expectRetryAfter = async (delay: number) => {
    const before = await streams.count()
    await page.clock.runFor(delay - 1)
    expect(await streams.count()).toBe(before)
    await page.clock.runFor(1)
    await expect.poll(() => streams.count()).toBe(before + 1)
  }

  await streams.fail() // rejected fetch
  await expectRetryAfter(1_000)
  await streams.respond(503, 'stream unavailable') // non-OK
  await expectRetryAfter(2_000)
  await streams.respond(204, null) // no body
  await expectRetryAfter(4_000)
  const beforeUsable = detailRequests
  await streams.open() // usable, then a read error
  await streams.send(readyFrame)
  await page.clock.runFor(250)
  await expect.poll(() => detailRequests).toBe(beforeUsable + 1)
  await streams.error()
  // A ready frame does not reset the wait; only an activity frame does.
  await expectRetryAfter(8_000)
  await streams.open() // usable, then a clean close without an activity frame
  await streams.send(readyFrame)
  await streams.close()
  await expectRetryAfter(16_000)
  await streams.fail()
  await expectRetryAfter(30_000)
  await streams.fail()
  await expectRetryAfter(30_000) // the delay is capped

  // A frame proves the stream worked, so the next wait starts over.
  await streams.open()
  await streams.send(activityFrame)
  await streams.close()
  await expectRetryAfter(1_000)
  expect(new Set(await streams.urls())).toEqual(
    new Set(['/v1/tasks/stream-retry/events/stream?mode=notify&workspace_id=demo']),
  )
})

test('ready and activity frames inside 250 ms join one refresh', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  let detailRequests = 0
  await page.route('**/v1/tasks/stream-debounce/activity*', async (route) => {
    detailRequests++
    await route.fulfill({ json: activity('stream-debounce', false) })
  })
  await page.clock.install({ time: new Date('2026-07-15T12:30:00Z') })
  await page.goto('/w/demo/tasks/stream-debounce/full')
  await expect(fullTaskHeader(page)).toContainText('Short task')
  await expect.poll(() => streams.countFor('stream-debounce')).toBeGreaterThan(0)
  await streams.open()
  await pauseClock(page)
  expect(detailRequests).toBe(1)

  // The ready frame schedules one refresh 250 ms out; frames split across
  // chunks still parse, and frames inside the delay join that refresh.
  await streams.send(readyFrame)
  await page.clock.runFor(100)
  await streams.send('event: activity\ndata: {}\n\nevent: act')
  await streams.send('ivity\ndata: {}\n\n')
  await page.clock.runFor(149)
  expect(detailRequests).toBe(1)
  await page.clock.runFor(1)
  await expect.poll(() => detailRequests).toBe(2)
  await page.clock.runFor(5_000)
  expect(detailRequests).toBe(2)
  // A frame after the refresh schedules the next one.
  await streams.send(activityFrame)
  await page.clock.runFor(249)
  expect(detailRequests).toBe(2)
  await page.clock.runFor(1)
  await expect.poll(() => detailRequests).toBe(3)
})

test('changing tasks stops the old stream, its retry wait, and its pending refresh', async ({ page }) => {
  const streams = await controlTaskStreams(page)
  const detailRequests = new Map<string, number>()
  await page.route(/\/v1\/tasks\/stream-owner-[ab]\/activity/, async (route) => {
    const taskId = new URL(route.request().url()).pathname.split('/')[3]
    detailRequests.set(taskId, (detailRequests.get(taskId) ?? 0) + 1)
    const item = activity(taskId, false)
    item.task.title = taskId === 'stream-owner-a' ? 'First stream owner' : 'Second stream owner'
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/activity*', (route) => {
    const first = activity('stream-owner-a', false)
    const second = activity('stream-owner-b', false)
    return route.fulfill({
      json: [
        { task: first.task, latest_stage: 'implement', last_event_at: '2026-07-15T12:02:00Z', needs_attention: false },
        { task: second.task, latest_stage: 'implement', last_event_at: '2026-07-15T12:01:00Z', needs_attention: false },
      ],
    })
  })
  await page.clock.install({ time: new Date('2026-07-15T12:30:00Z') })
  // Navigation follows the Board page, so the move happens on the Board sheet
  // (component-web-task-surfaces, Board navigation).
  await page.goto('/w/demo/tasks/stream-owner-a')
  const sheetHeader = page.getByRole('dialog', { name: 'Task detail' }).locator('header').first()
  await expect(sheetHeader).toContainText('First stream owner')
  const next = sheetHeader.getByRole('link', { name: 'Next task' })
  await expect(next).toBeVisible()
  await expect.poll(() => streams.countFor('stream-owner-a')).toBeGreaterThan(0)
  await streams.open()
  await pauseClock(page)

  // Leave a debounced refresh and a retry wait pending for the first task.
  await streams.send(activityFrame)
  await streams.close()
  const firstConnections = await streams.countFor('stream-owner-a')
  const firstDetail = detailRequests.get('stream-owner-a')
  await next.click()
  await expect.poll(() => streams.countFor('stream-owner-b')).toBeGreaterThan(0)

  await page.clock.runFor(60_000)
  expect(await streams.countFor('stream-owner-a')).toBe(firstConnections)
  expect(detailRequests.get('stream-owner-a')).toBe(firstDetail)
  const urls = await streams.urls()
  const aborted = await streams.aborted()
  // The second task's live connection is the only one still owned.
  expect(urls.filter((url, index) => !aborted[index] && url.includes('stream-owner-a'))).toHaveLength(0)
  await page.clock.resume()
  await expect(sheetHeader).toContainText('Second stream owner')
})

test('a request-changes conflict refetches task detail and renders the current gate', async ({ page }) => {
  // Held stream connections never open, so only the 409 path can refresh.
  await controlTaskStreams(page)
  let conflicted = false
  let detailRequests = 0
  let activityRequests = 0
  let changeRequests = 0
  await page.route('**/v1/tasks/merge-request-changes/activity*', async (route) => {
    detailRequests++
    const item = activity('merge-request-changes', false)
    if (conflicted) {
      // Another operator already approved; the task left the merge gate.
      item.task.state = 'approved'
      item.at_merge_gate = false
      item.merge_readiness = { state: 'MERGEABLE', head_sha: 'head-1', number: 12 }
    }
    await route.fulfill({ json: item })
  })
  await page.route('**/v1/activity*', async (route) => {
    activityRequests++
    await route.fallback()
  })
  await page.route('**/v1/tasks/merge-request-changes/request-changes*', async (route) => {
    changeRequests++
    conflicted = true
    await route.fulfill({ status: 409, json: { detail: 'task is not at the merge gate' } })
  })

  // The Board sheet keeps an active Board page, so the activity family has an
  // entry to refetch (component-web-task-surfaces, Board navigation).
  await page.goto('/w/demo/tasks/merge-request-changes')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await gate.getByRole('button', { name: 'Request changes' }).click()
  await gate.getByLabel('Changes you want').fill('Keep this exact feedback.')
  await expect.poll(() => detailRequests).toBe(1)
  await expect.poll(() => activityRequests).toBeGreaterThan(0)
  const feedRequests = activityRequests
  await gate.getByRole('button', { name: 'Request changes' }).last().click()

  await expect.poll(() => detailRequests).toBe(2)
  await expect.poll(() => activityRequests).toBeGreaterThan(feedRequests)
  // The panel renders the server's current gate while the refusal stays visible.
  await expect(gate.getByRole('button', { name: 'Merge', exact: true })).toBeEnabled()
  await expect(gate.getByRole('region', { name: 'What you are approving' })).toHaveCount(0)
  await expect(gate.getByText('task is not at the merge gate')).toBeVisible()
  // No retry, no success effects: the operator's feedback is kept.
  await expect(gate.getByLabel('Changes you want')).toHaveValue('Keep this exact feedback.')
  expect(changeRequests).toBe(1)
})

test('non-conflict gate failures keep their existing behavior without a refetch', async ({ page }) => {
  await controlTaskStreams(page)
  let detailRequests = 0
  await page.route('**/v1/tasks/merge-failure/activity*', async (route) => {
    detailRequests++
    await route.fulfill({ json: activity('merge-failure', false) })
  })
  await page.route('**/v1/tasks/merge-failure/merge*', (route) =>
    route.fulfill({ status: 502, body: 'GitHub merge failed' }),
  )

  await page.goto('/w/demo/tasks/merge-failure/full')
  const gate = page.getByRole('region', { name: 'Human gate' })
  await expect.poll(() => detailRequests).toBe(1)
  await gate.getByRole('button', { name: 'Merge', exact: true }).click()
  await expect(gate.getByText('GitHub merge failed')).toBeVisible()
  await expect(gate.getByRole('button', { name: 'Merge', exact: true })).toBeEnabled()
  expect(detailRequests).toBe(1)
})

// Recovery mutations refresh task detail under its workspace-scoped key
// (conveyor:web/src/lib/query-keys.ts#taskDetailQueryKey). The stream answers
// 204 and no fixture lists blockers, so neither SSE nor dependency polling can
// refetch detail: a second detail read after the mutation is the mutation's
// own refresh, and the refreshed title proves the panel rendered it.
interface RecoveryRefreshCase {
  name: string
  taskId: string
  mutation: string
  respond: unknown
  prepare?: (page: Page, item: ReturnType<typeof activity>) => Promise<void>
  act: (page: Page) => Promise<void>
}

const recoveryRefreshCases: RecoveryRefreshCase[] = [
  {
    name: 'checkpoint proposal recovery',
    taskId: 'operator-checkpoint',
    mutation: '**/v1/work-orders/attempt-recovery-implement-1/recover*',
    respond: { id: 'operator-checkpoint-implement-1', state: 'queued', claimable: true },
    prepare: async (page, item) => {
      item.task.context = { designs: [{ id: 'design-lifecycle', title: 'Work-order lifecycle', version: 1 }] }
      await page.route('**/v1/system-designs**', (route) =>
        route.fulfill({ json: designCollection('operator-checkpoint', false) }),
      )
      await page.route('**/v1/system-designs/design-lifecycle/versions/2/confirm**', (route) =>
        route.fulfill({ json: {} }),
      )
    },
    act: (page) =>
      page
        .getByRole('region', { name: 'Confirm proposals and recover work order' })
        .getByRole('button', { name: 'Confirm v2 and recover' })
        .click(),
  },
  {
    name: 'work-order recovery',
    taskId: 'recovery',
    mutation: '**/v1/work-orders/*/recover*',
    respond: { id: 'recovery-review-1-seat-1', state: 'queued', claimable: true },
    act: (page) => page.getByRole('button', { name: 'Recover work order' }).click(),
  },
  {
    name: 'interrupted review recovery',
    taskId: 'interrupted-review',
    mutation: '**/v1/tasks/*/review-round/recover*',
    respond: {
      request_id: 'recover-1',
      task_id: 'interrupted-review',
      review_round: 1,
      recovered_orders: [{ id: 'seat-2' }],
      retained_orders: [{ id: 'seat-1' }],
    },
    act: (page) => page.getByRole('button', { name: 'Recover interrupted review round' }).click(),
  },
  {
    name: 'review round retry',
    taskId: 'review-retry',
    mutation: '**/v1/tasks/*/review-round/retry*',
    respond: {
      request_id: 'retry-1',
      task_id: 'review-retry',
      prior_round: 1,
      new_round: 2,
      pr_head: 'abc',
      work_orders: [{ id: 'seat-1' }, { id: 'seat-2' }],
    },
    act: async (page) => {
      await page.getByLabel('Review retry reason').fill('Retry with the corrected harness configuration')
      await page.getByRole('button', { name: 'Retry review round' }).click()
    },
  },
  {
    name: 'redispatch',
    taskId: 'redispatch-refresh',
    mutation: '**/v1/tasks/redispatch-refresh/redispatch*',
    respond: {},
    prepare: async (_page, item) => {
      item.task.state = 'queued'
      item.work_orders = []
    },
    act: (page) => page.getByRole('button', { name: 'Redispatch', exact: true }).click(),
  },
]

for (const recovery of recoveryRefreshCases) {
  test(`recovery refreshes task detail after success (${recovery.name})`, async ({ page }) => {
    const item = activity(recovery.taskId, false)
    await recovery.prepare?.(page, item)
    const calls: string[] = []
    let mutated = false
    await page.route('**/v1/tasks/*/events/stream*', (route) => route.fulfill({ status: 204 }))
    await page.route(`**/v1/tasks/${recovery.taskId}/activity*`, (route) => {
      calls.push(`detail:${new URL(route.request().url()).searchParams.get('workspace_id')}`)
      return route.fulfill({
        json: mutated ? { ...item, task: { ...item.task, title: 'Refreshed after recovery' } } : item,
      })
    })
    await page.route(recovery.mutation, (route) => {
      if (route.request().method() !== 'POST') return route.fallback()
      calls.push('mutation')
      mutated = true
      return route.fulfill({ json: recovery.respond })
    })

    await page.goto(`/w/demo/tasks/${recovery.taskId}/full`)
    await expect.poll(() => calls).toEqual(['detail:demo'])
    await recovery.act(page)
    await expect(page.getByText('Refreshed after recovery').first()).toBeVisible()
    expect(calls.slice(0, 3)).toEqual(['detail:demo', 'mutation', 'detail:demo'])
  })
}

// A recovery that completes after the operator switched workspace refreshes
// only the workspace it was issued in. The held response is a deterministic
// barrier; the activity refetch its onSuccess triggers marks completion.
test('recovery completion after workspace switch does not refresh the new workspace task', async ({ page }) => {
  const item = activity('recovery', false)
  const detailReads: string[] = []
  const activityReads: string[] = []
  let releaseRecovery = () => {}
  const recoveryHeld = new Promise<void>((resolve) => {
    releaseRecovery = resolve
  })
  let recoveryReceived = false
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({
      json: [
        { id: 'demo', name: 'Demo' },
        { id: 'other', name: 'Other' },
      ],
    }),
  )
  await page.route('**/v1/tasks/*/events/stream*', (route) => route.fulfill({ status: 204 }))
  await page.route('**/v1/tasks/recovery/activity*', (route) => {
    const workspace = new URL(route.request().url()).searchParams.get('workspace_id') ?? ''
    detailReads.push(workspace)
    return route.fulfill({
      json: { ...item, task: { ...item.task, workspace, title: `Recovery in ${workspace}` } },
    })
  })
  await page.route('**/v1/activity*', (route) => {
    const workspace = new URL(route.request().url()).searchParams.get('workspace_id') ?? ''
    activityReads.push(workspace)
    return route.fulfill({
      json: [
        {
          task: { ...item.task, workspace, title: `Recovery in ${workspace}` },
          latest_stage: 'review',
          last_event_at: createdAt,
          needs_attention: false,
        },
      ],
    })
  })
  await page.route('**/v1/work-orders/*/recover*', async (route) => {
    recoveryReceived = true
    await recoveryHeld
    await route.fulfill({ json: { id: 'recovery-review-1-seat-1', state: 'queued', claimable: true } })
  })

  // Detail reads that delivered a response. A workspace change aborts the
  // read in flight (component-web-task-surfaces), so an aborted request never
  // reaches the page and is not a refresh.
  const completedReads: string[] = []
  page.on('requestfinished', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/v1/tasks/recovery/activity') completedReads.push(url.searchParams.get('workspace_id') ?? '')
  })
  await page.goto('/w/demo/tasks/recovery/full')
  await page.getByRole('button', { name: 'Recover work order' }).click()
  await expect.poll(() => recoveryReceived).toBe(true)

  // Switching lands on the Board; opening the same task ID there reads it in
  // the newly selected workspace.
  await page.getByRole('navigation', { name: 'Workspaces' }).getByRole('button', { name: 'Switch to Other' }).click()
  await page.getByText('Recovery in other').first().click()
  await expect(page.getByRole('dialog', { name: 'Task detail' })).toBeVisible()
  await expect.poll(() => completedReads.filter((workspace) => workspace === 'other').length).toBe(1)
  const otherReads = detailReads.filter((workspace) => workspace === 'other').length
  const demoReads = detailReads.filter((workspace) => workspace === 'demo').length
  const otherActivityReads = activityReads.filter((workspace) => workspace === 'other').length

  releaseRecovery()
  // onSuccess invalidates task detail before activity, so once the activity
  // refetch arrives the detail invalidation has already run.
  await expect
    .poll(() => activityReads.filter((workspace) => workspace === 'other').length)
    .toBeGreaterThan(otherActivityReads)
  expect(detailReads.filter((workspace) => workspace === 'other')).toHaveLength(otherReads)
  expect(completedReads.filter((workspace) => workspace === 'other')).toHaveLength(1)
  expect(detailReads.filter((workspace) => workspace === 'demo')).toHaveLength(demoReads)
})

// Work-order timeline entries name their stage. A verify order reads
// Verification in every state that becomes its own entry, never Review.
test('verify order timeline labels follow stage', async ({ page }) => {
  const item = activity('stage-labels', false)
  const base = (item.work_orders ?? [])[0] ?? {}
  const order = (id: string, stage: string, state: string, extra: Record<string, unknown> = {}) => ({
    ...base,
    id,
    task_id: 'stage-labels',
    job_id: id,
    stage,
    state,
    claimable: state === 'queued',
    retry_suppressed: false,
    queue_entered_at: '2026-07-15T12:00:00Z',
    queue_deadline: '2026-07-16T12:00:00Z',
    progress: undefined,
    ...extra,
  })
  item.jobs = []
  item.work_orders = [
    order('stage-labels-verify-1', 'verify', 'queued'),
    order('stage-labels-verify-2', 'verify', 'claimed', { agent: 'claude-code', model: 'claude-opus-5-5' }),
    order('stage-labels-verify-3', 'verify', 'stale'),
    order('stage-labels-verify-4', 'verify', 'timed_out', { execution_deadline: '2026-07-15T16:00:00Z' }),
    order('stage-labels-spec-1', 'spec', 'stale'),
    order('stage-labels-implement-1', 'implement', 'timed_out', { execution_deadline: '2026-07-15T16:00:00Z' }),
    order('stage-labels-review-1-seat-1', 'review', 'queued'),
  ]
  await page.route('**/v1/tasks/stage-labels/activity*', (route) => route.fulfill({ json: item }))
  await page.goto('/w/demo/tasks/stage-labels/full')

  for (const title of [
    'Verification — waiting for an operator agent',
    'Verification — in progress',
    'Verification — went stale in the queue',
    'Verification — timed out',
    'Plan — went stale in the queue',
    'Implementation — timed out',
    'Review — waiting for an operator agent',
  ]) {
    await expect(page.getByText(title, { exact: true }).first()).toBeVisible()
  }
  // Only the one review order reads Review.
  await expect(page.getByText(/^Review — /)).toHaveCount(1)
})

// The full route arranges the shared sections into a reading column and an
// inspector from lg up, and one column in DOM order below lg; the sheet keeps
// its stacked composition (component-web-task-surfaces, Task detail
// composition).
const inspectorTaskID = 'work-on-this-inspector'

async function routeInspectorTask(page: Page) {
  const flowReads: string[] = []
  page.on('request', (request) => {
    if (new URL(request.url()).pathname.endsWith('/flow')) flowReads.push(request.url())
  })
  await page.route(`**/v1/tasks/${inspectorTaskID}/activity*`, (route) => {
    const item = activity(inspectorTaskID, false)
    item.task.dependencies = [{ id: 'inspector-dependency', title: 'Backend contract', state: 'merged' }]
    item.task.context = {
      requirements: [{ id: 'req-inspector', title: 'Inspector outcome', version: 3 }],
      designs: [{ id: 'design-inspector', title: 'Inspector guidance', version: 2 }],
    }
    item.attachments = Array.from({ length: 3 }, (_, index) => ({
      id: `inspector-attachment-${index + 1}`,
      workspace: 'demo',
      name: `inspector-${index + 1}.txt`,
      content_type: 'text/plain',
      size_bytes: 1024,
      role: 'task_context',
      task_id: inspectorTaskID,
      created_at: createdAt,
    }))
    return route.fulfill({ json: item })
  })
  return flowReads
}

test('the full route places the reading column and the inspector side by side from lg up', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const flowReads = await routeInspectorTask(page)
  await page.goto(`/w/demo/tasks/${inspectorTaskID}/full`)
  const content = page.getByRole('region', { name: 'Task content' })
  const title = content.getByRole('heading', { level: 1 })
  await expect(title).toBeVisible()

  // The breadcrumb names the task ID; the title reads once, in the page.
  const header = page.locator('header').filter({ has: page.getByRole('link', { name: 'Back to board' }) })
  await expect(header.getByRole('navigation', { name: 'Breadcrumb' })).toHaveText(`Board/${inspectorTaskID}`)
  await expect(content.getByRole('heading', { level: 1 })).toHaveCount(1)

  const inspector = content.getByRole('region', { name: 'Task inspector' })
  const properties = content.getByRole('region', { name: 'Properties' })
  await expect(properties.locator('dt').filter({ hasText: /^Depends on$/ })).toHaveCount(1)
  await expect(inspector.getByRole('region', { name: 'Attached context' })).toBeVisible()
  await expect(inspector.getByRole('region', { name: 'Agent prompt' })).toBeVisible()
  await expect(inspector.getByRole('heading', { name: 'Attachments 3' })).toBeVisible()

  const box = async (locator: Locator) => (await locator.boundingBox())!
  const reading = [
    title,
    content.getByRole('region', { name: 'Execution plan' }),
    content.getByRole('region', { name: 'Next action' }),
    content.getByRole('tablist', { name: 'Task activity view' }),
  ]
  const side = [properties, inspector]
  const readingRight = Math.max(...(await Promise.all(reading.map(box))).map((b) => b.x + b.width))
  const sideLeft = Math.min(...(await Promise.all(side.map(box))).map((b) => b.x))
  expect(readingRight).toBeLessThan(sideLeft)
  // The reading column keeps its measure; the inspector is 20rem wide.
  expect((await box(content.getByRole('region', { name: 'Execution plan' }))).width).toBeLessThanOrEqual(760 + 1)
  expect(Math.round((await box(inspector)).width)).toBe(320)
  // The inspector's context rows drop their kind label.
  await expect(inspector.getByText('Outcome', { exact: true })).toHaveCount(0)
  // Timeline stays the default and reads no flow.
  await expect(content.getByRole('tab', { name: 'Timeline' })).toHaveAttribute('aria-selected', 'true')
  expect(flowReads).toEqual([])
})

test('below lg the full route is one column in reading order without horizontal scroll', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await routeInspectorTask(page)
  await page.goto(`/w/demo/tasks/${inspectorTaskID}/full`)
  const content = page.getByRole('region', { name: 'Task content' })
  await expect(content.getByRole('heading', { level: 1 })).toBeVisible()
  const order = [
    content.getByRole('heading', { level: 1 }),
    content.getByRole('region', { name: 'Properties' }),
    content.getByRole('region', { name: 'Execution plan' }),
    content.getByRole('region', { name: 'Attached context' }),
    content.getByRole('region', { name: 'Agent prompt' }),
    content.getByRole('heading', { name: 'Attachments 3' }),
    content.getByRole('region', { name: 'Next action' }),
    content.getByRole('tablist', { name: 'Task activity view' }),
  ]
  const tops: number[] = []
  for (const locator of order) tops.push((await locator.boundingBox())!.y)
  expect(tops).toEqual([...tops].sort((a, b) => a - b))
  // DOM order matches the visual order, so keyboard order follows it too.
  const domOrder = await content.evaluate((root) => {
    const marks = [
      root.querySelector('h1'),
      root.querySelector('[aria-label="Properties"]'),
      root.querySelector('[aria-label="Execution plan"]'),
      root.querySelector('[aria-label="Attached context"]'),
      root.querySelector('[aria-label="Agent prompt"]'),
      root.querySelector('[aria-label="Next action"]'),
    ]
    return marks.every(
      (mark, index) =>
        index === 0 || Boolean(marks[index - 1]!.compareDocumentPosition(mark!) & Node.DOCUMENT_POSITION_FOLLOWING),
    )
  })
  expect(domOrder).toBe(true)
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await expect.poll(() => content.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true)
})

test('the board sheet keeps its stacked composition without an inspector', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await routeInspectorTask(page)
  await page.goto(`/w/demo/tasks/${inspectorTaskID}`)
  const sheet = page.getByRole('dialog', { name: 'Task detail' })
  await expect(sheet.getByRole('heading', { level: 2 }).first()).toBeVisible()
  await expect(sheet.getByRole('region', { name: 'Task inspector' })).toHaveCount(0)
  // The sheet stacks: header facts, agent prompt, context, then the activity.
  const tops: number[] = []
  for (const locator of [
    sheet.locator('dt').filter({ hasText: /^Repository$/ }),
    sheet.getByRole('region', { name: 'Agent prompt' }),
    sheet.getByRole('region', { name: 'Attached context' }),
    sheet.getByRole('region', { name: 'Next action' }),
  ])
    tops.push((await locator.boundingBox())!.y)
  expect(tops).toEqual([...tops].sort((a, b) => a - b))
  // The sheet chrome keeps the task title.
  await expect(sheet.locator('header').first()).toContainText('Short task')
})

// The two task shapes 261016-6fa423 names, on the full route: a merged task
// with an approved plan, seven context documents, nine attachments, a review
// bounce, and long branch, dependency, and context values; and a queued task
// with three pending task-context proposals.
const at = (minutes: number) => new Date(Date.parse(createdAt) + minutes * 60_000).toISOString()
const longBranch = 'conveyor/task-merged-shape-with-a-deliberately-long-branch-name-that-must-truncate'

function reviewJob(taskId: string, id: string, start: number, end: number) {
  return {
    id,
    task_id: taskId,
    stage: 'review' as const,
    harness: 'codex',
    model_tier: 'gpt-6.1-sol',
    runner: 'worker' as const,
    confinement: 'none' as const,
    cost_usd: 0,
    tokens_in: 0,
    tokens_out: 0,
    state: 'done' as const,
    started_at: at(start),
    ended_at: at(end),
  }
}

function reviewOrder(taskId: string, id: string, round: number) {
  return {
    id,
    task_id: taskId,
    job_id: id,
    stage: 'review' as const,
    state: 'completed' as const,
    claimable: false,
    review_round: round,
    review_seat: 1,
    model: 'gpt-6.1-sol',
    queue_entered_at: at(0),
    queue_deadline: at(1440),
    redispatch_count: 0,
    tokens_in: 0,
    tokens_out: 0,
    usage_reported: false,
    self_reported: true,
  }
}

function mergedShape(taskId: string) {
  const item = activity(taskId, true)
  const review = (id: number, order: string, round: number, verdict: string, minute: number) => ({
    id,
    task_id: taskId,
    job_id: order,
    kind: 'review.completed',
    actor_id: 'worker',
    actor_role: 'runner' as const,
    payload: {
      review_work_order_id: order,
      review_round: round,
      review_seat: 1,
      verdict,
      summary: verdict === 'approve' ? 'Approved' : 'Changes requested',
      feedback: verdict === 'approve' ? 'Round-one gaps are closed.' : 'Add the masked-gate conformance cases.',
    },
    at: at(minute),
  })
  return {
    ...item,
    task: {
      ...item.task,
      state: 'merged' as const,
      branch: longBranch,
      dependencies: [
        {
          id: 'merged-dependency',
          title: `Remove the current attempt panel from the task timeline and its tests ${'and more '.repeat(6)}`,
          state: 'merged' as const,
        },
      ],
      blocking_task_ids: undefined,
      context: {
        requirements: [{ id: 'req-operator-next-action', title: 'Operator next action and inbox', version: 7 }],
        designs: [
          ['component-http-api', 'HTTP API: router, credential middleware, and task surfaces', 47],
          ['component-verification-strategy', 'Testing strategy', 31],
          ['component-web-dashboard', 'Web dashboard: shell, contract layer, and settings', 31],
          ['component-web-task-surfaces', 'Web task surfaces: board, tasks, and task detail', 13],
          ['feature-bounded-automatic-recovery', 'Bounded automatic recovery', 3],
          [
            'feature-operator-next-action',
            `Operator next action, inbox, and task flow ${'with a deliberately long title '.repeat(3)}`,
            14,
          ],
        ].map(([id, title, version]) => ({ id: String(id), title: String(title), version: Number(version) })),
      },
    },
    spec: {
      ...item.spec,
      approved: true,
      content: ['## Approach', '', ...Array.from({ length: 30 }, (_, index) => `Plan paragraph ${index + 1}.`)].join(
        '\n\n',
      ),
    },
    attachments: Array.from({ length: 9 }, (_, index) => ({
      id: `merged-attachment-${index + 1}`,
      workspace: 'demo',
      name: `${index < 3 ? 'mockup' : 'prototype'}-${index + 1}.txt`,
      content_type: 'text/plain',
      size_bytes: 1024 + index,
      role: 'task_context' as const,
      task_id: taskId,
      created_at: createdAt,
    })),
    jobs: [reviewJob(taskId, `${taskId}-review-1`, 30, 35), reviewJob(taskId, `${taskId}-review-2`, 60, 64)],
    work_orders: [reviewOrder(taskId, `${taskId}-review-1`, 1), reviewOrder(taskId, `${taskId}-review-2`, 2)],
    events: [
      review(101, `${taskId}-review-1`, 1, 'changes_requested', 35),
      {
        id: 102,
        task_id: taskId,
        kind: 'review.round_completed',
        actor_id: 'system',
        actor_role: 'system' as const,
        payload: {
          review_round: 1,
          verdict: 'changes_requested',
          reviews: [{ review_work_order_id: `${taskId}-review-1` }],
        },
        at: at(36),
      },
      {
        id: 103,
        task_id: taskId,
        kind: 'pipeline.bounced',
        actor_id: 'system',
        actor_role: 'system' as const,
        payload: { from: 'review', to: 'implement', count: 1, review_round: 1, source: 'review' },
        at: at(37),
      },
      review(104, `${taskId}-review-2`, 2, 'approve', 64),
      {
        id: 105,
        task_id: taskId,
        kind: 'review.round_completed',
        actor_id: 'system',
        actor_role: 'system' as const,
        payload: { review_round: 2, verdict: 'approve', reviews: [{ review_work_order_id: `${taskId}-review-2` }] },
        at: at(65),
      },
    ],
  }
}

const queuedTargets = [
  ['requirement', 'req-task-centric-operations-view', 'Task-centric operations view'],
  ['system_design', 'component-verification-strategy', 'Testing strategy'],
  ['system_design', 'component-web-task-surfaces', 'Web task surfaces: board, tasks, and task detail'],
] as const

function queuedShape(taskId: string) {
  const item = activity(taskId, false)
  return {
    ...item,
    task: {
      ...item.task,
      state: 'queued' as const,
      context: {
        requirements: [],
        designs: [],
        proposals: queuedTargets.map(([kind, id, title]) => ({
          task_id: taskId,
          target_kind: kind,
          target_id: id,
          target_title: title,
          state: 'proposed' as const,
          source: 'triage' as const,
          justification: `Triage suggests ${title} for this change.`,
          created_by_event_id: 1,
          proposed_by: 'queue',
          workspace: 'demo',
          created_at: createdAt,
          updated_at: createdAt,
        })),
      },
    },
    spec: undefined,
  }
}

async function expectInsideColumns(page: Page) {
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  const content = page.getByRole('region', { name: 'Task content' })
  await expect.poll(() => content.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true)
}

test('the merged shape fills the full-route inspector without widening it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const taskId = 'work-on-this-merged-shape'
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => route.fulfill({ json: mergedShape(taskId) }))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const content = page.getByRole('region', { name: 'Task content' })
  const inspector = content.getByRole('region', { name: 'Task inspector' })
  await expect(content.getByRole('heading', { level: 1 })).toBeVisible()

  // Seven compact context rows in the inspector, the long title truncated.
  const contextRows = inspector.getByRole('region', { name: 'Attached context' }).getByRole('listitem')
  await expect(contextRows).toHaveCount(7)
  await expect(inspector.getByText('Guidance', { exact: true })).toHaveCount(0)
  for (const row of await contextRows.all()) {
    const fits = await row.evaluate((node) => node.scrollWidth <= node.clientWidth + 1)
    expect(fits).toBe(true)
  }

  // Nine attachments open on one row; "+N more" reveals every tile in place.
  await expect(inspector.getByRole('heading', { name: 'Attachments 9' })).toBeVisible()
  const more = inspector.getByRole('button', { name: /^\+\d+ more$/ })
  await expect(more).toBeVisible()
  await more.click()
  await expect(inspector.getByRole('button', { name: /^Expand (mockup|prototype)-\d\.txt$/ })).toHaveCount(9)

  // Long branch and dependency values truncate inside Properties.
  const properties = content.getByRole('region', { name: 'Properties' })
  await expect(properties.getByText(longBranch)).toBeVisible()
  await expect(properties.getByRole('link', { name: /Remove the current attempt panel.*· Satisfied$/ })).toBeVisible()
  expect(await properties.evaluate((node) => node.scrollWidth <= node.clientWidth + 1)).toBe(true)
  expect(Math.round((await inspector.boundingBox())!.width)).toBe(320)

  // The review bounce reads in the reading column's timeline.
  await expect(content.locator('[data-review-verdict]')).toHaveText(['Changes requested', 'Approved'])
  await expect(content.getByRole('button', { name: 'Show full plan' })).toBeVisible()
  await expectInsideColumns(page)
})

for (const width of [1440, 390]) {
  test(`the queued shape decides task-context proposals from the full route at ${width} px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const taskId = 'work-on-this-queued-shape'
    let posted: { path: string; body: string | null } | undefined
    await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => route.fulfill({ json: queuedShape(taskId) }))
    await page.route(`**/v1/tasks/${taskId}/context/proposals/**`, (route) => {
      posted = { path: new URL(route.request().url()).pathname, body: route.request().postData() }
      return route.fulfill({ json: {} })
    })
    await page.goto(`/w/demo/tasks/${taskId}/full`)
    const content = page.getByRole('region', { name: 'Task content' })
    const card = content.getByRole('region', { name: 'Next action' })
    await expect(card.getByText('A context proposal is waiting for a decision.')).toBeVisible()

    // The compact suggestions inform; the next-action card decides.
    const suggestions = content.getByRole('region', { name: 'Suggested context' })
    await expect(suggestions.getByRole('listitem')).toHaveCount(3)
    await expect(suggestions.getByRole('button')).toHaveCount(0)
    const choices = card.locator('[data-next-action-choices]').getByRole('button')
    await expect(choices).toHaveCount(6)
    const cardBox = (await card.boundingBox())!
    for (const button of await choices.all()) {
      await expect(button).toBeEnabled()
      const box = (await button.boundingBox())!
      expect(box.x + box.width).toBeLessThanOrEqual(cardBox.x + cardBox.width + 0.5)
    }
    if (width === 1440) {
      // Suggestions sit in the inspector, the decision in the reading column.
      await expect(
        content.getByRole('region', { name: 'Task inspector' }).getByRole('region', { name: 'Suggested context' }),
      ).toBeVisible()
      expect(cardBox.x + cardBox.width).toBeLessThan((await suggestions.boundingBox())!.x)
    }
    await choices.first().click()
    await expect
      .poll(() => posted?.path)
      .toBe(`/v1/tasks/${taskId}/context/proposals/requirement/req-task-centric-operations-view/confirm`)
    expect(posted?.body).toBeNull()
    await expectInsideColumns(page)
  })
}

test('with a gate open the full route keeps verification evidence in the gate card, not the inspector', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/w/demo/tasks/evidence/full')
  const content = page.getByRole('region', { name: 'Task content' })
  const gate = content.getByRole('region', { name: 'Human gate' })
  await expect(gate).toBeVisible()
  await expect(gate.getByText('proof screenshot.png')).toBeVisible()
  const inspector = content.getByRole('region', { name: 'Task inspector' })
  await expect(inspector.getByRole('heading', { name: /^Verification evidence/ })).toHaveCount(0)
  // The gate card renders in the reading column, left of the inspector.
  expect((await gate.boundingBox())!.x + (await gate.boundingBox())!.width).toBeLessThan(
    (await inspector.boundingBox())!.x,
  )
  await expectInsideColumns(page)
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

test('review seat metadata stays with its historical round', async ({ page }) => {
  const taskId = 'seat-history'
  const item = seatPanelItem(taskId, {
    orders: [
      { round: 0, seat: 1, model: 'legacy-model', verdict: 'approve', id: 'seat-history-legacy-1' },
      ...seatHistory.orders,
      { round: 3, seat: 1, model: 'r3-model' },
      { round: 3, seat: 2, model: 'r3-model' },
    ],
    summaries: {
      review_round: seatMeta(3, 2, 'carried'),
      review_round_seats: {
        by_round: {
          '1': seatMeta(1, 2, 'minimum'),
          '2': seatMeta(2, 3, 'declared'),
          // A carried record without a known predecessor.
          '3': seatMeta(3, 2, 'carried'),
        },
        by_order: { 'seat-history-legacy-1': seatMeta(0, 1, 'minimum') },
      },
      review_seat_policy: { minimum_seat_count: 2 },
      review_rounds: {
        by_round: {
          ...seatHistory.rounds,
          '2': {
            event_id: '52',
            verdict: 'changes_requested',
            summary: 'Round 2 settled.',
            at: '2026-07-15T14:30:00Z',
          },
        },
        by_order: {},
      },
    },
  })
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => route.fulfill({ json: item }))
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(
    roundPanel(page, 'r1-model').getByTitle('Seat count is the workspace minimum', { exact: true }),
  ).toHaveText('Panel of 2 · minimum 2')
  await expect(
    roundPanel(page, 'r2-model').getByTitle('Seat count declared at submission', { exact: true }),
  ).toHaveText('Panel of 3 · minimum 2')
  await expect(
    roundPanel(page, 'r3-model').getByTitle('Seat count carried from the previous round', { exact: true }),
  ).toHaveText('Panel of 2 · minimum 2')
  // A round-zero panel is a single-seat panel with its own order's metadata.
  await expect(
    roundPanel(page, 'legacy-model').getByTitle('Seat count is the workspace minimum', { exact: true }),
  ).toHaveText('Panel of 1 · minimum 2')
  // The latest round's carried source is never copied onto older panels.
  await expect(roundPanel(page, 'r1-model').getByTitle(/carried/)).toHaveCount(0)
  await expect(roundPanel(page, 'r2-model').getByTitle(/carried/)).toHaveCount(0)
  // Existing seat rows, verdict pills, and counts are unchanged.
  await expect(roundPanel(page, 'r2-model')).toContainText('Changes requested')
  await expect(roundPanel(page, 'r1-model')).toContainText('Changes requested — 2 of 2 seats')

  // An older server without metadata prints the seat count alone.
  const legacyServer = seatPanelItem('seat-no-metadata', { orders: seatHistory.orders.slice(0, 2), summaries: {} })
  await page.route('**/v1/tasks/seat-no-metadata/activity*', (route) => route.fulfill({ json: legacyServer }))
  await page.goto('/w/demo/tasks/seat-no-metadata/full')
  const panel = roundPanel(page, 'r1-model')
  await expect(panel).toContainText('Panel of 2')
  await expect(panel).not.toContainText('minimum')
})

test('planned review seats appear on the displayed plan version', async ({ page }) => {
  const taskId = 'seat-plan'
  let reviewSeats: number | null = 4
  // The approved plan's count (3) differs from the displayed draft's (4).
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) =>
    route.fulfill({
      json: seatPanelItem(taskId, {
        orders: [],
        state: 'awaiting_human',
        spec: { review_seats: reviewSeats, approved: false },
        summaries: {
          review_round_seats: { by_round: {}, by_order: {} },
          review_seat_policy: { minimum_seat_count: 2, planned_seat_count: 3 },
        },
      }),
    }),
  )
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const plan = page.getByRole('region', { name: 'Execution plan' })
  await expect(plan.getByText('Review: 4 seats (minimum 2)', { exact: true })).toBeVisible()
  await expect(plan.getByText(/Review: 3 seats/)).toHaveCount(0)

  // The Board sheet shows the same line.
  await page.goto(`/w/demo/tasks/${taskId}`)
  await expect(page.getByText('Review: 4 seats (minimum 2)', { exact: true })).toBeVisible()

  // A version without a count shows no line.
  reviewSeats = null
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  await expect(page.getByRole('region', { name: 'Execution plan' }).getByText('Execution plan')).toBeVisible()
  await expect(page.getByText(/^Review: \d+ seat/)).toHaveCount(0)
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

test('review seat addition validates input and refreshes the panel', async ({ page }) => {
  const taskId = 'seat-add'
  let seats = 3
  let detailReads = 0
  const posts: { url: string; body: Record<string, unknown> }[] = []
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => {
    detailReads++
    return route.fulfill({ json: openRoundItem(taskId, { seatsInRound2: seats }) })
  })
  await page.route(`**/v1/tasks/${taskId}/review-round/seats*`, (route) => {
    const body = route.request().postDataJSON() as Record<string, unknown>
    posts.push({ url: route.request().url(), body })
    seats = 5
    return route.fulfill({
      json: { request_id: body.request_id, task_id: taskId, review_round: 2, prior_seat_count: 3, seat_count: 5 },
    })
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const panel = roundPanel(page, 'r2-model')
  await panel.getByRole('button', { name: 'Add reviewer seat' }).click()
  const form = panel.getByRole('form', { name: 'Add reviewer seats to round 2' })
  const count = form.getByLabel('Seats to add')
  const reason = form.getByLabel('Reason')
  await expect(count).toHaveValue('1')

  await count.fill('0')
  await reason.fill('Security-sensitive change')
  await form.getByRole('button', { name: 'Add seats' }).click()
  await expect(panel.getByRole('alert')).toHaveText('Enter a whole number of seats from 1 to 2147483647.')
  await count.fill('2')
  await reason.fill('   ')
  await form.getByRole('button', { name: 'Add seats' }).click()
  await expect(panel.getByRole('alert')).toHaveText('Give a reason for adding seats.')
  expect(posts).toHaveLength(0)

  const readsBefore = detailReads
  await reason.fill('  Security-sensitive change  ')
  await form.getByRole('button', { name: 'Add seats' }).click()
  await expect(panel.getByRole('status')).toHaveText('Round 2 now has 5 seats.')
  expect(posts).toHaveLength(1)
  expect(new URL(posts[0].url).searchParams.get('workspace_id')).toBe('demo')
  expect(Object.keys(posts[0].body).sort()).toEqual(['add', 'reason', 'request_id'])
  expect(posts[0].body).toMatchObject({ add: 2, reason: 'Security-sensitive change' })
  expect(posts[0].body.request_id).toMatch(/^[0-9a-f-]{36}$/)
  // Success refreshes the task detail, so the panel shows the added seats.
  await expect(panel.getByTitle(/Seat count carried from round 1/)).toHaveText('Panel of 5 · minimum 2')
  expect(detailReads).toBeGreaterThan(readsBefore)
  await expect(form).toHaveCount(0)
})

test('review seat addition keeps refusals and retry identity in place', async ({ page }) => {
  const taskId = 'seat-refusal'
  let settled = false
  let detailReads = 0
  const requestIds: string[] = []
  const responses: ((route: Route) => Promise<void>)[] = [
    (route) => route.abort('failed'),
    (route) =>
      route.fulfill({
        status: 409,
        json: { error: 'review_round_not_open', message: 'review round is not open for task seat-refusal' },
      }),
    (route) =>
      route.fulfill({
        status: 409,
        json: { error: 'review_seat_addition_conflict', message: 'request_id was already used with a different body' },
      }),
    (route) => {
      settled = true
      return route.fulfill({
        status: 409,
        json: { error: 'review_round_settled', message: 'review round 2 of task seat-refusal is settled' },
      })
    },
  ]
  await page.route(`**/v1/tasks/${taskId}/activity*`, (route) => {
    detailReads++
    const item = openRoundItem(taskId)
    if (settled)
      item.summaries = {
        ...item.summaries,
        review_rounds: {
          by_round: { ...seatHistory.rounds, '2': { event_id: '70', verdict: 'approve', at: '2026-07-15T15:00:00Z' } },
          by_order: {},
        },
      }
    return route.fulfill({ json: item })
  })
  await page.route(`**/v1/tasks/${taskId}/review-round/seats*`, (route) => {
    requestIds.push(String((route.request().postDataJSON() as { request_id: string }).request_id))
    return responses[requestIds.length - 1](route)
  })
  await page.goto(`/w/demo/tasks/${taskId}/full`)
  const panel = roundPanel(page, 'r2-model')
  await panel.getByRole('button', { name: 'Add reviewer seat' }).click()
  const form = panel.getByRole('form', { name: 'Add reviewer seats to round 2' })
  const submit = form.getByRole('button', { name: 'Add seats' })
  await form.getByLabel('Reason').fill('Needs a database reviewer')

  // A transport failure keeps the input and says a retry repeats the request.
  await submit.click()
  await expect(panel.getByRole('alert')).toContainText('Retrying sends the same request.')
  await expect(form.getByLabel('Reason')).toHaveValue('Needs a database reviewer')

  // The unchanged retry reuses the request ID; a stale round refreshes detail.
  const readsBefore = detailReads
  await submit.click()
  await expect(panel.getByRole('alert')).toHaveText('review round is not open for task seat-refusal')
  expect(requestIds[1]).toBe(requestIds[0])
  await expect.poll(() => detailReads).toBeGreaterThan(readsBefore)
  await expect(form.getByLabel('Reason')).toHaveValue('Needs a database reviewer')

  // Changed inputs take a fresh request ID.
  await form.getByLabel('Seats to add').fill('2')
  await submit.click()
  await expect(panel.getByRole('alert')).toHaveText('request_id was already used with a different body')
  expect(requestIds[2]).not.toBe(requestIds[1])
  await expect(form.getByLabel('Seats to add')).toHaveValue('2')

  // A settled refusal refreshes the panel; the refusal stays in place after
  // the round closes to additions.
  await submit.click()
  expect(requestIds[3]).toBe(requestIds[2])
  await expect(panel.getByText('review round 2 of task seat-refusal is settled')).toBeVisible()
  await expect(panel.locator('[data-review-verdict]')).toHaveText('Approved')
  await expect(panel.getByRole('button', { name: 'Add reviewer seat' })).toHaveCount(0)
  await expect(panel.getByText('review round 2 of task seat-refusal is settled')).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`/w/demo/tasks/${taskId}/full$`))
})
