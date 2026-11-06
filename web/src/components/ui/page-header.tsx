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
        'flex min-h-12 shrink-0 flex-wrap items-center gap-x-4 gap-y-1.5 border-b border-border px-4 py-1.5 lg:h-14 lg:flex-nowrap lg:px-6 lg:py-0',
        className,
      )}
    >
      <div className="min-w-0 flex-[1_1_7rem]">
        <div className="flex items-center gap-2">
          <h1 className="truncate text-[15px] font-semibold leading-5 tracking-tight">{title}</h1>
          {meta}
        </div>
        {description && (
          <div className="truncate text-xs leading-4 text-muted max-lg:whitespace-normal">{description}</div>
        )}
      </div>
      {children && <div className="ml-auto flex flex-wrap items-center gap-2">{children}</div>}
    </header>
  )
}
