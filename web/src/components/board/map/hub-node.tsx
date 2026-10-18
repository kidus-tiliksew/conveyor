// A requirement as the node its tasks deliver into.
import { Handle, type Node, type NodeProps, Position, useStore } from '@xyflow/react'
import { BookText, FileCode2 } from 'lucide-react'
import { memo } from 'react'
import { cn } from '../../../lib/utils'
import type { GraphStatus } from './graph-model'
import { statusMeta } from './graph-model'
import { HUB_HEIGHT, HUB_WIDTH } from './layout'
import { statusIcon, TITLE_ZOOM } from './task-node'

export interface DocStats {
  total: number
  merged: number
  open: number
  byStatus: Partial<Record<GraphStatus, number>>
}

export type HubNodeData = {
  doc: { id: string; title: string; kind: 'requirement' | 'design' }
  stats: DocStats
  emphasis: 'focus' | 'chain' | 'dim' | 'normal'
  vertical?: boolean
}

export type HubFlowNode = Node<HubNodeData, 'hub'>

const hidden = '!size-1 !min-w-0 !min-h-0 !border-0 !bg-transparent'
const summaryOrder: GraphStatus[] = ['blocked', 'plan_gate', 'merge_gate', 'needs_you', 'running', 'ready', 'waiting']

export const HubNode = memo(function HubNode({ data }: NodeProps<HubFlowNode>) {
  const { doc, stats, emphasis } = data
  const Icon = doc.kind === 'requirement' ? BookText : FileCode2
  const delivered = stats.total ? stats.merged / stats.total : 0
  const far = useStore((state) => state.transform[2] < TITLE_ZOOM)
  if (far) {
    return (
      <div
        className={cn(
          'flex flex-col justify-between overflow-hidden rounded-xl border-2 bg-card p-4 shadow-md',
          emphasis === 'focus' ? 'border-foreground' : 'border-edge',
          emphasis === 'dim' && 'opacity-25',
        )}
        style={{ width: HUB_WIDTH, height: HUB_HEIGHT }}
      >
        <Handle type="target" position={data.vertical ? Position.Top : Position.Left} className={hidden} />
        <p className="line-clamp-2 text-[24px] font-bold leading-[1.15] text-foreground">{doc.title}</p>
        <div>
          <p className="mb-1.5 text-[17px] text-muted">
            {stats.merged}/{stats.total} delivered
          </p>
          <div className="h-3 overflow-hidden rounded-full bg-raised">
            <div className="h-full rounded-full bg-positive" style={{ width: `${delivered * 100}%` }} />
          </div>
        </div>
      </div>
    )
  }
  return (
    <div
      className={cn(
        'flex flex-col overflow-hidden rounded-xl border bg-card shadow-md',
        emphasis === 'focus' ? 'border-foreground ring-2 ring-foreground' : 'border-edge',
        emphasis === 'dim' && 'opacity-25',
      )}
      style={{ width: HUB_WIDTH, height: HUB_HEIGHT }}
    >
      <Handle type="target" position={data.vertical ? Position.Top : Position.Left} className={hidden} />
      <div className="flex items-center gap-1.5 border-b border-border bg-surface px-3 py-1.5 text-[11px] text-muted">
        <Icon aria-hidden className="size-3.5" />
        {doc.kind === 'requirement' ? 'Requirement' : 'System design'}
        <span className="ml-auto truncate font-mono text-[10.5px] text-faint">{doc.id}</span>
      </div>
      <div className="flex flex-1 flex-col justify-between px-3 py-2">
        <p className="line-clamp-2 text-[13.5px] font-semibold leading-snug text-foreground" title={doc.title}>
          {doc.title}
        </p>
        <div>
          <div className="mb-1 flex items-baseline justify-between text-[11px] text-muted">
            <span>
              {stats.merged} of {stats.total} delivered
            </span>
            <span className="font-mono text-faint">{stats.open} open</span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-raised">
            <div className="h-full rounded-full bg-positive" style={{ width: `${delivered * 100}%` }} />
          </div>
          <div className="mt-1.5 flex flex-wrap gap-x-2.5 gap-y-0.5">
            {summaryOrder
              .filter((status) => stats.byStatus[status])
              .map((status) => {
                const StatusIcon = statusIcon[status]
                return (
                  <span
                    key={status}
                    className="inline-flex items-center gap-1 text-[11px] text-muted"
                    title={statusMeta[status].label}
                  >
                    <StatusIcon aria-hidden className={cn('size-3', statusMeta[status].tone)} />
                    {stats.byStatus[status]}
                  </span>
                )
              })}
          </div>
        </div>
      </div>
    </div>
  )
})
