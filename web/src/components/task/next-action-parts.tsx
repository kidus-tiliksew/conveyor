import { relativeTime } from '../../lib/utils'

// The deadline a next action states, absolute and relative. A next action
// without one says nothing rather than inventing a horizon.
export function NextActionDeadline({ deadline }: { deadline: string | null | undefined }) {
  if (!deadline) return null
  return (
    <p className="text-xs text-muted" data-next-action-deadline>
      Deadline <time dateTime={deadline}>{new Date(deadline).toLocaleString()}</time> ({relativeTime(deadline)})
    </p>
  )
}

// The server's identifiers for a next action, verbatim and collapsed: an
// operator who needs an order or reason code can read it without the card
// leading with machine vocabulary.
export function NextActionIDs({ value }: { value: unknown }) {
  return (
    <details className="text-xs text-muted">
      <summary className="cursor-pointer select-none">Show IDs</summary>
      <pre className="mt-1.5 max-w-full overflow-x-auto rounded-md bg-surface p-2 font-mono text-[11px] text-foreground/80">
        {JSON.stringify(value, null, 2)}
      </pre>
    </details>
  )
}
