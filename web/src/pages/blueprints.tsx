import { Link } from '@tanstack/react-router'
import { ArrowRight, Blocks, FileText, History } from 'lucide-react'
import { useBlueprints, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Card, CardContent } from '../components/ui/card'
import { PageHeader } from '../components/ui/page-header'
import { Skeleton } from '../components/ui/skeleton'
import { childRollup, deliveryLabel, deliveryTone } from '../lib/blueprint'
import { errorMessage } from '../lib/errors'
import type { BlueprintView } from '../lib/types'

// Historical Blueprints lens: retained records remain
// queryable, but this surface offers no creation path. Its navigation entry is
// now parked — the route stays mounted for deep
// links and reaches from blueprint-era task detail, and the page says plainly
// where live delivery is managed instead.
export function BlueprintsPage() {
  const { workspace } = useWorkspaceSelection()
  const { data: blueprints, isLoading, error } = useBlueprints()

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        title="Blueprint history"
        badge={<Badge variant="mono">{blueprints?.length ?? 0}</Badge>}
        description="Read-only records of legacy blueprints and the tasks they fanned out."
      />
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-5xl px-4 py-6 sm:px-6">
          <p className="mb-5 max-w-2xl text-sm text-muted">
            New delivery work is proposed through planning bundles and managed in{' '}
            <Link to="/w/$workspaceId/" params={{ workspaceId: workspace }} className="text-primary hover:underline">
              the Board
            </Link>
            .
          </p>
          {!workspace && <EmptyMessage>Choose a workspace to open its blueprints.</EmptyMessage>}
          {isLoading && workspace && <BlueprintListSkeleton />}
          {error != null && (
            <EmptyMessage tone="failure">{errorMessage(error, 'Could not load blueprints.')}</EmptyMessage>
          )}

          {blueprints?.length === 0 && (
            <Card className="mt-8 border-dashed">
              <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
                <History className="size-7 text-primary" />
                <h2 className="mt-4 text-base font-semibold">No historical blueprints</h2>
                <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                  This workspace has no legacy blueprint records.
                </p>
              </CardContent>
            </Card>
          )}

          {blueprints && blueprints.length > 0 && (
            <ul className="mt-7 space-y-3">
              {blueprints.map((view) => (
                <li key={view.task.id}>
                  <BlueprintListEntry view={view} />
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </div>
  )
}

function BlueprintListEntry({ view }: { view: BlueprintView }) {
  const { workspace } = useWorkspaceSelection()
  const specVersion = view.governing_version || view.spec?.version
  return (
    <Link
      to="/w/$workspaceId/blueprints/$taskId"
      params={{ workspaceId: workspace, taskId: view.task.id }}
      className="flex items-start gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-edge hover:bg-surface"
    >
      <Blocks className="mt-0.5 size-4 shrink-0 text-primary" />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <strong className="truncate text-sm font-medium">{view.task.title}</strong>
          <Badge variant={deliveryTone(view.delivery)}>{deliveryLabel(view.delivery)}</Badge>
          {specVersion ? <Badge variant="mono">Blueprint v{specVersion}</Badge> : null}
        </div>
        <p className="mt-1.5 text-xs text-muted">{childRollup(view.delivery)}</p>
        {view.planning_session?.model && (
          <p className="mt-1.5 flex flex-wrap gap-1.5 text-[11px] text-faint">
            <Badge variant="mono">
              {view.planning_session.model}
              {view.planning_session.effort ? ` · ${view.planning_session.effort}` : ''}
            </Badge>
            {view.planning_session.exploration_output_tokens ? (
              <Badge variant="mono">
                {view.planning_session.exploration_output_tokens.toLocaleString()} tokens/call
              </Badge>
            ) : null}
            {Object.entries(view.planning_session.pinned_revisions ?? {})
              .sort(([left], [right]) => left.localeCompare(right))
              .map(([repo, revision]) => (
                <Badge key={repo} variant="mono">
                  {repo}@{revision.slice(0, 12)}
                </Badge>
              ))}
          </p>
        )}
        {view.serves.length > 0 && (
          <p className="mt-1.5 flex flex-wrap items-center gap-1.5 text-[11px] text-faint">
            <FileText className="size-3 shrink-0" aria-hidden="true" />
            <span>Serves</span>
            {view.serves.map((requirement) => (
              <Badge key={requirement.id} variant="accent">
                {requirement.title}
              </Badge>
            ))}
          </p>
        )}
      </div>
      <ArrowRight className="mt-0.5 size-4 shrink-0 text-faint" />
    </Link>
  )
}

// The first load's placeholder: list entries with the list's spacing and an
// entry's border, padding, icon, title line, and rollup line, so the list
// that replaces it starts and sits where the placeholder did.
function BlueprintListSkeleton() {
  return (
    <div data-blueprints-skeleton aria-busy="true">
      <span className="sr-only">Loading blueprints…</span>
      <ul className="mt-7 space-y-3">
        {[0, 1, 2].map((entry) => (
          <li key={entry}>
            <div
              data-blueprints-skeleton-entry
              className="flex items-start gap-3 rounded-lg border border-border bg-card p-4"
            >
              <Skeleton className="mt-0.5 size-4 shrink-0" />
              <div className="min-w-0 flex-1">
                {/* The title line is as tall as the delivery badge every entry carries. */}
                <div className="flex h-[22px] items-center">
                  <Skeleton className="h-4 w-1/2" />
                </div>
                <Skeleton className="mt-1.5 h-4 w-1/3" />
              </div>
              <Skeleton className="mt-0.5 size-4 shrink-0" />
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}

function EmptyMessage({ children, tone = 'muted' }: { children: string; tone?: 'muted' | 'failure' }) {
  return (
    <p
      className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}
    >
      {children}
    </p>
  )
}
