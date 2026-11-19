// PROTOTYPE. A made-up crate that shows the page with a busy delivery: fourteen
// tasks across develop and master, two releases of develop to master, and
// every state a task can be in. Times are relative to now, so it always looks
// current. Nothing here is read from or written to a server.
import type {
  CrateActivityEvent,
  CrateDetail,
  CrateTask,
  NextActionCompact,
  Stage,
  TaskState,
} from '../../../lib/types'
import { DAY, type Promotion, type TaskExtra } from './model'

const HOUR = 3_600_000

interface Seed {
  n: number
  title: string
  base: 'develop' | 'master'
  // Days ago the task was created.
  created: number
  // Days ago each pull request opened, then the review verdicts that followed.
  rounds?: Array<{ opened: number; verdict?: 'approve' | 'changes_requested' }>
  merged?: number
  cancelled?: number
  state: TaskState
  stage?: Stage
  action: Partial<NextActionCompact> & { label: string }
}

const agent = (label: string, kind = 'working'): Seed['action'] => ({ label, actor: 'agent', kind })

const seeds: Seed[] = [
  {
    n: 1,
    title: 'Add the invoices table and ledger migrations',
    base: 'develop',
    created: 16,
    rounds: [{ opened: 15.2, verdict: 'approve' }],
    merged: 15,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 2,
    title: 'Stripe webhook intake with signature checks',
    base: 'develop',
    created: 16,
    rounds: [
      { opened: 14.6, verdict: 'changes_requested' },
      { opened: 14.1, verdict: 'approve' },
    ],
    merged: 14,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 3,
    title: 'Proration rules for mid-cycle plan changes',
    base: 'develop',
    created: 15,
    rounds: [{ opened: 12.4, verdict: 'approve' }],
    merged: 12.2,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 4,
    title: 'Invoice PDF rendering service',
    base: 'develop',
    created: 14,
    rounds: [
      { opened: 11.5, verdict: 'changes_requested' },
      { opened: 10.9, verdict: 'changes_requested' },
      { opened: 10.2, verdict: 'approve' },
    ],
    merged: 10,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 5,
    title: 'Fix double charge on retried webhooks',
    base: 'master',
    created: 9.5,
    rounds: [{ opened: 9.2, verdict: 'approve' }],
    merged: 9.1,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 6,
    title: 'Billing settings page: plans and payment methods',
    base: 'develop',
    created: 12,
    rounds: [{ opened: 7.6, verdict: 'approve' }],
    merged: 7.4,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 7,
    title: 'Dunning emails for failed payments',
    base: 'develop',
    created: 10,
    rounds: [
      { opened: 5.8, verdict: 'changes_requested' },
      { opened: 5.1, verdict: 'approve' },
    ],
    merged: 5,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 8,
    title: 'Usage metering pipeline and hourly rollups',
    base: 'develop',
    created: 9,
    rounds: [{ opened: 3.4, verdict: 'approve' }],
    merged: 3.2,
    state: 'merged',
    action: { label: 'Finished' },
  },
  {
    n: 9,
    title: 'Tax calculation through the provider API',
    base: 'develop',
    created: 7,
    rounds: [{ opened: 1.1, verdict: 'approve' }],
    state: 'awaiting_human',
    stage: 'gate',
    action: {
      label: 'Approve the merge',
      actor: 'operator',
      kind: 'merge_approval',
      judgment: true,
      summary: 'Review approved it. The merge waits for your approval.',
    },
  },
  {
    n: 10,
    title: 'Customer invoice history and download',
    base: 'develop',
    created: 6,
    rounds: [{ opened: 1.6, verdict: 'changes_requested' }, { opened: 0.3 }],
    state: 'running',
    stage: 'review',
    action: agent('In review'),
  },
  {
    n: 11,
    title: 'Refunds and credit notes',
    base: 'develop',
    created: 5,
    rounds: [{ opened: 0.15 }],
    state: 'queued',
    stage: 'review',
    action: agent('Ready for review', 'ready'),
  },
  {
    n: 12,
    title: 'Hotfix: currency rounding on yearly plans',
    base: 'master',
    created: 1.4,
    state: 'running',
    stage: 'implement',
    action: agent('Implementing'),
  },
  {
    n: 13,
    title: 'Revenue dashboard for finance',
    base: 'develop',
    created: 4,
    state: 'awaiting_human',
    stage: 'spec',
    action: {
      label: 'Approve the plan',
      actor: 'operator',
      kind: 'plan_approval',
      judgment: true,
      summary: 'The plan is written and waits for your approval.',
    },
  },
  {
    n: 14,
    title: 'Migrate legacy subscriptions to the new ledger',
    base: 'develop',
    created: 3,
    state: 'queued',
    stage: 'spec',
    action: {
      label: 'Waiting on Tax calculation through the provider API',
      actor: 'dependency',
      kind: 'blocked',
    },
  },
  {
    n: 15,
    title: 'PayPal as a second payment provider',
    base: 'develop',
    created: 11,
    cancelled: 8,
    state: 'closed',
    action: { label: 'Closed' },
  },
]

