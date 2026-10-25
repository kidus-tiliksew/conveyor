import type { TaskFlowStage, TaskFlowStatus } from '../../lib/types'

// The one status-to-tone mapping of a Board card's opt-in stage stripe
// (req-operator-next-action AC-3.2, AC-3.5; feature-operator-next-action NA-7).
// It is exhaustive over the server's TaskFlowStatus, so a status the server
// adds fails the type check here instead of rendering untoned, and it maps only
// the served status: it derives no stage, status, or actor.
const segmentTones: Record<TaskFlowStatus, string> = {
  done: 'bg-primary',
  // The card draws its in-progress pill as an overlay on an empty track.
  in_progress: '',
  waiting: 'bg-primary/35',
  needs_operator: 'bg-attention-dot',
  upcoming: 'bg-raised',
  off: 'h-0 rounded-none border-t-[1.5px] border-dashed border-edge bg-transparent',
}

export function stageSegmentClass(status: TaskFlowStatus): string {
  return segmentTones[status]
}

// Every segment's accessible name: the stage, then the server's own word for
// its status, falling back to the machine status only when none was served.
export function stageSegmentName(stage: Pick<TaskFlowStage, 'status' | 'status_label'>, label: string): string {
  return `${label} — ${stage.status_label || stage.status.replaceAll('_', ' ')}`
}
