import { CheckCircle2, FileText, LoaderCircle, TriangleAlert } from 'lucide-react'
import { forwardRef, type HTMLAttributes, type ReactNode } from 'react'
import { cn } from '../../lib/utils'

export const MessageScroller = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(function MessageScroller(
  { className, ...props },
  ref,
) {
  return <div ref={ref} className={cn('min-h-0 flex-1 overflow-y-auto', className)} {...props} />
})

export function Message({
  from,
  className,
  ...props
}: HTMLAttributes<HTMLDivElement> & { from: 'assistant' | 'user' | 'system' }) {
  return (
    <div
      className={cn(
        'flex gap-3',
        from === 'user' && 'flex-row-reverse',
        from === 'system' && 'justify-center',
        className,
      )}
      {...props}
    />
  )
}

export function Bubble({
  from,
  className,
  ...props
}: HTMLAttributes<HTMLDivElement> & { from: 'assistant' | 'user' | 'system' }) {
  return (
    <div
      className={cn(
        'rounded-xl px-3.5 py-2.5 text-sm leading-[1.6]',
        from === 'assistant' && 'rounded-tl-sm bg-surface',
        from === 'user' && 'ml-auto w-fit whitespace-pre-wrap rounded-tr-sm bg-raised text-foreground',
        from === 'system' && 'max-w-[82%] whitespace-pre-wrap border border-border text-muted',
        className,
      )}
      {...props}
    />
  )
}

export function Attachment({ name, contentType }: { name: string; contentType?: string }) {
  return (
    <span className="inline-flex max-w-full items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs text-muted">
      <FileText className="size-3.5 shrink-0 text-primary" aria-hidden="true" />
      <span className="truncate">{name}</span>
      {contentType && <span className="sr-only"> ({contentType})</span>}
    </span>
  )
}

export function Marker({
  name,
  state,
  children,
}: {
  name: string
  state: 'pending' | 'complete' | 'corrected' | 'deferred' | 'cancelled' | 'failed'
  children?: ReactNode
}) {
  const stateLabel = state === 'pending' ? 'in progress' : state
  return (
    <span
      role="img"
      aria-label={`${name}: ${stateLabel}`}
      className="inline-flex max-w-full items-center gap-2 rounded-full border border-border bg-surface px-2.5 py-1 text-[11px] text-muted"
    >
      {state === 'pending' && <LoaderCircle className="size-3 animate-spin text-primary" aria-hidden="true" />}
      {state === 'complete' && <CheckCircle2 className="size-3 text-positive" aria-hidden="true" />}
      {state === 'corrected' && <TriangleAlert className="size-3 text-attention" aria-hidden="true" />}
      {state === 'deferred' && <LoaderCircle className="size-3 text-muted" aria-hidden="true" />}
      {state === 'cancelled' && <TriangleAlert className="size-3 text-muted" aria-hidden="true" />}
      {state === 'failed' && <TriangleAlert className="size-3 text-failure" aria-hidden="true" />}
      <span className="truncate">
        {name} {stateLabel}
      </span>
      {children}
    </span>
  )
}
