import { type ReactNode, useEffect, useState } from 'react'
import { duration } from '../../lib/utils'
import { Disclosure } from '../ui/disclosure'

// A timeline card that stands for work with a start and an end shows its
// runtime on the card face. Its clock times sit behind the runtime in a
// disclosure that hover, tap, keyboard focus, and the accessible name all
// reach (DEC-127(1); component-web-task-surfaces invariants 40 to 43 and 70).

// The current time, kept fresh once a second while `live` is true. Each
// runtime owns its own timer, so a tick renders that runtime alone: never the
// timeline, never a card around it, and never a finished runtime, which starts
// no timer. The effect clears the timer when the span closes or the runtime
// unmounts. A tick reads only the browser clock and sends no request.
export function useLiveNow(live: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!live) return
    setNow(Date.now())
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [live])
  return now
}

// The runtime of a span. A recorded end gives the measured duration. An open
// span measures to the current time. A span with no recorded end that is not
// open has no runtime: the card never invents a finish.
export function spanRuntime(start: string, end: string | undefined, open: boolean, now: number) {
  if (end) return duration(start, end)
  if (open) return duration(start, new Date(now).toISOString())
  return undefined
}

export type RuntimeView = {
  // What the closed card face shows.
  face: ReactNode
  // The trigger's accessible name. It states the runtime first.
  label: string
  // The clock times behind the runtime.
  details: ReactNode
}

export function RuntimeDisclosure({
  start,
  end,
  open,
  render,
  className,
  triggerClassName,
  contentClassName,
  trigger,
}: {
  start: string
  end?: string
  // The span is still open, so its runtime advances once a second.
  open: boolean
  render: (runtime: string | undefined) => RuntimeView
  className?: string
  triggerClassName?: string
  contentClassName?: string
  // Data attributes for the trigger button.
  trigger?: Record<`data-${string}`, string>
}) {
  const live = open && !end
  const now = useLiveNow(live)
  const view = render(spanRuntime(start, end, live, now))
  return (
    <Disclosure
      className={className}
      contentClassName={contentClassName}
      triggerClassName={triggerClassName}
      label={view.label}
      content={view.details}
      renderTrigger={(props) => (
        <button {...props} {...trigger} data-runtime-live={live ? '' : undefined}>
          {view.face}
        </button>
      )}
    />
  )
}
