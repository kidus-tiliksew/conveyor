import { FileText } from 'lucide-react'
import type { ReactNode } from 'react'

/**
 * The category navigation tree that stands beside the document canvas on
 * Requirements and System Design. It
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
    <section className="mb-5 px-3 last:mb-0">
      <h2 className="px-2 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">{label}</h2>
      <div className="space-y-0.5">{children}</div>
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
      className={`relative flex w-full items-center gap-2.5 rounded-md py-2 pl-3 pr-2.5 text-left transition-colors before:absolute before:inset-y-1.5 before:left-0 before:w-0.5 before:rounded-full before:transition-colors ${
        selected
          ? 'bg-primary-soft text-primary before:bg-primary'
          : 'text-foreground before:bg-transparent hover:bg-surface'
      }`}
    >
      <FileText className={`size-4 shrink-0 ${selected ? 'text-primary' : 'text-faint'}`} />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm font-medium">{label}</span>
        {meta && (
          <span
            className={`mt-0.5 block truncate font-mono text-[10px] tracking-wide ${selected ? 'text-primary/70' : 'text-faint'}`}
          >
            {meta}
          </span>
        )}
      </span>
    </button>
  )
}

export function DocumentTreeNote({ children }: { children: ReactNode }) {
  return <p className="px-2 py-1.5 text-xs leading-5 text-muted">{children}</p>
}
