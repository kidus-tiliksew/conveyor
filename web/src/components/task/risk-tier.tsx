import type { RiskSource, RiskTier, TaskRisk } from '../../lib/types'
import { Badge } from '../ui/badge'

// The task's risk tier as the server states it (req-risk-based-review REQ-11,
// AC-11.2; component-web-task-surfaces). The page shows the tier, where it came
// from, its reason, and what the task needs. It computes nothing: an unrated
// task says so instead of showing an invented tier.

const tierLabels: Record<RiskTier, string> = { low: 'Low', medium: 'Medium', high: 'High' }

const sourceLabels: Record<RiskSource, string> = {
  plan: 'the plan',
  path_rule: 'path rules',
  reviewer: 'a reviewer',
  person: 'a person',
}

export function riskTierLabel(tier: RiskTier | undefined): string {
  return tier ? (tierLabels[tier] ?? tier) : 'Not rated yet'
}

function count(value: number, singular: string, plural: string): string {
  return value === 0 ? `no ${singular}` : `${value} ${value === 1 ? singular : plural}`
}

// "Needs 1 agent review and no human approval" states the requirements in
// effect, the counts that decide the task's review and merge.
export function riskNeedsText(risk: TaskRisk): string {
  return `Needs ${count(risk.required_agent_reviews, 'agent review', 'agent reviews')} and ${count(risk.required_human_approvals, 'human approval', 'human approvals')}`
}

// "1 of 2 human approvals recorded. 1 more needed, including a trusted
// author's." states an enforcing task's approvals of its reviewed head as the
// server counts them (req-risk-based-review AC-8.2, AC-11.1, AC-11.2). It is
// null for a task that needs no human approval or does not enforce.
export function riskApprovalText(risk: TaskRisk): string | null {
  if (risk.mode !== 'enforce' || risk.required_human_approvals < 1) return null
  const recorded = `${risk.recorded_approvals} of ${risk.required_human_approvals} human ${risk.required_human_approvals === 1 ? 'approval' : 'approvals'} recorded.`
  if (risk.approvals_satisfied) return `${recorded} The approvals are complete.`
  const remaining = risk.remaining_approvals ?? Math.max(risk.required_human_approvals - risk.recorded_approvals, 0)
  const conditions = [
    risk.author_approval_needed ? "a trusted author's" : '',
    risk.non_author_approval_needed ? 'one from a person who is not an author' : '',
  ].filter(Boolean)
  const including = conditions.length > 0 ? `, including ${conditions.join(' and ')}` : ''
  // With an untrusted author no author's approval counts, so the trusted
  // author's approval is met only by a non-author's waiver (AC-8.5, REQ-9).
  const waiver = risk.author_waiver_needed
    ? " A person who is not an author must waive the trusted author's approval."
    : ''
  if (remaining === 0) return `${recorded}${waiver}`
  return `${recorded} ${remaining} more needed${including}.${waiver}`
}

export function RiskTierBadge({ tier }: { tier: RiskTier | undefined }) {
  return (
    <Badge variant={tier ? 'default' : 'outline'} data-risk-tier={tier ?? 'unrated'}>
      {riskTierLabel(tier)}
    </Badge>
  )
}

// The task fact: the tier, its source and reason on one line, and the counts on
// the next. Long reasons wrap rather than widen the page.
export function RiskTierValue({ risk }: { risk: TaskRisk }) {
  const source = risk.source ? sourceLabels[risk.source] : undefined
  return (
    <span className="flex min-w-0 flex-col gap-1" data-task-risk>
      <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <RiskTierBadge tier={risk.tier} />
        {source && <span className="text-xs text-muted">From {source}</span>}
        {risk.mode === 'record' && <span className="text-xs text-faint">Recorded only</span>}
      </span>
      {risk.reason && (
        <span className="whitespace-normal break-words text-xs text-foreground/85" data-task-risk-reason>
          {risk.reason}
        </span>
      )}
      <span className="whitespace-normal text-xs text-muted" data-task-risk-needs>
        {riskNeedsText(risk)}
      </span>
      <RiskApprovals risk={risk} />
    </span>
  )
}

// A review seat's stated tier beside its verdict. The reason is the tooltip
// and screen-reader text, so the seat row stays one line.
export function SeatRiskTier({ tier, reason }: { tier?: RiskTier; reason?: string }) {
  if (!tier) return null
  return (
    <span className="inline-flex items-center gap-1 text-[11px] text-muted" title={reason} data-seat-risk-tier={tier}>
      Risk <RiskTierBadge tier={tier} />
      {reason && <span className="sr-only">: {reason}</span>}
    </span>
  )
}

// The approval count line of an enforcing task, wherever its tier shows.
export function RiskApprovals({ risk }: { risk: TaskRisk | undefined }) {
  const text = risk ? riskApprovalText(risk) : null
  if (!text) return null
  return (
    <span className="whitespace-normal text-xs text-foreground/85" data-task-risk-approvals>
      {text}
    </span>
  )
}
