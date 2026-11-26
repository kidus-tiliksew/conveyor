import { Radio, RadioTower } from 'lucide-react'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'

// Whether a watcher is listening for an environment, as the server serves it
// (component-production-feedback invariants 13, 21, and 22;
// req-production-feedback AC-2.2, AC-2.3). The dashboard computes nothing:
// it prints the served `listening` flag and the served last signal time.
export function WatcherState({
  listening,
  lastSignalAt,
  className,
}: {
  listening: boolean
  lastSignalAt: string | null | undefined
  className?: string
}) {
  const Icon = listening ? RadioTower : Radio
  return (
    <span
      data-watcher-state={listening ? 'listening' : 'not-listening'}
      className={cn('inline-flex min-w-0 flex-wrap items-baseline gap-x-1.5', className)}
    >
      <Icon aria-hidden className={cn('size-3 shrink-0 self-center', listening ? 'text-positive' : 'text-faint')} />
      <span className={listening ? 'text-foreground' : 'text-attention'}>
        {listening ? 'A watcher is listening.' : 'No watcher is listening.'}
      </span>
      {/* A real space, so the two sentences read apart in text and to a
          screen reader. The flex gap only spaces them on screen. */}{' '}
      <span className="text-muted">
        {lastSignalAt ? (
          <>
            Last signal{' '}
            <time dateTime={lastSignalAt} title={absoluteTime(lastSignalAt)}>
              {relativeTime(lastSignalAt)}
            </time>
            .
          </>
        ) : (
          'No signal has arrived.'
        )}
      </span>
    </span>
  )
}
