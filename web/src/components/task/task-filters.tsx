import { useQuery } from '@tanstack/react-query'
import {
  CalendarDays,
  Check,
  ChevronDown,
  CircleDot,
  Code2,
  GitBranch,
  Search,
  SlidersHorizontal,
  X,
} from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { fetchRequirements, fetchSystemDesigns } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import { useWorkspace, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Select } from '../ui/input'

export type UpdatedWindow = 'any' | '7d' | '30d' | '90d' | 'custom'

export interface TaskFilterState {
  query: string
  state: string
  repository: string
  updated: UpdatedWindow
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
export const boardDefaultTaskFilter: TaskFilterState = { ...emptyTaskFilter, updated: '30d' }

const updatedWindowLabels: Record<UpdatedWindow, string> = {
  any: 'Any time',
  '7d': 'Last 7 days',
  '30d': 'Last month',
  '90d': 'Last quarter',
  custom: 'Custom range',
}
const presetDays: Record<string, number> = { '7d': 7, '30d': 30, '90d': 90 }

export interface TaskFilterParams {
  [key: string]: string | undefined
  q?: string
  state?: string
  repository?: string
  updated_from?: string
  updated_to?: string
  serves_requirement?: string
  governing_design?: string
}

function startOfLocalDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate())
}
function localDayStartInstant(day: string, offsetDays = 0): string | undefined {
  const parts = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day)
  if (!parts) return undefined
  return new Date(Number(parts[1]), Number(parts[2]) - 1, Number(parts[3]) + offsetDays).toISOString()
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
  return Object.keys(taskFilterParams(filter)).length > 0
}
export function taskFilterRangeError(filter: TaskFilterState): string {
  if (filter.updated !== 'custom') return ''
  const params = taskFilterParams(filter)
  return params.updated_from && params.updated_to && params.updated_from >= params.updated_to
    ? 'Choose an end date on or after the start date.'
    : ''
}

function storageKey(surface: string, workspace: string) {
  return `conveyor-task-filters:${surface}:${workspace}`
}
function readStoredFilter(key: string, fallback: TaskFilterState): TaskFilterState {
  try {
    const raw = localStorage.getItem(key)
    return raw ? { ...fallback, ...(JSON.parse(raw) as Partial<TaskFilterState>) } : fallback
  } catch {
    return fallback
  }
}
export function useTaskFilters(surface: 'tasks' | 'board', fallback: TaskFilterState = emptyTaskFilter) {
  const { workspace } = useWorkspaceSelection()
  const key = storageKey(surface, workspace)
  const [filter, setFilter] = useState(() => readStoredFilter(key, fallback))
  useEffect(() => {
    setFilter(readStoredFilter(key, fallback))
  }, [key, fallback])
  const update = useCallback(
    (next: TaskFilterState) => {
      setFilter(next)
      try {
        localStorage.setItem(key, JSON.stringify(next))
      } catch {
        /* persistence is best effort */
      }
    },
    [key],
  )
  return [filter, update, fallback] as const
}

type Option = { id: string; title: string }

function SearchableFilter({
  label,
  value,
  options,
  onChange,
  emptyLabel,
  help,
}: {
  label: string
  value: string
  options: Option[]
  onChange: (value: string) => void
  emptyLabel: string
  help: string
}) {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState('')
  const ref = useRef<HTMLDivElement>(null)
  const selected = options.find((option) => option.id === value)
  const filtered = options.filter((option) => option.title.toLowerCase().includes(search.toLowerCase()))
  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [])
  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        className="flex h-9 w-full min-w-52 items-center justify-between gap-3 rounded-md border border-edge bg-background px-3 text-left text-xs text-foreground transition-colors hover:bg-surface focus-visible:outline-2 focus-visible:outline-primary/40"
        aria-label={label}
        aria-expanded={open}
        title={help}
        onClick={() => setOpen(!open)}
      >
        <span className={selected ? 'truncate' : 'truncate text-muted'}>{selected?.title ?? emptyLabel}</span>
        <ChevronDown className="size-4 shrink-0 text-faint" />
      </button>
      {open && (
        <div className="absolute right-0 z-20 mt-2 w-72 rounded-lg border border-border bg-surface p-2 shadow-xl">
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
            <Input
              autoFocus
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder={`Search ${label.toLowerCase().replace('filter by ', '')}`}
              aria-label={`Search ${label.toLowerCase().replace('filter by ', '')}`}
              className="h-8 pl-8 text-xs"
            />
          </div>
          <div className="mt-2 max-h-56 overflow-y-auto" role="listbox" aria-label={label}>
            <button
              type="button"
              role="option"
              aria-selected={!value}
              className="flex w-full items-center justify-between rounded px-2 py-2 text-left text-xs hover:bg-raised"
              onClick={() => {
                onChange('')
                setOpen(false)
                setSearch('')
              }}
            >
              {emptyLabel}
              {!value && <Check className="size-4 text-primary" />}
            </button>
            {filtered.map((option) => (
              <button
                type="button"
                role="option"
                aria-selected={value === option.id}
                key={option.id}
                className="flex w-full items-center justify-between gap-2 rounded px-2 py-2 text-left text-xs hover:bg-raised"
                onClick={() => {
                  onChange(option.id)
                  setOpen(false)
                  setSearch('')
                }}
              >
                <span className="truncate">{option.title}</span>
                {value === option.id && <Check className="size-4 shrink-0 text-primary" />}
              </button>
            ))}
            {!filtered.length && <p className="px-2 py-3 text-xs text-muted">No matches found.</p>}
          </div>
        </div>
      )}
    </div>
  )
}

