import { useEffect, useRef, useState, type ReactNode } from 'react'
import { ChevronDown } from 'lucide-react'
import { Button } from './button'
import { cn } from '../../lib/utils'

export function DropdownMenu({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)

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

  return <div ref={root} className={cn('relative', className)}>
    <Button size="sm" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen((value) => !value)}>
      {label}<ChevronDown />
    </Button>
    {open && <div
	  role="menu"
	  aria-label={label}
	  className="absolute right-0 z-20 mt-1 min-w-64 overflow-hidden rounded-md border border-border bg-card p-1 shadow-lg"
	  onClick={() => setOpen(false)}
	  onKeyDown={(event) => { if (event.key === 'Enter' || event.key === ' ') setOpen(false) }}
	>{children}</div>}
  </div>
}

export function DropdownMenuItem({ onSelect, children }: { onSelect: () => void; children: ReactNode }) {
  return <button type="button" role="menuitem" className="flex w-full flex-col items-start rounded-sm px-2.5 py-2 text-left outline-none hover:bg-raised focus-visible:bg-raised" onClick={onSelect}>
    {children}
  </button>
}
