import type { Decision, DecisionCitation, DecisionSupersessionSweepEntry } from '../../lib/types'

// Pure derivations behind the Decisions surface and the decision page
// (component-web-decision-surfaces). Every value comes from the workspace's
// decision list and its decision citations; nothing here reads the server.

export type DecisionFilter = 'in_force' | 'superseded' | 'dismissed' | 'all'

export const decisionFilters: ReadonlyArray<{ value: DecisionFilter; label: string }> = [
  { value: 'in_force', label: 'In force' },
  { value: 'superseded', label: 'Superseded' },
  { value: 'dismissed', label: 'Dismissed' },
  { value: 'all', label: 'All' },
]

const filterStatus: Record<Exclude<DecisionFilter, 'all'>, Decision['status']> = {
  in_force: 'confirmed',
  superseded: 'superseded',
  dismissed: 'dismissed',
}

// The numeric part of `DEC-n`; an ID without one sorts last.
export function decisionNumber(id: string) {
  const match = /^DEC-(\d+)$/i.exec(id)
  return match ? Number(match[1]) : -1
}

// Highest DEC-n first (req-document-operating-surfaces AC-7.1).
export function sortDecisions(decisions: readonly Decision[]) {
  return [...decisions].sort(
    (left, right) => decisionNumber(right.id) - decisionNumber(left.id) || left.id.localeCompare(right.id),
  )
}

export function inFilter(decision: Decision, filter: DecisionFilter) {
  return filter === 'all' || decision.status === filterStatus[filter]
}

// Case-insensitive containment in the DEC-n, statement, or context (AC-7.3).
export function matchesSearch(decision: Decision, query: string) {
  const needle = query.trim().toLocaleLowerCase()
  if (!needle) return true
  return [decision.id, decision.statement, decision.context].some((field) =>
    (field ?? '').toLocaleLowerCase().includes(needle),
  )
}

export function openSweepEntries(decision: Decision): DecisionSupersessionSweepEntry[] {
  return (decision.sweep?.entries ?? []).filter((entry) => entry.status === 'open')
}

// A statement splits into clauses only when it begins with `(1) ` and every
// clause marker after it continues the sequence `(2) `, `(3) `, … in order.
// Anything else renders as one paragraph (AC-8.2).
export function statementClauses(statement: string): string[] | null {
  if (!statement.startsWith('(1) ')) return null
  const markers = [...statement.matchAll(/(^|\s)\((\d+)\) /g)]
  if (markers.length < 2) return null
  if (markers.some((marker, index) => Number(marker[2]) !== index + 1)) return null
  return markers.map((marker, index) => {
    const start = (marker.index ?? 0) + marker[0].length
    const end = index + 1 < markers.length ? (markers[index + 1].index ?? statement.length) : statement.length
    return statement.slice(start, end).trim()
  })
}

// One row per rejected alternative. The record holds prose that names each
// alternative before a colon, so a row starts at each line and at each
// sentence holding a colon; a sentence without one continues the row before
// it. Prose without any colon keeps one row per line.
export function alternativeRows(text: string): string[] {
  const lines = text
    .split(/\n+/)
    .map((line) => line.trim())
    .filter(Boolean)
  if (!lines.some((line) => line.includes(':'))) return lines
  const rows: string[] = []
  for (const line of lines) {
    line.split(/(?<=\.)\s+(?=[A-Z`"'])/).forEach((sentence, index) => {
      if (index === 0 || sentence.includes(':')) rows.push(sentence)
      else rows[rows.length - 1] = `${rows[rows.length - 1]} ${sentence}`
    })
  }
  return rows
}

// The supersession chain through `supersedes` and `superseded_by`, oldest
// first (AC-8.3). A decision the list lacks ends the walk, and a repeated ID
// stops it, so a malformed chain still renders.
export function supersessionChain(decision: Decision, byId: ReadonlyMap<string, Decision>): Decision[] {
  const seen = new Set([decision.id])
  const older: Decision[] = []
  let cursor = decision.supersedes ? byId.get(decision.supersedes) : undefined
  while (cursor && !seen.has(cursor.id)) {
    seen.add(cursor.id)
    older.unshift(cursor)
    cursor = cursor.supersedes ? byId.get(cursor.supersedes) : undefined
  }
  const newer: Decision[] = []
  cursor = decision.superseded_by ? byId.get(decision.superseded_by) : undefined
  while (cursor && !seen.has(cursor.id)) {
    seen.add(cursor.id)
    newer.push(cursor)
    cursor = cursor.superseded_by ? byId.get(cursor.superseded_by) : undefined
  }
  return [...older, decision, ...newer]
}

// Citations grouped by decision ID, from the one workspace citations read.
export function citationsByDecision(citations: readonly DecisionCitation[]) {
  const grouped = new Map<string, DecisionCitation[]>()
  for (const citation of citations) {
    const list = grouped.get(citation.decision_id) ?? []
    list.push(citation)
    grouped.set(citation.decision_id, list)
  }
  return grouped
}

// A stored zero time (`0001-01-01T00:00:00Z`) means the event never happened.
export function recordedTime(value?: string) {
  if (!value) return undefined
  const time = Date.parse(value)
  return Number.isNaN(time) || time <= 0 ? undefined : value
}

// The date a row and the page header show: confirmation for an in-force or
// superseded decision, dismissal for a dismissed one, proposal otherwise.
export function decisionDate(decision: Decision) {
  if (decision.status === 'dismissed') return recordedTime(decision.dismissed_at) ?? decision.created_at
  if (decision.status === 'proposed') return decision.created_at
  return recordedTime(decision.confirmed_at) ?? decision.created_at
}

export function findDecision(decisions: readonly Decision[], id: string) {
  return (
    decisions.find((decision) => decision.id === id) ??
    decisions.find((decision) => decision.id.toLowerCase() === id.toLowerCase())
  )
}

export function formatDecisionDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(value))
}

// The Decisions surface's address: the filter (In force when absent), the
// search text, and whether the list is narrowed to the decisions that hold an
// open flag (AC-7.2 to AC-7.4).
export interface DecisionsSearch {
  filter?: DecisionFilter
  q?: string
  flagged?: boolean
}

export function validateDecisionsSearch(search: Record<string, unknown>): DecisionsSearch {
  const filter = decisionFilters.find((option) => option.value === search.filter)?.value
  return {
    filter: filter === 'in_force' ? undefined : filter,
    q: typeof search.q === 'string' && search.q ? search.q : undefined,
    flagged: search.flagged === true || search.flagged === 'true' ? true : undefined,
  }
}
