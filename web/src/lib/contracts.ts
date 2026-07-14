import type { InterventionAction } from './types'

// The six fixed feed groups (spec §13.3) plus a collapsed archive so merged
// and closed work stays reachable without cluttering the factory's WIP view.
export type GroupKey = 'triage' | 'spec' | 'implement' | 'review' | 'verify' | 'human' | 'done'

export const stageGroups: ReadonlyArray<{ key: GroupKey; label: string }> = [
  { key: 'triage', label: 'Triage' },
  { key: 'spec', label: 'Spec' },
  { key: 'implement', label: 'Implementing' },
  { key: 'review', label: 'Reviewing' },
  { key: 'verify', label: 'Verifying' },
  { key: 'human', label: 'Awaiting human' },
  { key: 'done', label: 'Completed' },
]

export const interventionActions: ReadonlyArray<{
  action: InterventionAction
  label: string
  hint: string
}> = [
  { action: 'approve', label: 'Approve', hint: 'Merge or advance the task' },
  { action: 'reject', label: 'Reject', hint: 'Close with a reason code' },
  { action: 'redirect', label: 'Redirect', hint: 'Comment; return the pushed branch to the implementing agent' },
  { action: 'pull_to_local', label: 'Pull to local', hint: 'Available after the implementation agent pushes the assigned branch' },
]

// Baseline reason-code taxonomy (spec §13.2). The API accepts free-form
// codes; these are the curated defaults per action.
export const reasonCodesByAction: Record<InterventionAction, readonly string[]> = {
  approve: ['approved'],
  reject: ['spec-wrong', 'scope-creep', 'hallucinated-api', 'broken-pair', 'style', 'needs-human'],
  redirect: ['style', 'spec-wrong', 'hallucinated-api', 'scope-creep', 'flaky-env', 'broken-pair'],
  pull_to_local: ['needs-human', 'broken-pair', 'flaky-env'],
}

export const stageLabels: Record<string, string> = {
  triage: 'Triage',
  spec: 'Spec',
  implement: 'Implement',
  review: 'Code review',
  verify: 'Verify',
  gate: 'Human gate',
  merge: 'Merge',
  monitor: 'Monitor',
}

export const taskStateLabels: Record<string, string> = {
  claiming: 'Claiming',
  queued: 'Queued',
  running: 'Running',
  awaiting_human: 'Awaiting human',
  approved: 'Approved',
  merged: 'Merged',
  closed: 'Closed',
  parked: 'Parked',
}
