import { cn } from '../../lib/utils'

// Budget meter: accent while healthy, amber only once the breaker threshold
// is in sight — amber is reserved for attention states (spec §13.3).
export function Progress({ value, className }: { value: number; className?: string }) {
  const clamped = Math.min(100, Math.max(0, value))
  return (
    <div className={cn('h-1.5 w-full overflow-hidden rounded-full bg-raised', className)} role="progressbar" aria-valuenow={Math.round(clamped)} aria-valuemin={0} aria-valuemax={100}>
      <div
        className={cn('h-full rounded-full transition-[width] duration-500', clamped >= 90 ? 'bg-attention-dot' : 'bg-primary')}
        style={{ width: `${clamped}%` }}
      />
    </div>
  )
}
