import { Link } from '@tanstack/react-router'
import { ArrowRight } from 'lucide-react'
import type { InboxItem, Task } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'

const chip = 'inline-flex min-w-0 max-w-full items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px]'

// A task waiting on other tasks (AC-2.4). Its blockers render as chips. A
// blocker that is itself a decision is marked "needs you" in the attention
// tone and selects that decision in the inbox; any other blocker links to its
// task (feature-operator-next-action NA-6).
export function BlockedRow({
  item,
  task,
  decisionTaskIds,
}: {
  item: InboxItem
  task: Task
  decisionTaskIds: ReadonlySet<string>
}) {
  const { workspace } = useWorkspaceSelection()
  const blockers = item.blockers ?? item.next_action?.blocked_by ?? []
  return (
    <li data-inbox-item={task.id} className="min-w-0 px-4 py-3">
      <Link
        to="/w/$workspaceId/tasks/$taskId"
        params={{ workspaceId: workspace, taskId: task.id }}
        title={task.title}
        className="block truncate text-sm font-medium hover:underline"
      >
        {task.title}
      </Link>
      {item.next_action?.summary && <p className="mt-0.5 text-xs text-muted">{item.next_action.summary}</p>}
      {blockers.length > 0 && (
        <p className="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-muted">
          <span>Waits for</span>
          {blockers.map((blocker) =>
            decisionTaskIds.has(blocker.task_id) ? (
              <Link
                key={blocker.task_id}
                data-blocker={blocker.task_id}
                data-blocker-needs-you
                to="/w/$workspaceId/inbox"
                params={{ workspaceId: workspace }}
                search={{ selected: blocker.task_id }}
                className={`${chip} border-attention/30 bg-attention-soft text-attention hover:underline`}
              >
                <ArrowRight className="size-3 shrink-0" aria-hidden />
                <span className="min-w-0 truncate">{blocker.title || blocker.task_id}</span>
                <span className="shrink-0 font-semibold">· needs you</span>
              </Link>
            ) : (
              <Link
                key={blocker.task_id}
                data-blocker={blocker.task_id}
                to="/w/$workspaceId/tasks/$taskId"
                params={{ workspaceId: workspace, taskId: blocker.task_id }}
                className={`${chip} border-border text-muted hover:text-foreground hover:underline`}
              >
                <ArrowRight className="size-3 shrink-0" aria-hidden />
                <span className="min-w-0 truncate">{blocker.title || blocker.task_id}</span>
              </Link>
            ),
          )}
        </p>
      )}
    </li>
  )
}
