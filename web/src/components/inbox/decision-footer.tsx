import type { ReactNode } from 'react'
import { cn } from '../../lib/utils'

// The one footer every Inbox decision shares (component-next-action;
// req-operator-next-action AC-2.1, AC-2.11): what happens if nobody acts, then the actions from the left edge, then
// a quiet strip for the move keys and the identifiers. It stays in view while
// the detail's body scrolls.
export function DecisionFooter({
  consequence,
  ids,
  shortcutMerge = false,
  children,
}: {
  consequence?: ReactNode
  ids?: ReactNode
  shortcutMerge?: boolean
  children: ReactNode
}) {
  return (
    <div
      data-decision-footer
      data-shortcut-merge={shortcutMerge ? '' : undefined}
      className="z-10 shrink-0 rounded-b-xl border-t border-border bg-surface max-md:sticky max-md:bottom-0"
    >
      <div className="min-w-0 space-y-2.5 px-5 py-3">
        {consequence}
        {children}
      </div>
      <div
        data-footer-strip
        className={cn(
          'min-w-0 flex-wrap items-center gap-x-3 gap-y-1 border-t border-border px-5 py-1.5 text-xs text-faint',
          // Without the identifiers it holds only the move hint, which the list
          // header repeats, so a narrow detail gives its height back.
          ids ? 'flex' : 'hidden xl:flex',
        )}
      >
        <div className="ml-auto min-w-0 max-w-full has-[details[open]]:basis-full">{ids}</div>
      </div>
    </div>
  )
}
