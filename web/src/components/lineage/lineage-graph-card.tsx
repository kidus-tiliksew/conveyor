import { ArrowRight, GitFork } from 'lucide-react'
import type { LineageGraph, LineageNodeType } from '../../lib/types'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'

const nodeLabels: Record<LineageNodeType, string> = {
  planning_session: 'Planning session', requirement: 'Requirement', requirement_version: 'Requirement version',
  blueprint: 'Blueprint', blueprint_version: 'Blueprint version', task: 'Task', work_order: 'Work order',
  pull_request: 'Pull request', commit_range: 'Commit range', evidence: 'Evidence', verdict: 'Verdict',
}

const edgeLabels: Record<string, string> = {
  produced_requirement: 'produced requirement', produced_blueprint: 'produced blueprint', serves: 'serves',
  versions: 'has version', supersedes: 'supersedes', materializes: 'materializes child', depends_on: 'depends on',
  dispatches: 'dispatches as', submitted_as: 'submitted as', submitted_range: 'submitted commit range',
  merged_range: 'merged commit range', produced_verdict: 'produced verdict', supports: 'supports verdict', proved_by: 'proved by',
}

export function LineageGraphCard({ graph, title = 'Lineage graph' }: { graph: LineageGraph; title?: string }) {
  const labels = new Map(graph.nodes.map((node) => [`${node.type}:${node.id}`, node.label]))
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <span className="flex items-center gap-1.5">
          {graph.truncated && <Badge variant="attention">Bounded view</Badge>}
          {(graph.omitted_nodes ?? 0) + (graph.omitted_links ?? 0) > 0 && <Badge variant="attention">{(graph.omitted_nodes ?? 0) + (graph.omitted_links ?? 0)} omitted</Badge>}
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
                  <LineageNode type={link.src_type} id={link.src_id} label={labels.get(`${link.src_type}:${link.src_id}`)} />
                  <span className="flex items-center gap-1 font-mono text-[10px] text-faint"><ArrowRight className="size-3" /> {humanize(link.kind)}</span>
                  <LineageNode type={link.dst_type} id={link.dst_id} label={labels.get(`${link.dst_type}:${link.dst_id}`)} />
                </li>
              ))}
            </ol>
          </details>
        )}
      </CardContent>
    </Card>
  )
}

function LineageNode({ type, id, label }: { type: LineageNodeType; id: string; label?: string }) {
  return <span className="min-w-0"><span className="block text-[10px] uppercase tracking-wide text-faint">{nodeLabels[type] ?? humanize(type)}</span><span className="block truncate" title={id}>{label || humanize(type)}</span></span>
}

function humanize(value: string) { return edgeLabels[value] ?? value.replaceAll('_', ' ') }
