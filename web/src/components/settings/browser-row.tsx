import type { LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'

// A row of the This browser group. It has no open state: its control acts
// directly in the row.
export function BrowserRow({
  icon: Icon,
  title,
  description,
  control,
}: {
  icon: LucideIcon
  title: string
  description: ReactNode
  control: ReactNode
}) {
  return (
    <div className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-3 px-4 py-3">
      <Icon className="mt-0.5 size-4 shrink-0 text-faint" aria-hidden="true" />
      <div className="min-w-0 flex-1 basis-48 space-y-1 text-sm">
        <p className="font-medium text-foreground">{title}</p>
        <p className="leading-6 text-muted">{description}</p>
      </div>
      <div className="ml-7 shrink-0 sm:ml-0">{control}</div>
    </div>
  )
}
