import { useEffect, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

// Open sheets, innermost last. A sheet can now open over another one — the
// related-records panel opens from inside the task panel (AC-3.1) — and only
// the topmost may answer Escape. Without this, one keypress would dismiss the
// panel the operator opened *and* the surface underneath it.
const openSheets: symbol[] = []

// Right-hand overlay sheet: dialog semantics, Escape/overlay close, body
// scroll lock, and focus capture/restore. Hand-rolled to match the
// dependency-free component idiom — consumers own the header and content.
export function Sheet({
  onClose,
  label,
  // A sheet is half the viewport because task detail needs the room. A panel
  // that is only a list of links does not, so it can ask to stay narrow.
  width = 'md:w-1/2',
  children,
}: {
  onClose: () => void
  label: string
  width?: string
  children: ReactNode
}) {
  const panelRef = useRef<HTMLElement>(null)
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    const previous = document.activeElement
    panelRef.current?.focus()
    const identity = Symbol('sheet')
    openSheets.push(identity)
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && openSheets.at(-1) === identity) onCloseRef.current()
    }
    document.addEventListener('keydown', onKeyDown)
    const overflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      openSheets.splice(openSheets.indexOf(identity), 1)
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = overflow
      if (previous instanceof HTMLElement) previous.focus()
    }
  }, [])

  return createPortal(
    <div className="fixed inset-0 z-40">
      <div
        aria-hidden
        className="absolute inset-0 animate-overlay-in bg-foreground/25"
        onClick={() => onCloseRef.current()}
      />
      <aside
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        tabIndex={-1}
        className={`absolute inset-y-0 right-0 flex w-full animate-sheet-in flex-col border-l border-border bg-background shadow-xl outline-none ${width}`}
      >
        {children}
      </aside>
    </div>,
    document.body,
  )
}