function DateFilter({ value, set }: { value: TaskFilterState; set: (patch: Partial<TaskFilterState>) => void }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [])
  const summary =
    value.updated === 'custom' && value.updatedFrom
      ? `${value.updatedFrom}${value.updatedTo ? ` → ${value.updatedTo}` : ''}`
      : updatedWindowLabels[value.updated]
  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        aria-label="Filter by last update"
        aria-expanded={open}
        className="flex h-9 min-w-44 items-center justify-between gap-3 rounded-md border border-edge bg-background px-3 text-left text-xs hover:bg-surface focus-visible:outline-2 focus-visible:outline-primary/40"
        onClick={() => setOpen(!open)}
      >
        <span className="flex min-w-0 items-center gap-2 truncate">
          <CalendarDays className="size-4 shrink-0 text-muted" />
          <span className="truncate">{summary}</span>
        </span>
        <ChevronDown className="size-4 shrink-0 text-faint" />
      </button>
      {open && (
        <div className="absolute right-0 z-20 mt-2 w-72 rounded-lg border border-border bg-surface p-2 shadow-xl">
          <p className="px-2 pb-2 text-[11px] font-medium uppercase tracking-wider text-muted">Updated</p>
          {(Object.keys(updatedWindowLabels) as UpdatedWindow[]).map((window) => (
            <button
              type="button"
              key={window}
              className="flex w-full items-center justify-between rounded px-2 py-2 text-left text-xs hover:bg-raised"
              onClick={() => {
                set({ updated: window })
                if (window !== 'custom') setOpen(false)
              }}
            >
              {updatedWindowLabels[window]}
              {value.updated === window && <Check className="size-4 text-primary" />}
            </button>
          ))}
          {value.updated === 'custom' && (
            <div className="mt-2 grid grid-cols-2 gap-2 border-t border-border px-2 pt-3">
              <label htmlFor="task-filter-updated-from" className="text-[11px] text-muted">
                From
                <Input
                  id="task-filter-updated-from"
                  aria-label="Updated from"
                  type="date"
                  value={value.updatedFrom}
                  onChange={(event) => set({ updatedFrom: event.target.value })}
                  className="mt-1 h-8 text-xs"
                />
              </label>
              <label htmlFor="task-filter-updated-to" className="text-[11px] text-muted">
                To
                <Input
                  id="task-filter-updated-to"
                  aria-label="Updated to"
                  type="date"
                  value={value.updatedTo}
                  onChange={(event) => set({ updatedTo: event.target.value })}
                  className="mt-1 h-8 text-xs"
                />
              </label>
              <Button size="sm" variant="secondary" className="col-span-2" onClick={() => setOpen(false)}>
                Apply date range
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

type FilterCategory = 'state' | 'repository' | 'updated' | 'requirement' | 'design'

const filterCategoryIcons = {
  state: CircleDot,
  repository: GitBranch,
  updated: CalendarDays,
  requirement: Code2,
  design: SlidersHorizontal,
} as const

function CompactFilterMenu({
  value,
  set,
  states,
  repos,
  requirements,
  designs,
  fallback,
  changed,
  activeCount,
  rangeError,
}: {
  value: TaskFilterState
  set: (patch: Partial<TaskFilterState>) => void
  states: string[]
  repos: string[]
  requirements: Option[]
  designs: Option[]
  fallback: TaskFilterState
  changed: boolean
  activeCount: number
  rangeError: string
}) {
  const [open, setOpen] = useState(false)
  const [category, setCategory] = useState<FilterCategory>('state')
  const [search, setSearch] = useState('')
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [])
  const categories: { id: FilterCategory; label: string; active: boolean }[] = [
    { id: 'state', label: 'Status', active: Boolean(value.state) },
    { id: 'repository', label: 'Repository', active: Boolean(value.repository) },
    { id: 'updated', label: 'Updated', active: value.updated !== fallback.updated },
    { id: 'requirement', label: 'Requirement', active: Boolean(value.requirement) },
    { id: 'design', label: 'System design', active: Boolean(value.design) },
  ]
  const selected =
    category === 'state'
      ? value.state
      : category === 'repository'
        ? value.repository
        : category === 'requirement'
          ? value.requirement
          : category === 'design'
            ? value.design
            : value.updated
  const options =
    category === 'state'
      ? states.map((id) => ({ id, title: taskStateLabels[id] ?? id }))
      : category === 'repository'
        ? repos.map((id) => ({ id, title: id }))
        : category === 'requirement'
          ? requirements
          : category === 'design'
            ? designs
            : (Object.keys(updatedWindowLabels) as UpdatedWindow[]).map((id) => ({
                id,
                title: updatedWindowLabels[id],
              }))
  const filtered = options.filter((option) => option.title.toLowerCase().includes(search.toLowerCase()))
  const updateSelection = (id: string) => {
    if (category === 'state') set({ state: id })
    else if (category === 'repository') set({ repository: id })
    else if (category === 'requirement') set({ requirement: id })
    else if (category === 'design') set({ design: id })
    else set({ updated: id as UpdatedWindow })
  }
  return (
    <div className="relative" ref={ref}>
      <Button
        type="button"
        variant={changed ? 'secondary' : 'outline'}
        size="sm"
        aria-label="Open filters"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <SlidersHorizontal />
        Filters
        {activeCount > 0 && (
          <span className="rounded-full bg-primary/15 px-1.5 text-[10px] text-primary">{activeCount}</span>
        )}
      </Button>
      {open && (
        <div className="absolute right-0 z-30 mt-2 flex w-[min(42rem,calc(100vw-2rem))] overflow-hidden rounded-xl border border-border bg-surface shadow-2xl">
          <div className="w-44 shrink-0 border-r border-border p-2">
            <div className="relative mb-2">
              <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-faint" />
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder="Search filters"
                aria-label="Search filter categories"
                className="h-8 pl-7 text-xs"
              />
            </div>
            <div className="flex flex-col gap-0.5" role="tablist" aria-label="Filter categories">
              {categories.map((item) => {
                const Icon = filterCategoryIcons[item.id]
                return (
                  <button
                    key={item.id}
                    type="button"
                    role="tab"
                    aria-selected={category === item.id}
                    className="flex items-center justify-between rounded-md px-2.5 py-2 text-left text-xs hover:bg-raised aria-selected:bg-raised"
                    onClick={() => {
                      setCategory(item.id)
                      setSearch('')
                    }}
                  >
                    <span className="flex min-w-0 items-center gap-2">
                      <Icon className="size-3.5 shrink-0 text-muted" aria-hidden="true" />
                      <span className="truncate">{item.label}</span>
                    </span>
                    {item.active && <span className="size-1.5 rounded-full bg-primary" />}
                  </button>
                )
              })}
            </div>
          </div>
          <div className="min-w-0 flex-1 p-2">
            <div className="flex items-center justify-between px-2 pb-2">
              <p className="text-xs font-medium">{categories.find((item) => item.id === category)?.label}</p>
              <button
                type="button"
                className="text-[11px] text-muted hover:text-foreground"
                onClick={() => {
                  updateSelection('')
                  setSearch('')
                }}
              >
                Clear
              </button>
            </div>
            <div className="relative mb-2">
              <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-faint" />
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder={`Search ${categories.find((item) => item.id === category)?.label.toLowerCase()}`}
                aria-label={`Search ${categories.find((item) => item.id === category)?.label}`}
                className="h-8 pl-7 text-xs"
              />
            </div>
            <div
              className="max-h-60 overflow-y-auto"
              role="listbox"
              aria-label={categories.find((item) => item.id === category)?.label}
            >
              <button
                type="button"
                role="option"
                aria-selected={!selected}
                className="flex w-full items-center justify-between rounded-md px-2.5 py-2 text-left text-xs hover:bg-raised"
                onClick={() => updateSelection('')}
              >
                Any {categories.find((item) => item.id === category)?.label.toLowerCase()}
                {!selected && <Check className="size-4 text-primary" />}
              </button>
              {filtered.map((option) => (
                <button
                  key={option.id}
                  type="button"
                  role="option"
                  aria-selected={selected === option.id}
                  className="flex w-full items-center justify-between rounded-md px-2.5 py-2 text-left text-xs hover:bg-raised"
                  onClick={() => updateSelection(option.id)}
                >
                  <span className="truncate">{option.title}</span>
                  {selected === option.id && <Check className="size-4 shrink-0 text-primary" />}
                </button>
              ))}
              {!filtered.length && <p className="px-2.5 py-3 text-xs text-muted">No matches found.</p>}
            </div>
            {category === 'updated' && value.updated === 'custom' && (
              <div className="mt-2 grid grid-cols-2 gap-2 border-t border-border px-2 pt-3">
                <Input
                  aria-label="Updated from"
                  type="date"
                  value={value.updatedFrom}
                  onChange={(event) => set({ updatedFrom: event.target.value })}
                />
                <Input
                  aria-label="Updated to"
                  type="date"
                  value={value.updatedTo}
                  onChange={(event) => set({ updatedTo: event.target.value })}
                />
              </div>
            )}
            {rangeError && (
              <p className="mt-2 px-2 text-xs text-failure" role="alert">
                {rangeError}
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

export function TaskFilters({
  value,
  onChange,
  fallback = emptyTaskFilter,
  className = '',
  compact = false,
}: {
  value: TaskFilterState
  onChange: (next: TaskFilterState) => void
  fallback?: TaskFilterState
  className?: string
  compact?: boolean
}) {
  const { workspace } = useWorkspaceSelection()
  const { data: workspaceInfo } = useWorkspace()
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
  const requirementOptions = (requirements.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.requirement.id, title: item.requirement.title }))
  const designOptions = (designs.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.document.id, title: item.document.title }))
  const set = (patch: Partial<TaskFilterState>) => onChange({ ...value, ...patch })
  const rangeError = taskFilterRangeError(value)
  const changed = JSON.stringify(value) !== JSON.stringify(fallback)
  const activeCount = [
    value.state,
    value.repository,
    value.updated !== fallback.updated ? value.updated : '',
    value.requirement,
    value.design,
  ].filter(Boolean).length
  if (compact) {
    return (
      <div className={`flex items-center gap-2 ${className}`}>
        <label className="relative w-56" htmlFor={`task-filter-search-${workspace}`}>
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
          <Input
            id={`task-filter-search-${workspace}`}
            aria-label="Search tasks"
            type="search"
            value={value.query}
            onChange={(event) => set({ query: event.target.value })}
            placeholder="Search tasks"
            className="h-9 pl-8 text-xs"
          />
        </label>
        <CompactFilterMenu
          value={value}
          set={set}
          states={states}
          repos={repos}
          requirements={requirementOptions}
          designs={designOptions}
          fallback={fallback}
          changed={changed}
          activeCount={activeCount}
          rangeError={rangeError}
        />
        {changed && (
          <Button variant="ghost" size="sm" aria-label="Reset filters" onClick={() => onChange(fallback)}>
            <X /> Reset
          </Button>
        )}
      </div>
    )
  }
  return (
    <div className={className}>
      <div className="flex flex-wrap items-center gap-2 rounded-xl border border-border bg-background/60 p-2">
        <label className="relative min-w-52 flex-1" htmlFor={`task-filter-search-${workspace}`}>
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
          <Input
            id={`task-filter-search-${workspace}`}
            aria-label="Search tasks"
            type="search"
            value={value.query}
            onChange={(event) => set({ query: event.target.value })}
            placeholder="Search tasks"
            className="h-9 pl-8 text-xs"
          />
        </label>
        <Select
          aria-label="Filter by state"
          value={value.state}
          onChange={(event) => set({ state: event.target.value })}
          className="w-36 text-xs"
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
          className="w-40 text-xs"
        >
          <option value="">All repositories</option>
          {repos.map((repo) => (
            <option key={repo} value={repo}>
              {repo}
            </option>
          ))}
        </Select>
        <DateFilter value={value} set={set} />
        <SearchableFilter
          label="Filter by requirement served"
          value={value.requirement}
          options={requirementOptions}
          onChange={(requirement) => set({ requirement })}
          emptyLabel="Any requirement"
          help="Show only tasks that serve this confirmed requirement."
        />
        <SearchableFilter
          label="Filter by design guidance"
          value={value.design}
          options={designOptions}
          onChange={(design) => set({ design })}
          emptyLabel="Any design guidance"
          help="Show only tasks governed by this confirmed design document."
        />
        {changed && (
          <Button variant="ghost" size="sm" onClick={() => onChange(fallback)}>
            <X />
            Reset
            {activeCount > 0 && (
              <span className="rounded-full bg-primary/15 px-1.5 py-0.5 text-[10px] text-primary">{activeCount}</span>
            )}
          </Button>
        )}
      </div>
      {rangeError && (
        <p className="mt-2 text-xs text-failure" role="alert">
          {rangeError}
        </p>
      )}
      {(requirements.isLoading || designs.isLoading) && (
        <p className="sr-only" role="status">
          Loading filter options
        </p>
      )}
    </div>
  )
}
