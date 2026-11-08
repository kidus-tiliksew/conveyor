import type { ActivityItem } from '../../lib/types'
import { relativeTime } from '../../lib/utils'

// Why a parked task is parked: what triage recorded when it parked the task,
// shown in the next-action card on the Board sheet and the full task route.
// It reads the bounded detail's `summaries.park_reason`, folded from the
// complete ledger, and never the event page (component-web-task-surfaces
// invariant 25; req-operator-next-action AC-1.1). It prints recorded facts
// only, so the card's statement stays the server's own (DEC-61; AC-1.7).
export function NextActionParkReason({ item }: { item: ActivityItem }) {
  const reason = item.summaries?.park_reason
  if (!reason) {
    return (
      <p data-park-reason className="text-xs text-muted">
        Triage recorded no reason.
      </p>
    )
  }
  const lists = [
    { label: 'Open questions', entries: reason.questions ?? [] },
    { label: 'Risks', entries: reason.risks ?? [] },
  ].filter((list) => list.entries.length > 0)
  return (
    <section
      aria-label="Why it is parked"
      data-park-reason
      className="min-w-0 space-y-2 rounded-md border border-border/70 px-3 py-2.5"
    >
      <div className="min-w-0">
        <h3 className="text-xs font-medium text-muted">Triage summary</h3>
        <p className="mt-0.5 whitespace-pre-wrap break-words text-sm">
          {reason.summary || 'Triage stated no summary.'}
        </p>
      </div>
      {lists.map((list) => (
        <section key={list.label} aria-label={list.label} className="min-w-0">
          <h3 className="text-xs font-medium text-muted">{list.label}</h3>
          <ul className="mt-0.5 list-disc space-y-0.5 pl-5 text-sm">
            {list.entries.map((entry, index) => (
              <li key={`${index}:${entry}`} className="break-words">
                {entry}
              </li>
            ))}
          </ul>
        </section>
      ))}
      <p className="text-xs text-faint">
        Recorded by triage <time dateTime={reason.at}>{relativeTime(reason.at)}</time>
        {reason.truncated ? ' · shortened' : ''}
      </p>
    </section>
  )
}
