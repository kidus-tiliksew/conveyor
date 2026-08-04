import { useEffect, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { cn } from '../../lib/utils'

// Centered modal dialog: same dependency-free behavior contract as Sheet
// (portal, Escape/overlay close, body scroll lock, focus capture/restore).
// Consumers own the header and content; className widens the default panel.
export function Dialog({
  onClose,
  label,
  className,
  children,
}: {
  onClose: () => void
  label: string
  className?: string
  children: ReactNode
}) {
  const panelRef = useRef<HTMLDivElement>(null)
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    const previous = document.activeElement
    panelRef.current?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onCloseRef.current()
    }
    document.addEventListener('keydown', onKeyDown)
    const overflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = overflow
      if (previous instanceof HTMLElement) previous.focus()
    }
  }, [])

  return createPortal(
    <div className="fixed inset-0 z-40 grid place-items-center p-4">
      <div
        aria-hidden
        className="absolute inset-0 animate-overlay-in bg-foreground/40"
        onClick={() => onCloseRef.current()}
      />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        tabIndex={-1}
        className={cn(
          'relative max-h-[85vh] w-full max-w-lg animate-dialog-in overflow-y-auto rounded-lg border border-border bg-background shadow-xl outline-none',
          className,
        )}
      >
        {children}
      </div>
    </div>,
    document.body,
  )
}
