import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLink, Share2, X } from 'lucide-react'
import { fetchLineage } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { LineageGraph, LineageNode, LineageNodeType } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'

/**
 * The lineage explorer (spec §21.61 change 2; REQ-3). A corner affordance on
 * task, requirement, and System Design detail opens a right panel of the
 * records this one connects to, grouped into work, documents, delivery, and
 * evidence (AC-3.1).
 *
 * Everything it shows comes from one read of the canonical lineage API
 * (AC-3.2): the panel groups and links the labeled nodes that read returns and
 * derives no relationship of its own, so the walk's budget and its truncation
 * report are the panel's boundaries too. There is no graph drawing here on
 * purpose — grouped lists are what an operator traces work with.
 */
export function LineageExplorer({ type, id }: { type: LineageNodeType; id: string }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
        <Share2 /> Related
      </Button>
      {/* Mounted only once opened, so the walk is the on-demand read REQ-3
          asks for rather than a cost every detail view pays. */}
      {open && <RelatedPanel type={type} id={id} onClose={() => setOpen(false)} />}
    </>
  )
}

function RelatedPanel({ type, id, onClose }: { type: LineageNodeType; id: string; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const { data, isLoading, error } = useQuery({
    queryKey: ['lineage', workspace, type, id],
    queryFn: () => fetchLineage(type, id),
  })
  return (
    <Sheet onClose={onClose} label="Related records" width="md:w-[26rem]">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2.5">
        <h2 className="mr-auto text-sm font-medium">Related records</h2>
        <Button variant="ghost" size="icon" aria-label="Close related records" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4">
        {isLoading && (
          <div className="space-y-3">
            <Skeleton className="h-20" />
            <Skeleton className="h-20" />
          </div>
        )}
        {error != null && (
          <p className="text-sm text-failure">{errorMessage(error, 'Could not load related records.')}</p>
        )}
        {data && <RelatedGroups graph={data} />}
      </div>
    </Sheet>
  )
}

function RelatedGroups({ graph }: { graph: LineageGraph }) {
  const roots = new Set(graph.roots.map(nodeKey))
  const related = graph.nodes.filter((node) => !roots.has(nodeKey(node)))
  const omitted = (graph.omitted_nodes ?? 0) + (graph.omitted_links ?? 0)
  return (
    <div className="space-y-6">
      {related.length === 0 && (
        <p className="text-sm text-muted">
          Nothing is linked to this record yet. Relationships appear as planning and delivery advance.
        </p>
      )}
      {/* Empty groups collapse rather than render an empty heading. */}
      {groupOrder.map(({ key, title }) => {
        const entries = related
          .filter((node) => nodeGroups[node.type] === key)
          .sort((left, right) => entryLabel(left).localeCompare(entryLabel(right)))
        if (entries.length === 0) return null
        return (
          <section key={key} aria-label={title}>
            <h3 className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
              {title}
              <Badge variant="mono">{entries.length}</Badge>
            </h3>
            <ul className="mt-2 space-y-1.5">
              {entries.map((node) => (
                <li key={nodeKey(node)}>
                  <RelatedEntry node={node} />
                </li>
              ))}
            </ul>
          </section>
        )
      })}
      {/* The walk is bounded server-side, so a partial answer says so rather
          than reading as the whole picture (AC-3.2). */}
      {(graph.truncated || omitted > 0) && (
        <p className="border-t border-border pt-3 text-xs text-muted">
          This is a bounded view
          {omitted > 0 ? `: ${omitted} further ${omitted === 1 ? 'connection' : 'connections'} were not read.` : '.'}
        </p>
      )}
    </div>
  )
}

function RelatedEntry({ node }: { node: LineageNode }) {
  const body = (
    <>
      <span className="block text-[10px] uppercase tracking-wide text-faint">{nodeTypeLabels[node.type]}</span>
      <span className="block truncate" title={node.id}>
        {entryLabel(node)}
      </span>
    </>
  )
  const destination = nodeDestination(node)
  if (!destination) {
    // A node type with no surface of its own is still part of the answer, so
    // it is listed as a fact rather than dressed up as a link that goes nowhere.
    return <span className="block rounded-md border border-border bg-surface px-3 py-2 text-xs">{body}</span>
  }
  return (
    <EntryLink destination={destination}>
      {body}
      {destination.kind === 'external' && (
        <ExternalLink className="absolute right-3 top-3 size-3 text-faint" aria-hidden="true" />
      )}
    </EntryLink>
  )
}

const entryClassName =
  'relative block rounded-md border border-border bg-surface px-3 py-2 text-xs transition-colors hover:border-edge hover:bg-raised'

function EntryLink({ destination, children }: { destination: Destination; children: ReactNode }) {
  switch (destination.kind) {
    case 'task':
      return (
        <Link to="/tasks/$taskId/full" params={{ taskId: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'blueprint':
      return (
        <Link to="/blueprints/$taskId" params={{ taskId: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'requirement':
      return (
        <Link to="/requirements" search={{ requirement: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'overview':
      return (
        <Link to="/requirements" hash={destination.id} className={entryClassName}>
          {children}
        </Link>
      )
    case 'design':
      return (
        <Link to="/system-design" search={{ document: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'decision':
      return (
        <Link to="/system-design" hash={destination.id} className={entryClassName}>
          {children}
        </Link>
      )
    // Planning left the primary navigation while its presentation is parked
    // (§21.61 change 3), and its route stayed mounted for exactly this: the
    // records remain readable where they live.
    case 'planning':
      return (
        <Link to="/planning" className={entryClassName}>
          {children}
        </Link>
      )
    case 'external':
      return (
        <a href={destination.href} target="_blank" rel="noreferrer" className={entryClassName}>
          {children}
        </a>
      )
  }
}

type Destination =
  | { kind: 'task' | 'blueprint' | 'requirement' | 'overview' | 'design' | 'decision'; id: string }
  | { kind: 'planning' }
  | { kind: 'external'; href: string }

// Which surface each node type belongs to. The mapping is deterministic and
// total: a node the dashboard has no home for resolves to undefined and is
// listed without a link rather than pointed at an approximate page.
function nodeDestination(node: LineageNode): Destination | undefined {
  switch (node.type) {
    case 'task':
      return { kind: 'task', id: node.id }
    case 'blueprint':
      return { kind: 'blueprint', id: node.id }
    case 'blueprint_version':
      return { kind: 'blueprint', id: versionBase(node.id) }
    case 'requirement':
      return { kind: 'requirement', id: node.id }
    case 'requirement_version':
      return { kind: 'requirement', id: versionBase(node.id) }
    case 'reference_document_version': {
      // The overviews canvas already selects from `#reference-<id>-v<n>`, the
      // same anchor requirement proposals cite as their source.
      const index = node.id.lastIndexOf(':v')
      return index > 0
        ? { kind: 'overview', id: `reference-${node.id.slice(0, index)}-v${node.id.slice(index + 2)}` }
        : undefined
    }
    case 'system_design':
      return { kind: 'design', id: node.id }
    case 'system_design_version':
      return { kind: 'design', id: versionBase(node.id) }
    case 'decision':
      return { kind: 'decision', id: `decision-${node.id.toLowerCase()}` }
    case 'planning_session':
    case 'planning_bundle':
      return { kind: 'planning' }
    case 'pull_request': {
      const match = /^(\S+\/\S+)#(\d+)$/.exec(node.id)
      return match ? { kind: 'external', href: `https://github.com/${match[1]}/pull/${match[2]}` } : undefined
    }
    case 'commit_range': {
      const match = /^(\S+\/\S+)@([^.\s]+)\.\.([^.\s]+)$/.exec(node.id)
      return match
        ? { kind: 'external', href: `https://github.com/${match[1]}/compare/${match[2]}...${match[3]}` }
        : undefined
    }
    // Work orders, governed paths, artifacts, and verdicts are read inside the
    // surfaces that own them; none has an address of its own.
    default:
      return undefined
  }
}

const groupOrder: Array<{ key: LineageGroup; title: string }> = [
  { key: 'work', title: 'Work' },
  { key: 'documents', title: 'Documents' },
  { key: 'delivery', title: 'Delivery' },
  { key: 'evidence', title: 'Evidence' },
]

type LineageGroup = 'work' | 'documents' | 'delivery' | 'evidence'

// Grouping is the only client-side derivation the panel performs (AC-3.2):
// each node lands in a group by its own type, never by inspecting an edge.
const nodeGroups: Record<LineageNodeType, LineageGroup> = {
  task: 'work',
  work_order: 'work',
  planning_session: 'documents',
  planning_bundle: 'documents',
  requirement: 'documents',
  requirement_version: 'documents',
  reference_document: 'documents',
  reference_document_version: 'documents',
  system_design: 'documents',
  system_design_version: 'documents',
  decision: 'documents',
  blueprint: 'documents',
  blueprint_version: 'documents',
  repository_path: 'delivery',
  pull_request: 'delivery',
  commit_range: 'delivery',
  evidence: 'evidence',
  verdict: 'evidence',
}

const nodeTypeLabels: Record<LineageNodeType, string> = {
  planning_session: 'Planning conversation',
  planning_bundle: 'Planning bundle',
  requirement: 'Requirement',
  requirement_version: 'Requirement version',
  reference_document: 'Product overview',
  reference_document_version: 'Product overview version',
  system_design: 'System Design',
  system_design_version: 'System Design version',
  decision: 'Decision',
  repository_path: 'Governed path',
  blueprint: 'Blueprint',
  blueprint_version: 'Blueprint version',
  task: 'Task',
  work_order: 'Work order',
  pull_request: 'Pull request',
  commit_range: 'Commit range',
  evidence: 'Attached file',
  verdict: 'Review verdict',
}

// Labels come from the lineage response; the identifier is the fallback so an
// entry never renders blank and never invents a title the API did not carry.
function entryLabel(node: LineageNode) {
  return node.label?.trim() || node.id
}

function nodeKey(node: LineageNode) {
  return `${node.type}:${node.id}`
}

function versionBase(id: string) {
  const index = id.lastIndexOf(':v')
  return index > 0 ? id.slice(0, index) : id
}
