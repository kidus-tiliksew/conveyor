import type { KeyboardEvent, ReactNode } from 'react'
import { cn } from '../../lib/utils'

export type TabItem<T extends string> = {
  id: T
  label: ReactNode
  attributes?: Record<`data-${string}`, string>
}

const tabElementId = (base: string, value: string) => `${base}-${value}-tab`
const panelElementId = (base: string, value: string) => `${base}-${value}-panel`

// Spread on the one mounted panel, so it and the selected tab name each other.
export function tabPanelProps(base: string, value: string) {
  return {
    role: 'tabpanel' as const,
    id: panelElementId(base, value),
    'aria-labelledby': tabElementId(base, value),
  }
}

// The one underlined tab row. Only the selected tab is in the page's tab order;
// arrow keys, Home and End move between tabs and select as they go. `onChange`
// may return a promise when selecting navigates, and focus follows once it
// settles. The baseline is an inset shadow, not a border, because a row that
// scrolls would clip an underline overlapping its border.
export function Tabs<T extends string>({
  id,
  items,
  value,
  onChange,
  'aria-label': ariaLabel,
  className,
}: {
  id: string
  items: TabItem<T>[]
  value: T
  onChange: (next: T) => void | Promise<void>
  'aria-label': string
  className?: string
}) {
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.altKey || event.ctrlKey || event.metaKey) return
    const index = items.findIndex((item) => item.id === value)
    const target = {
      ArrowRight: items[(index + 1) % items.length],
      ArrowLeft: items[(index - 1 + items.length) % items.length],
      Home: items[0],
      End: items[items.length - 1],
    }[event.key]
    if (!target) return
    event.preventDefault()
    // Looked up afterwards, not kept: a navigating change can replace the whole
    // row, with new ids, before it settles.
    void Promise.resolve(onChange(target.id)).then(() =>
      (
        document.getElementById(tabElementId(id, target.id)) ??
        document.querySelector<HTMLElement>(
          `[role=tablist][aria-label="${CSS.escape(ariaLabel)}"] [role=tab][aria-selected=true]`,
        )
      )?.focus(),
    )
  }
  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className={cn(
        'flex gap-1 overflow-x-auto shadow-[inset_0_-1px_0_var(--color-border)] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        className,
      )}
    >
      {items.map((item) => {
        const selected = item.id === value
        return (
          <button
            key={item.id}
            type="button"
            role="tab"
            id={tabElementId(id, item.id)}
            aria-selected={selected}
            aria-controls={selected ? panelElementId(id, item.id) : undefined}
            tabIndex={selected ? 0 : -1}
            onClick={() => onChange(item.id)}
            className={cn(
              'inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors pointer-coarse:min-h-11 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary [&_svg]:size-3.5',
              selected ? 'border-primary text-foreground' : 'border-transparent text-muted hover:text-foreground',
            )}
            {...item.attributes}
          >
            {item.label}
          </button>
        )
      })}
    </div>
  )
}
