import { useEffect, useRef, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlowStage, TaskOperationsItem } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useTaskFlow } from '../task/use-task-flow'
import { stageSegmentClass, stageSegmentName } from './stage-segments'

// A Board list row's Progress cell (req-operator-next-action AC-6.7, AC-6.8;
// component-web-task-surfaces, Board and Tasks list). A non-terminal task
// shows its current stage over one segment per stage of its flow; a terminal
// task names how it ended and links its pull request. The list shows progress
// whatever the card stage-progress preference holds, because that preference
// governs cards only (AC-3.3).
export function BoardListProgress({ item, operator }: { item: TaskOperationsItem; operator: boolean }) {
  const suffix = planStatusSuffix(item)
  if (item.task.state === 'merged' || item.task.state === 'closed') {
    return <TerminalProgress item={item} suffix={suffix} />
  }
  return <LiveProgress taskId={item.task.id} operator={operator} suffix={suffix} />
}

// A plan still asking for someone stays visible on its row (AC-1.4). When the
// next action is the plan gate itself, the operator line beneath the title
// already says so; otherwise the Progress label carries it.
export function planStatusSuffix(item: Pick<TaskOperationsItem, 'plan' | 'next_action'>): string | null {
  const { plan, next_action: action } = item
  if (plan.state !== 'pending_gate' && plan.state !== 'redirected') return null
  if (action?.actor === 'operator' && action.kind === 'plan_approval') return null
  const version = plan.version ? ` v${plan.version}` : ''
  return plan.state === 'pending_gate' ? `Plan awaiting approval${version}` : `Plan changes requested${version}`
}

function TerminalProgress({ item, suffix }: { item: TaskOperationsItem; suffix: string | null }) {
  const merged = item.task.state === 'merged'
  const pr = item.pull_request
  return (
    <p className="truncate text-xs leading-5" data-list-progress="terminal">
      <span className={merged ? 'font-medium text-foreground' : 'text-faint'}>{merged ? 'Merged' : 'Closed'}</span>
      {pr && (
        <>
          <span className="text-faint"> · </span>
          <a
            href={pr.url}
            target="_blank"
            rel="noreferrer"
            className="text-muted hover:text-foreground hover:underline"
          >
            {pr.number != null ? `PR #${pr.number}` : 'PR'}
          </a>
        </>
      )}
      {suffix && <span className="text-faint"> · {suffix}</span>}
    </p>
  )
}

// The flow is read only once the cell is on screen, so a page of rows does not
// issue a flow read per task up front (as StageStripe does for cards).
function LiveProgress({ taskId, operator, suffix }: { taskId: string; operator: boolean; suffix: string | null }) {
  const ref = useRef<HTMLDivElement>(null)
  const [seen, setSeen] = useState(false)
  useEffect(() => {
    const element = ref.current
    if (!element || seen) return
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) setSeen(true)
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [seen])
  return (
    <div ref={ref} className="min-w-0" data-list-progress={seen ? 'seen' : 'pending'}>
      {seen ? <FlowProgress taskId={taskId} operator={operator} suffix={suffix} /> : <Placeholder suffix={suffix} />}
    </div>
  )
}

// The one flow reader of the row: the label and the segments both read the
// flow this single useTaskFlow call returns.
function FlowProgress({ taskId, operator, suffix }: { taskId: string; operator: boolean; suffix: string | null }) {
  const { data: flow, isError } = useTaskFlow(taskId)
  if (!flow) {
    if (isError) {
      return (
        <p className="truncate text-xs leading-5 text-faint">
          Progress unavailable{suffix && <span> · {suffix}</span>}
        </p>
      )
    }
    return <Placeholder suffix={suffix} />
  }
  const current = flow.stages.find((stage) => stage.id === flow.current_stage)
  const faint = [current?.status === 'waiting' ? 'waiting' : '', suffix ?? ''].filter(Boolean)
  return (
    <>
      <p className="truncate text-xs leading-5" data-progress-label>
        {current && (
          <span className={cn('font-medium', operator ? 'text-attention' : 'text-foreground')}>
            {stageName(current)}
          </span>
        )}
        {faint.length > 0 && (
          <span className="text-faint">
            {current ? ' · ' : ''}
            {faint.join(' · ')}
          </span>
        )}
      </p>
      <ol aria-label="Stage progress" className="mt-1 flex h-1.5 items-center gap-0.5">
        {flow.stages.map((stage) => {
          const isCurrent = stage.id === flow.current_stage
          return (
            <li key={stage.id} className={cn('flex h-full min-w-0 items-center', isCurrent ? 'flex-[1.6]' : 'flex-1')}>
              <span
                role="img"
                aria-label={stageSegmentName(stage, stageName(stage))}
                data-stage={stage.id}
                data-status={stage.status}
                data-current={isCurrent ? 'true' : undefined}
                className={cn('block h-1.5 w-full min-w-0 rounded-full', stageSegmentClass(stage.status, 'list'))}
              />
            </li>
          )
        })}
      </ol>
    </>
  )
}

// The server names each node; the dashboard's own stage label is only the
// fallback for a flow that served none.
function stageName(stage: Pick<TaskFlowStage, 'id' | 'label'>) {
  return stage.label || stageLabels[stage.id] || stage.id
}

// Neutral until the flow arrives: no stage path is drawn before the server
// states one.
function Placeholder({ suffix }: { suffix: string | null }) {
  return (
    <>
      <p className="h-5 truncate text-xs leading-5 text-faint">{suffix}</p>
      <span aria-hidden="true" className="mt-1 block h-1.5 rounded-full bg-raised/60" />
    </>
  )
}
