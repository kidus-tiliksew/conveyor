import { ChevronDown, ChevronUp } from 'lucide-react'
import { type ReactNode, useId, useLayoutEffect, useRef, useState } from 'react'
import { cn } from '../../lib/utils'

// A past report at a bounded height, with the shared bottom fade and a toggle
// shown only when the text overflows. The full text stays in the document.
export function ClampedBlock({
  children,
  clamp = true,
  maxHeightClassName = 'max-h-[3.75rem]',
  toggleClassName,
}: {
  children: ReactNode
  clamp?: boolean
  maxHeightClassName?: string
  toggleClassName?: string
}) {
  const [expanded, setExpanded] = useState(false)
  const [hasOverflow, setHasOverflow] = useState(false)
  const viewportRef = useRef<HTMLDivElement>(null)
  const viewportID = useId()

  useLayoutEffect(() => {
    const viewport = viewportRef.current
    if (!viewport || expanded || !clamp) {
      if (!clamp) setHasOverflow(false)
      return
    }
    const measure = () => setHasOverflow(viewport.scrollHeight > viewport.clientHeight + 1)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    if (viewport.firstElementChild) observer.observe(viewport.firstElementChild)
    return () => observer.disconnect()
  }, [clamp, expanded])

  return (
    <div>
      <div className="relative">
        <div
          id={viewportID}
          ref={viewportRef}
          className={cn(clamp && !expanded && `${maxHeightClassName} overflow-hidden`)}
        >
          <div>{children}</div>
        </div>
        {clamp && hasOverflow && !expanded && (
          <div
            aria-hidden="true"
            className="spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0 h-6"
          />
        )}
      </div>
      {hasOverflow && (
        <button
          type="button"
          aria-controls={viewportID}
          aria-expanded={expanded}
          className={cn(
            'mt-1 inline-flex items-center gap-1 text-[11px] font-medium text-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
            toggleClassName,
          )}
          onClick={() => setExpanded((value) => !value)}
        >
          {expanded ? (
            <ChevronUp aria-hidden="true" className="size-3" />
          ) : (
            <ChevronDown aria-hidden="true" className="size-3" />
          )}
          {expanded ? 'Show less' : 'Show full report'}
        </button>
      )}
    </div>
  )
}
