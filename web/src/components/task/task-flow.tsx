import { Link } from '@tanstack/react-router'
import { Check } from 'lucide-react'
import { Fragment, type ReactNode, useLayoutEffect, useRef, useState } from 'react'
import type { TaskFlow, TaskFlowSideName, TaskFlowStage, TaskFlowStageId, TaskFlowStatus } from '../../lib/types'
import { cn } from '../../lib/utils'
import { Card, CardContent } from '../ui/card'
import { Skeleton } from '../ui/skeleton'
import { TaskFlowStageSheet } from './task-flow-stage-sheet'
import { useTaskFlow } from './use-task-flow'

// Below this much chart width the side column folds into chips above the
// path (component-web-task-surfaces, Activity tabs and the task flow view).
const sideColumnMinWidth = 640

// Node tones by the server's status; the words are always the server's.
const nodeTone: Record<TaskFlowStatus, string> = {
  done: 'border-primary bg-card text-foreground',
  in_progress: 'border-primary bg-primary-soft text-primary ring-2 ring-primary/20',
  waiting: 'border-dashed border-primary/70 bg-card text-foreground',
  needs_operator: 'border-attention bg-attention-soft text-attention',
  upcoming: 'border-border bg-card text-muted',
  off: 'border-dashed border-edge bg-surface text-faint',
}

const gateOutline: Record<TaskFlowStatus, string> = {
  done: 'stroke-primary fill-card',
  in_progress: 'stroke-primary fill-primary-soft',
  waiting: 'stroke-primary fill-card',
  needs_operator: 'stroke-attention fill-attention-soft',
  upcoming: 'stroke-edge fill-card',
  off: 'stroke-edge fill-surface',
}

const gateText: Record<TaskFlowStatus, string> = {
  done: 'text-foreground',
  in_progress: 'text-primary',
  waiting: 'text-foreground',
  needs_operator: 'text-attention',
  upcoming: 'text-muted',
  off: 'text-faint',
}

const dashed = (status: TaskFlowStatus) => status === 'waiting' || status === 'off'

// A side state's tone while present; an absent one is faint.
const sideTone: Record<TaskFlowSideName, string> = {
  agent_question: 'border-attention bg-attention-soft text-attention',
  blocked_by: 'border-primary bg-primary-soft text-foreground',
  retrying: 'border-primary bg-primary-soft text-primary',
  parked: 'border-attention bg-attention-soft text-attention',
  closed: 'border-edge bg-raised text-foreground',
}

const absentTone = 'border-dashed border-border bg-card text-faint'

// The order of the side column: the states a step can wait in, then, under
// the from-any-step rule, the two a task can leave by.
const sideOrder: TaskFlowSideName[] = ['agent_question', 'blocked_by', 'retrying', 'parked', 'closed']

type PathNode =
  | { kind: 'stage'; stage: TaskFlowStage; status: TaskFlowStatus; label: string; traversed: boolean }
  | { kind: 'gate'; stage: TaskFlowStage; status: TaskFlowStatus; label: string; traversed: boolean }

// The fixed main path: Triage, Plan, Plan approval, Implement, Verify, Review,
// Merge approval, Merged. The plan gate follows its stage and the merge gate
// precedes the merge it allows; the node facts are the server's.
function pathNodes(flow: TaskFlow): PathNode[] {
  const nodes: PathNode[] = []
  for (const stage of flow.stages) {
    const stageNode: PathNode = {
      kind: 'stage',
      stage,
      status: stage.status,
      label: stage.label,
      traversed: stage.traversed,
    }
    const gateNode: PathNode | null = stage.gate_status
      ? { kind: 'gate', stage, status: stage.gate_status, label: stage.gate_label, traversed: stage.gate_traversed }
      : null
    if (stage.id === 'merge') {
      if (gateNode) nodes.push(gateNode)
      nodes.push(stageNode)
    } else {
      nodes.push(stageNode)
      if (gateNode) nodes.push(gateNode)
    }
  }
  return nodes
}

