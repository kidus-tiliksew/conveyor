// What a member task waits on and how many members wait on it, as the task
// list row, the line's card, and the branch map's card each state it
// (req-crate-insights AC-6.1, AC-6.2; component-crates invariant 39). Every
// title, mark, and count is the insight view's: one line for each served
// `waiting` entry, in the served order, and the length of `waited_on_by`.
import { CornerDownRight, CornerUpRight } from 'lucide-react'
import type { CrateInsightDependency, CrateInsightTask } from '../../lib/types'
import { cn } from '../../lib/utils'

// The marks after a title: a task outside the crate, and a task that closed
// without merging.
function marks(entry: CrateInsightDependency) {
  const notes = [!entry.member && 'outside this crate', entry.state === 'closed' && 'closed without merging'].filter(
    Boolean,
  )
  return notes.length > 0 ? ` (${notes.join(', ')})` : ''
}

export function waitedOnByText(count: number) {
  return count === 1 ? '1 task waits on this' : `${count} tasks wait on this`
}

// The number of lines these details state for the task.
export function dependencyLineCount(task: CrateInsightTask) {
  return task.depends_on.filter((entry) => entry.waiting).length + (task.waited_on_by.length > 0 ? 1 : 0)
}

// In the task list, a line for a member opens that task's detail as the row
// does (component-crates invariant 37), and a line for a task outside the
// crate is plain text. On a card, `onActivate` is absent and every line is
// text, because a card takes no pointer.
export function DependencyDetails({
  task,
  onActivate,
  className,
}: {
  // Absent until the insight view serves the task, and then nothing shows.
  task: CrateInsightTask | undefined
  onActivate?: (id: string) => void
  className?: string
}) {
  if (!task || dependencyLineCount(task) === 0) return null
  const waiting = task.depends_on.filter((entry) => entry.waiting)
  const count = task.waited_on_by.length
  return (
    <div data-dependency-details={task.id} className={cn('space-y-0.5 text-[11px] leading-4', className)}>
      {waiting.map((entry) => {
        const text = (
          <>
            <span className="text-muted">Waits on </span>
            <span className="text-foreground">{entry.title || entry.id}</span>
            <span className="text-faint">{marks(entry)}</span>
          </>
        )
        return (
          <p
            key={entry.id}
            data-waits-on={entry.id}
            data-outside={entry.member ? undefined : true}
            data-closed-unmerged={entry.state === 'closed' || undefined}
            className="flex min-w-0 items-start gap-1"
          >
            <CornerDownRight aria-hidden className="mt-px size-3 shrink-0 text-faint" />
            {onActivate && entry.member ? (
              <button
                type="button"
                onClick={() => onActivate(entry.id)}
                className="min-w-0 break-words rounded-sm text-left hover:underline focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-primary"
              >
                {text}
              </button>
            ) : (
              <span className="min-w-0 break-words">{text}</span>
            )}
          </p>
        )
      })}
      {count > 0 && (
        <p data-waited-on-by={count} className="flex min-w-0 items-start gap-1 text-muted">
          <CornerUpRight aria-hidden className="mt-px size-3 shrink-0 text-faint" />
          {waitedOnByText(count)}
        </p>
      )}
    </div>
  )
}
