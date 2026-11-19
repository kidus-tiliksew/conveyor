import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { ChevronRight, Package, Plus } from 'lucide-react'
import { useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import {
  CrateCounts,
  CrateFormDialog,
  CrateProgress,
  CrateStateBadge,
  TargetDate,
} from '../components/crate/crate-parts'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { ErrorState } from '../components/ui/error-state'
import { PageHeader } from '../components/ui/page-header'
import { SegmentedControl } from '../components/ui/segmented-control'
import { Skeleton } from '../components/ui/skeleton'
import { fetchCrates } from '../lib/api'
import { cratesQueryKey } from '../lib/query-keys'
import type { Crate } from '../lib/types'

export interface CratesSearch {
  // Closed crates show only on request (req-crates AC-5.1).
  show?: 'closed'
}

export function validateCratesSearch(search: Record<string, unknown>): CratesSearch {
  return { show: search.show === 'closed' ? 'closed' : undefined }
}

// The crates list (component-crates invariant 17; req-crates AC-5.1): each
// crate's name, target date, and open, merged, and total counts, in the order
// the server returns them. The page never re-sorts.
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
  })

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
                className="hidden grid-cols-[minmax(0,1fr)_10rem_minmax(10rem,14rem)_1rem] gap-x-6 border-b border-border px-4 py-2 text-xs font-medium text-faint md:grid"
              >
                <span>Crate</span>
                <span>Target date</span>
                <span>Tasks</span>
                <span />
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

function CrateListRow({ crate }: { crate: Crate }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <Link
      to="/w/$workspaceId/crates/$crateId"
      params={{ workspaceId: workspace, crateId: crate.id }}
      data-crate-row={crate.id}
      className="group grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-2 px-4 py-3.5 transition-colors hover:bg-surface focus-visible:bg-surface focus-visible:outline-none md:grid-cols-[minmax(0,1fr)_10rem_minmax(10rem,14rem)_1rem] md:gap-x-6"
    >
      <span className="flex min-w-0 items-center gap-3">
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
          <TargetDate crate={crate} className="mt-0.5 text-xs md:hidden" />
        </span>
      </span>
      <ChevronRight aria-hidden className="size-4 text-faint transition-colors group-hover:text-muted md:hidden" />
      <TargetDate crate={crate} className="hidden text-sm md:inline-flex" />
      <span className="col-span-2 flex min-w-0 flex-col gap-1.5 pl-11 md:col-span-1 md:pl-0">
        <CrateProgress crate={crate} />
        <CrateCounts crate={crate} />
      </span>
      <ChevronRight
        aria-hidden
        className="hidden size-4 text-faint transition-colors group-hover:text-muted md:block"
      />
    </Link>
  )
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
