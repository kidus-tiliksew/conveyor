import { useNavigate } from '@tanstack/react-router'
import { Columns3, List } from 'lucide-react'
import { Button } from '../ui/button'
import type { BoardSearch } from './board-search'

// Kanban or list (AC-6.1). The view is written to the address with replace,
// so a reload keeps it and switching does not pile up history entries.
export function BoardViewSwitch({ view }: { view: 'kanban' | 'list' }) {
  const navigate = useNavigate()
  const show = (next: 'kanban' | 'list') =>
    void navigate({
      to: '.',
      replace: true,
      search: (prev: BoardSearch) => ({ ...prev, view: next === 'list' ? ('list' as const) : undefined }),
    })
  return (
    <fieldset aria-label="Board view" className="m-0 flex gap-1 border-0 p-0">
      <Button
        size="sm"
        variant={view === 'kanban' ? 'default' : 'ghost'}
        aria-pressed={view === 'kanban'}
        onClick={() => show('kanban')}
      >
        <Columns3 />
        Kanban
      </Button>
      <Button
        size="sm"
        variant={view === 'list' ? 'default' : 'ghost'}
        aria-pressed={view === 'list'}
        onClick={() => show('list')}
      >
        <List />
        List
      </Button>
    </fieldset>
  )
}
