import { type ButtonHTMLAttributes, type ReactNode, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { cn } from '../../lib/utils'

const openDisclosures: symbol[] = []

type TriggerProps = ButtonHTMLAttributes<HTMLButtonElement>

/** Pointer hover, keyboard focus and explicit tap share one disclosure state. */
export function Disclosure({
  children,
  content,
  className,
  triggerClassName,
  contentClassName,
  label,
  title,
  renderTrigger,
}: {
  children?: ReactNode
  content: ReactNode
  className?: string
  triggerClassName?: string
  contentClassName?: string
  label?: string
  title?: string
  renderTrigger?: (props: TriggerProps) => ReactNode
}) {
  const id = useId()
  const root = useRef<HTMLSpanElement>(null)
  const tooltip = useRef<HTMLSpanElement>(null)
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

  // Keep the caller's placement unless it would put details outside the viewport.
  useLayoutEffect(() => {
    if (!open) return
    const position = () => {
      const element = tooltip.current
      if (!element) return
      element.style.transform = ''
      const bounds = element.getBoundingClientRect()
      const shift = Math.max(8 - bounds.left, Math.min(0, window.innerWidth - 8 - bounds.right))
      if (shift) element.style.transform = `translateX(${shift}px)`
    }
    position()
    window.addEventListener('resize', position)
    document.addEventListener('scroll', position, true)
    return () => {
      window.removeEventListener('resize', position)
      document.removeEventListener('scroll', position, true)
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !root.current?.contains(event.target)) {
        setHovered(false)
        setFocused(false)
        setPinned(false)
      }
    }
    const identity = Symbol('disclosure')
    openDisclosures.push(identity)
    const onEscape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || openDisclosures.at(-1) !== identity) return
      event.preventDefault()
      event.stopPropagation()
      setHovered(false)
      setFocused(false)
      setPinned(false)
    }
    document.addEventListener('pointerdown', outside, true)
    document.addEventListener('keydown', onEscape, true)
    return () => {
      openDisclosures.splice(openDisclosures.indexOf(identity), 1)
      document.removeEventListener('pointerdown', outside, true)
      document.removeEventListener('keydown', onEscape, true)
    }
  }, [open])

  const trigger: TriggerProps = {
    type: 'button',
    id: `${id}-trigger`,
    title,
    'aria-label': label,
    'aria-expanded': open,
    'aria-describedby': `${id}-content`,
    className: cn(
      'inline-flex min-w-0 items-center text-left pointer-coarse:min-h-10 pointer-coarse:min-w-10',
      triggerClassName,
    ),
    onClick: (event) => {
      event.preventDefault()
      event.stopPropagation()
      if (pinned) dismiss()
      else setPinned(true)
    },
  }
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: This wrapper tracks focus and hover across the native button and its linked content.
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
    >
      {renderTrigger ? renderTrigger(trigger) : <button {...trigger}>{children}</button>}
      <span
        ref={tooltip}
        id={`${id}-content`}
        role="tooltip"
        className={cn(
          'absolute left-0 top-full z-20 w-max max-w-[min(20rem,80vw)] whitespace-normal break-words rounded-md border border-border bg-background px-3 py-2 text-xs font-normal text-foreground shadow-lg',
          contentClassName,
          !open && 'invisible pointer-events-none opacity-0',
        )}
      >
        {open ? content : null}
      </span>
    </span>
  )
}
