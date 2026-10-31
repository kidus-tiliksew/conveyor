import { Link } from '@tanstack/react-router'
import type { FactoryAct, InboxItem, Task } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { ActsLog } from './acts-log'
import { NextActionChoices } from './choice-button'
import { KindIcon } from './inbox-kind'
import { useNextAction } from './use-next-actions'

// Work the factory carries on its own (AC-2.5), in one panel with the log of
// what it did. An item the server marks `no_judgment_needed` offers its one
// server choice; the rest only report.
export function AutomaticBand({ items, acts }: { items: InboxItem[]; acts: FactoryAct[] }) {
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      {items.length > 0 && (
        <ul className="divide-y divide-border">
          {items.map((item) => (item.task ? <AutomaticRow key={item.task.id} item={item} task={item.task} /> : null))}
        </ul>
      )}
      <ActsLog acts={acts} bordered={items.length > 0} />
    </div>
  )
}

function AutomaticRow({ item, task }: { item: InboxItem; task: Task }) {
  const { workspace } = useWorkspaceSelection()
  const { data: action } = useNextAction(task.id, Boolean(item.no_judgment_needed))
  return (
    <li data-inbox-item={task.id} className="min-w-0 space-y-1.5 px-4 py-3">
      <div className="flex min-w-0 items-center gap-2">
        <KindIcon kind={item.next_action?.kind ?? ''} />
        <Link
          to="/w/$workspaceId/tasks/$taskId"
          params={{ workspaceId: workspace, taskId: task.id }}
          title={task.title}
          className="min-w-0 truncate text-sm font-medium hover:underline"
        >
          {task.title}
        </Link>
        {item.next_action?.label && (
          <span data-automatic-label className="ml-auto shrink-0 rounded bg-surface px-1.5 text-[11px] text-muted">
            {item.next_action.label}
          </span>
        )}
      </div>
      {item.next_action?.summary && <p className="text-xs text-muted">{item.next_action.summary}</p>}
      {item.no_judgment_needed && action && <NextActionChoices taskId={task.id} choices={action.choices} />}
    </li>
  )
}
