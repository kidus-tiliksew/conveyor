import type { InterventionAction } from './types'

// The six fixed feed groups (spec §13.3) plus a collapsed archive so merged
// and closed work stays reachable without cluttering the factory's WIP view.
export type GroupKey = 'triage' | 'spec' | 'implement' | 'review' | 'verify' | 'human' | 'done'

// "Needs operator" is not a pipeline stage — it collects tasks holding at a
// human gate from every stage. It leads the board so the work waiting on you
// is the first thing read, and never the column that scrolled off the edge.
export const stageGroups: ReadonlyArray<{ key: GroupKey; label: string }> = [
  { key: 'human', label: 'Needs operator' },
  { key: 'triage', label: 'Triage' },
  { key: 'spec', label: 'Spec' },
  { key: 'implement', label: 'Implementing' },
  { key: 'review', label: 'Reviewing' },
  { key: 'verify', label: 'Verifying' },
  { key: 'done', label: 'Completed' },
]

// The decisions the gate offers. Diverges from spec §13.2 pending amendment
// (operator decision, 2026-07-15): pull-to-local is retired from the UI in
// the MCP pull model (agents pull work orders; humans use the checkout
// helper), and "redirect" surfaces as "Request changes" — the wire action is
// unchanged.
export const interventionActions: ReadonlyArray<{
  action: InterventionAction
  label: string
  hint: string
  confirmLabel: string
}> = [
  { action: 'approve', label: 'Approve', hint: 'Merge or advance the task', confirmLabel: 'Approve' },
  { action: 'redirect', label: 'Request changes', hint: 'Written feedback returns the pushed branch to the implementing agent', confirmLabel: 'Send feedback' },
  { action: 'reject', label: 'Reject', hint: 'Close the task', confirmLabel: 'Reject task' },
]

// The API requires a reason code on every decision (§13.2 — the training
// signal for self-improvement). The operator no longer picks one; it is
// derived from the action, and the free-text comment carries the nuance.
export const defaultReasonCode: Record<InterventionAction, string> = {
  approve: 'approved',
  redirect: 'changes-requested',
  reject: 'rejected',
  pull_to_local: 'needs-human',
	cancel: 'cancelled',
}

export const stageLabels: Record<string, string> = {
  triage: 'Triage',
  spec: 'Spec',
  implement: 'Implement',
  review: 'Code review',
  verify: 'Verify',
  gate: 'Approval',
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
