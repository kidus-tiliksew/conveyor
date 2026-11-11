import type { ReactNode } from 'react'
import type { RiskSource, RiskTier, TaskRisk } from '../../lib/types'
import { Badge } from '../ui/badge'
import { Disclosure } from '../ui/disclosure'

// The task's risk tier as the server states it (req-risk-based-review REQ-11,
// AC-11.2; component-web-task-surfaces invariant 66; DEC-127(1), (2)). The page
// shows the tier chip alone. Where the tier came from, its reason, and what the
// task needs sit behind the chip, which hover, tap, keyboard focus, and its
// accessible name reach. It computes nothing: an unrated task says so instead
// of showing an invented tier.

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

// A tier chip with facts behind it: hover, tap, and keyboard focus open them,
// and the chip's accessible name carries them (DEC-127(1)). The negative
// margin keeps the 40 px coarse-pointer target from growing the row.
export function RiskTierDisclosure({
  tier,
  label,
  content,
}: {
  tier: RiskTier | undefined
  label: string
  content: ReactNode
}) {
  return (
    <Disclosure label={label} content={content} triggerClassName="rounded-md pointer-coarse:-my-2.5">
      <RiskTierBadge tier={tier} />
    </Disclosure>
  )
}

// Joins facts into one accessible name, one sentence each.
function sentences(parts: Array<string | null | undefined>): string {
  return parts
    .map((part) => part?.trim())
    .filter((part): part is string => Boolean(part))
    .map((part) => (/[.!?]$/.test(part) ? part : `${part}.`))
    .join(' ')
}

// The task fact: the tier chip alone. Behind it, in order: the source and
// "Recorded only" when they apply, the reason, what the task needs, and the
// approval count of an enforcing task (component-web-task-surfaces invariant
// 66). The merge gate card keeps its own approval count on screen
// (req-risk-based-review AC-8.2).
export function RiskTierValue({ risk }: { risk: TaskRisk }) {
  const source = risk.source ? `From ${sourceLabels[risk.source]}` : undefined
  const recorded = risk.mode === 'record' ? 'Recorded only' : undefined
  const needs = riskNeedsText(risk)
  const approvals = riskApprovalText(risk)
  return (
    <span className="inline-flex min-w-0" data-task-risk>
      <RiskTierDisclosure
        tier={risk.tier}
        label={sentences([`Risk tier ${riskTierLabel(risk.tier)}`, source, recorded, risk.reason, needs, approvals])}
        content={
          <span className="flex min-w-0 flex-col gap-1" data-task-risk-details>
            {(source || recorded) && (
              <span className="flex flex-wrap gap-x-2 gap-y-0.5">
                {source && <span className="text-muted">{source}</span>}
                {recorded && <span className="text-faint">{recorded}</span>}
              </span>
            )}
            {risk.reason && (
              <span className="whitespace-normal break-words text-foreground/85" data-task-risk-reason>
                {risk.reason}
              </span>
            )}
            <span className="whitespace-normal text-muted" data-task-risk-needs>
              {needs}
            </span>
            <RiskApprovals risk={risk} />
          </span>
        }
      />
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

// The approval count line of an enforcing task, behind the task's tier chip.
export function RiskApprovals({ risk }: { risk: TaskRisk | undefined }) {
  const text = risk ? riskApprovalText(risk) : null
  if (!text) return null
  return (
    <span className="whitespace-normal text-xs text-foreground/85" data-task-risk-approvals>
      {text}
    </span>
  )
}