// Node n sits on grid row 2n+1; the edge into it on row 2n.
const nodeRow = (index: number) => 2 * index + 1

// The task flowchart on task detail's Flow tab (feature-operator-next-action
// NA-7; req-operator-next-action AC-7.5 to AC-7.7). The server folds the flow
// and states every word; this component arranges geometry and classifies no
// lifecycle state. It prints no next-action summary: the card above it does
// (DEC-61).
export function TaskFlowChart({ taskId }: { taskId: string }) {
  const { data: flow, isLoading, error } = useTaskFlow(taskId)
  const [selectedId, setSelectedId] = useState<TaskFlowStageId | null>(null)
  const selected = flow?.stages.find((stage) => stage.id === selectedId)

  return (
    <section aria-label="Task flow" data-task-flow>
      <Card>
        <CardContent className="space-y-4">
          {isLoading && <Skeleton className="h-64" />}
          {error != null && <p className="text-sm text-failure">{String(error)}</p>}
          {flow && (
            <>
              <Legend flow={flow} />
              <FlowDiagram flow={flow} onSelect={setSelectedId} />
            </>
          )}
        </CardContent>
      </Card>
      {flow && selected && <TaskFlowStageSheet stage={selected} onClose={() => setSelectedId(null)} />}
    </section>
  )
}

function Legend({ flow }: { flow: TaskFlow }) {
  return (
    <ul aria-label="Legend" className="flex flex-wrap gap-x-4 gap-y-1.5 text-xs text-muted">
      {flow.display.legend.map((entry) => (
        <li key={entry.status} data-flow-legend={entry.status} className="flex items-center gap-1.5">
          <span aria-hidden className={cn('size-3 shrink-0 rounded-sm border', nodeTone[entry.status])} />
          {entry.label}
        </li>
      ))}
    </ul>
  )
}

// The chart's available width, from its own container rather than the
// viewport, so the same chart fits the Board sheet and the full route.
function useContainerWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    const element = ref.current
    if (!element) return
    setWidth(element.getBoundingClientRect().width)
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) setWidth(entry.contentRect.width)
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return [ref, width] as const
}

type Lane = { key: string; top: number; bottom: number; label: string; kind: 'loop' | 'bypass'; count?: number }

