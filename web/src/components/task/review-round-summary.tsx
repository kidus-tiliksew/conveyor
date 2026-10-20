import type { ReactNode } from 'react'
import type { TaskReviewRoundSummary, TaskReviewSummaryFinding } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'

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
  audit,
}: {
  round: number
  summary: TaskReviewRoundSummary
  // The full-text audit disclosure, rendered when the server shortened text.
  audit?: ReactNode
}) {
  const findings = Array.isArray(summary.findings) ? summary.findings : []
  const groups = groupFindings(findings)
  const model = summary.author?.model?.trim()
  return (
    <section
      aria-label={`Round ${round} summary`}
      data-review-round-summary
      className="border-b border-border px-4 py-3 text-sm"
    >
      <div className="mb-1.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
        <h3 className="text-xs font-medium text-muted">Round {round} summary</h3>
        {summary.version_count > 1 && (
          <span className="text-[11px] text-faint" data-review-summary-version>
            version {summary.version} of {summary.version_count}
          </span>
        )}
        <span className="ml-auto text-[11px] text-faint" data-review-summary-author>
          By {summary.author?.actor_id || 'unknown author'}
          {model && (
            <>
              {' · '}
              <span className="font-mono">{model}</span> <span>(self-reported)</span>
            </>
          )}
          {summary.recorded_at && <> · {absoluteTime(summary.recorded_at)}</>}
        </span>
      </div>
      <p className="whitespace-pre-line break-words leading-6 text-foreground/85">{summary.summary}</p>
      {groups.map((group) => (
        <div key={group.severity} className="mt-2.5" data-review-summary-severity={group.severity}>
          <h4
            className={cn(
              'text-[11px] font-medium uppercase tracking-wide',
              group.severity === 'blocking' ? 'text-attention' : 'text-muted',
            )}
          >
            {group.label} {group.findings.length}
          </h4>
          <ul className="mt-1 space-y-1.5">
            {group.findings.map((finding, index) => (
              <li
                // Findings have no identity of their own; one version's order is stable.
                key={index}
                className="grid grid-cols-[auto_1fr] gap-x-2.5"
                data-review-summary-finding
              >
                <span className="pt-0.5 text-xs text-faint">{seatLabel(finding.seats ?? [])}</span>
                <div className="min-w-0">
                  <p className="whitespace-pre-line break-words text-foreground/85">{finding.problem}</p>
                  {finding.location && <p className="break-all font-mono text-[11px] text-muted">{finding.location}</p>}
                  {finding.fix && (
                    <p className="whitespace-pre-line break-words text-xs text-muted">Fix: {finding.fix}</p>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </div>
      ))}
      {summary.truncated && audit && <div className="mt-1 text-xs text-muted">{audit}</div>}
    </section>
  )
}
