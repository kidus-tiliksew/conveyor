import type { ReactNode } from 'react'
import { cn } from '../../lib/utils'

// The one header every workspace page opens with, so moving between pages
// never shifts the title, its line of context, or the page's own actions.
export function PageHeader({
  title,
  meta,
  description,
  className,
  children,
}: {
  title: ReactNode
  meta?: ReactNode
  description?: ReactNode
  className?: string
  children?: ReactNode
}) {
  return (
    <header
      className={cn(
        'flex min-h-14 shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-2.5 lg:px-6',
        className,
      )}
    >
      <div className="min-w-0 flex-[1_1_7rem]">
        <div className="flex items-center gap-2">
          <h1 className="truncate text-[15px] font-semibold tracking-tight">{title}</h1>
          {meta}
        </div>
        {description && <div className="mt-px text-xs text-muted">{description}</div>}
      </div>
      {children && <div className="ml-auto flex flex-wrap items-center gap-2">{children}</div>}
    </header>
  )
}
