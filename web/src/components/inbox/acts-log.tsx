import type { FactoryAct } from '../../lib/types'
import { relativeTime } from '../../lib/utils'

// What the factory did on its own, collapsed by default (AC-2.5).
export function ActsLog({ acts }: { acts: FactoryAct[] }) {
  if (acts.length === 0) return null
  return (
    <details className="rounded-lg border border-border bg-card px-4 py-3 text-sm" data-acts-log>
      <summary className="cursor-pointer select-none text-xs font-medium text-muted">
        Factory acts ({acts.length})
      </summary>
      <ul className="mt-2 space-y-1.5">
        {acts.map((act) => (
          <li key={act.id} className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs">
            <span className="font-mono text-faint">{act.task_id}</span>
            <span className="min-w-0 break-words">{act.summary ?? act.kind}</span>
            <time dateTime={act.at} className="text-faint">
              {relativeTime(act.at)}
            </time>
          </li>
        ))}
      </ul>
    </details>
  )
}