const taskId = (n: number) => `sample-${String(n).padStart(2, '0')}`

export const SAMPLE_CRATE_ID = 'sample-billing-v2'

export function sampleCrate(now = Date.now()) {
  const at = (daysAgo: number) => new Date(now - daysAgo * DAY).toISOString()
  const tasks: CrateTask[] = seeds.map((seed) => {
    const lastRound = seed.rounds?.[seed.rounds.length - 1]
    return {
      id: taskId(seed.n),
      title: seed.title,
      state: seed.state,
      branch: `conveyor/task-${taskId(seed.n)}`,
      pull_request: lastRound ? { number: 400 + seed.n * 3, url: '', repository: 'acme/billing' } : null,
      next_action: {
        actor: 'none',
        kind: 'none',
        judgment: false,
        summary: '',
        deadline: null,
        ...seed.action,
      } as NextActionCompact,
    }
  })

  const events: CrateActivityEvent[] = []
  let eventId = 1
  const push = (seed: Seed, daysAgo: number, kind: string, more: Partial<CrateActivityEvent> = {}) =>
    events.push({
      id: eventId++,
      kind,
      at: at(daysAgo),
      actor_id: 'user:sample',
      actor_role: 'human',
      task_id: taskId(seed.n),
      task_title: seed.title,
      ...more,
    })
  for (const seed of seeds) {
    push(seed, seed.created, 'task.created')
    push(seed, seed.created - 0.001, 'task.crate_set')
    for (const round of seed.rounds ?? []) {
      push(seed, round.opened, 'pull_request.opened', { pull_request: { number: 400 + seed.n * 3, url: '' } })
      if (round.verdict) push(seed, round.opened - 0.12, 'review.round_completed', { verdict: round.verdict })
    }
    if (seed.merged !== undefined) push(seed, seed.merged, 'merge.confirmed')
    if (seed.cancelled !== undefined) push(seed, seed.cancelled, 'task.cancelled')
  }
  events.sort((a, b) => Date.parse(b.at) - Date.parse(a.at))

  const extras: Record<string, TaskExtra> = {}
  for (const seed of seeds)
    extras[taskId(seed.n)] = { base_branch: seed.base, created_at: at(seed.created), next_stage: seed.stage }

  const open = tasks.filter((task) => task.state !== 'merged' && task.state !== 'closed').length
  const merged = tasks.filter((task) => task.state === 'merged').length
  const target = new Date(now + 8 * DAY).toISOString().slice(0, 10)
  const crate: CrateDetail = {
    id: SAMPLE_CRATE_ID,
    name: 'Billing v2',
    target_date: target,
    state: 'open',
    shipped: false,
    created_by: 'user:sample',
    created_at: at(16.2),
    closed_at: null,
    open_task_count: open,
    merged_task_count: merged,
    total_task_count: tasks.length,
    requirements: [
      { id: 'req-billing', title: 'Billing and invoicing', task_count: 9 },
      { id: 'req-payments', title: 'Payment providers', task_count: 4 },
      { id: 'req-tax', title: 'Tax compliance', task_count: 1 },
      { id: 'req-finance-reporting', title: 'Finance reporting', task_count: 0 },
    ],
    undeclared_requirements: [{ id: 'req-notifications', title: 'Customer notifications', task_count: 1 }],
    tasks,
  } as CrateDetail

  // develop was released to master twice.
  const promotions: Promotion[] = [
    { at: now - 11.4 * DAY, from: 'develop', to: 'master' },
    { at: now - 4.3 * DAY - 2 * HOUR, from: 'develop', to: 'master' },
  ]
  return { crate, events, extras, promotions }
}
