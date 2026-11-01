import type { FactoryAct } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'

// What the factory did on its own, collapsed by default (AC-2.5), as a
// timeline inside the automatic panel.
export function ActsLog({ acts, bordered }: { acts: FactoryAct[]; bordered?: boolean }) {
  if (acts.length === 0) return null
  return (
    <details className={cn('px-4 py-2.5 text-xs', bordered && 'border-t border-border')} data-acts-log>
      <summary className="cursor-pointer select-none font-medium text-muted">
        Recent factory acts · {acts.length}
      </summary>
      <ol className="mt-2 space-y-2 border-l border-border pl-3">
        {acts.map((act) => (
          <li key={act.id} className="relative flex min-w-0 flex-wrap items-baseline gap-x-2">
            <span aria-hidden className="absolute top-1.5 -left-[15px] size-1.5 rounded-full bg-edge" />
            <span className="min-w-0 break-words text-foreground">{act.summary ?? act.kind}</span>
            <span className="font-mono text-faint">{act.task_id}</span>
            <time dateTime={act.at} className="text-faint">
              {relativeTime(act.at)}
            </time>
          </li>
        ))}
      </ol>
    </details>
  )
}
