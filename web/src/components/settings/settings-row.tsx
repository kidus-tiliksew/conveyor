import { ChevronDown, type LucideIcon } from 'lucide-react'
import { type ReactNode, useId, useState } from 'react'
import { cn } from '../../lib/utils'

export type SettingsRowAction = 'Change' | 'Manage' | 'Show'

/**
 * One Settings row that opens in place (component-web-dashboard, Settings).
 *
 * The header is one native button: icon, title, the current state beneath the
 * title, and an action word with a chevron. The open content stays mounted
 * while the row is closed, inside a region with the `hidden` attribute, so a
 * draft, a stale-save panel, or a new token value that the person has not
 * dismissed outlives closing the row. The `hidden` attribute removes the
 * content from sight, from the tab order, and from assistive technology.
 * `aside` is a control beside the header button, never inside it, that works
 * while the row is closed.
 */
export function SettingsRow({
  icon: Icon,
  title,
  summary,
  action,
  badge,
  aside,
  defaultOpen = false,
  children,
}: {
  icon: LucideIcon
  title: string
  summary: ReactNode
  action: SettingsRowAction
  badge?: ReactNode
  aside?: ReactNode
  defaultOpen?: boolean
  children: ReactNode
}) {
  const id = useId()
  const [open, setOpen] = useState(defaultOpen)
  const titleID = `${id}-title`
  const actionID = `${id}-action`
  const summaryID = `${id}-summary`
  const regionID = `${id}-region`
  return (
    <div className="min-w-0">
      <div className="flex min-w-0 items-start">
        <button
          type="button"
          data-settings-row={title}
          aria-expanded={open}
          aria-controls={regionID}
          aria-labelledby={`${titleID} ${actionID}`}
          aria-describedby={open ? undefined : summaryID}
          onClick={() => setOpen((current) => !current)}
          className={cn(
            'group flex min-h-11 min-w-0 flex-1 items-start gap-3 rounded-lg py-3 pl-4 text-left focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary',
            aside && !open ? 'pr-2' : 'pr-4',
          )}
        >
          <Icon className="mt-0.5 size-4 shrink-0 text-faint" aria-hidden="true" />
          <span className="min-w-0 flex-1">
            <span className="flex min-w-0 items-start gap-2">
              <span className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-1">
                <span id={titleID} className="text-sm font-medium text-foreground">
                  {title}
                </span>
                {badge}
              </span>
              <span className="flex shrink-0 items-center gap-1 text-sm font-medium text-primary group-hover:underline">
                <span id={actionID}>{open ? 'Close' : action}</span>
                <ChevronDown
                  className={cn('size-4 transition-transform motion-reduce:transition-none', open && 'rotate-180')}
                  aria-hidden="true"
                />
              </span>
            </span>
            {!open && (
              <span id={summaryID} className="mt-1 block min-w-0 break-words text-sm leading-5 text-muted">
                {summary}
              </span>
            )}
          </span>
        </button>
        {aside && !open && <div className="flex shrink-0 items-center py-2 pr-3">{aside}</div>}
      </div>
      <section id={regionID} aria-labelledby={titleID} hidden={!open} className="min-w-0 px-4 pb-4">
        {children}
      </section>
    </div>
  )
}

/** One fact of a closed row's summary, on its own line. */
export function SummaryLine({ className, children }: { className?: string; children: ReactNode }) {
  return <span className={cn('block min-w-0 break-words', className)}>{children}</span>
}
