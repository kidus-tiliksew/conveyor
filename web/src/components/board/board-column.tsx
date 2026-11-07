import { Link } from '@tanstack/react-router'
import {
  Archive,
  Code2,
  FileText,
  GitMerge,
  GitPullRequest,
  ListFilter,
  type LucideIcon,
  ShieldCheck,
} from 'lucide-react'
import type { GroupKey } from '../../lib/contracts'
import type { ActivitySummary } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { TaskCard } from './task-card'

// Neutral column identity: position and icon carry the stage; color is
// reserved for state (attention/positive) per the visual-economy rule.
const groupIcons: Record<GroupKey, LucideIcon> = {
  triage: ListFilter,
  spec: FileText,
  implement: Code2,
  review: GitPullRequest,
  verify: ShieldCheck,
  merge: GitMerge,
  done: Archive,
}

// Terminal work archives under "Completed"; keep the column light so the
// board stays a WIP view rather than a history dump.
const DONE_CAP = 30

export function BoardColumn({
  groupKey,
  label,
  items,
  selectedId,
}: {
  groupKey: GroupKey
  label: string
  items: ActivitySummary[]
  selectedId?: string
}) {
  const { workspace } = useWorkspaceSelection()
  const Icon = groupIcons[groupKey]
  // The items in this column waiting on an operator, by the server's actor;
  // they are decided in the Inbox, so the count links there.
  const operator = items.filter((item) => item.next_action?.actor === 'operator').length
  const visible = groupKey === 'done' ? items.slice(0, DONE_CAP) : items
  // Every stage keeps the same lane width as work moves or filters change
  // which columns are populated. The shared flex rule fills spare row width
  // evenly and preserves horizontal scrolling once all seven lanes no longer
  // fit.
  const idle = items.length === 0
  return (
    <section
      aria-label={label}
      data-lane-populated={idle ? undefined : ''}
      className="flex h-full min-w-[14.5rem] flex-[3_1_14.5rem] snap-start flex-col"
    >
      <header className="flex h-9 shrink-0 items-center gap-2 px-1">
        <Icon className="size-3.5 shrink-0 text-faint" />
        <h2 className="truncate text-[13px] font-semibold text-foreground">{label}</h2>
        <span className="text-xs tabular-nums text-faint">{items.length}</span>
        {operator > 0 && (
          <Link
            to="/w/$workspaceId/inbox"
            params={{ workspaceId: workspace }}
            className="ml-auto flex"
            aria-label={`${operator} waiting on an operator — open the Inbox`}
            data-operator-count
          >
            <Badge variant="attention">{operator}</Badge>
          </Link>
        )}
      </header>
      {!idle && (
        <div className="min-h-0 flex-1 space-y-2 overflow-y-auto px-px pb-2 pt-px">
          {visible.map((item) => (
            <TaskCard key={item.task.id} item={item} selected={item.task.id === selectedId} />
          ))}
          {items.length > visible.length && (
            <p className="px-2 py-1.5 text-center text-xs text-faint">
              +{items.length - visible.length} more completed
            </p>
          )}
        </div>
      )}
    </section>
  )
}
