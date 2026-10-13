import { Link } from '@tanstack/react-router'
import type { InboxItem, Task } from '../../lib/types'
import { NextActionDeadline, NextActionIDs } from '../task/next-action-parts'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import { NextActionChoices } from './choice-button'
import { useNextAction } from './use-next-actions'

// One task decision in the Inbox (AC-2.1). The list item carries only the
// compact next action, so the card reads the task's full next action for the
// question, choices, consequence, deadline and identifiers. Every line is
// server text.
export function DecisionCard({ item, task, onView }: { item: InboxItem; task: Task; onView: (task: Task) => void }) {
  const { data: action, isLoading } = useNextAction(task.id)
  const compact = item.next_action
  const pullRequest = item.pull_request
  return (
    <li
      data-inbox-item={task.id}
      className="min-w-0 space-y-2.5 rounded-lg border border-attention/40 bg-card px-4 py-4"
    >
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="attention">{action?.label ?? compact?.label}</Badge>
        {pullRequest?.issue_number != null &&
          (pullRequest.issue_url ? (
            <a href={pullRequest.issue_url} className="font-mono text-xs text-primary hover:underline">
              {pullRequest.repository}#{pullRequest.issue_number}
            </a>
          ) : (
            <span className="font-mono text-xs text-muted">
              {pullRequest.repository}#{pullRequest.issue_number}
            </span>
          ))}
        <Button size="sm" variant="ghost" className="ml-auto" onClick={() => onView(task)}>
          View
        </Button>
      </div>
      <h3 className="break-words font-medium leading-6">
        <Link to="/tasks/$taskId" params={{ taskId: task.id }} className="hover:underline">
          {task.title}
        </Link>
      </h3>
      {(item.serves?.length ?? 0) > 0 && (
        <p className="break-words text-xs text-muted">Serves {item.serves?.join(', ')}</p>
      )}
      <p className="text-sm">{action?.summary ?? compact?.summary}</p>
      {isLoading && <Skeleton className="h-12" />}
      {action?.question && <p className="text-sm font-medium">{action.question}</p>}
      {item.recommendation && <p className="text-sm text-muted">Recommendation: {item.recommendation}</p>}
      {action && <NextActionChoices taskId={task.id} choices={action.choices} />}
      {action?.if_ignored && <p className="text-xs text-muted">If ignored: {action.if_ignored}</p>}
      <NextActionDeadline deadline={action ? action.deadline : (item.deadline ?? compact?.deadline)} />
      {action && <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />}
    </li>
  )
}
