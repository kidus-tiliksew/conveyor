import { Link } from '@tanstack/react-router'
import { ChevronRight, ShieldCheck } from 'lucide-react'
import { Fragment, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlow, TaskFlowStage, TaskFlowStageId, TaskFlowStatus } from '../../lib/types'
import { cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Skeleton } from '../ui/skeleton'
import { flowStatusBadge, TaskFlowStageSheet } from './task-flow-stage-sheet'
import { useTaskFlow } from './use-task-flow'

// The statuses that mean the task has reached a stage. `upcoming` and `off`
// have not been reached.
const reachedStatuses: Record<TaskFlowStatus, boolean> = {
  done: true,
  in_progress: true,
  waiting: true,
  needs_operator: true,
  upcoming: false,
  off: false,
}

// The plan approval follows the plan it approves. The merge approval precedes
// the merge it allows.
const gateBefore = (stage: TaskFlowStage) => stage.gate === 'merge_approval'

type FlowNode =
  | { kind: 'stage'; stage: TaskFlowStage; reached: boolean }
  | { kind: 'gate'; stage: TaskFlowStage; gate: NonNullable<TaskFlowStage['gate']>; reached: boolean }

// The main column, top to bottom, with each gate placed around its stage. The
// server's statuses decide which nodes the task has reached. A stage that
// waits at the gate in front of it has reached the gate, not the stage.
function flowNodes(flow: TaskFlow): FlowNode[] {
  const nodes: FlowNode[] = []
  for (const stage of flow.stages) {
    const reached = reachedStatuses[stage.status]
    if (stage.gate && gateBefore(stage)) {
      nodes.push({ kind: 'gate', stage, gate: stage.gate, reached })
      nodes.push({ kind: 'stage', stage, reached: reached && stage.status !== 'needs_operator' })
    } else {
      nodes.push({ kind: 'stage', stage, reached })
      if (stage.gate) nodes.push({ kind: 'gate', stage, gate: stage.gate, reached: stage.status === 'needs_operator' })
    }
  }
  // An approval that follows its stage was passed once the next stage was reached.
  for (let index = nodes.length - 2; index >= 0; index--) {
    const node = nodes[index]
    if (node.kind === 'gate' && !gateBefore(node.stage) && nodes[index + 1].reached) node.reached = true
  }
  return nodes
}

// The task flow chart on every task-detail surface (feature-operator-next-action
// NA-7). The server folds the flow; this component draws it and nothing more.
export function TaskFlowChart({ taskId }: { taskId: string }) {
  const { data: flow, isLoading, error } = useTaskFlow(taskId)
  const [selectedId, setSelectedId] = useState<TaskFlowStageId | null>(null)
  const selected = flow?.stages.find((stage) => stage.id === selectedId)

  return (
    <section aria-label="Task flow">
      <Card>
        <CardHeader>
          <CardTitle>Task flow</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {isLoading && <Skeleton className="h-48" />}
          {error != null && <p className="text-sm text-failure">{String(error)}</p>}
          {flow && <FlowDiagram flow={flow} onSelect={setSelectedId} />}
          {flow?.next_action.summary && (
            <p data-flow-next-action className="border-t border-border pt-3 text-sm text-markdown">
              {flow.next_action.summary}
            </p>
          )}
        </CardContent>
      </Card>
      {flow && selected && <TaskFlowStageSheet stage={selected} raw={flow.raw} onClose={() => setSelectedId(null)} />}
    </section>
  )
}

