import { Link } from '@tanstack/react-router'
import type { TaskContext } from '../../lib/types'

// The task's pinned authority: the confirmed outcomes and technical guidance
// the factory serves to this task's sessions. Rows, not prose — the link is
// the point and the version pin is the caveat.
export function TaskContextCard({ context }: { context?: TaskContext }) {
  const requirements = context?.requirements ?? []
  const designs = context?.designs ?? []
  if (requirements.length === 0 && designs.length === 0) return null
  return (
    <section aria-label="Attached context" className="rounded-md border border-border bg-card">
      <h3 className="border-b border-border px-4 py-2.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
        Attached context
      </h3>
      <ul className="px-4 py-2">
        {requirements.map((item) => (
          <ContextRow key={item.id} kind="Outcome" meta={`${item.id} · v${item.version}`}>
            <Link
              to="/requirements"
              search={{ requirement: item.id }}
              className="truncate text-primary hover:underline"
            >
              {item.title}
            </Link>
          </ContextRow>
        ))}
        {designs.map((item) => (
          <ContextRow key={item.id} kind="Guidance" meta={`${item.id} · v${item.version}`}>
            <Link to="/system-design" search={{ document: item.id }} className="truncate text-primary hover:underline">
              {item.title}
            </Link>
          </ContextRow>
        ))}
      </ul>
    </section>
  )
}

function ContextRow({ kind, meta, children }: { kind: string; meta: string; children: React.ReactNode }) {
  return (
    <li className="flex items-baseline gap-3 py-1.5 text-sm">
      <span className="w-16 shrink-0 text-[10px] font-medium uppercase tracking-wider text-faint">{kind}</span>
      {children}
      <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">{meta}</span>
    </li>
  )
}
