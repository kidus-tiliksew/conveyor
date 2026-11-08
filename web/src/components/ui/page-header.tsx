import type { ReactNode } from 'react'
import { cn } from '../../lib/utils'

// The one header every workspace page opens with: same height, same title
// size, same left edge, so moving between pages never shifts the frame.
export function PageHeader({
  title,
  description,
  badge,
  children,
  className,
}: {
  title: ReactNode
  description?: ReactNode
  badge?: ReactNode
  children?: ReactNode
  className?: string
}) {
  return (
    <header
      className={cn(
        'flex min-h-[52px] shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-2.5 lg:px-6',
        className,
      )}
    >
      <div className="flex min-w-0 items-baseline gap-2.5">
        <h1 className="text-base font-semibold tracking-tight">{title}</h1>
        {badge}
        {description && <p className="hidden min-w-0 truncate text-sm text-faint xl:block">{description}</p>}
      </div>
      {children && <div className="ml-auto flex flex-wrap items-center gap-2">{children}</div>}
    </header>
  )
}
