import type { ReactNode } from 'react'
import { cn } from '../../lib/utils'

// The one heading voice on task detail (component-web-task-surfaces, Task
// detail composition): sentence case at one size and weight, followed by a
// count where a list follows. Scoped to task detail so other surfaces keep
// their own CardTitle.
export function SectionHeading({
  children,
  count,
  as: Tag = 'h3',
  className,
}: {
  children: ReactNode
  count?: number | string
  as?: 'h2' | 'h3' | 'h4' | 'p' | 'span'
  className?: string
}) {
  return (
    <Tag className={cn('flex min-w-0 items-center gap-2 text-sm font-semibold text-foreground', className)}>
      {children}
      {count != null && <SectionCount>{count}</SectionCount>}
    </Tag>
  )
}

export function SectionCount({ children }: { children: ReactNode }) {
  return (
    <span className="rounded-full bg-raised px-1.5 text-[11px] font-normal leading-4 text-faint tabular-nums">
      {children}
    </span>
  )
}

// A row's kind ("Outcome", "Guidance"): small, muted, sentence case.
export function KindLabel({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn('shrink-0 text-[11px] text-faint', className)}>{children}</span>
}
