import { useEffect, useRef, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlowStatus } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useTaskFlow } from '../task/use-task-flow'
import { stageSegmentClass, stageSegmentName } from './stage-segments'

// A card's stage stripe, shown only when the viewer opts in
// (req-operator-next-action AC-3.2, AC-3.3, AC-7.5, AC-7.7; component-next-action
// invariants 28, 30, 39). One segment per stage in the server's flow, toned by
// the stage's status, and a diamond for each approval gate the frozen policy
// keeps, marked and named only by what the flow serves for that gate. The flow
// is read only once the card is on screen, so a full Board page does not issue
// a flow read per task up front, and the card unmounts this component entirely
// while the preference is off.
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
        const gate =
          stage.gate && stage.gate_status ? (
            <GateDiamond
              gate={stage.gate}
              status={stage.gate_status}
              name={`${stage.gate_label} — ${stage.gate_status_label}`}
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

// The one tone mapping of a gate's served mark. It is exhaustive over the
// server's TaskFlowStatus, so a status the server adds fails the type check
// here instead of rendering untoned. It maps only the served gate_status: the
// stage row's own status never marks a gate, so a stop that holds the stage
// without being the gate's decision leaves the gate's mark alone.
const gateTones: Record<TaskFlowStatus, string> = {
  done: 'size-2 bg-primary',
  in_progress: 'size-2 border-[1.5px] border-primary',
  waiting: 'size-2 bg-primary/35',
  needs_operator: 'size-2.5 bg-attention-dot ring-2 ring-attention-dot/40 ring-offset-1 ring-offset-card',
  upcoming: 'size-2 border-[1.5px] border-edge',
  off: 'size-2 border-[1.5px] border-dashed border-edge',
}

function GateDiamond({ gate, status, name }: { gate: string; status: TaskFlowStatus; name: string }) {
  return (
    <span
      role="img"
      aria-label={name}
      data-gate={gate}
      data-gate-state={status}
      className={cn('shrink-0 rotate-45 rounded-[1.5px]', gateTones[status])}
    />
  )
}
