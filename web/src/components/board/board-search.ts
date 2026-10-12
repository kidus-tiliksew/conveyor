import { emptyTaskFilter, type TaskFilterState } from '../task/task-filters'

// The Board's address (req-operator-next-action REQ-6): the view, task intake,
// and the filter parameters the list view accepts on arrival. Kanban is the
// default and is never written to the address (AC-6.1).
export interface BoardSearch {
  view?: 'list'
  create?: true
  q?: string
  state?: string[]
  repository?: string[]
  updated_from?: string
  updated_to?: string
  serves_requirement?: string[]
  governing_design?: string[]
  assignee?: string
}

const text = (value: unknown) => (typeof value === 'string' && value ? value : undefined)

function list(value: unknown) {
  const values = (Array.isArray(value) ? value : [value]).filter(
    (entry): entry is string => typeof entry === 'string' && entry !== '',
  )
  return values.length > 0 ? values : undefined
}

// Accepts exactly the Board's parameters; anything else is dropped (AC-6.3).
export function validateBoardSearch(search: Record<string, unknown>): BoardSearch {
  return {
    view: search.view === 'list' ? 'list' : undefined,
    create: search.create === true || search.create === 'true' ? true : undefined,
    q: text(search.q),
    state: list(search.state),
    repository: list(search.repository),
    updated_from: text(search.updated_from),
    updated_to: text(search.updated_to),
    serves_requirement: list(search.serves_requirement),
    governing_design: list(search.governing_design),
    assignee: text(search.assignee),
  }
}

// The filter the address names, or undefined when it names none. A date keeps
// its calendar day, the unit the Updated window speaks.
export function boardSearchFilter(search: BoardSearch): TaskFilterState | undefined {
  const named = Boolean(
    search.q ||
      search.state ||
      search.repository ||
      search.updated_from ||
      search.updated_to ||
      search.serves_requirement ||
      search.governing_design ||
      search.assignee,
  )
  if (!named) return undefined
  const dated = Boolean(search.updated_from || search.updated_to)
  return {
    ...emptyTaskFilter,
    query: search.q ?? '',
    states: search.state ?? [],
    repositories: search.repository ?? [],
    requirements: search.serves_requirement ?? [],
    designs: search.governing_design ?? [],
    assignee: search.assignee ?? '',
    updated: dated ? 'custom' : 'any',
    updatedFrom: search.updated_from?.slice(0, 10) ?? '',
    updatedTo: search.updated_to?.slice(0, 10) ?? '',
  }
}
