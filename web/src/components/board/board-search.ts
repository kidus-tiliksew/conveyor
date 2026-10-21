import type { RequirementInsightTab } from '../../lib/types'
import { emptyTaskFilter, type TaskFilterState } from '../task/task-filters'

// The Board's address (req-operator-next-action REQ-6): the view, task intake,
// and the filter parameters the list view accepts on arrival. Kanban is the
// default and is never written to the address (AC-6.1). The map view adds its
// arrangement, requirement, and task (req-board-map AC-1.1), and the open
// requirement insight tab (req-requirement-insights AC-1.2).
export interface BoardSearch {
  view?: 'list' | 'map'
  // The map's arrangement; requirements when absent.
  arrangement?: 'requirements' | 'dependencies'
  // The map's selected task.
  focus?: string
  // The requirement the map is drilled into.
  scope?: string
  // The requirement insight panel's open tab, for the requirement scope
  // names (feature-requirement-insights RI-7).
  insight?: RequirementInsightTab
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

const insightTabs: readonly RequirementInsightTab[] = ['summary', 'charts', 'documents']

// The group of the map's open tasks that serve no requirement; it names no
// requirement, so it opens no insight panel.
const UNATTACHED_SCOPE = '__none'

// Accepts exactly the Board's parameters; anything else is dropped (AC-6.3).
// An insight tab is kept only with the map view arranged by requirement and a
// scope that can name a requirement (req-requirement-insights AC-1.2); the
// map drops it once its read shows the scope names no requirement group.
export function validateBoardSearch(search: Record<string, unknown>): BoardSearch {
  const view = search.view === 'list' || search.view === 'map' ? search.view : undefined
  const arrangement =
    search.arrangement === 'requirements' || search.arrangement === 'dependencies' ? search.arrangement : undefined
  const scope = text(search.scope)
  const insight =
    view === 'map' &&
    arrangement !== 'dependencies' &&
    scope &&
    scope !== UNATTACHED_SCOPE &&
    insightTabs.includes(search.insight as RequirementInsightTab)
      ? (search.insight as RequirementInsightTab)
      : undefined
  return {
    view,
    arrangement,
    focus: text(search.focus),
    scope,
    insight,
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
