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

// The band switch over the one inbox read (component-next-action, Public
// surfaces):
// a segmented control whose selected segment is the `band` search parameter.
// Each count comes from that read, so a band link and the counts never
// disagree. The server's order is stated beneath it. Before the first read
// answers, each count is a placeholder of the same size, so the header keeps
// its shape when the counts arrive.
export function InboxBandChips({ search, counts }: { search: InboxSearch; counts?: Record<InboxBand, number> }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <div className="flex w-full min-w-0 flex-col items-start gap-1.5 sm:w-auto sm:items-end">
      <nav
        aria-label="Inbox bands"
        className="flex w-full items-center gap-0.5 overflow-x-auto rounded-lg bg-surface p-0.5 text-xs sm:w-auto"
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
                'inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-md px-2 py-1 font-medium pointer-coarse:min-h-10 max-sm:flex-1',
                selected ? 'bg-card text-foreground shadow-sm' : 'text-muted hover:text-foreground',
              )}
            >
              {segment.label}
              {segment.band &&
                (counts ? (
                  <span className={cn('tabular-nums', selected ? 'text-muted' : 'text-faint')}>
                    {counts[segment.band]}
                  </span>
                ) : (
                  <span
                    aria-hidden
                    data-band-count-placeholder
                    className="inline-block h-3 w-[1ch] animate-pulse rounded-sm bg-raised/60"
                  />
                ))}
            </Link>
          )
        })}
      </nav>
      <span data-inbox-order className="sr-only">
        Blockers first, then nearest deadline
      </span>
    </div>
  )
}
