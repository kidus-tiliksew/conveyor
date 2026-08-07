import { useQuery } from '@tanstack/react-query'
import { Search, X } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { fetchRequirements, fetchSystemDesigns } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import { useWorkspace, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Select } from '../ui/input'

// One filter family for the Tasks list and the Board (AC-2.4). Both surfaces
// mount this component and send what it produces to the server, so a filter
// cannot mean one thing on one surface and something else on the other, and
// neither surface narrows a fully-loaded workspace in the browser (AC-2.3).
//
// Nothing here filters on priority, assignee, or a declared phase, and no such
// control may be added: the barred fields do not exist on the wire and adding a
// control for one would be the first step to inventing them (AC-1.5).

// The updated-at window is a preset rather than two date pickers for the
// common case, because "updated in the last month" is the question operators
// actually ask; `custom` opens the explicit range for the rest.
export type UpdatedWindow = 'any' | '7d' | '30d' | '90d' | 'custom'

export interface TaskFilterState {
  query: string
  state: string
  repository: string
  updated: UpdatedWindow
  // Local calendar days (YYYY-MM-DD), only read when `updated` is `custom`.
  updatedFrom: string
  updatedTo: string
  requirement: string
  design: string
}

export const emptyTaskFilter: TaskFilterState = {
  query: '',
  state: '',
  repository: '',
  updated: 'any',
  updatedFrom: '',
  updatedTo: '',
  requirement: '',
  design: '',
}

// The Board opens on the last month of activity (AC-2.4). It is a starting
// point rather than a rule: the operator adjusts it, and the adjustment is what
// gets remembered.
export const boardDefaultTaskFilter: TaskFilterState = { ...emptyTaskFilter, updated: '30d' }

const updatedWindowLabels: Record<UpdatedWindow, string> = {
  any: 'Any time',
  '7d': 'Updated in the last week',
  '30d': 'Updated in the last month',
  '90d': 'Updated in the last quarter',
  custom: 'Updated in a date range…',
}

const presetDays: Record<string, number> = { '7d': 7, '30d': 30, '90d': 90 }

// The wire shape both surfaces send. Absent members are omitted rather than
// sent empty, so an inactive filter produces the same request it always did.
export interface TaskFilterParams {
  // The members are named for the reader; the index signature is what lets the
  // two fetchers take the whole family without restating it.
  [key: string]: string | undefined
  q?: string
  state?: string
  repository?: string
  updated_from?: string
  updated_to?: string
  serves_requirement?: string
  governing_design?: string
}

// Bounds are resolved to absolute instants here, in the browser, where the
// operator's own day boundaries live — the server never has to guess a timezone
// for "the last month". They land on local midnight, so the resolved value is
// stable for the whole day and a React Query key built from it does not churn
// on every render.
function startOfLocalDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate())
}

function localDayStartInstant(day: string, offsetDays = 0): string | undefined {
  const parts = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day)
  if (!parts) return undefined
  const date = new Date(Number(parts[1]), Number(parts[2]) - 1, Number(parts[3]) + offsetDays)
  return date.toISOString()
}

export function taskFilterParams(filter: TaskFilterState): TaskFilterParams {
  const params: TaskFilterParams = {}
  if (filter.query.trim()) params.q = filter.query.trim()
  if (filter.state) params.state = filter.state
  if (filter.repository) params.repository = filter.repository
  if (filter.requirement) params.serves_requirement = filter.requirement
  if (filter.design) params.governing_design = filter.design
  if (filter.updated === 'custom') {
    const from = localDayStartInstant(filter.updatedFrom)
    // The server's upper bound is exclusive, so an operator-chosen end date
    // resolves to the start of the following day: picking a day includes it.
    const to = localDayStartInstant(filter.updatedTo, 1)
    if (from) params.updated_from = from
    if (to) params.updated_to = to
  } else if (presetDays[filter.updated]) {
    const start = startOfLocalDay(new Date())
    start.setDate(start.getDate() - presetDays[filter.updated])
    params.updated_from = start.toISOString()
  }
  return params
}

export function taskFilterActive(filter: TaskFilterState): boolean {
  const params = taskFilterParams(filter)
  return Object.keys(params).length > 0
}

// An inverted custom range matches nothing and the server rejects it, so the
// surface says why instead of rendering an empty workspace behind an error.
export function taskFilterRangeError(filter: TaskFilterState): string {
  if (filter.updated !== 'custom') return ''
  const params = taskFilterParams(filter)
  if (!params.updated_from || !params.updated_to) return ''
  return params.updated_from < params.updated_to ? '' : 'Choose an end date on or after the start date.'
}

function storageKey(surface: string, workspace: string) {
  return `conveyor-task-filters:${surface}:${workspace}`
}

function readStoredFilter(key: string, fallback: TaskFilterState): TaskFilterState {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return fallback
    // Merge over the default so a filter stored by an older build gains new
    // members instead of arriving with holes in it.
    return { ...fallback, ...(JSON.parse(raw) as Partial<TaskFilterState>) }
  } catch {
    return fallback
  }
}