function FlowDiagram({ flow, onSelect }: { flow: TaskFlow; onSelect: (id: TaskFlowStageId) => void }) {
  const nodes = flowNodes(flow)
  // Node n sits on grid row 2n+1, and the edge into it on row 2n.
  const stageRow = new Map(
    nodes.flatMap((node, index) => (node.kind === 'stage' ? [[node.stage.id, 2 * index + 1] as const] : [])),
  )
  // Longer loops take the outer gutters so no back-edge crosses another.
  const loops = flow.loops
    .flatMap((loop) => {
      const top = stageRow.get(loop.to)
      const bottom = stageRow.get(loop.from)
      return top !== undefined && bottom !== undefined && top < bottom ? [{ ...loop, top, bottom }] : []
    })
    .sort((a, b) => b.bottom - b.top - (a.bottom - a.top))
  const { side } = flow
  const hasSide = side.blocked_by.length > 0 || side.retrying || side.parked || side.closed || side.agent_question

  return (
    <div className="flex flex-col gap-4 sm:flex-row">
      <ol
        aria-label="Stages"
        className="grid min-w-0 flex-1"
        style={{ gridTemplateColumns: `repeat(${loops.length}, 1.5rem) minmax(0, 1fr)` }}
      >
        {loops.map((loop, column) => (
          <li
            key={`${loop.from}-${loop.to}`}
            data-flow-loop={`${loop.from}-${loop.to}`}
            aria-label={`${stageLabels[loop.from] ?? loop.from} back to ${stageLabels[loop.to] ?? loop.to}, ${loop.count}×`}
            className="relative my-4 ml-2 rounded-l-md border-y-2 border-l-2 border-edge"
            style={{ gridColumn: column + 1, gridRow: `${loop.top} / ${loop.bottom + 1}` }}
          >
            <ChevronRight aria-hidden className="absolute -right-2 -top-[9px] size-4 text-faint" />
            <span className="absolute top-1/2 -left-2 -translate-y-1/2 bg-card py-0.5 font-mono text-[11px] text-muted">
              ×{loop.count}
            </span>
          </li>
        ))}
        {nodes.map((node, index) => (
          <Fragment key={`${node.kind}-${node.stage.id}`}>
            {index > 0 && (
              <li
                aria-hidden
                data-flow-edge={node.reached ? 'visited' : 'upcoming'}
                className={cn(
                  'ml-4 h-3 border-l-2',
                  node.reached ? 'border-solid border-muted' : 'border-dashed border-edge',
                )}
                style={{ gridColumn: loops.length + 1, gridRow: 2 * index }}
              />
            )}
            <li style={{ gridColumn: loops.length + 1, gridRow: 2 * index + 1 }}>
              {node.kind === 'stage' ? (
                <StageNode stage={node.stage} onSelect={onSelect} />
              ) : (
                <GateNode gate={node.gate} waiting={node.stage.status === 'needs_operator'} />
              )}
            </li>
          </Fragment>
        ))}
      </ol>
      {hasSide && (
        <ul aria-label="Side conditions" className="flex flex-wrap content-start gap-2 sm:w-48 sm:flex-col">
          {side.blocked_by.map((blocker) => (
            <li
              key={blocker.task_id}
              data-flow-side="blocked_by"
              className="rounded-md border border-border p-2 text-xs"
            >
              <p className="font-mono text-muted">blocked_by</p>
              <Link
                to="/tasks/$taskId"
                params={{ taskId: blocker.task_id }}
                className="mt-1 block truncate font-medium text-foreground hover:underline"
              >
                {blocker.title || blocker.task_id}
              </Link>
              <p className="mt-0.5 flex flex-wrap gap-1.5 font-mono text-faint">
                <span>{blocker.task_id}</span>
                <span>{blocker.state}</span>
              </p>
            </li>
          ))}
          {side.retrying && <SideToken name="retrying" detail={`${side.retrying.attempt}/${side.retrying.of}`} />}
          {side.parked && <SideToken name="parked" />}
          {side.closed && <SideToken name="closed" />}
          {side.agent_question && <SideToken name="agent_question" />}
        </ul>
      )}
    </div>
  )
}

function StageNode({ stage, onSelect }: { stage: TaskFlowStage; onSelect: (id: TaskFlowStageId) => void }) {
  const off = stage.status === 'off'
  return (
    <Button
      variant="outline"
      size="sm"
      aria-haspopup="dialog"
      data-flow-stage={stage.id}
      data-status={stage.status}
      className={cn('h-8 w-full justify-start', off && 'opacity-60')}
      onClick={() => onSelect(stage.id)}
    >
      <span className={cn('min-w-0 truncate', off && 'text-faint')}>{stageLabels[stage.id] ?? stage.id}</span>
      {stage.status === 'in_progress' && (
        <span
          aria-hidden
          data-flow-live
          className="size-1.5 shrink-0 animate-pulse rounded-full bg-primary motion-reduce:animate-none"
        />
      )}
      <span className="ml-auto shrink-0">
        {off ? (
          <span className="text-xs font-normal text-faint">{stage.status_label}</span>
        ) : (
          <Badge variant={flowStatusBadge[stage.status]}>{stage.status_label}</Badge>
        )}
      </span>
    </Button>
  )
}

function GateNode({ gate, waiting }: { gate: string; waiting: boolean }) {
  return (
    <div data-flow-gate={gate} className="flex h-8 items-center gap-2 pl-3 text-xs text-muted">
      <ShieldCheck aria-hidden className="size-4 shrink-0 text-faint" />
      <span>{stageLabels.gate}</span>
      <Badge variant={waiting ? 'attention' : 'mono'}>{gate}</Badge>
    </div>
  )
}

function SideToken({ name, detail }: { name: string; detail?: string }) {
  return (
    <li data-flow-side={name} className="rounded-md border border-border p-2 font-mono text-xs text-muted">
      {name}
      {detail && <span className="ml-1.5 text-foreground">{detail}</span>}
    </li>
  )
}
