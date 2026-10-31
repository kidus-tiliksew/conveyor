import { Link } from '@tanstack/react-router'
import type { InboxBand } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import type { InboxSearch } from './inbox-search'

const bands: { band?: InboxBand; label: string }[] = [
  { label: 'Everything' },
  { band: 'decisions', label: 'Decisions' },
  { band: 'blocked', label: 'Waiting' },
  { band: 'automatic', label: 'Automatic' },
]

// The band switch over the one inbox read (feature-operator-next-action NA-6):
// a segmented control whose selected segment is the `band` search parameter.
// Each count comes from that read, so a band link and the counts never
// disagree. The server's order is stated beneath it.
export function InboxBandChips({ search, counts }: { search: InboxSearch; counts: Record<InboxBand, number> }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <div className="flex flex-col items-start gap-1.5 sm:items-end">
      <nav
        aria-label="Inbox bands"
        className="flex flex-wrap items-center gap-0.5 rounded-lg border border-border bg-surface p-0.5 text-xs"
      >
        {bands.map((segment) => {
          const selected = search.band === segment.band
          return (
            <Link
              key={segment.label}
              to="/w/$workspaceId/inbox"
              params={{ workspaceId: workspace }}
              search={{ ...search, band: segment.band }}
              aria-current={selected ? 'page' : undefined}
              data-band-chip={segment.band ?? 'all'}
              className={cn(
                'inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 font-medium pointer-coarse:min-h-10',
                selected ? 'bg-card text-foreground shadow-sm' : 'text-muted hover:text-foreground',
              )}
            >
              {segment.label}
              {segment.band && (
                <span className={cn('tabular-nums', selected ? 'text-muted' : 'text-faint')}>
                  {counts[segment.band]}
                </span>
              )}
            </Link>
          )
        })}
      </nav>
      <span data-inbox-order className="text-[11px] text-faint">
        Blockers first, then nearest deadline
      </span>
    </div>
  )
}
