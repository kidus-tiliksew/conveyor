import { Link } from '@tanstack/react-router'
import { ArrowUpRight, CornerDownRight, Sparkles } from 'lucide-react'
import { errorMessage } from '../../lib/errors'
import type { InboxItem, InboxPullRequest, NextActionBlocker, Task } from '../../lib/types'
import { cn, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { GateChoices } from '../task/merge-at-gate'
import { NextActionIDs } from '../task/next-action-parts'
import { Button } from '../ui/button'
import { Disclosure } from '../ui/disclosure'
import { Skeleton } from '../ui/skeleton'
import {
  ContextProposalChecklist,
  ContextProposalFooter,
  MonitorOriginLine,
  useContextProposalSelection,
  useContextProposalSends,
} from './context-proposal-rows'
import { DecisionContext } from './inbox-context'
import { Kbd, ShortcutChoiceProvider, shortcutChoiceId } from './inbox-keyboard'
import { DueChip, KindPill, timeOfDay } from './inbox-kind'
import { useNextAction } from './use-next-actions'

// What the detail's context section holds, by next-action kind (AC-2.3).
const contextHeadings: Record<string, string> = {
  plan_approval: 'Plan',
  plan_revision: 'Plan',
  agent_question: 'Checkpoint',
  review_bounce_limit: 'Latest review',
  verification_disposition: 'Operation log',
  context_proposal: 'Proposed context',
  parked: 'Why it is parked',
  dispatch_exhausted: 'Why dispatch stopped',
}

const pill =
  'inline-flex min-w-0 max-w-full items-center gap-1 rounded-md border border-border bg-background/40 px-1.5 py-0.5'

// A recorded pull request as the inbox names it: "<repository> PR #<n>", or
// "PR" when the event stated no number (feature-operator-next-action NA-6).
export function pullRequestLabel(pullRequest: InboxPullRequest) {
  const name = pullRequest.number != null ? `PR #${pullRequest.number}` : 'PR'
  return pullRequest.repository ? `${pullRequest.repository} ${name}` : name
}

// The selected task decision in the Inbox's detail view (AC-2.1, AC-2.3,
// AC-2.8; DEC-82(1)). The list item carries only the compact next action, so
// the detail reads the task's full next action for the question, choices,
// consequence, deadline and identifiers, and renders the decision's context in
// place. The label, question, choices and consequence are server text; the
// detail adds only its chrome. `onActed` runs after each recorded act, so the
// page can move the selection when the item leaves (AC-2.10).
export function DecisionDetail({ item, task, onActed }: { item: InboxItem; task: Task; onActed: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const { data: action, isLoading, error, refetch, isFetching } = useNextAction(task.id)
  const compact = item.next_action
  const kind = action?.kind ?? compact?.kind ?? ''
  const blocks = action?.blocks ?? compact?.blocks ?? []
  const pullRequest = item.pull_request
  const issue = task.github?.issue_number != null ? task.github : undefined
  const titles = new Map((task.context?.requirements ?? []).map((requirement) => [requirement.id, requirement.title]))
  const sends = useContextProposalSends(task.id, onActed)
  const selection = useContextProposalSelection(task.id)
  // A context decision lists its proposed documents as a checklist in place
  // of the flat choice row, with Apply and Dismiss all in the footer (NA-6).
  const contextRows = kind === 'context_proposal' && action?.details?.context_proposals ? action : undefined
  const shortcut = action ? shortcutChoiceId(action) : null
  const deadline = action ? action.deadline : (item.deadline ?? compact?.deadline)
  const validDeadline = deadline && Number.isFinite(new Date(deadline).getTime()) ? deadline : undefined
  return (
    <ShortcutChoiceProvider choiceId={shortcut}>
      <article
        data-inbox-item={task.id}
        aria-label={task.title}
        className="flex min-h-0 min-w-0 flex-1 flex-col rounded-xl border border-border bg-card md:overflow-hidden"
      >
        <div className="min-h-0 min-w-0 flex-1 space-y-3 overflow-y-auto px-5 pb-5 pt-5">
          <div className="flex flex-wrap items-center gap-2">
            <KindPill kind={kind} label={action?.label ?? compact?.label ?? ''} />
            {blocks.length > 0 && <BlocksChip blocks={blocks} />}
            <DueChip deadline={validDeadline} className="py-0.5 text-xs" />
            {item.last_event_at && (
              <time dateTime={item.last_event_at} className="ml-auto text-xs text-faint">
                {relativeTime(item.last_event_at)}
              </time>
            )}
          </div>
          <h2 data-detail-title className="break-words text-[17px] font-semibold leading-snug">
            <Link
              to="/w/$workspaceId/tasks/$taskId"
              params={{ workspaceId: workspace, taskId: task.id }}
              className="hover:underline"
            >
              {task.title}
            </Link>
          </h2>
          <div className="flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-muted">
            {pullRequest ? (
              <a
                data-pull-request
                href={pullRequest.url}
                className={cn(pill, 'font-mono text-primary hover:border-primary/40')}
              >
                {pullRequestLabel(pullRequest)}
              </a>
            ) : (
              <span className={pill}>No PR yet</span>
            )}
            {issue &&
              (issue.issue_url ? (
                <a data-issue href={issue.issue_url} className={cn(pill, 'hover:text-foreground hover:underline')}>
                  Issue #{issue.issue_number}
                </a>
              ) : (
                <span data-issue className={pill}>
                  Issue #{issue.issue_number}
                </span>
              ))}
            {item.serves?.map((id) => (
              <span key={id} data-serves={id} className={cn(pill, 'text-foreground')}>
                <span className="font-mono text-primary">{id}</span>
                {titles.get(id) && <span className="min-w-0 truncate">{titles.get(id)}</span>}
              </span>
            ))}
            <Link
              to="/w/$workspaceId/tasks/$taskId/full"
              params={{ workspaceId: workspace, taskId: task.id }}
              className="ml-auto inline-flex items-center gap-0.5 font-medium text-muted hover:text-foreground"
            >
              Open task
              <ArrowUpRight className="size-3.5" aria-hidden />
            </Link>
          </div>
          {isLoading && <Skeleton className="h-12" />}
          {error != null && (
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="text-failure">{errorMessage(error, 'Could not load this decision.')}</span>
              <Button size="sm" variant="secondary" disabled={isFetching} onClick={() => void refetch()}>
                Retry
              </Button>
            </div>
          )}
          {action?.question && (
            <p data-decision-question className="break-words pt-1 text-[15px] font-medium leading-relaxed">
              {action.question}
            </p>
          )}
          {contextRows && (
            <div className="space-y-2">
              <MonitorOriginLine action={contextRows} />
              <ContextProposalChecklist action={contextRows} selection={selection} sends={sends} />
            </div>
          )}
          {item.recommendation && (
            <div
              data-recommendation
              className="flex gap-2.5 rounded-lg border border-primary/25 bg-primary-soft/50 px-3 py-2.5 text-sm"
            >
              <Sparkles className="mt-0.5 size-4 shrink-0 text-primary" aria-hidden />
              <div className="min-w-0">
                <span className="text-xs font-semibold text-primary">Agent recommends</span>
                <p className="mt-0.5 break-words leading-6">{item.recommendation}</p>
              </div>
            </div>
          )}
          {kind && (
            <div className="space-y-2 pt-2">
              <h3 className="flex items-center gap-2 text-xs font-semibold text-muted">
                <span data-context-heading>{contextHeadings[kind] ?? 'Context'}</span>
                <span aria-hidden className="h-px flex-1 bg-border" />
              </h3>
              <DecisionContext taskId={task.id} kind={kind} />
            </div>
          )}
        </div>
        <div
          data-decision-footer
          data-shortcut-merge={shortcut === 'merge' ? '' : undefined}
          className="z-10 shrink-0 space-y-2.5 rounded-b-xl border-t border-border bg-surface px-5 py-3 max-md:sticky max-md:bottom-0"
        >
          {action?.if_ignored && (
            <p data-if-ignored className="flex min-w-0 items-start gap-1.5 text-xs text-muted">
              <CornerDownRight className="mt-0.5 size-3.5 shrink-0" aria-hidden />
              <span className="min-w-0 break-words">
                {validDeadline ? (
                  <>
                    If nobody acts by{' '}
                    <time
                      data-next-action-deadline
                      dateTime={validDeadline}
                      title={new Date(validDeadline).toLocaleString()}
                    >
                      {timeOfDay(validDeadline)}
                    </time>
                    :{' '}
                  </>
                ) : (
                  'If nobody acts: '
                )}
                <span className="font-medium text-foreground">{action.if_ignored}</span>
              </span>
            </p>
          )}
          <div className="flex min-w-0 flex-wrap items-end gap-x-3 gap-y-2">
            {/* Buttons inherit their font (styles.css), so the wrapper sets their size. */}
            <div className="min-w-0 flex-1">
              {contextRows ? (
                <ContextProposalFooter action={contextRows} selection={selection} sends={sends} />
              ) : (
                action && (
                  <div className="text-[13px]">
                    <GateChoices taskId={task.id} action={action} onActed={onActed} onRestarted={onActed} />
                  </div>
                )
              )}
            </div>
            <div className="ml-auto flex shrink-0 items-center gap-3 text-[11px] text-faint">
              <span className="hidden items-center gap-1 lg:flex" data-footer-move-hint>
                <Kbd>j</Kbd>
                <Kbd>k</Kbd>
                move
              </span>
              {action && <NextActionIDs value={{ details: action.details, current_order: action.current_order }} />}
            </div>
          </div>
        </div>
      </article>
    </ShortcutChoiceProvider>
  )
}

// The tasks this decision unblocks, named on hover, focus or tap.
export function BlocksChip({ blocks }: { blocks: NextActionBlocker[] }) {
  return (
    <Disclosure
      className="text-[11px]"
      label={`Unblocks ${blocks.length} ${blocks.length === 1 ? 'task' : 'tasks'}`}
      triggerClassName="gap-1.5 rounded-md bg-attention-soft px-2 py-0.5 text-xs font-medium text-attention"
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
      Unblocks {blocks.length} {blocks.length === 1 ? 'task' : 'tasks'}
    </Disclosure>
  )
}
