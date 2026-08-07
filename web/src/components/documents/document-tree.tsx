import type { ReactNode } from 'react'

/**
 * The category navigation tree that stands beside the document canvas on
 * Requirements and System Design (spec §21.61 change 1; REQ-2, AC-2.1). It
 * carries navigation only: a document's machinery signals belong to its
 * attention surface on the canvas, never to a badge in the tree (AC-1.2).
 */
export function DocumentTree({ children }: { children: ReactNode }) {
  return (
    <nav
      aria-label="Document tree"
      className="w-[264px] shrink-0 overflow-y-auto border-r border-border bg-surface/40 py-4"
    >
      {children}
    </nav>
  )
}

export function DocumentTreeGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <section className="mb-5 last:mb-0">
      <h2 className="px-5 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">{label}</h2>
      {children}
    </section>
  )
}

export function DocumentTreeItem({
  label,
  meta,
  selected,
  onClick,
}: {
  label: string
  /** Quiet document identity — the confirmed version, never a signal. */
  meta?: string
  selected: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      aria-current={selected ? 'true' : undefined}
      onClick={onClick}
      className={`block w-full border-l-2 py-2 pl-[18px] pr-4 text-left transition-colors ${
        selected ? 'border-primary bg-primary-soft text-primary' : 'border-transparent hover:bg-surface'
      }`}
    >
      <span className="block truncate text-sm font-medium">{label}</span>
      {meta && <span className="mt-0.5 block truncate text-[11px] text-faint">{meta}</span>}
    </button>
  )
}

export function DocumentTreeNote({ children }: { children: ReactNode }) {
  return <p className="px-5 py-1.5 text-xs leading-5 text-muted">{children}</p>
}
