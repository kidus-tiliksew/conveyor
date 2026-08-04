import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { useBlueprints } from '../components/app-shell'
import { BlueprintDetail, BlueprintDetailFallback } from '../components/blueprint/blueprint-detail'
import { useTaskDetail } from '../components/task/use-task-detail'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { findBlueprint, isBlueprintAnchor } from '../lib/blueprint'
import { errorMessage } from '../lib/errors'

// The canonical home of a materialized blueprint (spec §21.49 change 1). The
// anchor is an approved delivery contract, so it gets its own route rather than
// borrowing the task view's costume — and the route header goes back to the
// Blueprints list, not to the board, because a blueprint was never on it.
//
// Two reads back this page: the blueprint projection carries what was promised
// (spec, children, delivery, serves, lineage) and the task detail carries the
// events the batch timeline renders.
export function BlueprintDetailPage() {
  const { taskId } = useParams({ from: '/blueprints/$taskId' })
  const { data: blueprints, isLoading: blueprintsLoading, error: blueprintsError } = useBlueprints()
  const { data: item, isLoading, error } = useTaskDetail(taskId)
  const view = findBlueprint(blueprints, taskId)

  return (
    <div className="flex h-full flex-col">
      <header className="flex shrink-0 items-center gap-1 border-b border-border px-4 py-2.5">
        <Link to="/blueprints" aria-label="Back to blueprints">
          <Button variant="ghost" size="icon" tabIndex={-1}>
            <ArrowLeft />
          </Button>
        </Link>
        <span className="mr-auto truncate text-sm font-medium text-muted">{view?.task.title ?? item?.task.title}</span>
      </header>
      {(error != null || blueprintsError != null) && (
        <p className="px-6 py-6 text-sm text-failure">
          {errorMessage(error ?? blueprintsError, 'Could not load this blueprint.')}
        </p>
      )}
      {error == null && blueprintsError == null && (
        <section aria-label="Blueprint content" className="min-h-0 flex-1 overflow-y-auto">
          <BlueprintBody taskId={taskId} view={view} item={item} loading={isLoading || blueprintsLoading} />
        </section>
      )}
    </div>
  )
}

function BlueprintBody({
  taskId,
  view,
  item,
  loading,
}: {
  taskId: string
  view: ReturnType<typeof findBlueprint>
  item: ReturnType<typeof useTaskDetail>['data']
  loading: boolean
}) {
  if (view && item) return <BlueprintDetail view={view} item={item} />
  if (loading) {
    return (
      <div className="space-y-3 px-6 py-6">
        <Skeleton className="h-24" />
        <Skeleton className="h-64" />
      </div>
    )
  }
  // Loaded, and this task is ordinary work — say so plainly and point at the
  // view that does belong to it rather than bouncing between two routes.
  if (item && !isBlueprintAnchor(item.task)) {
    return (
      <div className="px-6 py-6">
        <p className="rounded-md border border-border bg-surface p-4 text-sm text-muted">
          This task is not a blueprint — it has no plan that fanned out into child tasks.{' '}
          <Link to="/tasks/$taskId/full" params={{ taskId }} className="text-primary hover:underline">
            Open the task
          </Link>
          .
        </p>
      </div>
    )
  }
  return (
    <div className="px-6 py-6">
      <BlueprintDetailFallback />
    </div>
  )
}