function FlowDiagram({ flow, onSelect }: { flow: TaskFlow; onSelect: (id: TaskFlowStageId) => void }) {
  const [ref, width] = useContainerWidth()
  const wide = width >= sideColumnMinWidth
  const nodes = pathNodes(flow)
  const stageIndex = new Map(
    nodes.flatMap((node, index) => (node.kind === 'stage' ? [[node.stage.id, index] as const] : [])),
  )
  const lanes: Lane[] = []
  const plan = flow.stages.find((stage) => stage.id === 'spec')
  const triage = stageIndex.get('triage')
  const implement = stageIndex.get('implement')
  if (plan?.status === 'off' && triage !== undefined && implement !== undefined) {
    lanes.push({
      key: 'bypass',
      top: nodeRow(triage),
      bottom: nodeRow(implement),
      label: flow.display.plan_bypass_label,
      kind: 'bypass',
    })
  }
  for (const loop of flow.loops) {
    const to = stageIndex.get(loop.to)
    const from = stageIndex.get(loop.from)
    // A loop runs back up the path; anything else is not a loop to draw.
    if (to === undefined || from === undefined || to >= from) continue
    lanes.push({
      key: `${loop.from}-${loop.to}`,
      top: nodeRow(to),
      bottom: nodeRow(from),
      label: loop.label,
      kind: 'loop',
      count: loop.count,
    })
  }
  // The longest span takes the outermost lane so no lane crosses another.
  lanes.sort((a, b) => b.bottom - b.top - (a.bottom - a.top))

  const laneColumn = (lane: number) => lane + 2
  const mainColumn = lanes.length + 2
  const columns = [
    'max-content',
    ...lanes.map(() => '1.5rem'),
    'minmax(8.5rem, 12rem)',
    ...(wide ? ['2rem', 'minmax(8rem, 11rem)'] : []),
  ].join(' ')
  const rows = nodes.map((_, index) => (index === 0 ? '2.5rem' : '1.25rem 2.5rem')).join(' ')

  return (
    <div ref={ref} className="min-w-0 space-y-3">
      {!wide && <SideChips flow={flow} />}
      <div
        data-flow-chart
        data-flow-layout={wide ? 'column' : 'chips'}
        className="grid justify-center"
        style={{ gridTemplateColumns: columns, gridTemplateRows: rows }}
      >
        {/* Reserve the width the lane labels overhang into. */}
        <div aria-hidden className="invisible h-0 overflow-hidden" style={{ gridColumn: 1, gridRow: 1 }}>
          {lanes.map((lane) => (
            <LanePill key={lane.key} lane={lane} />
          ))}
        </div>
        {lanes.map((lane, index) => (
          <div
            key={lane.key}
            data-flow-loop={lane.kind === 'loop' ? lane.key : undefined}
            data-flow-bypass={lane.kind === 'bypass' ? '' : undefined}
            className={cn(
              'relative my-[1.25rem] ml-[0.6875rem] rounded-l-md border-y-2 border-l-2',
              lane.kind === 'bypass' && !(implement !== undefined && nodes[implement].traversed)
                ? 'border-dashed border-edge'
                : 'border-primary',
            )}
            style={{ gridColumn: `${laneColumn(index)} / ${mainColumn}`, gridRow: `${lane.top} / ${lane.bottom + 1}` }}
          >
            <span
              aria-hidden
              className={cn(
                'absolute -right-px size-0 border-y-[5px] border-l-[7px] border-y-transparent border-l-primary',
                lane.kind === 'loop' ? '-top-[6px]' : '-bottom-[6px]',
              )}
            />
            <span className="absolute top-1/2 right-[calc(100%-0.5rem)] -translate-y-1/2">
              <LanePill lane={lane} />
            </span>
          </div>
        ))}
        {nodes.map((node, index) => (
          <Fragment key={`${node.kind}-${node.stage.id}`}>
            {index > 0 && (
              <div
                aria-hidden
                data-flow-edge={`${nodes[index - 1].kind}-${nodes[index - 1].stage.id}:${node.kind}-${node.stage.id}`}
                data-traversed={node.traversed}
                className={cn(
                  'mx-auto h-full w-0 border-l-2',
                  node.traversed ? 'border-solid border-primary' : 'border-dashed border-edge',
                )}
                style={{ gridColumn: mainColumn, gridRow: 2 * index }}
              />
            )}
            <div className="relative min-w-0" style={{ gridColumn: mainColumn, gridRow: nodeRow(index) }}>
              {node.kind === 'stage' ? (
                <StageNode node={node} skippedLabel={flow.display.skipped_label} onSelect={onSelect} />
              ) : (
                <GateNode node={node} skippedLabel={flow.display.skipped_label} />
              )}
            </div>
          </Fragment>
        ))}
        {wide && <SideColumn flow={flow} nodes={nodes} mainColumn={mainColumn} />}
      </div>
    </div>
  )
}

function LanePill({ lane }: { lane: Lane }) {
  return (
    <span
      data-flow-lane-label
      className={cn(
        'block w-max max-w-[6.5rem] rounded-full border bg-card px-2 py-0.5 text-center text-[10px] leading-3.5 font-medium',
        lane.kind === 'loop' ? 'whitespace-nowrap border-primary/60 text-primary' : 'border-transparent text-primary',
      )}
    >
      {lane.label}
    </span>
  )
}

