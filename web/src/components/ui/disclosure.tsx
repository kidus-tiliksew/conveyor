import { type ButtonHTMLAttributes, type ReactNode, useEffect, useId, useRef, useState } from 'react'
import { cn } from '../../lib/utils'

type TriggerProps = ButtonHTMLAttributes<HTMLButtonElement>

/** Pointer hover, keyboard focus and explicit tap share one disclosure state. */
export function Disclosure({
  children,
  content,
  className,
  triggerClassName,
  contentClassName,
  label,
  renderTrigger,
}: {
  children?: ReactNode
  content: ReactNode
  className?: string
  triggerClassName?: string
  contentClassName?: string
  label?: string
  renderTrigger?: (props: TriggerProps) => ReactNode
}) {
  const id = useId()
  const root = useRef<HTMLSpanElement>(null)
  const touch = useRef(false)
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const [pinned, setPinned] = useState(false)
  const open = Boolean(content) && (hovered || focused || pinned)
  const dismiss = () => {
    setHovered(false)
    setFocused(false)
    setPinned(false)
  }

  useEffect(() => {
    if (!open) return
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !root.current?.contains(event.target)) {
        setHovered(false)
        setFocused(false)
        setPinned(false)
      }
    }
    document.addEventListener('pointerdown', outside, true)
    return () => document.removeEventListener('pointerdown', outside, true)
  }, [open])

  const trigger: TriggerProps = {
    type: 'button',
    id: `${id}-trigger`,
    'aria-label': label,
    'aria-expanded': open,
    'aria-describedby': `${id}-content`,
    className: cn('inline-flex items-center text-left', triggerClassName),
    onClick: (event) => {
      event.stopPropagation()
      if (pinned) dismiss()
      else setPinned(true)
    },
  }
  return (
    <span
      ref={root}
      className={cn('relative inline-flex min-w-0', className)}
      onPointerDownCapture={(event) => {
        touch.current = event.pointerType === 'touch'
      }}
      onPointerEnter={(event) => {
        if (event.pointerType !== 'touch') setHovered(true)
      }}
      onPointerLeave={() => setHovered(false)}
      onFocus={() => {
        if (!touch.current) setFocused(true)
      }}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) {
          setFocused(false)
          touch.current = false
        }
      }}
      onKeyDownCapture={(event) => {
        touch.current = false
        if (event.key === 'Escape' && open) {
          event.stopPropagation()
          event.preventDefault()
          dismiss()
        }
      }}
    >
      {renderTrigger ? renderTrigger(trigger) : <button {...trigger}>{children}</button>}
      <span
        id={`${id}-content`}
        role="tooltip"
        className={cn(
          'absolute left-0 top-full z-20 w-max max-w-[min(20rem,80vw)] rounded-md border border-border bg-background px-3 py-2 text-xs font-normal text-foreground shadow-lg',
          contentClassName,
          !open && 'invisible pointer-events-none opacity-0',
        )}
      >
        {content}
      </span>
    </span>
  )
}
