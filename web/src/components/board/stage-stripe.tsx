import { useEffect, useRef, useState } from 'react'
import { stageLabels } from '../../lib/contracts'
import type { TaskFlowStatus } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useTaskFlow } from '../task/use-task-flow'

const statusTone: Record<TaskFlowStatus, string> = {
  done: 'bg-positive',
  in_progress: 'bg-primary',
  waiting: 'bg-faint',
  needs_operator: 'bg-attention',
  upcoming: 'bg-border',
  off: 'bg-transparent border border-dashed border-border',
}

// A card's stage stripe (AC-3.2): one segment per stage in the server's flow
// projection, toned by the stage's status, with the approval gates drawn as
// points. The flow is read only while the card is on screen, so a full Board
// page does not issue a flow read per task up front.
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
    <div ref={ref} className="mt-2 h-1.5">
      {seen && <Segments taskId={taskId} />}
    </div>
  )
}

function Segments({ taskId }: { taskId: string }) {
  const { data: flow } = useTaskFlow(taskId)
  if (!flow) return null
  return (
    <ol aria-label="Stages" className="flex h-1.5 items-center gap-0.5">
      {flow.stages.map((stage) => (
        <li
          key={stage.id}
          data-stage={stage.id}
          data-status={stage.status}
          title={`${stageLabels[stage.id] ?? stage.id}: ${stage.status.replaceAll('_', ' ')}`}
          className="flex h-full flex-1 items-center gap-0.5"
        >
          {stage.gate === 'merge_approval' && <GatePoint gate={stage.gate} />}
          <span className={cn('h-full flex-1 rounded-full', statusTone[stage.status])} />
          {stage.gate && stage.gate !== 'merge_approval' && <GatePoint gate={stage.gate} />}
        </li>
      ))}
    </ol>
  )
}

function GatePoint({ gate }: { gate: string }) {
  return <span data-gate={gate} aria-hidden className="size-1.5 shrink-0 rotate-45 bg-foreground/60" />
}