function StageNode({
  node,
  skippedLabel,
  onSelect,
}: {
  node: Extract<PathNode, { kind: 'stage' }>
  skippedLabel: string
  onSelect: (id: TaskFlowStageId) => void
}) {
  const { stage, status } = node
  return (
    <button
      type="button"
      aria-haspopup="dialog"
      aria-label={`${node.label} — ${stage.status_label}`}
      data-flow-node={stage.id}
      data-status={status}
      className={cn(
        'relative flex h-10 w-full flex-col items-center justify-center rounded-md border-[1.5px] px-2 text-sm outline-none transition-colors focus-visible:ring-2 focus-visible:ring-primary/50 hover:brightness-[0.98]',
        nodeTone[status],
      )}
      onClick={() => onSelect(stage.id)}
    >
      <span className="max-w-full truncate font-medium leading-4">{node.label}</span>
      <NodeSubLabel status={status} label={stage.status_label} skippedLabel={skippedLabel} />
      {status === 'done' && <DoneBadge />}
    </button>
  )
}

function GateNode({ node, skippedLabel }: { node: Extract<PathNode, { kind: 'gate' }>; skippedLabel: string }) {
  const { stage, status } = node
  return (
    <div
      role="img"
      aria-label={`${node.label} — ${stage.gate_status_label}`}
      data-flow-gate={stage.id}
      data-status={status}
      className={cn('relative flex h-10 w-full flex-col items-center justify-center px-4 text-sm', gateText[status])}
    >
      <svg aria-hidden="true" viewBox="0 0 100 40" preserveAspectRatio="none" className="absolute inset-0 size-full">
        <polygon
          points="8,1 92,1 99,20 92,39 8,39 1,20"
          vectorEffect="non-scaling-stroke"
          strokeWidth={1.5}
          strokeDasharray={dashed(status) ? '4 3' : undefined}
          className={gateOutline[status]}
        />
      </svg>
      <span className="relative max-w-full truncate font-medium leading-4">{node.label}</span>
      <span className="relative">
        <NodeSubLabel status={status} label={stage.gate_status_label} skippedLabel={skippedLabel} />
      </span>
      {status === 'done' && <DoneBadge />}
    </div>
  )
}

// A node states what is not plain: skipped, or the server's status label while
// it is current. A done node carries a check badge instead.
function NodeSubLabel({
  status,
  label,
  skippedLabel,
}: {
  status: TaskFlowStatus
  label: string
  skippedLabel: string
}) {
  if (status === 'off') return <span className="text-[10px] leading-3">{skippedLabel}</span>
  if (status === 'in_progress' || status === 'waiting' || status === 'needs_operator')
    return <span className="max-w-full truncate text-[10px] leading-3">{label}</span>
  return null
}

function DoneBadge() {
  return (
    <span
      aria-hidden
      data-flow-done
      className="absolute -top-1.5 -right-1.5 flex size-4 items-center justify-center rounded-full bg-primary text-primary-foreground"
    >
      <Check className="size-2.5" strokeWidth={3} />
    </span>
  )
}

// What each side state says: its served label while present, the display
// label while absent, and, for a present blocker, each blocking task by title.
function sideStates(flow: TaskFlow) {
  return sideOrder.map((name) => {
    const present = flow.side.labels[name] !== undefined
    return {
      name,
      present,
      label: flow.side.labels[name] ?? flow.display.side_labels[name],
      blockers: name === 'blocked_by' ? flow.side.blocked_by : [],
    }
  })
}

function Blockers({ blockers }: { blockers: TaskFlow['side']['blocked_by'] }) {
  if (blockers.length === 0) return null
  return (
    <ul className="mt-0.5 space-y-0.5 text-[11px] leading-4 font-normal">
      {blockers.map((blocker) => (
        <li key={blocker.task_id} className="min-w-0">
          <Link
            to="/tasks/$taskId"
            params={{ taskId: blocker.task_id }}
            data-flow-blocker={blocker.task_id}
            className="block break-words text-foreground underline-offset-2 hover:underline"
          >
            {blocker.title || blocker.task_id}
          </Link>
        </li>
      ))}
    </ul>
  )
}

