import { BookOpenText } from 'lucide-react'
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

// While the first read is pending no task is loaded, so the detail header
// renders no Knowledge explorer and no Previous or Next control. This keeps
// their footprint, with nothing to click, focus, or read (the label width comes
// from generated content, not text), so the header does not move when the task
// arrives (component-web-task-surfaces).
export function TaskControlsPlaceholder({ part }: { part: 'explorer' | 'navigation' }) {
  const square = 'size-8 shrink-0 pointer-coarse:min-h-10 pointer-coarse:min-w-10'
  return (
    <span aria-hidden data-task-controls-placeholder={part} className="invisible flex shrink-0 items-center gap-1">
      {part === 'explorer' ? (
        <span
          data-label="Knowledge explorer"
          className="inline-flex h-7 shrink-0 items-center gap-1.5 whitespace-nowrap px-2.5 text-xs font-medium pointer-coarse:min-h-10 pointer-coarse:min-w-10 sm:after:content-[attr(data-label)] [&_svg]:size-3.5"
        >
          <BookOpenText />
        </span>
      ) : (
        <>
          <span className={square} />
          <span className={square} />
        </>
      )}
    </span>
  )
}
