import { ArrowRight, GitFork } from 'lucide-react'
import type { LineageGraph, LineageNodeType } from '../../lib/types'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'

const nodeLabels: Record<LineageNodeType, string> = {
  planning_session: 'Planning session', requirement: 'Requirement', requirement_version: 'Requirement version',
  blueprint: 'Blueprint', blueprint_version: 'Blueprint version', task: 'Task', work_order: 'Work order',
  pull_request: 'Pull request', commit_range: 'Commit range', evidence: 'Evidence', verdict: 'Verdict',
}

export function LineageGraphCard({ graph, title = 'Lineage graph' }: { graph: LineageGraph; title?: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <span className="flex items-center gap-1.5">
          {graph.truncated && <Badge variant="attention">Bounded view</Badge>}
          <Badge variant="mono">{graph.nodes.length} nodes · {graph.links.length} links</Badge>
        </span>
      </CardHeader>
      <CardContent>
        {graph.links.length === 0 && <p className="text-sm text-muted">Lineage appears as planning and delivery advance.</p>}
        {graph.links.length > 0 && (
          <details>
            <summary className="flex cursor-pointer list-none items-center gap-2 text-sm font-medium">
              <GitFork className="size-4 text-primary" /> Trace planning to delivery evidence
            </summary>
            <ol className="mt-3 space-y-2">
              {graph.links.map((link) => (
                <li key={`${link.created_by_event_id}-${link.src_type}-${link.src_id}-${link.kind}`} className="grid gap-1 rounded-md border border-border bg-surface px-3 py-2 text-xs sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] sm:items-center">
                  <LineageNode type={link.src_type} id={link.src_id} />
                  <span className="flex items-center gap-1 font-mono text-[10px] text-faint"><ArrowRight className="size-3" /> {humanize(link.kind)}</span>
                  <LineageNode type={link.dst_type} id={link.dst_id} />
                </li>
              ))}
            </ol>
          </details>
        )}
      </CardContent>
    </Card>
  )
}

function LineageNode({ type, id }: { type: LineageNodeType; id: string }) {
  return <span className="min-w-0"><span className="block text-[10px] uppercase tracking-wide text-faint">{nodeLabels[type]}</span><span className="block truncate font-mono" title={id}>{id}</span></span>
}

function humanize(value: string) { return value.replaceAll('_', ' ') }
