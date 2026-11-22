import { type QueryClient, type UseQueryResult, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { ChevronRight, Package, Plus } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { callerIdentityQueryOptions, useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { briefingOlderThanLimit } from '../components/crate/crate-briefing'
import { CrateFormDialog, CrateStateBadge, TargetDate } from '../components/crate/crate-parts'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { ErrorState } from '../components/ui/error-state'
import { PageHeader } from '../components/ui/page-header'
import { SegmentedControl } from '../components/ui/segmented-control'
import { Skeleton } from '../components/ui/skeleton'
import { CrateBriefingError, fetchCrates, generateCrateBriefing } from '../lib/api'
import { cratesQueryKey } from '../lib/query-keys'
import type { Crate, CrateListRow as CrateListItem } from '../lib/types'
import { cn } from '../lib/utils'

export interface CratesSearch {
  // Closed crates show only on request (req-crates AC-5.1).
  show?: 'closed'
}

export function validateCratesSearch(search: Record<string, unknown>): CratesSearch {
  return { show: search.show === 'closed' ? 'closed' : undefined }
}

// The crates list (component-crates invariant 17; req-crates AC-5.1): each
// crate's name, the headline of its stored briefing, its target date, and
// its completion pie, in the order the server returns them. The page never
// re-sorts.
export function CratesPage() {
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as CratesSearch
  const state = search.show ?? 'open'
  const canCreate = useWorkspaceCapability('create_tasks')
  const [creating, setCreating] = useState(false)
  const crates = useQuery({
    queryKey: cratesQueryKey(workspace, state),
    queryFn: () => fetchCrates(workspace, state),
    // No placeholder: the open list must never stand in for the closed one
    // while the closed list loads, or the reverse.
    enabled: Boolean(workspace),
    // Each opening reads the list again even when a fresh copy is cached,
    // because the briefing request decision waits for an answer that arrives
    // in this opening (component-crates invariant 35).
    refetchOnMount: 'always',
  })
  useListBriefingRequests(workspace, state, crates)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        title="Crates"
        badge={crates.data && <Badge variant="mono">{crates.data.length}</Badge>}
        description="Groups of tasks that ship together."
      >
        {canCreate && (
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus />
            New crate
          </Button>
        )}
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-5xl px-4 py-5 lg:px-6">
          <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
            <SegmentedControl
              label="Crate state"
              value={state}
              onChange={(next) =>
                void navigate({
                  to: '/w/$workspaceId/crates',
                  params: { workspaceId: workspace },
                  search: next === 'closed' ? { show: 'closed' } : {},
                })
              }
              options={[
                { value: 'open', label: 'Open' },
                { value: 'closed', label: 'Closed' },
              ]}
            />
            <p className="text-xs text-faint">Soonest target date first. Crates with no date come last.</p>
          </div>

          {crates.isPending && <CrateListSkeleton />}
          {crates.error != null && (
            <ErrorState
              title="Crates did not load"
              error={crates.error}
              onRetry={() => void crates.refetch()}
              retrying={crates.isFetching}
            />
          )}
          {crates.data && crates.data.length === 0 && (
            <EmptyCrates closed={state === 'closed'} canCreate={canCreate} onCreate={() => setCreating(true)} />
          )}
          {crates.data && crates.data.length > 0 && (
            <div className="overflow-hidden rounded-lg border border-border bg-card">
              <div
                aria-hidden
                data-crate-list-header
                className={cn(
                  'hidden gap-x-6 border-b border-border px-4 py-2 text-xs font-medium text-faint md:grid',
                  rowColumns,
                )}
              >
                <span>Crate</span>
                <span>Target date</span>
                <span>Done</span>
              </div>
              <ul aria-label={state === 'closed' ? 'Closed crates' : 'Open crates'} className="divide-y divide-border">
                {crates.data.map((crate) => (
                  <li key={crate.id}>
                    <CrateListRow crate={crate} />
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      </div>
      {creating && (
        <CrateFormDialog
          onClose={() => setCreating(false)}
          onSaved={(crate) => {
            setCreating(false)
            void navigate({
              to: '/w/$workspaceId/crates/$crateId',
              params: { workspaceId: workspace, crateId: crate.id },
            })
          }}
        />
      )}
    </div>
  )
}

// At most this many briefing requests from one list opening are in flight
// at once (component-crates invariant 35).
const listBriefingRequestLimit = 2

// Whether the list asks for a briefing for this row: an open crate with a
// member whose briefing is missing or was generated more than 30 minutes
// before the list read (component-crates invariant 35; req-crate-insights
// AC-5.2). The server's fingerprint check still decides whether the model
// runs.
export function listBriefingWanted(crate: CrateListItem, readAt: number) {
  if (crate.state !== 'open' || crate.total_task_count <= 0) return false
  return crate.briefing == null || briefingOlderThanLimit(crate.briefing.generated_at, readAt)
}

// The briefing requests of one list opening (component-crates invariant 35;
// req-crate-insights AC-5.2; DEC-144(4)(c)). The opening decides once per
// workspace, from its first successful list read answered after the page
// mounted and that read's time: a copy cached by an earlier visit does not
// count. When that read answers before the identity read, the decision waits
// for the identity read and still uses the first read. Only a user whose
// identity read lists create_tasks posts. The eligible crates and the read
// time are fixed before the first request. A filter change, a refetch, and
// a workspace change stream update never decide again. A new visit or
// another workspace decides afresh.
function useListBriefingRequests(workspace: string, state: 'open' | 'closed', crates: UseQueryResult<CrateListItem[]>) {
  const queryClient = useQueryClient()
  const mayCreate = useWorkspaceCapability('create_tasks')
  const identity = useQuery({ ...callerIdentityQueryOptions(workspace), enabled: Boolean(workspace) })
  const decided = useRef<string | null>(null)
  const firstRead = useRef<{
    workspace: string
    state: 'open' | 'closed'
    rows: CrateListItem[]
    readAt: number
  } | null>(null)
  const queue = useRef<{ cancelled: boolean } | null>(null)
  const answered = crates.isFetchedAfterMount && crates.status === 'success'
  useEffect(() => {
    if (!workspace || decided.current === workspace) return
    if (answered && crates.data && firstRead.current?.workspace !== workspace)
      firstRead.current = { workspace, state, rows: crates.data, readAt: crates.dataUpdatedAt }
    const first = firstRead.current
    if (!first || first.workspace !== workspace || identity.status === 'pending') return
    decided.current = workspace
    if (!mayCreate) return
    const ids = first.rows.filter((crate) => listBriefingWanted(crate, first.readAt)).map((crate) => crate.id)
    if (ids.length === 0) return
    const run = { cancelled: false }
    queue.current = run
    void requestListBriefings(queryClient, first.workspace, first.state, ids, run)
  }, [workspace, state, answered, crates.data, crates.dataUpdatedAt, identity.status, mayCreate, queryClient])
  // Queued requests end with their view: leaving the page or switching the
  // workspace discards every request not yet sent and every refetch not yet
  // asked for.
  useEffect(
    () => () => {
      if (queue.current) queue.current.cancelled = true
      queue.current = null
    },
    [workspace],
  )
}

// Posts one briefing request per crate, with at most two in flight. Each
// success refetches the list of the workspace and state that decided. A 409
// means the server has nothing to do, so the list refetches without a notice.
// Any other failure shows nothing and keeps the stored headline. No request
// is tried again.
async function requestListBriefings(
  queryClient: QueryClient,
  workspace: string,
  state: 'open' | 'closed',
  ids: string[],
  run: { cancelled: boolean },
) {
  let next = 0
  // The original list refetches even when a filter change left it inactive,
  // so the next view of it shows the new headline. A closed view or a
  // workspace change cancels the run and refetches nothing.
  const refetch = () => {
    if (!run.cancelled)
      void queryClient.invalidateQueries({
        queryKey: cratesQueryKey(workspace, state),
        exact: true,
        refetchType: 'all',
      })
  }
  const worker = async () => {
    while (!run.cancelled && next < ids.length) {
      const crateId = ids[next]
      next += 1
      try {
        await generateCrateBriefing(workspace, crateId)
        refetch()
      } catch (error) {
        if (error instanceof CrateBriefingError && error.status === 409) refetch()
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(listBriefingRequestLimit, ids.length) }, worker))
}

// The header and every row share these desktop tracks: the crate, the target
// date, and the completion pie with the chevron.
const rowColumns = 'md:grid-cols-[minmax(0,1fr)_10rem_4.5rem]'

// One crate row. The left cell holds the icon tile, the name, the headline of
// the stored briefing cut to two lines, and, below 768 px, the target date.
// The headline is plain text with no generated marker, model name, or time,
// and a crate with no briefing shows no line for it (component-crates
// invariant 17; DEC-144(7)). The right cell holds the completion pie and the
// chevron. The row link is the row's only control.
function CrateListRow({ crate }: { crate: CrateListItem }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <Link
      to="/w/$workspaceId/crates/$crateId"
      params={{ workspaceId: workspace, crateId: crate.id }}
      data-crate-row={crate.id}
      className={cn(
        'group grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 px-4 py-3.5 transition-colors hover:bg-surface focus-visible:bg-surface focus-visible:outline-none md:gap-x-6',
        rowColumns,
      )}
    >
      <span className="flex min-w-0 items-center gap-3" data-crate-row-main>
        <span className="grid size-8 shrink-0 place-items-center rounded-md bg-primary-soft text-primary">
          <Package aria-hidden className="size-4" />
        </span>
        <span className="min-w-0">
          <span className="flex min-w-0 items-center gap-2">
            <span className="truncate text-sm font-medium text-foreground" data-crate-name>
              {crate.name}
            </span>
            <CrateStateBadge crate={crate} />
          </span>
          {crate.briefing && (
            <span className="mt-0.5 line-clamp-2 text-xs leading-5 text-muted" data-crate-headline>
              {crate.briefing.headline}
            </span>
          )}
          <TargetDate crate={crate} className="mt-0.5 text-xs md:hidden" />
        </span>
      </span>
      <TargetDate crate={crate} className="hidden text-sm md:inline-flex" />
      <span className="flex items-center justify-end gap-3 md:justify-between" data-crate-row-end>
        <CompletionPie crate={crate} />
        <ChevronRight aria-hidden className="size-4 shrink-0 text-faint transition-colors group-hover:text-muted" />
      </span>
    </Link>
  )
}

// The completion share of component-crates invariant 9: merged members over
// merged and open members. A closed member counts toward neither, and with
// no counted member there is no share. The page formats the served counts
// and derives nothing else (invariant 21).
function crateCompletion(crate: Pick<Crate, 'merged_task_count' | 'open_task_count'>) {
  const merged = crate.merged_task_count
  const counted = merged + crate.open_task_count
  if (counted <= 0) return { share: null, label: 'No open or merged tasks' }
  const share = merged / counted
  // Only the stated percentage is rounded. The wedge and the empty and full
  // discs use the unrounded share.
  return { share, label: `${Math.round(share * 100)}% done · ${merged} of ${counted} merged` }
}

// A small disc whose filled wedge is the completion share (component-crates
// invariant 17; req-crates AC-5.1). Pointing at it shows the share as text.
// The same text is its accessible name. At rest the row shows no count or
// percentage text.
function CompletionPie({ crate }: { crate: Crate }) {
  const { share, label } = crateCompletion(crate)
  const fill = share == null || share <= 0 ? 'empty' : share >= 1 ? 'full' : 'partial'
  return (
    <span className="group/pie relative inline-flex shrink-0" data-completion-pie={fill}>
      <svg role="img" aria-label={label} viewBox="0 0 20 20" className="size-5">
        <circle cx="10" cy="10" r="9.5" className="fill-raised stroke-edge" strokeWidth="1" />
        {fill === 'full' && <circle cx="10" cy="10" r="10" className="fill-positive" />}
        {fill === 'partial' && share != null && <path d={wedgePath(share)} className="fill-positive" />}
      </svg>
      <span
        aria-hidden="true"
        data-completion-tooltip
        className="pointer-events-none invisible absolute top-1/2 right-full z-20 mr-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-edge bg-card px-2.5 py-1.5 text-xs leading-4 text-foreground shadow-md group-focus-visible:visible group-hover/pie:visible"
      >
        {label}
      </span>
    </span>
  )
}

// The wedge from twelve o'clock, clockwise, for a share strictly between 0
// and 1, on a disc of radius 10 centred at (10, 10).
function wedgePath(share: number) {
  const angle = share * 2 * Math.PI
  const x = 10 + 10 * Math.sin(angle)
  const y = 10 - 10 * Math.cos(angle)
  const large = share > 0.5 ? 1 : 0
  return `M10 10 L10 0 A10 10 0 ${large} 1 ${x.toFixed(3)} ${y.toFixed(3)} Z`
}

function EmptyCrates({ closed, canCreate, onCreate }: { closed: boolean; canCreate: boolean; onCreate: () => void }) {
  return (
    <div className="flex min-h-64 flex-col items-center justify-center rounded-lg border border-dashed border-border px-6 py-12 text-center">
      <span className="grid size-10 place-items-center rounded-lg bg-primary-soft text-primary">
        <Package aria-hidden className="size-5" />
      </span>
      <h2 className="mt-4 text-base font-semibold tracking-tight">{closed ? 'No closed crates' : 'No open crates'}</h2>
      <p className="mt-1.5 max-w-sm text-sm leading-6 text-muted">
        {closed
          ? 'A crate you close stays readable here, with its tasks and history.'
          : 'A crate groups the tasks of one delivery, with a target date and the requirements it serves, so you can follow its progress on one page.'}
      </p>
      {!closed && canCreate && (
        <Button className="mt-5" onClick={onCreate}>
          <Plus />
          New crate
        </Button>
      )}
    </div>
  )
}

function CrateListSkeleton() {
  return (
    <div className="space-y-px overflow-hidden rounded-lg border border-border">
      {[0, 1, 2].map((key) => (
        <div key={key} className="flex items-center gap-3 bg-card px-4 py-4">
          <Skeleton className="size-8" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-3.5 w-40" />
            <Skeleton className="h-2 w-64 max-w-full" />
          </div>
        </div>
      ))}
    </div>
  )
}
