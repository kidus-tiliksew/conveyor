import type { ReactNode } from 'react'
import { cn } from '../../lib/utils'

// A choice of how to view something, never a decision: the selected segment
// lifts quietly, so it can't be mistaken for a primary action beside it. It
// matches the Inbox band switch.
export function SegmentedControl<T extends string>({
  label,
  options,
  value,
  onChange,
  className,
}: {
  label: string
  options: { value: T; label: ReactNode; count?: number }[]
  value: T
  onChange: (next: T) => void
  className?: string
}) {
  return (
    <fieldset
      aria-label={label}
      className={cn(
        'm-0 inline-flex min-w-0 max-w-full gap-0.5 overflow-x-auto rounded-lg border-0 bg-surface p-0.5 text-xs [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        className,
      )}
    >
      {options.map((option) => {
        const selected = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={selected}
            onClick={() => onChange(option.value)}
            className={cn(
              'inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-md px-2 py-1 font-medium transition-colors pointer-coarse:min-h-10 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary',
              selected ? 'bg-card text-foreground shadow-sm' : 'text-muted hover:text-foreground',
            )}
          >
            {option.label}
            {option.count !== undefined && (
              <span className={cn('tabular-nums', selected ? 'text-muted' : 'text-faint')}>{option.count}</span>
            )}
          </button>
        )
      })}
    </fieldset>
  )
}
