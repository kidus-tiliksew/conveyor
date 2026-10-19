import type { TaskFlowStage, TaskFlowStatus } from '../../lib/types'

// Where a stage segment is drawn. A Board card's opt-in stripe keeps its
// outlined in-progress pill; a Board list row fills the stage under way and
// gives waiting a stronger tint, so the bar reads at a glance in a dense list
// (component-web-task-surfaces, Board and Tasks list).
export type StageSegmentPresentation = 'card' | 'list'

// The one status-to-tone mapping both stage-progress readers share
// (req-operator-next-action AC-3.2, AC-3.5, AC-6.7; feature-operator-next-action
// NA-7). It is exhaustive over the server's TaskFlowStatus, so a status the
// server adds fails the type check here instead of rendering untoned, and it
// maps only the served status: it derives no stage, status, or actor.
const segmentTones: Record<TaskFlowStatus, Record<StageSegmentPresentation, string>> = {
  done: { card: 'bg-primary', list: 'bg-primary' },
  // The card draws its in-progress pill as an overlay on an empty track.
  in_progress: { card: '', list: 'progress-sheen bg-primary' },
  waiting: { card: 'bg-primary/35', list: 'bg-primary/55' },
  needs_operator: { card: 'bg-attention-dot', list: 'bg-attention-dot' },
  upcoming: { card: 'bg-raised', list: 'bg-raised' },
  off: {
    card: 'h-0 rounded-none border-t-[1.5px] border-dashed border-edge bg-transparent',
    list: 'h-0 rounded-none border-t-[1.5px] border-dashed border-edge bg-transparent',
  },
}

export function stageSegmentClass(status: TaskFlowStatus, presentation: StageSegmentPresentation): string {
  return segmentTones[status][presentation]
}

// Every segment's accessible name: the stage, then the server's own word for
// its status, falling back to the machine status only when none was served.
export function stageSegmentName(stage: Pick<TaskFlowStage, 'status' | 'status_label'>, label: string): string {
  return `${label} — ${stage.status_label || stage.status.replaceAll('_', ' ')}`
}
