import type { ReactNode } from 'react'

// The one footer every Inbox decision shares (component-next-action;
// req-operator-next-action AC-2.1, AC-2.11): the actions from the left edge.
// It stays in view while the detail's body scrolls. It holds no if-ignored
// sentence and no identifiers (AC-1.9; component-next-action invariant 41).
export function DecisionFooter({ shortcutMerge = false, children }: { shortcutMerge?: boolean; children: ReactNode }) {
  return (
    <div
      data-decision-footer
      data-shortcut-merge={shortcutMerge ? '' : undefined}
      className="z-10 shrink-0 rounded-b-xl border-t border-border bg-surface max-md:sticky max-md:bottom-0"
    >
      <div className="min-w-0 space-y-2.5 px-5 py-3">{children}</div>
    </div>
  )
}
