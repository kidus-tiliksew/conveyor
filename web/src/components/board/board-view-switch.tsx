import { useNavigate } from '@tanstack/react-router'
import { Columns3, List, Network } from 'lucide-react'
import { Button } from '../ui/button'
import type { BoardSearch } from './board-search'

// Kanban, list, or map (AC-6.1; req-board-map AC-1.1). The view is written
// to the address with replace, so a reload keeps it and switching does not
// pile up history entries.
export function BoardViewSwitch({ view }: { view: 'kanban' | 'list' | 'map' }) {
  const navigate = useNavigate()
  const show = (next: 'kanban' | 'list' | 'map') =>
    void navigate({
      to: '.',
      replace: true,
      search: (prev: BoardSearch) => ({
        ...prev,
        arrangement: undefined,
        focus: undefined,
        scope: undefined,
        view: next === 'kanban' ? undefined : next,
      }),
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
      <Button
        size="sm"
        variant={view === 'map' ? 'default' : 'ghost'}
        aria-pressed={view === 'map'}
        onClick={() => show('map')}
      >
        <Network />
        Graph
      </Button>
    </fieldset>
  )
}
