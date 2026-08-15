import { useQuery } from '@tanstack/react-query'
import {
  CalendarDays,
  Check,
  ChevronLeft,
  ChevronRight,
  CircleDot,
  Code2,
  GitBranch,
  Search,
  SlidersHorizontal,
  UserRound,
  X,
} from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { fetchRequirements, fetchSystemDesigns } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import { useWorkspace, useWorkspaceMembers, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Input } from '../ui/input'

export type CreatedWindow = 'any' | '7d' | '30d' | '90d' | 'custom'

// Status, repository, requirement, and design are lists: each is a disjunction
// the server evaluates — a task matches on any checked value — while distinct
// members intersect (AC-2.4). An empty list leaves that member inactive.
export interface TaskFilterState {
  query: string
  states: string[]
  repositories: string[]
  created: CreatedWindow
  createdFrom: string
  createdTo: string
  requirements: string[]
  designs: string[]
  // Assignee is single-valued, unlike the list members: a task has one
  // assignee, so a union of two people is a question nobody asks. Empty means
  // anyone, UNASSIGNED_ASSIGNEE selects tasks nobody holds, and any other
  // value is a workspace member's user ID (REQ-4, DEC-18).
  assignee: string
}

// The server's sentinel for "nobody holds this", distinct from the empty
// default that leaves the member inactive (conveyor:internal/store/store.go).
export const UNASSIGNED_ASSIGNEE = 'unassigned'

export const emptyTaskFilter: TaskFilterState = {
  query: '',
  states: [],
  repositories: [],
  created: 'any',
  createdFrom: '',
  createdTo: '',
  requirements: [],
  designs: [],
  assignee: '',
}
export const boardDefaultTaskFilter: TaskFilterState = { ...emptyTaskFilter, created: '30d' }

const createdWindowLabels: Record<CreatedWindow, string> = {
  any: 'Any time',
  '7d': 'Last 7 days',
  '30d': 'Last month',
  '90d': 'Last quarter',
  custom: 'Custom range',
}
const presetDays: Record<string, number> = { '7d': 7, '30d': 30, '90d': 90 }

// Every list member repeats its query parameter (`state=a&state=b`), the
// spelling parseTaskFilter reads on the server.
export interface TaskFilterParams {
  [key: string]: string | string[] | undefined
  q?: string
  state?: string[]
  repository?: string[]
  created_from?: string
  created_to?: string
  serves_requirement?: string[]
  governing_design?: string[]
  assignee?: string
}

function startOfLocalDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate())
}
function localDayStartInstant(day: string, offsetDays = 0): string | undefined {
  const parts = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day)
  if (!parts) return undefined
  return new Date(Number(parts[1]), Number(parts[2]) - 1, Number(parts[3]) + offsetDays).toISOString()
}
// The calendar and its presets speak local calendar days, the same unit the
// operator reads on the row; taskFilterParams turns them into instants.
function toLocalDay(date: Date): string {
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

export function taskFilterParams(filter: TaskFilterState): TaskFilterParams {
  const params: TaskFilterParams = {}
  if (filter.query.trim()) params.q = filter.query.trim()
  if (filter.states.length) params.state = filter.states
  if (filter.repositories.length) params.repository = filter.repositories
  if (filter.requirements.length) params.serves_requirement = filter.requirements
  if (filter.designs.length) params.governing_design = filter.designs
  if (filter.assignee) params.assignee = filter.assignee
  if (filter.created === 'custom') {
    const from = localDayStartInstant(filter.createdFrom)
    const to = localDayStartInstant(filter.createdTo, 1)
    if (from) params.created_from = from
    if (to) params.created_to = to
  } else if (presetDays[filter.created]) {
    const start = startOfLocalDay(new Date())
    start.setDate(start.getDate() - presetDays[filter.created])
    params.created_from = start.toISOString()
  }
  return params
}
export function taskFilterActive(filter: TaskFilterState): boolean {
  return Object.keys(taskFilterParams(filter)).length > 0
}
export function taskFilterRangeError(filter: TaskFilterState): string {
  if (filter.created !== 'custom') return ''
  const params = taskFilterParams(filter)
  return params.created_from && params.created_to && params.created_from >= params.created_to
    ? 'Choose an end date on or after the start date.'
    : ''
}

function storageKey(surface: string, workspace: string) {
  return `conveyor-task-filters:${surface}:${workspace}`
}
// Stored filters predate the list members — a saved `state: "running"` from the
// single-select era must come back as `states: ["running"]` rather than being
// dropped, so the operator's remembered narrowing survives the upgrade.
function storedList(modern: unknown, legacy: unknown): string[] | undefined {
  if (Array.isArray(modern)) return modern.filter((entry): entry is string => typeof entry === 'string')
  if (typeof legacy === 'string' && legacy) return [legacy]
  return undefined
}
function storedText(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}
function readStoredFilter(key: string, fallback: TaskFilterState): TaskFilterState {
  try {
    const raw = localStorage.getItem(key)
    if (!raw) return fallback
    const stored = JSON.parse(raw) as Record<string, unknown>
    // Read the canonical Created shape first, then migrate the legacy Updated
    // window in memory. The next user change persists only the Created shape.
    const created = storedText(stored.created) ?? storedText(stored.updated)
    return {
      query: storedText(stored.query) ?? fallback.query,
      states: storedList(stored.states, stored.state) ?? fallback.states,
      repositories: storedList(stored.repositories, stored.repository) ?? fallback.repositories,
      created: created && created in createdWindowLabels ? (created as CreatedWindow) : fallback.created,
      createdFrom: storedText(stored.createdFrom) ?? storedText(stored.updatedFrom) ?? fallback.createdFrom,
      createdTo: storedText(stored.createdTo) ?? storedText(stored.updatedTo) ?? fallback.createdTo,
      requirements: storedList(stored.requirements, stored.requirement) ?? fallback.requirements,
      designs: storedList(stored.designs, stored.design) ?? fallback.designs,
      assignee: storedText(stored.assignee) ?? fallback.assignee,
    }
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

// The quick spans offered inside the custom range, mirroring the shortcut rows
// of a conventional analytics range picker. Each resolves to concrete local
// days so the operator sees exactly what was chosen in the From/To fields.
const rangePresets: { label: string; range: () => { from: string; to: string } }[] = [
  { label: 'Today', range: () => spanEndingToday(0) },
  { label: 'Yesterday', range: () => shiftedDay(1) },
  { label: 'Last 7 days', range: () => spanEndingToday(6) },
  { label: 'Last 14 days', range: () => spanEndingToday(13) },
  { label: 'Last 30 days', range: () => spanEndingToday(29) },
  { label: 'This month', range: () => monthSpan(0) },
  { label: 'Last month', range: () => monthSpan(-1) },
]
function spanEndingToday(daysBack: number) {
  const today = startOfLocalDay(new Date())
  const from = new Date(today)
  from.setDate(from.getDate() - daysBack)
  return { from: toLocalDay(from), to: toLocalDay(today) }
}
function shiftedDay(daysBack: number) {
  const day = startOfLocalDay(new Date())
  day.setDate(day.getDate() - daysBack)
  return { from: toLocalDay(day), to: toLocalDay(day) }
}
function monthSpan(offset: number) {
  const now = new Date()
  const first = new Date(now.getFullYear(), now.getMonth() + offset, 1)
  const last = offset === 0 ? startOfLocalDay(now) : new Date(now.getFullYear(), now.getMonth() + offset + 1, 0)
  return { from: toLocalDay(first), to: toLocalDay(last) }
}

const weekdayHeader = ['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa']

// A single-month range calendar: pick a start day, then an end day; picking
// before the start restarts the range there. Kept dependency-free on purpose —
// the dashboard carries no date-picker library.
function RangeCalendar({
  from,
  to,
  onSelect,
}: {
  from: string
  to: string
  onSelect: (range: { from: string; to: string }) => void
}) {
  const anchor = localDayStartInstant(from) ? new Date(from) : new Date()
  const [month, setMonth] = useState(() => new Date(anchor.getFullYear(), anchor.getMonth(), 1))
  const monthLabel = month.toLocaleDateString(undefined, { month: 'long', year: 'numeric' })
  const firstWeekday = month.getDay()
  const daysInMonth = new Date(month.getFullYear(), month.getMonth() + 1, 0).getDate()
  const today = toLocalDay(new Date())
  const pick = (day: string) => {
    // A complete range restarts, an open range closes — unless the pick lands
    // before its own start, which restarts from the earlier day instead.
    if (!from || (from && to) || day < from) onSelect({ from: day, to: '' })
    else onSelect({ from, to: day })
  }
  const cells: (string | null)[] = [
    ...Array.from({ length: firstWeekday }, () => null),
    ...Array.from({ length: daysInMonth }, (_, index) =>
      toLocalDay(new Date(month.getFullYear(), month.getMonth(), index + 1)),
    ),
  ]
  return (
    <div>
      <div className="flex items-center justify-between px-1 pb-1">
        <button
          type="button"
          aria-label="Previous month"
          className="rounded p-1 text-muted hover:bg-raised hover:text-foreground"
          onClick={() => setMonth(new Date(month.getFullYear(), month.getMonth() - 1, 1))}
        >
          <ChevronLeft className="size-4" />
        </button>
        <span className="text-xs font-medium">{monthLabel}</span>
        <button
          type="button"
          aria-label="Next month"
          className="rounded p-1 text-muted hover:bg-raised hover:text-foreground"
          onClick={() => setMonth(new Date(month.getFullYear(), month.getMonth() + 1, 1))}
        >
          <ChevronRight className="size-4" />
        </button>
      </div>
      <div className="grid grid-cols-7 text-center text-[10px] text-faint">
        {weekdayHeader.map((day) => (
          <span key={day} className="py-1">
            {day}
          </span>
        ))}
      </div>
      <div className="grid grid-cols-7">
        {cells.map((day, index) =>
          day === null ? (
            <span key={`blank-${index}`} />
          ) : (
            <button
              key={day}
              type="button"
              aria-label={day}
              aria-pressed={day === from || day === to}
              className={`h-7 rounded text-[11px] tabular-nums transition-colors ${
                day === from || day === to
                  ? 'bg-primary text-primary-foreground'
                  : from && to && day > from && day < to
                    ? 'bg-primary/15 text-foreground'
                    : `hover:bg-raised ${day === today ? 'font-semibold text-primary' : 'text-foreground'}`
              }`}
              onClick={() => pick(day)}
            >
              {Number(day.slice(-2))}
            </button>
          ),
        )}
      </div>
    </div>
  )
}

// The custom range editor: manual From/To fields, one-click preset spans, and
// the calendar — all writing the same two local days.
function CustomRangeEditor({ value, set }: { value: TaskFilterState; set: (patch: Partial<TaskFilterState>) => void }) {
  return (
    <div className="mt-2 border-t border-border px-2 pt-3">
      <div className="grid grid-cols-2 gap-2">
        <Input
          aria-label="Created from"
          type="date"
          value={value.createdFrom}
          onChange={(event) => set({ createdFrom: event.target.value })}
          className="h-8 text-xs"
        />
        <Input
          aria-label="Created to"
          type="date"
          value={value.createdTo}
          onChange={(event) => set({ createdTo: event.target.value })}
          className="h-8 text-xs"
        />
      </div>
      <div className="mt-2 flex flex-wrap gap-1.5">
        {rangePresets.map((preset) => {
          const span = preset.range()
          const active = value.createdFrom === span.from && value.createdTo === span.to
          return (
            <button
              key={preset.label}
              type="button"
              aria-pressed={active}
              className={`rounded-full border px-2 py-0.5 text-[11px] transition-colors ${
                active
                  ? 'border-primary bg-primary/15 text-primary'
                  : 'border-edge text-muted hover:bg-raised hover:text-foreground'
              }`}
              onClick={() => set({ createdFrom: span.from, createdTo: span.to })}
            >
              {preset.label}
            </button>
          )
        })}
      </div>
      <div className="mt-2">
        <RangeCalendar
          from={value.createdFrom}
          to={value.createdTo}
          onSelect={(range) => set({ createdFrom: range.from, createdTo: range.to })}
        />
      </div>
    </div>
  )
}

type FilterCategory = 'state' | 'repository' | 'created' | 'requirement' | 'design' | 'assignee'

const filterCategoryIcons = {
  state: CircleDot,
  repository: GitBranch,
  created: CalendarDays,
  requirement: Code2,
  design: SlidersHorizontal,
  assignee: UserRound,
} as const

const listCategoryKeys = {
  state: 'states',
  repository: 'repositories',
  requirement: 'requirements',
  design: 'designs',
} as const

function FilterMenu({
  value,
  set,
  states,
  repos,
  requirements,
  designs,
  assignees,
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
  assignees: Option[]
  fallback: TaskFilterState
  changed: boolean
  activeCount: number
  rangeError: string
}) {
  const [open, setOpen] = useState(false)
  const [category, setCategory] = useState<FilterCategory>('state')
  const [search, setSearch] = useState('')
  // The panel opens toward whichever side has room: the Board mounts this at
  // the right edge, the Tasks header near the left, and an edge-anchored panel
  // must not slide under the navigation rail.
  const [align, setAlign] = useState<'left' | 'right'>('right')
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const close = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [])
  const categories: { id: FilterCategory; label: string; active: boolean }[] = [
    { id: 'state', label: 'Status', active: value.states.length > 0 },
    { id: 'repository', label: 'Repository', active: value.repositories.length > 0 },
    { id: 'created', label: 'Created', active: value.created !== fallback.created },
    { id: 'requirement', label: 'Requirement', active: value.requirements.length > 0 },
    { id: 'design', label: 'System design', active: value.designs.length > 0 },
    { id: 'assignee', label: 'Assignee', active: value.assignee !== '' },
  ]
  const activeLabel = categories.find((item) => item.id === category)?.label ?? ''
  // The Created member is a single window — overlapping spans have no union an
  // operator would ask for — so its rows check exclusively. Every other
  // category checks cumulatively and the menu stays open for the next check.
  // Created and Assignee each hold one value; the rest are cumulative lists.
  const selected: string[] =
    category === 'created'
      ? [value.created]
      : category === 'assignee'
        ? value.assignee
          ? [value.assignee]
          : []
        : value[listCategoryKeys[category]]
  const options: Option[] =
    category === 'state'
      ? states.map((id) => ({ id, title: taskStateLabels[id] ?? id }))
      : category === 'repository'
        ? repos.map((id) => ({ id, title: id }))
        : category === 'requirement'
          ? requirements
          : category === 'design'
            ? designs
            : category === 'assignee'
              ? assignees
              : (Object.keys(createdWindowLabels) as CreatedWindow[]).map((id) => ({
                  id,
                  title: createdWindowLabels[id],
                }))
  const filtered = options.filter((option) => option.title.toLowerCase().includes(search.toLowerCase()))
  const ValueIcon = filterCategoryIcons[category]
  const toggleSelection = (id: string) => {
    if (category === 'created') {
      set({ created: id as CreatedWindow })
      return
    }
    // One assignee at a time: choosing the current one again clears it, which
    // is how the row returns to "anyone" without reaching for the Any option.
    if (category === 'assignee') {
      set({ assignee: value.assignee === id ? '' : id })
      return
    }
    const key = listCategoryKeys[category]
    const current = value[key]
    set({ [key]: current.includes(id) ? current.filter((entry) => entry !== id) : [...current, id] })
  }
  const clearCategory = () => {
    if (category === 'created') set({ created: fallback.created, createdFrom: '', createdTo: '' })
    else if (category === 'assignee') set({ assignee: '' })
    else set({ [listCategoryKeys[category]]: [] })
  }
  return (
    <div className="relative" ref={ref}>
      <Button
        type="button"
        variant={changed ? 'secondary' : 'outline'}
        size="sm"
        aria-label="Open filters"
        aria-expanded={open}
        onClick={() => {
          const anchor = ref.current?.getBoundingClientRect()
          if (anchor) setAlign(anchor.left + anchor.width / 2 < window.innerWidth / 2 ? 'left' : 'right')
          setOpen(!open)
        }}
      >
        <SlidersHorizontal />
        Filters
        {activeCount > 0 && (
          <span className="rounded-full bg-primary/15 px-1.5 text-[10px] text-primary">{activeCount}</span>
        )}
      </Button>
      {open && (
        <div
          className={`absolute z-30 mt-2 flex w-[min(42rem,calc(100vw-2rem))] overflow-hidden rounded-xl border border-border bg-surface shadow-2xl ${align === 'left' ? 'left-0' : 'right-0'}`}
        >
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
              <p className="text-xs font-medium">{activeLabel}</p>
              <button
                type="button"
                className="text-[11px] text-muted hover:text-foreground"
                onClick={() => {
                  clearCategory()
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
                placeholder={`Search ${activeLabel.toLowerCase()}`}
                aria-label={`Search ${activeLabel}`}
                className="h-8 pl-7 text-xs"
              />
            </div>
            <div
              className="max-h-60 overflow-y-auto"
              role="listbox"
              aria-label={activeLabel}
              aria-multiselectable={category !== 'created' && category !== 'assignee'}
            >
              {category !== 'created' && (
                <button
                  type="button"
                  role="option"
                  aria-selected={selected.length === 0}
                  className="flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-xs hover:bg-raised aria-selected:bg-raised"
                  onClick={clearCategory}
                >
                  <span
                    className={`flex size-4 shrink-0 items-center justify-center rounded border ${selected.length === 0 ? 'border-primary bg-primary text-primary-foreground' : 'border-muted'}`}
                  >
                    {selected.length === 0 && <Check className="size-3" aria-hidden="true" />}
                  </span>
                  <span className="truncate">Any {activeLabel.toLowerCase()}</span>
                </button>
              )}
              {filtered.map((option) => {
                const checked = selected.includes(option.id)
                return (
                  <button
                    key={option.id}
                    type="button"
                    role="option"
                    aria-selected={checked}
                    className="flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-xs hover:bg-raised aria-selected:bg-raised"
                    onClick={() => toggleSelection(option.id)}
                  >
                    <span
                      className={`flex size-4 shrink-0 items-center justify-center rounded border ${checked ? 'border-primary bg-primary text-primary-foreground' : 'border-muted'}`}
                    >
                      {checked && <Check className="size-3" aria-hidden="true" />}
                    </span>
                    <ValueIcon className="size-4 shrink-0 text-muted" aria-hidden="true" />
                    <span className="truncate">{option.title}</span>
                  </button>
                )
              })}
              {!filtered.length && <p className="px-2.5 py-3 text-xs text-muted">No matches found.</p>}
            </div>
            {category === 'created' && value.created === 'custom' && <CustomRangeEditor value={value} set={set} />}
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
}: {
  value: TaskFilterState
  onChange: (next: TaskFilterState) => void
  fallback?: TaskFilterState
  className?: string
}) {
  const { workspace } = useWorkspaceSelection()
  const { data: workspaceInfo } = useWorkspace()
  const states = useMemo(() => Object.keys(taskStateLabels).sort(), [])
  const repos = useMemo(() => (workspaceInfo?.repos ?? []).map((entry) => entry.name).sort(), [workspaceInfo])
  const requirements = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const designs = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  // Co-members, never a user directory: this read is workspace-scoped and the
  // server answers it for any member (AC-3.2).
  const members = useWorkspaceMembers()
  const requirementOptions = (requirements.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.requirement.id, title: item.requirement.title }))
  const designOptions = (designs.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.document.id, title: item.document.title }))
  // "Unassigned" is a choice of its own, not the absence of one: it selects the
  // tasks nobody holds, which the "Any assignee" default does not.
  const assigneeOptions: Option[] = [
    { id: UNASSIGNED_ASSIGNEE, title: 'Unassigned' },
    ...(members.data ?? []).map((member) => ({
      id: member.user_id,
      title: member.display_name || member.email || member.user_id,
    })),
  ]
  const set = (patch: Partial<TaskFilterState>) => onChange({ ...value, ...patch })
  const rangeError = taskFilterRangeError(value)
  const changed = JSON.stringify(value) !== JSON.stringify(fallback)
  // One count per narrowed category, matching the dots on the category rail.
  const activeCount = [
    value.states.length > 0,
    value.repositories.length > 0,
    value.created !== fallback.created,
    value.requirements.length > 0,
    value.designs.length > 0,
    value.assignee !== '',
  ].filter(Boolean).length
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
      <FilterMenu
        value={value}
        set={set}
        states={states}
        repos={repos}
        requirements={requirementOptions}
        designs={designOptions}
        assignees={assigneeOptions}
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
      {(requirements.isLoading || designs.isLoading) && (
        <p className="sr-only" role="status">
          Loading filter options
        </p>
      )}
    </div>
  )
}