// Filter state, remembered per operator and scoped to the workspace it was set
// in (AC-2.4): switching workspaces restores that workspace's own view rather
// than carrying a repository filter into a workspace that has no such repo.
export function useTaskFilters(surface: 'tasks' | 'board', fallback: TaskFilterState = emptyTaskFilter) {
  const { workspace } = useWorkspaceSelection()
  const key = storageKey(surface, workspace)
  const [filter, setFilter] = useState(() => readStoredFilter(key, fallback))
  // Switching workspaces re-reads that workspace's own stored view. Both call
  // sites pass a module constant, so this reloads on the workspace, not on
  // every render.
  useEffect(() => {
    setFilter(readStoredFilter(key, fallback))
  }, [key, fallback])
  const update = useCallback(
    (next: TaskFilterState) => {
      setFilter(next)
      try {
        localStorage.setItem(key, JSON.stringify(next))
      } catch {
        // A browser refusing storage costs persistence, never filtering.
      }
    },
    [key],
  )
  return [filter, update, fallback] as const
}

export function TaskFilters({
  value,
  onChange,
  fallback = emptyTaskFilter,
  className = '',
}: {
  value: TaskFilterState
  onChange: (next: TaskFilterState) => void
  fallback?: TaskFilterState
  className?: string
}) {
  const { workspace } = useWorkspaceSelection()
  const { data: workspaceInfo } = useWorkspace()
  // The option lists come from workspace-stable sources rather than from the
  // rows on screen: a server-side filter removes the very rows whose values
  // would otherwise populate the menus.
  const states = useMemo(() => Object.keys(taskStateLabels).sort(), [])
  const repos = useMemo(() => (workspaceInfo?.repos ?? []).map((entry) => entry.name).sort(), [workspaceInfo])
  const requirements = useQuery({
    queryKey: ['requirements', workspace, 'task-filters'],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
  })
  const designs = useQuery({
    queryKey: ['system-designs', workspace, 'task-filters'],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
  })
  // Only a confirmed document can be attached to a task, so only a confirmed
  // document can narrow the list.
  const requirementOptions = (requirements.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.requirement.id, title: item.requirement.title }))
  const designOptions = (designs.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.document.id, title: item.document.title }))
  const set = (patch: Partial<TaskFilterState>) => onChange({ ...value, ...patch })
  const rangeError = taskFilterRangeError(value)
  const changed = JSON.stringify(value) !== JSON.stringify(fallback)

  return (
    <div className={className}>
      <div className="flex flex-wrap items-center gap-2">
        <label className="relative" htmlFor={`task-filter-search-${workspace}`}>
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
          <Input
            id={`task-filter-search-${workspace}`}
            aria-label="Search tasks"
            type="search"
            value={value.query}
            onChange={(event) => set({ query: event.target.value })}
            placeholder="Search tasks"
            className="h-8 w-56 pl-8 text-xs"
          />
        </label>
        <Select
          aria-label="Filter by state"
          value={value.state}
          onChange={(event) => set({ state: event.target.value })}
          className="h-8 w-40 text-xs"
        >
          <option value="">All states</option>
          {states.map((state) => (
            <option key={state} value={state}>
              {taskStateLabels[state] ?? state}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by repository"
          value={value.repository}
          onChange={(event) => set({ repository: event.target.value })}
          className="h-8 w-40 text-xs"
        >
          <option value="">All repositories</option>
          {repos.map((repo) => (
            <option key={repo} value={repo}>
              {repo}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by last update"
          title="Filters on the same “Updated” time each row shows."
          value={value.updated}
          onChange={(event) => set({ updated: event.target.value as UpdatedWindow })}
          className="h-8 w-52 text-xs"
        >
          {(Object.keys(updatedWindowLabels) as UpdatedWindow[]).map((window) => (
            <option key={window} value={window}>
              {updatedWindowLabels[window]}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by requirement served"
          title="Show only tasks that serve this confirmed requirement."
          value={value.requirement}
          onChange={(event) => set({ requirement: event.target.value })}
          className="h-8 w-52 text-xs"
        >
          <option value="">Any requirement</option>
          {requirementOptions.map((option) => (
            <option key={option.id} value={option.id}>
              {option.title}
            </option>
          ))}
        </Select>
        <Select
          aria-label="Filter by design guidance"
          title="Show only tasks governed by this confirmed design document."
          value={value.design}
          onChange={(event) => set({ design: event.target.value })}
          className="h-8 w-52 text-xs"
        >
          <option value="">Any design guidance</option>
          {designOptions.map((option) => (
            <option key={option.id} value={option.id}>
              {option.title}
            </option>
          ))}
        </Select>
        {changed && (
          <Button variant="ghost" size="sm" onClick={() => onChange(fallback)}>
            <X />
            Reset filters
          </Button>
        )}
      </div>
      {value.updated === 'custom' && (
        <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted">
          <label className="flex items-center gap-1.5" htmlFor={`task-filter-from-${workspace}`}>
            From
            <Input
              id={`task-filter-from-${workspace}`}
              aria-label="Updated from"
              type="date"
              value={value.updatedFrom}
              onChange={(event) => set({ updatedFrom: event.target.value })}
              className="h-8 w-40 text-xs"
            />
          </label>
          <label className="flex items-center gap-1.5" htmlFor={`task-filter-to-${workspace}`}>
            To
            <Input
              id={`task-filter-to-${workspace}`}
              aria-label="Updated to"
              type="date"
              value={value.updatedTo}
              onChange={(event) => set({ updatedTo: event.target.value })}
              className="h-8 w-40 text-xs"
            />
          </label>
          {rangeError && <span className="text-failure">{rangeError}</span>}
        </div>
      )}
    </div>
  )
}
