import { type ReactNode, useId } from 'react'

// One group of the Settings page: a title and a one-sentence description
// beside, or above, one bordered container of rows (component-web-dashboard,
// Settings). From 1024 px wide the title sits in a left column.
export function SettingsGroup({
  title,
  description,
  children,
}: {
  title: ReactNode
  description: ReactNode
  children: ReactNode
}) {
  const id = useId()
  return (
    <section aria-labelledby={`${id}-title`} className="lg:grid lg:grid-cols-[14rem_minmax(0,1fr)] lg:gap-8">
      <div className="mb-3 space-y-1 lg:mb-0 lg:pt-3">
        <h2 id={`${id}-title`} className="text-sm font-semibold text-foreground">
          {title}
        </h2>
        <p className="text-sm leading-6 text-muted">{description}</p>
      </div>
      <div className="min-w-0 divide-y divide-border rounded-lg border border-border bg-card">{children}</div>
    </section>
  )
}
