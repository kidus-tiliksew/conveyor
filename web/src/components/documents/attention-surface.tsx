import { Check, TriangleAlert } from 'lucide-react'
import { type ReactNode, useState } from 'react'

/**
 * One document carries exactly one attention surface.
 * Every machinery signal the document produces — unreconciled repository
 * changes, code that shipped past confirmed intent, versions waiting on an
 * operator — is listed here with the action that resolves it,
 * and nowhere else on the surface (REQ-1, AC-1.1 and AC-1.2). A document with
 * nothing outstanding says so in one quiet line rather than rendering an empty
 * alarm card (AC-1.3).
 */
export type AttentionItem = {
  /** Stable within one document's list; used only as the React key. */
  id: string
  /** Optional element id so an existing deep link still lands on the entry. */
  anchor?: string
  /** Plain-language statement of what needs the operator, not the event name. */
  title: string
  /** Secondary context — the causal delivery, the affected paths, the source. */
  detail?: ReactNode
  /** The resolving affordance, rendered in place beside its entry. */
  action?: ReactNode
  /** Failure text from the resolving action, kept beside the entry it came from. */
  error?: string
  /** False when this is a contextual rendering of a signal counted on its subject document. */
  countable?: boolean
}

const COLLAPSED_ITEMS = 2

export function AttentionSurface({ items }: { items: AttentionItem[] }) {
  // A long list would push the document itself off the page, so the panel shows
  // the first items and opens the rest on request, or when a link points at one.
  const [expanded, setExpanded] = useState(false)
  const hash = typeof window === 'undefined' ? '' : window.location.hash.slice(1)
  const linked = items.findIndex((item) => item.anchor === hash)
  const collapsible = items.length > COLLAPSED_ITEMS + 1 && !(hash && linked >= COLLAPSED_ITEMS)
  const shown = collapsible && !expanded ? items.slice(0, COLLAPSED_ITEMS) : items
  if (items.length === 0)
    return (
      <section
        aria-label="Needs your attention"
        className="flex items-center gap-2.5 rounded-lg border border-border bg-surface/40 px-4 py-3 text-xs text-muted"
      >
        <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-positive-soft text-positive">
          <Check className="size-3" />
        </span>
        Nothing needs your attention on this document.
      </section>
    )
  const attentionCount = new Set(items.filter((item) => item.countable !== false).map((item) => item.id)).size
  return (
    <section
      aria-label="Needs your attention"
      className="overflow-hidden rounded-lg border border-border bg-card shadow-xs"
    >
      <h2 className="flex items-center gap-1.5 border-b border-border bg-attention-soft/60 px-4 py-2.5 text-xs font-medium text-attention">
        <TriangleAlert className="size-3" />
        {attentionCount > 0 ? 'Needs your attention' : 'Related attention context'}
        {attentionCount > 0 && (
          <span className="ml-0.5 rounded-full bg-attention/15 px-1.5 py-0.5 text-xs leading-none text-attention">
            {attentionCount}
          </span>
        )}
      </h2>
      <ul className="divide-y divide-border">
        {shown.map((item) => (
          <li
            key={item.id}
            id={item.anchor}
            className="flex scroll-mt-6 flex-wrap items-start justify-between gap-3 border-l-2 border-transparent px-4 py-3.5 transition-colors hover:border-l-attention hover:bg-attention-soft/20"
          >
            {/* A wide basis sends the actions under the text when both would not fit
                on one line, rather than squeezing the title around them. */}
            <div className="min-w-0 flex-[1_1_26rem]">
              <p className="text-pretty text-sm font-medium leading-5">{item.title}</p>
              {item.detail && <div className="mt-1 text-xs leading-5 text-muted">{item.detail}</div>}
            </div>
            {item.action && <div className="flex min-w-0 max-w-full flex-wrap items-center gap-2">{item.action}</div>}
            {item.error && <p className="basis-full text-xs text-failure">{item.error}</p>}
          </li>
        ))}
      </ul>
      {collapsible && (
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() => setExpanded((value) => !value)}
          className="w-full border-t border-border px-4 py-2 text-left text-xs font-medium text-muted transition-colors hover:bg-surface hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-primary"
        >
          {expanded ? 'Show fewer' : `Show ${items.length - COLLAPSED_ITEMS} more`}
        </button>
      )}
    </section>
  )
}
