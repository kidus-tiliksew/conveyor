import { Link } from '@tanstack/react-router'
import type { InboxItem, NextActionBlocker, Task } from '../../lib/types'
import { relativeTime } from '../../lib/utils'
import { GateChoices } from '../task/merge-at-gate'
import { NextActionDeadline, NextActionIDs } from '../task/next-action-parts'
import { Disclosure } from '../ui/disclosure'
import { Skeleton } from '../ui/skeleton'
import {
  ContextProposalBatch,
  ContextProposalError,
  ContextProposalRows,
  MonitorOriginLine,
  useContextProposalSends,
} from './context-proposal-rows'
import { useNextAction } from './use-next-actions'

// What the card's context link opens, by next-action kind (AC-2.3). A kind
// without in-place context opens the sheet's pointer to the task.
const contextLinks: Record<string, string> = {
  plan_approval: 'View plan',
  plan_revision: 'View plan',
  agent_question: 'View checkpoint',
  review_bounce_limit: 'View review',
  verification_disposition: 'View operation log',
  context_proposal: 'View proposed context',
}

// One task decision in the Inbox (AC-2.1; feature-operator-next-action NA-6).
// The list item carries only the compact next action, so the card reads the
// task's full next action for the question, choices, consequence, deadline
// and identifiers. The label, question, choices and consequence are server
// text; the card adds only its chrome.
export function DecisionCard({ item, task, onView }: { item: InboxItem; task: Task; onView: (task: Task) => void }) {
  const { data: action, isLoading } = useNextAction(task.id)
  const compact = item.next_action
  const kind = action?.kind ?? compact?.kind ?? ''
  const blocks = action?.blocks ?? compact?.blocks ?? []
  const pullRequest = item.pull_request
  const titles = new Map((task.context?.requirements ?? []).map((requirement) => [requirement.id, requirement.title]))
  const sends = useContextProposalSends(task.id)
  // A context decision lists one row per proposed document in place of the
  // flat choice row (NA-6).
  const contextRows = kind === 'context_proposal' && action?.details?.context_proposals ? action : undefined
  return (
    <li data-inbox-item={task.id} className="min-w-0 overflow-hidden rounded-lg border border-attention/40 bg-card">
      <div className="min-w-0 space-y-2.5 px-4 pb-3 pt-4">
        <div className="flex flex-wrap items-center gap-2">
          <span data-decision-label className="text-[11px] font-semibold uppercase tracking-[0.08em] text-attention">
            {action?.label ?? compact?.label}
          </span>
          {blocks.length > 0 && <BlocksChip blocks={blocks} />}
          {item.last_event_at && (
            <time dateTime={item.last_event_at} className="ml-auto text-xs text-faint">
              {relativeTime(item.last_event_at)}
            </time>
          )}
        </div>
        <h3 className="break-words font-medium leading-6">
          <Link to="/tasks/$taskId" params={{ taskId: task.id }} className="hover:underline">
            {task.title}
          </Link>
        </h3>
        <div className="flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted">
          {pullRequest?.issue_number != null ? (
            pullRequest.issue_url ? (
              <a href={pullRequest.issue_url} className="font-mono text-primary hover:underline">
                {pullRequest.repository}#{pullRequest.issue_number}
              </a>
            ) : (
              <span className="font-mono">
                {pullRequest.repository}#{pullRequest.issue_number}
              </span>
            )
          ) : (
            <span>No PR yet</span>
          )}
          {item.serves?.map((id) => (
            <span
              key={id}
              data-serves={id}
              className="inline-flex min-w-0 max-w-full items-center gap-1 rounded border border-border px-1.5 py-0.5 text-foreground"
            >
              <span className="font-mono text-primary">{id}</span>
              {titles.get(id) && <span className="min-w-0 truncate">{titles.get(id)}</span>}
            </span>
          ))}
        </div>
        {isLoading && <Skeleton className="h-12" />}
        {action?.question && <p className="text-sm leading-6">{action.question}</p>}
        {contextRows && (
          <div className="space-y-2">
            <MonitorOriginLine action={contextRows} />
            <ContextProposalRows action={contextRows} sends={sends} />
          </div>
        )}
        {item.recommendation && <p className="text-sm text-muted">Recommendation: {item.recommendation}</p>}
        <div className="text-xs">
          <button
            type="button"
            data-context-link
            className="text-xs font-medium text-primary hover:underline"
            onClick={() => onView(task)}
          >
            {contextLinks[kind] ?? 'View context'} →
          </button>
        </div>
      </div>
      <div data-decision-footer className="space-y-2 border-t border-border bg-surface/60 px-4 py-3">
        {/* Buttons inherit their font (styles.css), so the wrapper sets their size. */}
        {contextRows ? (
          <>
            <ContextProposalBatch action={contextRows} sends={sends} />
            <ContextProposalError sends={sends} />
          </>
        ) : (
          action && (
            <div className="text-[13px]">
              <GateChoices taskId={task.id} action={action} />
            </div>
          )
        )}
        <div className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-1 text-xs text-muted">
          {action?.if_ignored && (
            <p className="min-w-0 break-words">
              If you do nothing: <span className="font-medium text-foreground">{action.if_ignored}</span>
            </p>
          )}
          <NextActionDeadline deadline={action ? action.deadline : (item.deadline ?? compact?.deadline)} />
          {action && (
            <div className="ml-auto">
              <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />
            </div>
          )}
        </div>
      </div>
    </li>
  )
}

// The tasks waiting on this decision, named on hover, focus or tap.
function BlocksChip({ blocks }: { blocks: NextActionBlocker[] }) {
  return (
    <Disclosure
      className="text-[11px]"
      label={`Blocks ${blocks.length} ${blocks.length === 1 ? 'task' : 'tasks'}`}
      triggerClassName="gap-1.5 rounded bg-attention-soft px-1.5 py-0.5 text-[11px] font-medium text-attention"
      content={
        <ul className="space-y-0.5" data-blocks-list>
          {blocks.map((blocker) => (
            <li key={blocker.task_id} className="break-words">
              {blocker.title || blocker.task_id}
            </li>
          ))}
        </ul>
      }
    >
      <span aria-hidden className="size-1.5 rounded-full bg-attention-dot" />
      Blocks {blocks.length} {blocks.length === 1 ? 'task' : 'tasks'}
    </Disclosure>
  )
}
