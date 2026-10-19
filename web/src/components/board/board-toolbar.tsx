import { useSearch } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import type { BoardSearch } from './board-search'
import { BoardViewSwitch } from './board-view-switch'

// The Board's second row, shared by Kanban, List, and Graph
// (component-web-task-surfaces). Each view fills the left side with its own
// controls; the optional pager and the view switch close the row at the same
// right edge in every view, so switching views never moves the switch.
export function BoardToolbar({ children, pager }: { children?: ReactNode; pager?: ReactNode }) {
  const search = useSearch({ strict: false }) as BoardSearch
  const view = search.view === 'map' ? 'map' : search.view === 'list' ? 'list' : 'kanban'
  return (
    <div
      data-testid="board-toolbar"
      className="flex min-h-13 shrink-0 flex-wrap items-center gap-x-3 gap-y-2 border-b border-border px-4 py-2 text-xs text-muted lg:px-6"
    >
      <div className="flex min-w-0 flex-[1_1_auto] flex-wrap items-center gap-2">{children}</div>
      <div className="ml-auto flex shrink-0 items-center gap-3">
        {pager}
        {pager && <span aria-hidden="true" className="h-5 w-px bg-border" />}
        <BoardViewSwitch view={view} />
      </div>
    </div>
  )
}
