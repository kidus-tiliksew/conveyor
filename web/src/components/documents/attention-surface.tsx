import type { ReactNode } from 'react'
import { Check, TriangleAlert } from 'lucide-react'

/**
 * One document carries exactly one attention surface (spec §21.61 change 1).
 * Every machinery signal the document produces — unreconciled repository
 * changes, code that shipped past confirmed intent, versions and decisions
 * waiting on an operator — is listed here with the action that resolves it,
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
}

export function AttentionSurface({ items }: { items: AttentionItem[] }) {
  if (items.length === 0)
    return (
      <section
        aria-label="Needs your attention"
        className="flex items-center gap-2 rounded-lg border border-border bg-surface/40 px-3.5 py-2.5 text-xs text-muted"
      >
        <Check className="size-3.5 shrink-0 text-positive" />
        Nothing needs your attention on this document.
      </section>
    )
  return (
    <section aria-label="Needs your attention" className="overflow-hidden rounded-lg border border-attention/25">
      <h2 className="flex items-center gap-1.5 bg-attention-soft/40 px-4 py-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-attention">
        <TriangleAlert className="size-3" />
        Needs your attention
        <span className="ml-0.5 font-mono text-attention/70">{items.length}</span>
      </h2>
      <ul className="divide-y divide-border bg-attention-soft/10">
        {items.map((item) => (
          <li
            key={item.id}
            id={item.anchor}
            className="flex scroll-mt-6 flex-wrap items-start justify-between gap-3 px-4 py-3"
          >
            <div className="min-w-0 flex-1 basis-64">
              <p className="text-sm font-medium leading-5">{item.title}</p>
              {item.detail && <div className="mt-1 text-xs leading-5 text-muted">{item.detail}</div>}
            </div>
            {item.action && <div className="flex shrink-0 flex-wrap items-center gap-2">{item.action}</div>}
            {item.error && <p className="basis-full text-xs text-failure">{item.error}</p>}
          </li>
        ))}
      </ul>
    </section>
  )
}
