import { ArrowDown, ArrowUp, ChevronRight, FileText, Search } from 'lucide-react'
import { type ReactNode, useEffect, useRef, useState } from 'react'
import { cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import type { DocumentSortDirection } from './document-sort'

export type { DocumentSortDirection } from './document-sort'

// The tree is resizable between these bounds; the chosen width is a local
// reading preference, so it persists per browser rather than per workspace.
const minTreeWidth = 220
const maxTreeWidth = 480
const defaultTreeWidth = 264
const treeWidthKey = 'conveyor-document-tree-width'

function clampTreeWidth(width: number) {
  return Math.min(maxTreeWidth, Math.max(minTreeWidth, width))
}

function readStoredTreeWidth() {
  try {
    const stored = Number(window.localStorage.getItem(treeWidthKey))
    return Number.isFinite(stored) && stored > 0 ? clampTreeWidth(stored) : defaultTreeWidth
  } catch {
    return defaultTreeWidth
  }
}

function persistTreeWidth(width: number) {
  try {
    window.localStorage.setItem(treeWidthKey, String(width))
  } catch {
    // A blocked store only loses the preference, never the resize itself.
  }
}

/**
 * The category navigation tree that stands beside the document canvas on
 * Requirements and System Design. Detailed machinery signals and actions stay
 * on the canvas; callers may also supply a compact aggregate when the
 * governing document contract allows attention in navigation. The right edge
 * is a drag handle: pointer or arrow keys resize, double-click resets.
 */
export function DocumentTree({ children }: { children: ReactNode }) {
  const [width, setWidth] = useState(readStoredTreeWidth)
  const dragOrigin = useRef<{ x: number; width: number } | null>(null)

  const applyWidth = (next: number) => {
    const clamped = clampTreeWidth(next)
    setWidth(clamped)
    persistTreeWidth(clamped)
    return clamped
  }

  return (
    <>
      <nav
        aria-label="Document tree"
        style={{ width }}
        className="shrink-0 overflow-y-auto border-r border-border bg-surface/40 py-4"
      >
        {children}
      </nav>
      {/* biome-ignore lint/a11y/useSemanticElements: a native <hr> cannot carry the drag behavior */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize the document list"
        aria-valuenow={width}
        aria-valuemin={minTreeWidth}
        aria-valuemax={maxTreeWidth}
        tabIndex={0}
        title="Drag to resize · double-click to reset"
        className="-ml-[3px] w-[5px] shrink-0 cursor-col-resize transition-colors hover:bg-primary/30 focus-visible:bg-primary/40 focus-visible:outline-none active:bg-primary/40"
        onPointerDown={(event) => {
          event.preventDefault()
          event.currentTarget.setPointerCapture(event.pointerId)
          dragOrigin.current = { x: event.clientX, width }
        }}
        onPointerMove={(event) => {
          if (dragOrigin.current)
            setWidth(clampTreeWidth(dragOrigin.current.width + event.clientX - dragOrigin.current.x))
        }}
        onPointerUp={(event) => {
          event.currentTarget.releasePointerCapture(event.pointerId)
          dragOrigin.current = null
          persistTreeWidth(clampTreeWidth(width))
        }}
        onDoubleClick={() => applyWidth(defaultTreeWidth)}
        onKeyDown={(event) => {
          if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
          event.preventDefault()
          applyWidth(width + (event.key === 'ArrowRight' ? 16 : -16))
        }}
      />
    </>
  )
}

export function DocumentTreeGroup({
  label,
  children,
  collapsible = false,
  defaultOpen = true,
}: {
  label: string
  children: ReactNode
  collapsible?: boolean
  defaultOpen?: boolean
}) {
  if (collapsible)
    return (
      <details className="group mb-5 px-3 last:mb-0" open={defaultOpen || undefined}>
        <summary className="flex cursor-pointer list-none items-center gap-1 px-2 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
          <ChevronRight className="size-3 transition-transform group-open:rotate-90" />
          {label}
        </summary>
        <div className="space-y-0.5">{children}</div>
      </details>
    )
  return (
    <section className="mb-5 px-3 last:mb-0">
      <h2 className="px-2 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">{label}</h2>
      <div className="space-y-0.5">{children}</div>
    </section>
  )
}

export interface DocumentSortOption {
  value: string
  label: string
  /** The direction this option starts in when first chosen. */
  initialDirection: DocumentSortDirection
}

/**
 * One quiet field above a tree group: a search input with the sort control
 * riding its right edge. Choosing the active option again flips the
 * direction, so the whole ordering story lives in a single affordance.
 */
export function DocumentTreeToolbar({
  searchLabel,
  sortLabel,
  query,
  onQueryChange,
  options,
  sort,
  direction,
  onSortChange,
}: {
  searchLabel: string
  sortLabel: string
  query: string
  onQueryChange: (query: string) => void
  options: DocumentSortOption[]
  sort: string
  direction: DocumentSortDirection
  onSortChange: (sort: string, direction: DocumentSortDirection) => void
}) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  const active = options.find((option) => option.value === sort)
  const DirectionIcon = direction === 'ascending' ? ArrowUp : ArrowDown

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    document.addEventListener('pointerdown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('pointerdown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  return (
    <div className="mb-2 px-2">
      <div className="flex h-8 items-stretch rounded-md border border-edge bg-background transition-colors focus-within:border-primary">
        <label className="flex min-w-0 flex-1 items-center">
          <Search aria-hidden="true" className="ml-2.5 size-3 shrink-0 text-faint" />
          <input
            type="search"
            aria-label={searchLabel}
            placeholder={searchLabel}
            value={query}
            onChange={(event) => onQueryChange(event.target.value)}
            className="h-full min-w-0 flex-1 bg-transparent px-2 text-[11px] text-foreground outline-none placeholder:text-faint"
          />
        </label>
        <div ref={root} className="relative flex shrink-0">
          <button
            type="button"
            aria-label={sortLabel}
            aria-haspopup="menu"
            aria-expanded={open}
            title={`Sorted by ${active?.label.toLocaleLowerCase() ?? sort}, ${direction}`}
            onClick={() => setOpen((value) => !value)}
            className={cn(
              'flex items-center gap-1 rounded-r-[5px] border-l border-border px-2 text-[10px] font-medium transition-colors hover:bg-surface hover:text-foreground',
              open ? 'bg-surface text-foreground' : 'text-muted',
            )}
          >
            <DirectionIcon aria-hidden="true" className="size-3 text-faint" />
            {active?.label}
          </button>
          {open && (
            <div
              role="menu"
              aria-label={sortLabel}
              className="absolute right-0 top-full z-20 mt-1 w-44 rounded-md border border-border bg-card p-1 shadow-lg"
            >
              <p className="px-2 pb-1 pt-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
                Sort by
              </p>
              {options.map((option) => {
                const current = option.value === sort
                return (
                  <button
                    key={option.value}
                    type="button"
                    role="menuitemradio"
                    aria-checked={current}
                    onClick={() => {
                      // Re-choosing the active option flips its direction.
                      onSortChange(
                        option.value,
                        current ? (direction === 'ascending' ? 'descending' : 'ascending') : option.initialDirection,
                      )
                      setOpen(false)
                    }}
                    className={cn(
                      'flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-[11px] outline-none hover:bg-raised focus-visible:bg-raised',
                      current && 'font-medium text-primary',
                    )}
                  >
                    <span className="min-w-0 flex-1 truncate">{option.label}</span>
                    {current && <DirectionIcon aria-hidden="true" className="size-3" />}
                  </button>
                )
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

export function DocumentTreeItem({
  label,
  meta,
  title,
  tooltip,
  attentionCount,
  selected,
  onClick,
}: {
  label: string
  /** Quiet document identity — the confirmed version, never a signal. */
  meta?: string
  title?: string
  tooltip?: ReactNode
  /** Compact navigation signal; detailed attention remains on the canvas. */
  attentionCount?: number
  selected: boolean
  onClick: () => void
}) {
  return (
    <div className="group relative">
      <button
        type="button"
        title={title}
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
        {Boolean(attentionCount) && (
          <Badge
            variant="attention"
            aria-label={`${attentionCount} attention ${attentionCount === 1 ? 'item' : 'items'}`}
            className="shrink-0 px-1.5"
          >
            {attentionCount}
          </Badge>
        )}
      </button>
      {tooltip && (
        <span className="invisible absolute left-3 top-full z-20 mt-1 w-max max-w-80 rounded-md border border-border bg-background px-3 py-2 opacity-0 shadow-lg group-hover:visible group-hover:opacity-100 group-focus-within:visible group-focus-within:opacity-100">
          {tooltip}
        </span>
      )}
    </div>
  )
}

export function DocumentTreeNote({ children }: { children: ReactNode }) {
  return <p className="px-2 py-1.5 text-xs leading-5 text-muted">{children}</p>
}
