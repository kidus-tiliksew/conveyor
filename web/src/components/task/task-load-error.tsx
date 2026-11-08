import { ErrorState } from '../ui/error-state'
import { isTaskNotFound } from './use-task-detail'

// The task detail read's failure, in the words of one page state. A missing task
// is a fact about the link, not a fault to retry.
export function TaskLoadError({
  error,
  retrying,
  onRetry,
}: {
  error: unknown
  retrying: boolean
  onRetry: () => void
}) {
  if (isTaskNotFound(error)) {
    return (
      <ErrorState title="Task not found" fallback="It may have moved to another workspace, or the link may be wrong." />
    )
  }
  return <ErrorState title="Could not load this task" error={error} retrying={retrying} onRetry={onRetry} />
}