// The side states placed beside the path: each sits on a fixed row, and a
// present one is joined to the current stage by a connector.
function SideColumn({ flow, nodes, mainColumn }: { flow: TaskFlow; nodes: PathNode[]; mainColumn: number }) {
  const rowOf = (kind: PathNode['kind'], id: TaskFlowStageId) =>
    nodeRow(nodes.findIndex((node) => node.kind === kind && node.stage.id === id))
  const slots: Record<TaskFlowSideName, number> = {
    agent_question: rowOf('stage', 'spec'),
    blocked_by: rowOf('stage', 'implement'),
    retrying: rowOf('stage', 'verify'),
    parked: rowOf('stage', 'merge') - 2,
    closed: rowOf('stage', 'merge'),
  }
  const states = sideStates(flow)
  const current = flow.current_stage ? rowOf('stage', flow.current_stage) : undefined
  const presentRows = states.filter((state) => state.present).map((state) => slots[state.name])
  const connect = current !== undefined && current > 0 && presentRows.length > 0
  const busTop = connect ? Math.min(current, ...presentRows) : 0
  const busBottom = connect ? Math.max(current, ...presentRows) : 0
  const connector = mainColumn + 1
  const side = mainColumn + 2
  return (
    <>
      {connect && busBottom > busTop && (
        <div
          aria-hidden
          data-flow-connector="bus"
          className="mx-auto my-[1.25rem] w-0 border-l-2 border-primary/60"
          style={{ gridColumn: connector, gridRow: `${busTop} / ${busBottom + 1}` }}
        />
      )}
      {connect && (
        <div
          aria-hidden
          data-flow-connector="stage"
          className="mt-[1.1875rem] h-0 w-1/2 border-t-2 border-primary/60"
          style={{ gridColumn: connector, gridRow: current }}
        />
      )}
      <p
        className="self-end border-b border-border pb-0.5 text-center text-[10px] leading-3 text-faint"
        style={{ gridColumn: side, gridRow: slots.parked - 1 }}
      >
        {flow.display.from_any_step_label}
      </p>
      {states.map((state) => (
        <Fragment key={state.name}>
          {connect && state.present && (
            <div
              aria-hidden
              data-flow-connector={state.name}
              className="mt-[1.1875rem] ml-auto h-0 w-1/2 border-t-2 border-primary/60"
              style={{ gridColumn: connector, gridRow: slots[state.name] }}
            />
          )}
          <div
            data-flow-side={state.name}
            data-present={state.present}
            className={cn(
              'flex min-h-10 min-w-0 flex-col justify-center rounded-full border-[1.5px] px-3 py-1 text-center text-xs font-medium',
              state.present ? sideTone[state.name] : absentTone,
              state.blockers.length > 0 && 'rounded-xl',
            )}
            style={{ gridColumn: side, gridRow: slots[state.name] }}
          >
            <span>{state.label}</span>
            {state.present && <Blockers blockers={state.blockers} />}
          </div>
        </Fragment>
      ))}
    </>
  )
}

// Below the side column's width the same states, in the same order and with
// the same words, wrap as chips above the path.
function SideChips({ flow }: { flow: TaskFlow }) {
  return (
    <ul aria-label="Side states" data-flow-chips className="flex flex-wrap gap-1.5">
      {sideStates(flow).map((state) => (
        <li
          key={state.name}
          data-flow-side={state.name}
          data-present={state.present}
          className={cn(
            'max-w-full min-w-0 rounded-xl border-[1.5px] px-2.5 py-0.5 text-xs font-medium',
            state.present ? sideTone[state.name] : absentTone,
          )}
        >
          <ChipBody label={state.label}>{state.present && <Blockers blockers={state.blockers} />}</ChipBody>
        </li>
      ))}
    </ul>
  )
}

function ChipBody({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <span className="break-words">{label}</span>
      {children}
    </>
  )
}
