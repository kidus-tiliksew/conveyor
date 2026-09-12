import { createContext, useContext, useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { ChevronDown, Ellipsis } from 'lucide-react'
import { Button } from './button'
import { cn } from '../../lib/utils'

const MenuSelectionContext = createContext(() => {})

/**
 * `trigger` picks the button that opens the menu. `label` renders the menu's
 * name as button text with a chevron — a menu that is the page's main call to
 * action. `overflow` renders the quiet ellipsis icon button instead, with the
 * name carried by aria-label and title: the convention for a set of secondary
 * actions that should never outweigh the content beside it.
 */
export function DropdownMenu({
  label,
  trigger = 'label',
  children,
  className,
}: {
  label: string
  trigger?: 'label' | 'overflow'
  children: ReactNode
  className?: string
}) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  const menu = useRef<HTMLDivElement>(null)
  const initialFocus = useRef<'first' | 'last'>('first')
  const menuID = useId()
  const close = () => {
    setOpen(false)
    root.current?.querySelector<HTMLButtonElement>(':scope > button')?.focus()
  }

  useEffect(() => {
    if (!open) return
    const items = menu.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')
    items?.[initialFocus.current === 'last' ? items.length - 1 : 0]?.focus()
  }, [open])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.stopImmediatePropagation()
        close()
      }
    }
    document.addEventListener('pointerdown', onPointerDown)
    document.addEventListener('keydown', onKeyDown, true)
    return () => {
      document.removeEventListener('pointerdown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown, true)
    }
  }, [open])

  return (
    <div ref={root} className={cn('relative', className)}>
      <Button
        variant={trigger === 'overflow' ? 'ghost' : 'default'}
        size={trigger === 'overflow' ? 'icon' : 'sm'}
        aria-label={trigger === 'overflow' ? label : undefined}
        title={trigger === 'overflow' ? label : undefined}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuID : undefined}
        onClick={() => {
          initialFocus.current = 'first'
          setOpen((value) => !value)
        }}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault()
            initialFocus.current = event.key === 'ArrowUp' ? 'last' : 'first'
            setOpen(true)
          }
        }}
      >
        {trigger === 'overflow' ? (
          <Ellipsis aria-hidden="true" />
        ) : (
          <>
            {label}
            <ChevronDown />
          </>
        )}
      </Button>
      {open && (
        <div
          ref={menu}
          id={menuID}
          role="menu"
          aria-label={label}
          className="absolute right-0 z-20 mt-1 min-w-64 overflow-hidden rounded-md border border-border bg-card p-1 shadow-lg"
          onKeyDown={(event) => {
            const items = Array.from(menu.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? [])
            const index = items.indexOf(document.activeElement as HTMLButtonElement)
            if (event.key === 'Tab') {
              close()
              return
            }
            if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key) || !items.length) return
            event.preventDefault()
            const next =
              event.key === 'Home'
                ? 0
                : event.key === 'End'
                  ? items.length - 1
                  : (index + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length
            items[next]?.focus()
          }}
        >
          <MenuSelectionContext.Provider value={close}>{children}</MenuSelectionContext.Provider>
        </div>
      )}
    </div>
  )
}

export function DropdownMenuItem({
  onSelect,
  children,
  destructive = false,
}: {
  onSelect: () => void
  children: ReactNode
  destructive?: boolean
}) {
  const close = useContext(MenuSelectionContext)
  return (
    <button
      type="button"
      role="menuitem"
      tabIndex={-1}
      className={cn(
        'flex w-full items-center gap-2 rounded-sm px-2.5 py-2 text-left text-sm outline-none hover:bg-raised focus-visible:bg-raised [&_svg]:size-4',
        destructive && 'text-failure hover:bg-failure-soft focus-visible:bg-failure-soft',
      )}
      onClick={() => {
        // Close and select in the same handler so unmounting cannot discard selection.
        close()
        onSelect()
      }}
    >
      {children}
    </button>
  )
}
