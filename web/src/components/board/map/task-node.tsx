// One task on the map. Zoomed in it is a card; zoomed out it collapses to a
// dot sized by how much work it unblocks.
import { Handle, type Node, type NodeProps, Position, useStore } from '@xyflow/react'
import {
  Ban,
  Check,
  CircleDot,
  Diamond,
  GitMerge,
  Hand,
  Hourglass,
  type LucideIcon,
  Pause,
  Play,
  X,
} from 'lucide-react'
import { memo } from 'react'
import { cn } from '../../../lib/utils'
import type { GraphStatus, GraphTask } from './graph-model'
import { labelOf, statusMeta } from './graph-model'
import { CARD_HEIGHT, CARD_WIDTH } from './layout'

// Three levels of detail: full cards, then a bold title (status and title
// are all that read at mid zoom), then dots.
export const DOT_ZOOM = 0.28
export const TITLE_ZOOM = 0.62

export const statusIcon: Record<GraphStatus, LucideIcon> = {
  blocked: Ban,
  plan_gate: Diamond,
  merge_gate: GitMerge,
  needs_you: Hand,
  running: Play,
  ready: CircleDot,
  parked: Pause,
  merged: Check,
  closed: X,
}

export type TaskNodeData = {
  task: GraphTask
  unblocks: number
  emphasis: 'focus' | 'chain' | 'dim' | 'normal'
  match: boolean
  // Set for a neighbour outside the document in view: where it lives.
  external?: string
  // Laid out top to bottom rather than left to right.
  vertical?: boolean
}

export type TaskFlowNode = Node<TaskNodeData, 'task'>

const hidden = '!size-1 !min-w-0 !min-h-0 !border-0 !bg-transparent'

export const TaskNode = memo(function TaskNode({ data }: NodeProps<TaskFlowNode>) {
  const detail = useStore((state) =>
    state.transform[2] < DOT_ZOOM ? 'dot' : state.transform[2] < TITLE_ZOOM ? 'title' : 'full',
  )
  const dots = detail === 'dot'
  const { task, unblocks, emphasis, match, external } = data
  const meta = statusMeta[task.status]
  const Icon = statusIcon[task.status]
  const done = task.status === 'merged' || task.status === 'closed'
  // Waiting on another task keeps the prototype's waiting treatment as a mark
  // beside the stated status, never in its place (req-board-map AC-2.2).
  const waiting = task.blocking.length > 0

  if (dots) {
    const size = 34 + Math.min(unblocks, 40) * 1.7
    const hollow = waiting || task.status === 'ready' || done
    return (
      <div
        className={cn('relative', emphasis === 'dim' && 'opacity-15')}
        style={{ width: CARD_WIDTH, height: CARD_HEIGHT }}
        title={`${task.title} · ${labelOf(task)}${unblocks ? ` · unblocks ${unblocks}` : ''}`}
      >
        <Handle
          type="target"
          position={data.vertical ? Position.Top : Position.Left}
          className={hidden}
          style={{ left: '50%', top: '50%' }}
        />
        <div
          className={cn(
            'absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 rounded-full border-[5px]',
            meta.ring,
            hollow ? 'bg-card' : meta.dot,
            waiting && 'border-dashed',
            done && 'opacity-50',
            emphasis === 'focus' && 'outline-[6px] outline-offset-4 outline-foreground',
            match && 'outline-[6px] outline-offset-4 outline-primary',
          )}
          style={{ width: size, height: size }}
        />
        <Handle
          type="source"
          position={data.vertical ? Position.Bottom : Position.Right}
          className={hidden}
          style={{ left: '50%', top: '50%' }}
        />
      </div>
    )
  }

  if (detail === 'title') {
    return (
      <div
        className={cn(
          'relative flex items-center gap-3 overflow-hidden rounded-lg border bg-card py-2 pl-4 pr-3',
          waiting || external ? 'border-dashed border-edge' : 'border-border',
          external && 'bg-surface opacity-80',
          done && 'opacity-55',
          emphasis === 'dim' && 'opacity-20',
          emphasis === 'focus' && 'ring-4 ring-foreground',
          match && 'ring-4 ring-primary',
        )}
        style={{ width: CARD_WIDTH, height: CARD_HEIGHT }}
        title={`${task.title} · ${labelOf(task)}`}
      >
        <Handle type="target" position={data.vertical ? Position.Top : Position.Left} className={hidden} />
        <span className={cn('absolute inset-y-0 left-0 w-2', meta.dot, waiting && 'opacity-40')} />
        <Icon aria-hidden className={cn('size-7 shrink-0', meta.tone)} />
        <p className="line-clamp-2 text-[21px] font-semibold leading-[1.15] text-foreground">{task.title}</p>
        <Handle type="source" position={data.vertical ? Position.Bottom : Position.Right} className={hidden} />
      </div>
    )
  }

  return (
    <div
      className={cn(
        'group relative flex flex-col justify-between overflow-hidden rounded-lg border bg-card px-3 py-2 shadow-sm',
        waiting || external ? 'border-dashed border-edge' : 'border-border',
        external && 'bg-surface opacity-80',
        done && 'opacity-55',
        emphasis === 'dim' && 'opacity-20',
        emphasis === 'focus' && 'ring-2 ring-foreground',
        emphasis === 'chain' && 'shadow-md',
        match && 'ring-2 ring-primary',
        'hover:shadow-md',
      )}
      style={{ width: CARD_WIDTH, height: CARD_HEIGHT }}
    >
      <Handle type="target" position={data.vertical ? Position.Top : Position.Left} className={hidden} />
      <span className={cn('absolute inset-y-0 left-0 w-1', meta.dot, waiting && 'opacity-40')} />
      <div className="flex items-center gap-1.5 text-[11px] font-medium">
        <Icon aria-hidden className={cn('size-3.5 shrink-0', meta.tone)} />
        <span className={meta.tone}>{labelOf(task)}</span>
        {waiting && <DependencyMark task={task} />}
        <span
          className="ml-auto rounded bg-surface px-1.5 font-mono text-[10.5px] text-muted"
          title={`${unblocks} open tasks wait on this`}
        >
          →{unblocks}
        </span>
      </div>
      <p className="line-clamp-2 text-[12.5px] font-medium leading-[1.25] text-foreground" title={task.title}>
        {task.title}
      </p>
      <p className="truncate font-mono text-[10.5px] text-faint">
        {external ? `↗ ${external}` : task.id}
        {!external && task.repo && ` · ${task.repo}`}
      </p>
      <Handle type="source" position={data.vertical ? Position.Bottom : Position.Right} className={hidden} />
    </div>
  )
})

// The blockers are named on hover or focus, as on Board cards
// (req-operator-next-action AC-3.4). A blocker that closed without merging
// takes the attention tone: the task cannot proceed until someone acts.
export function DependencyMark({ task }: { task: GraphTask }) {
  const closed = task.blocking.filter((blocker) => blocker.unsatisfiable)
  const explanation = closed.length
    ? `Needs attention: ${closed.map((blocker) => blocker.title).join(', ')} closed without merging`
    : `Waiting for ${task.blocking.map((blocker) => blocker.title).join(', ')}`
  return (
    <span
      role="img"
      aria-label={explanation}
      title={explanation}
      data-dependency-mark={closed.length ? 'unsatisfiable' : 'waiting'}
      className={cn('inline-flex shrink-0', closed.length ? 'text-attention' : 'text-faint')}
    >
      <Hourglass aria-hidden className="size-3.5" />
    </span>
  )
}
