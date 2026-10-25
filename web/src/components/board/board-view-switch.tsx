import { useNavigate } from '@tanstack/react-router'
import { Columns3, Network } from 'lucide-react'
import { cn } from '../../lib/utils'
import { Button } from '../ui/button'
import type { BoardSearch } from './board-search'

type BoardView = 'kanban' | 'map'

const views = [
  { id: 'kanban', label: 'Kanban', Icon: Columns3 },
  { id: 'map', label: 'Graph', Icon: Network },
] as const

// Kanban or map, and no list view (AC-6.1; req-board-map AC-1.1). The view is written
// to the address with replace, so a reload keeps it and switching does not
// pile up history entries. Icon-only buttons keep their view names as
// accessible names and tooltips; the selection takes the soft treatment so
// New task stays the header's only filled action.
export function BoardViewSwitch({ view }: { view: BoardView }) {
  const navigate = useNavigate()
  const show = (next: BoardView) =>
    void navigate({
      to: '.',
      replace: true,
      search: (prev: BoardSearch) => ({
        ...prev,
        arrangement: undefined,
        focus: undefined,
        scope: undefined,
        insight: undefined,
        view: next === 'kanban' ? undefined : next,
      }),
    })
  return (
    <fieldset
      aria-label="Board view"
      className="m-0 flex shrink-0 gap-0.5 rounded-lg border border-border bg-surface p-0.5"
    >
      {views.map(({ id, label, Icon }) => (
        <Button
          key={id}
          size="icon"
          variant="ghost"
          aria-label={label}
          title={label}
          aria-pressed={view === id}
          onClick={() => show(id)}
          className={cn(
            'h-7 w-8',
            view === id && 'bg-primary-soft text-primary hover:bg-primary-soft hover:text-primary',
          )}
        >
          <Icon />
        </Button>
      ))}
    </fieldset>
  )
}
