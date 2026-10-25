import { useEffect, useRef, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlowStage } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useTaskFlow } from '../task/use-task-flow'
import { stageSegmentClass, stageSegmentName } from './stage-segments'

type GateState = 'upcoming' | 'passed' | 'needs_operator'

// A card's stage stripe, shown only when the viewer opts in
// (req-operator-next-action AC-3.2, AC-3.3; feature-operator-next-action NA-7).
// One segment per stage in the server's flow, toned by the stage's status, and
// a diamond for each approval gate the frozen policy keeps. The flow is read
// only once the card is on screen, so a full Board page does not issue a flow
// read per task up front, and the card unmounts this component entirely while
// the preference is off.
export function StageStripe({ taskId }: { taskId: string }) {
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
    <div ref={ref} data-stage-stripe className="mt-2.5 h-3">
      {seen && <Segments taskId={taskId} />}
    </div>
  )
}

function Segments({ taskId }: { taskId: string }) {
  const { data: flow } = useTaskFlow(taskId)
  if (!flow) return null
  return (
    <ol aria-label="Stage progress" className="flex h-3 items-center gap-1">
      {flow.stages.map((stage) => {
        const label = stageLabels[stage.id] ?? stage.id
        const gate = stage.gate ? (
          <GateDiamond
            gate={stage.gate}
            state={gateState(stage)}
            name={`${gateLabels[stage.gate] ?? stage.gate} — ${gateStateLabels[gateState(stage)]}`}
          />
        ) : null
        return (
          <li
            key={stage.id}
            className={cn(
              'flex h-full min-w-0 items-center gap-1',
              stage.status === 'in_progress' ? 'flex-[1.6]' : 'flex-1',
            )}
          >
            {stage.gate === 'merge_approval' && gate}
            <span
              role="img"
              aria-label={stageSegmentName(stage, label)}
              data-stage={stage.id}
              data-status={stage.status}
              className={cn('relative block h-1.5 min-w-0 flex-1 rounded-full', stageSegmentClass(stage.status))}
            >
              {stage.status === 'in_progress' && <InProgress />}
            </span>
            {stage.gate && stage.gate !== 'merge_approval' && gate}
          </li>
        )
      })}
    </ol>
  )
}

// The stage under way: an outlined pill, filled part way and ending in a dot.
function InProgress() {
  return (
    <span className="absolute inset-x-0 -inset-y-0.5 rounded-full ring-[1.5px] ring-primary">
      <span className="absolute inset-y-[3px] left-[3px] w-3/5 rounded-full bg-primary" />
      <span className="absolute left-[calc(60%-1px)] top-1/2 size-2 -translate-y-1/2 rounded-full bg-primary ring-2 ring-card" />
    </span>
  )
}

const gateLabels: Record<string, string> = {
  plan_approval: 'Plan approval',
  merge_approval: 'Merge approval',
}

const gateStateLabels: Record<GateState, string> = {
  upcoming: 'upcoming',
  passed: 'passed',
  needs_operator: 'waiting for you',
}

// A gate reads from the stage it guards: the plan approval follows the plan, so
// it has passed once the plan is done; the merge approval precedes the merge,
// so it has passed once the merge is under way or done. Either waits on the
// operator while its stage does.
function gateState(stage: TaskFlowStage): GateState {
  if (stage.status === 'needs_operator') return 'needs_operator'
  if (stage.gate === 'plan_approval') return stage.status === 'done' ? 'passed' : 'upcoming'
  return stage.status === 'done' || stage.status === 'in_progress' ? 'passed' : 'upcoming'
}

function GateDiamond({ gate, state, name }: { gate: string; state: GateState; name: string }) {
  return (
    <span
      role="img"
      aria-label={name}
      data-gate={gate}
      data-gate-state={state}
      className={cn(
        'shrink-0 rotate-45 rounded-[1.5px]',
        state === 'needs_operator'
          ? 'size-2.5 bg-attention-dot ring-2 ring-attention-dot/40 ring-offset-1 ring-offset-card'
          : state === 'passed'
            ? 'size-2 bg-primary'
            : 'size-2 border-[1.5px] border-edge',
      )}
    />
  )
}
