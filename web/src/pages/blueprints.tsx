import { Link } from '@tanstack/react-router'
import { ArrowRight, Blocks, FileText, History } from 'lucide-react'
import { useBlueprints, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Card, CardContent } from '../components/ui/card'
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
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <header className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-xl font-semibold tracking-tight">Blueprint history</h1>
              <Badge variant="mono">{blueprints?.length ?? 0}</Badge>
            </div>
            <p className="mt-1 max-w-2xl text-sm text-muted">
              Read-only records of legacy blueprints and the tasks they fanned out. New delivery work is proposed
              through planning bundles and managed in{' '}
              <Link to="/tasks" className="text-primary hover:underline">
                Tasks
              </Link>
              .
            </p>
          </div>
        </header>

        {!workspace && <EmptyMessage>Choose a workspace to open its blueprints.</EmptyMessage>}
        {isLoading && workspace && <EmptyMessage>Loading blueprints…</EmptyMessage>}
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
  )
}

function BlueprintListEntry({ view }: { view: BlueprintView }) {
  const specVersion = view.governing_version || view.spec?.version
  return (
    <Link
      to="/blueprints/$taskId"
      params={{ taskId: view.task.id }}
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

function EmptyMessage({ children, tone = 'muted' }: { children: string; tone?: 'muted' | 'failure' }) {
  return (
    <p
      className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}
    >
      {children}
    </p>
  )
}
