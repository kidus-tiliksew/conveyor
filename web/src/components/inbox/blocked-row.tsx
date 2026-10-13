import { Link } from '@tanstack/react-router'
import type { InboxItem, Task } from '../../lib/types'

// A task waiting on other tasks (AC-2.4). Each blocker links to its own
// inbox card when it is itself waiting on the operator, else to the task.
export function BlockedRow({
  item,
  task,
  decisionTaskIds,
}: {
  item: InboxItem
  task: Task
  decisionTaskIds: ReadonlySet<string>
}) {
  const blockers = item.blockers ?? item.next_action?.blocked_by ?? []
  return (
    <li data-inbox-item={task.id} className="min-w-0 space-y-1 px-4 py-3">
      <Link
        to="/tasks/$taskId"
        params={{ taskId: task.id }}
        className="break-words text-sm font-medium hover:underline"
      >
        {task.title}
      </Link>
      {item.next_action?.summary && <p className="text-xs text-muted">{item.next_action.summary}</p>}
      {blockers.length > 0 && (
        <p className="flex flex-wrap gap-x-2 gap-y-1 text-xs text-muted">
          <span>Waits for</span>
          {blockers.map((blocker) =>
            decisionTaskIds.has(blocker.task_id) ? (
              <Link
                key={blocker.task_id}
                to="/inbox"
                search={{ task: blocker.task_id }}
                className="break-all font-medium text-primary hover:underline"
              >
                {blocker.title || blocker.task_id}
              </Link>
            ) : (
              <Link
                key={blocker.task_id}
                to="/tasks/$taskId"
                params={{ taskId: blocker.task_id }}
                className="break-all font-medium text-primary hover:underline"
              >
                {blocker.title || blocker.task_id}
              </Link>
            ),
          )}
        </p>
      )}
    </li>
  )
}
