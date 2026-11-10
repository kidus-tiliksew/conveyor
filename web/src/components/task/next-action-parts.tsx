import { useState } from 'react'
import type { NextAction } from '../../lib/types'
import { cn, fullTime, relativeTime } from '../../lib/utils'

// The deadline a next action states, absolute and relative. A next action
// without one says nothing rather than inventing a horizon.
export function NextActionDeadline({ deadline }: { deadline: string | null | undefined }) {
  if (!deadline) return null
  return (
    <p className="text-xs text-muted" data-next-action-deadline>
      Deadline <time dateTime={deadline}>{fullTime(deadline)}</time> ({relativeTime(deadline)})
    </p>
  )
}

// The server's identifiers for a next action, collapsed and secondary to the
// card's sentence (req-operator-next-action AC-1.8, AC-7.1): an operator who
// needs an order or reason code reads it here as a labelled list, never as a
// raw object. `current_order` contributes its ID and stage; every other field,
// and each field of `details`, prints under its own label with nested values
// flattened to text. The list mounts only while the disclosure is open, so a
// detail that repeats the card's sentence is never read twice.
export function NextActionIDs({ value }: { value: Record<string, unknown> }) {
  const [open, setOpen] = useState(false)
  if (identifierRows(value).length === 0) return null
  return (
    <details
      className="text-xs text-muted"
      data-next-action-ids
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
      <summary className="cursor-pointer select-none">Show IDs</summary>
      {open && <NextActionIDList value={value} className="mt-1.5" />}
    </details>
  )
}

// The labelled identifier list itself, for a caller that owns its own
// disclosure, such as the passive banner's "Show IDs" menu item.
export function NextActionIDList({ value, className }: { value: Record<string, unknown>; className?: string }) {
  const rows = identifierRows(value)
  if (rows.length === 0) return <p className={cn('text-xs text-muted', className)}>No identifiers.</p>
  return (
    <dl
      className={cn(
        'grid max-w-full grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1 rounded-md bg-surface p-2 text-xs',
        className,
      )}
    >
      {rows.map(([label, text], index) => (
        <div key={`${index}:${label}`} className="contents" data-next-action-id={label}>
          <dt className="text-faint">{label}</dt>
          <dd className="min-w-0 break-words font-mono text-[11px] text-foreground/80">{text}</dd>
        </div>
      ))}
    </dl>
  )
}

function identifierRows(value: Record<string, unknown>) {
  const rows: [string, string][] = []
  const add = (key: string, field: unknown) => {
    const text = identifierText(field)
    if (text) rows.push([identifierLabel(key), text])
  }
  // The order leads, whatever key order the caller built the value in.
  const order = isRecord(value.current_order) ? value.current_order : {}
  add('order_id', order.id)
  add('stage', order.stage)
  for (const [key, field] of Object.entries(value)) {
    if (key === 'current_order') continue
    if (key === 'details' && isRecord(field)) {
      for (const [detail, detailValue] of Object.entries(field)) add(detail, detailValue)
    } else {
      add(key, field)
    }
  }
  return rows
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// "order_ids" reads "Order IDs", "origin_id" reads "Origin ID".
function identifierLabel(key: string) {
  const words = key.split('_').filter(Boolean)
  const label = words
    .map((word) => (word === 'id' ? 'ID' : word === 'ids' ? 'IDs' : word === 'url' ? 'URL' : word))
    .join(' ')
  return label.charAt(0).toUpperCase() + label.slice(1)
}

// A nested value as text: a list joins its items, and an object lists its
// fields as "Label: value", so no braces or quotes reach the page.
function identifierText(value: unknown): string {
  if (value == null) return ''
  if (typeof value === 'string') return value.trim()
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  if (Array.isArray(value)) {
    const items = value.map(identifierText).filter(Boolean)
    return items.join(value.some(isRecord) ? '; ' : ', ')
  }
  if (isRecord(value)) {
    return Object.entries(value)
      .map(([key, field]) => [identifierLabel(key), identifierText(field)] as const)
      .filter(([, text]) => text)
      .map(([label, text]) => `${label}: ${text}`)
      .join(', ')
  }
  return ''
}

// The lines under a decision: what ignoring it costs, when it is due, and the
// server's identifiers. They print once, wherever the choices are.
export function NextActionNotes({ action }: { action: NextAction }) {
  return (
    <>
      {action.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
      <NextActionDeadline deadline={action.deadline} />
      <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />
    </>
  )
}
