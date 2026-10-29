import { Link } from '@tanstack/react-router'
import type { FactoryAct, InboxItem, Task } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { ActsLog } from './acts-log'
import { NextActionChoices } from './choice-button'
import { useNextAction } from './use-next-actions'

// Work the factory carries on its own (AC-2.5). An item the server marks
// `no_judgment_needed` offers its one server choice; the rest only report.
export function AutomaticBand({ items, acts }: { items: InboxItem[]; acts: FactoryAct[] }) {
  return (
    <div className="space-y-3">
      {items.length > 0 && (
        <ul className="divide-y divide-border rounded-lg border border-border bg-card">
          {items.map((item) => (item.task ? <AutomaticRow key={item.task.id} item={item} task={item.task} /> : null))}
        </ul>
      )}
      <ActsLog acts={acts} />
    </div>
  )
}

function AutomaticRow({ item, task }: { item: InboxItem; task: Task }) {
  const { workspace } = useWorkspaceSelection()
  const { data: action } = useNextAction(task.id, Boolean(item.no_judgment_needed))
  return (
    <li data-inbox-item={task.id} className="min-w-0 space-y-1.5 px-4 py-3">
      <Link
        to="/w/$workspaceId/tasks/$taskId"
        params={{ workspaceId: workspace, taskId: task.id }}
        className="break-words text-sm font-medium hover:underline"
      >
        {task.title}
      </Link>
      {item.next_action?.summary && <p className="text-xs text-muted">{item.next_action.summary}</p>}
      {item.no_judgment_needed && action && <NextActionChoices taskId={task.id} choices={action.choices} />}
    </li>
  )
}
