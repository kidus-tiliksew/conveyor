import { Link } from '@tanstack/react-router'
import type { InboxBand } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import type { InboxSearch } from './inbox-search'

const chips: { band?: InboxBand; label: string }[] = [
  { label: 'All' },
  { band: 'decisions', label: 'Decisions' },
  { band: 'blocked', label: 'Blocked by tasks' },
  { band: 'automatic', label: 'Auto' },
]

// The band switch over the one inbox read (feature-operator-next-action NA-6).
// Each count comes from that read, and the selected chip is the `band` search
// parameter, so a band link and the counts never disagree.
export function InboxBandChips({ search, counts }: { search: InboxSearch; counts: Record<InboxBand, number> }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <nav aria-label="Inbox bands" className="mt-5 flex flex-wrap items-center gap-2">
      {chips.map((chip) => {
        const selected = search.band === chip.band
        return (
          <Link
            key={chip.label}
            to="/w/$workspaceId/inbox"
            params={{ workspaceId: workspace }}
            search={{ ...search, band: chip.band }}
            aria-current={selected ? 'page' : undefined}
            data-band-chip={chip.band ?? 'all'}
            className={cn(
              'inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium pointer-coarse:min-h-10',
              selected
                ? 'border-foreground bg-foreground text-background'
                : 'border-border bg-card text-foreground hover:bg-surface',
            )}
          >
            {chip.label}
            {chip.band && <span className={selected ? 'opacity-70' : 'text-muted'}>{counts[chip.band]}</span>}
          </Link>
        )
      })}
      <span className="ml-auto text-xs text-faint">Blockers first, then nearest deadline</span>
    </nav>
  )
}
