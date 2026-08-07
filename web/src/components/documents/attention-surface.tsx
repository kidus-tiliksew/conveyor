import type { ReactNode } from 'react'
import { Check } from 'lucide-react'

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
  /** Plain-language statement of what needs the operator, not the event name. */
  title: string
  /** Secondary context — the causal delivery, the affected paths, the source. */
  detail?: ReactNode
  /** The resolving affordance, rendered in place beside its entry. */
  action?: ReactNode
}

export function AttentionSurface({ items }: { items: AttentionItem[] }) {
  if (items.length === 0)
    return (
      <section aria-label="Needs your attention">
        <p className="flex items-center gap-2 text-xs text-muted">
          <Check className="size-3.5 text-positive" />
          Nothing needs your attention on this document.
        </p>
      </section>
    )
  return (
    <section
      aria-label="Needs your attention"
      className="overflow-hidden rounded-lg border border-attention/30 bg-attention-soft/25"
    >
      <h2 className="border-b border-attention/20 px-4 py-2.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-attention">
        Needs your attention
      </h2>
      <ul className="divide-y divide-attention/15">
        {items.map((item) => (
          <li key={item.id} className="flex flex-wrap items-start justify-between gap-3 px-4 py-3">
            <div className="min-w-0 flex-1 basis-64">
              <p className="text-sm font-medium leading-5">{item.title}</p>
              {item.detail && <div className="mt-1 text-xs leading-5 text-muted">{item.detail}</div>}
            </div>
            {item.action && <div className="flex shrink-0 flex-wrap items-center gap-2">{item.action}</div>}
          </li>
        ))}
      </ul>
    </section>
  )
}
