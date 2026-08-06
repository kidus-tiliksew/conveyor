import { Link } from '@tanstack/react-router'
import type { TaskContext } from '../../lib/types'

export function TaskContextCard({ context }: { context?: TaskContext }) {
  const requirements = context?.requirements ?? []
  const designs = context?.designs ?? []
  if (requirements.length === 0 && designs.length === 0) return null
  return (
    <section aria-label="Attached context" className="rounded-lg border border-border bg-surface p-3">
      <h3 className="text-sm font-medium">Attached context</h3>
      {requirements.length > 0 && (
        <div className="mt-2">
          <p className="text-[11px] font-medium uppercase tracking-wider text-muted">Product outcomes</p>
          <ul className="mt-1 space-y-1 text-sm">
            {requirements.map((item) => (
              <li key={item.id}>
                <Link to="/requirements" search={{ requirement: item.id }} className="text-primary hover:underline">
                  {item.title}
                </Link>{' '}
                <span className="font-mono text-xs text-faint">
                  {item.id} · v{item.version}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {designs.length > 0 && (
        <div className="mt-2">
          <p className="text-[11px] font-medium uppercase tracking-wider text-muted">Technical guidance</p>
          <ul className="mt-1 space-y-1 text-sm">
            {designs.map((item) => (
              <li key={item.id}>
                <Link to="/system-design" search={{ document: item.id }} className="text-primary hover:underline">
                  {item.title}
                </Link>{' '}
                <span className="font-mono text-xs text-faint">
                  {item.id} · v{item.version}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}
