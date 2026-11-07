import type { ReactNode } from 'react'
import { actorName } from '../../lib/activity'
import type { TaskReviewRoundSummary, TaskReviewSummaryFinding } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useWorkspaceMembers } from '../app-shell'
import { ClampedBlock } from './clamped-block'

// A settled review round's current summary, shown inside its review panel
// above the seat rows (req-review-gates-evidence AC-10.5; DEC-79;
// feature-review-round-summary SU-6; component-web-task-surfaces). The summary
// is informative: seat rows, verdicts, and feedback render unchanged below it,
// and it decides nothing. Text renders as plain text, never as markup.

const severityGroups: { severity: string; label: string }[] = [
  { severity: 'blocking', label: 'Blocking' },
  { severity: 'non-blocking', label: 'Non-blocking' },
]

function groupFindings(findings: TaskReviewSummaryFinding[]) {
  const known = new Set(severityGroups.map((group) => group.severity))
  const groups = severityGroups.map((group) => ({
    ...group,
    findings: findings.filter((finding) => finding.severity === group.severity),
  }))
  // A severity outside the recorded vocabulary keeps its own group rather
  // than disappearing.
  for (const severity of new Set(findings.map((finding) => finding.severity))) {
    if (known.has(severity)) continue
    groups.push({
      severity,
      label: severity || 'Unclassified',
      findings: findings.filter((f) => f.severity === severity),
    })
  }
  return groups.filter((group) => group.findings.length > 0)
}

function seatLabel(seats: number[]) {
  if (seats.length === 0) return ''
  return `${seats.length === 1 ? 'Seat' : 'Seats'} ${seats.join(', ')}`
}

export function ReviewRoundSummary({
  round,
  summary,
  singleSeat = false,
  past = false,
  audit,
}: {
  round: number
  summary: TaskReviewRoundSummary
  singleSeat?: boolean
  // A settled round the task has moved past reads quieter and starts clipped.
  past?: boolean
  // The full-text audit disclosure, rendered when the server shortened text.
  audit?: ReactNode
}) {
  const findings = Array.isArray(summary.findings) ? summary.findings : []
  const groups = groupFindings(findings)
  const model = summary.author?.model?.trim()
  const members = useWorkspaceMembers().data ?? []
  const memberName = (actor: string) => {
    const member = members.find((entry) => entry.user_id === actor)
    return member?.display_name || member?.email || actorName(actor, members)
  }
  return (
    <section aria-label={`Round ${round} summary`} data-review-round-summary className="px-4 pt-1.5 pb-2 text-sm">
      <h3 className="sr-only">Round {round} summary</h3>
      <ClampedBlock clamp={past} toggleClassName="max-w-[37rem]">
        <p
          className={cn(
            'max-w-[37rem] whitespace-pre-line break-words',
            past ? 'text-[13px] leading-5 text-muted' : 'leading-6 text-foreground/85',
          )}
        >
          {summary.summary}
        </p>
        <div className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
          {summary.version_count > 1 && (
            <span className="text-[11px] text-faint" data-review-summary-version>
              version {summary.version} of {summary.version_count}
            </span>
          )}
          <span className="text-[11px] text-faint" data-review-summary-author>
            By {summary.author?.actor_id ? memberName(summary.author.actor_id) : 'unknown author'}
            {summary.author?.actor_owner_user_id && (
              <span data-review-summary-owner> (owned by {memberName(summary.author.actor_owner_user_id)})</span>
            )}
            {model && (
              <>
                {' · '}
                <span className="font-mono">{model}</span> <span>(self-reported)</span>
              </>
            )}
            {summary.recorded_at && <> · {absoluteTime(summary.recorded_at)}</>}
          </span>
        </div>
        {groups.map((group) => (
          <div key={group.severity} className="mt-2.5" data-review-summary-severity={group.severity}>
            <h4 className={cn('text-xs font-medium', group.severity === 'blocking' ? 'text-attention' : 'text-muted')}>
              {group.label} {group.findings.length}
            </h4>
            <ul className="mt-1 space-y-1.5">
              {group.findings.map((finding, index) => (
                <li
                  // Findings have no identity of their own; one version's order is stable.
                  key={index}
                  className={cn('max-w-[37rem]', !singleSeat && 'grid grid-cols-[auto_1fr] gap-x-2.5')}
                  data-review-summary-finding
                >
                  <span className={singleSeat ? 'sr-only' : 'pt-0.5 text-xs text-faint'}>
                    {seatLabel(finding.seats ?? [])}
                  </span>
                  <div className="min-w-0">
                    <p className="whitespace-pre-line break-words text-foreground/85">{finding.problem}</p>
                    {finding.location && (
                      <p className="font-mono text-[11px] text-muted [overflow-wrap:anywhere]">{finding.location}</p>
                    )}
                    {finding.fix && (
                      <p className="whitespace-pre-line break-words text-xs text-muted">Fix: {finding.fix}</p>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </ClampedBlock>
      {summary.truncated && audit && <div className="mt-1 text-xs text-muted">{audit}</div>}
    </section>
  )
}
