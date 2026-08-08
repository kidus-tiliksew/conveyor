import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { BookOpenText, X } from 'lucide-react'
import { fetchLineage } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { LineageGraph, LineageNode, LineageNodeType } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'

/**
 * A corner affordance on task, requirement, and System Design detail opens a
 * focused hierarchy of the product knowledge that gives the current record
 * context: requirements, System Designs, and tasks.
 *
 * Everything it shows comes from one read of the canonical lineage API
 * The panel filters and orders one canonical lineage read. The walk's budget
 * and truncation report remain the panel's boundaries too.
 */
export function LineageExplorer({ type, id }: { type: LineageNodeType; id: string }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
        <BookOpenText /> Knowledge explorer
      </Button>
      {/* Mounted only once opened, so the walk is the on-demand read REQ-3
          asks for rather than a cost every detail view pays. */}
      {open && <KnowledgePanel type={type} id={id} onClose={() => setOpen(false)} />}
    </>
  )
}

function KnowledgePanel({ type, id, onClose }: { type: LineageNodeType; id: string; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const { data, isLoading, error } = useQuery({
    queryKey: ['lineage', workspace, type, id],
    queryFn: () => fetchLineage(type, id),
  })
  return (
    <Sheet onClose={onClose} label="Knowledge explorer" width="md:w-[26rem]">
      <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2.5">
        <BookOpenText className="size-4 text-primary" aria-hidden="true" />
        <h2 className="mr-auto text-sm font-medium">Knowledge explorer</h2>
        <Button variant="ghost" size="icon" aria-label="Close Knowledge explorer" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4">
        {isLoading && (
          <div className="space-y-3" role="status" aria-label="Loading Knowledge explorer">
            <Skeleton className="h-20" />
            <Skeleton className="h-20" />
          </div>
        )}
        {error != null && (
          <p className="text-sm text-failure">{errorMessage(error, 'Could not load Knowledge explorer.')}</p>
        )}
        {data && <KnowledgeHierarchy graph={data} origin={{ type, id }} />}
      </div>
    </Sheet>
  )
}

function KnowledgeHierarchy({ graph, origin }: { graph: LineageGraph; origin: Pick<LineageNode, 'type' | 'id'> }) {
  const entries = focusedEntries(graph, origin)
  const relatedCount = entries.filter((entry) => !entry.current).length
  const omitted = (graph.omitted_nodes ?? 0) + (graph.omitted_links ?? 0)
  return (
    <div className="space-y-6">
      {relatedCount === 0 && (
        <p className="text-sm text-muted">
          No related requirements, System Designs, or tasks are linked to this record yet.
        </p>
      )}
      {hierarchyOrder.map(({ kind, title }) => {
        const groupedEntries = entries.filter((entry) => entry.kind === kind)
        if (groupedEntries.length === 0) return null
        return (
          <section key={kind} aria-label={title}>
            <h3 className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
              {title}
              <Badge variant="mono">{groupedEntries.length}</Badge>
            </h3>
            <ul className="mt-2 space-y-1.5">
              {groupedEntries.map((entry) => (
                <li key={`${entry.kind}:${entry.id}`}>
                  <KnowledgeEntry entry={entry} />
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

function KnowledgeEntry({ entry }: { entry: FocusedEntry }) {
  const body = (
    <>
      <span className={`block text-[10px] uppercase tracking-wide ${entry.current ? 'text-primary/70' : 'text-faint'}`}>
        {focusedTypeLabels[entry.kind]}
      </span>
      <span className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate" title={entry.node.id}>
          {entryLabel(entry.node)}
        </span>
        {entry.current && <Badge variant="accent">Current</Badge>}
      </span>
    </>
  )
  if (entry.current) {
    return (
      <span
        aria-current="true"
        className="block rounded-md border border-primary bg-primary-soft px-3 py-2 text-xs text-primary"
      >
        {body}
      </span>
    )
  }
  return <EntryLink destination={nodeDestination(entry.node)}>{body}</EntryLink>
}

const entryClassName =
  'relative block rounded-md border border-border bg-surface px-3 py-2 text-xs transition-colors hover:border-edge hover:bg-raised'

function EntryLink({ destination, children }: { destination: FocusedDestination; children: ReactNode }) {
  switch (destination.kind) {
    case 'task':
      return (
        <Link to="/tasks/$taskId/full" params={{ taskId: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'requirement':
      return (
        <Link to="/requirements" search={{ requirement: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
    case 'design':
      return (
        <Link to="/system-design" search={{ document: destination.id }} className={entryClassName}>
          {children}
        </Link>
      )
  }
}

type FocusedDestination = { kind: FocusedKind; id: string }

// Every focused entry has an exact durable-record destination. Other lineage
// node kinds are filtered before this routing boundary.
function nodeDestination(node: LineageNode): FocusedDestination {
  const focused = focusedIdentity(node)
  if (!focused) throw new Error(`Unsupported Knowledge explorer node type: ${node.type}`)
  return { kind: focused.kind, id: focused.id }
}

const hierarchyOrder: Array<{ kind: FocusedKind; title: string }> = [
  { kind: 'requirement', title: 'Requirements' },
  { kind: 'design', title: 'System Designs' },
  { kind: 'task', title: 'Tasks' },
]

type FocusedKind = 'requirement' | 'design' | 'task'

interface FocusedEntry {
  kind: FocusedKind
  id: string
  node: LineageNode
  current: boolean
}

const focusedTypeLabels: Record<FocusedKind, string> = {
  requirement: 'Requirement',
  design: 'System Design',
  task: 'Task',
}

function focusedEntries(graph: LineageGraph, origin: Pick<LineageNode, 'type' | 'id'>): FocusedEntry[] {
  const originIdentity = focusedIdentity(origin)
  const entries = new Map<string, FocusedEntry>()
  for (const node of [...graph.roots, ...graph.nodes]) {
    const identity = focusedIdentity(node)
    if (!identity) continue
    const current = originIdentity?.kind === identity.kind && originIdentity.id === identity.id
    // A task view ends at the current task, and a design view centers the
    // current design. Peers of those same types belong only downstream of a
    // requirement or System Design respectively.
    if (originIdentity?.kind === 'task' && identity.kind === 'task' && !current) continue
    if (originIdentity?.kind === 'design' && identity.kind === 'design' && !current) continue
    if (originIdentity?.kind === 'requirement' && identity.kind === 'requirement' && !current) continue
    const key = `${identity.kind}:${identity.id}`
    const existing = entries.get(key)
    // Prefer the durable record node over a version node when both occur in
    // the walk. Version-only walks still route to the durable record.
    if (!existing || isVersionNode(existing.node)) {
      entries.set(key, { ...identity, node, current })
    }
  }
  return [...entries.values()].sort((left, right) => {
    const kindOrder =
      hierarchyOrder.findIndex((group) => group.kind === left.kind) -
      hierarchyOrder.findIndex((group) => group.kind === right.kind)
    if (kindOrder !== 0) return kindOrder
    if (left.current !== right.current) return left.current ? 1 : -1
    return entryLabel(left.node).localeCompare(entryLabel(right.node)) || left.id.localeCompare(right.id)
  })
}

function focusedIdentity(node: Pick<LineageNode, 'type' | 'id'>): Pick<FocusedEntry, 'kind' | 'id'> | undefined {
  switch (node.type) {
    case 'requirement':
      return { kind: 'requirement', id: node.id }
    case 'requirement_version':
      return { kind: 'requirement', id: versionBase(node.id) }
    case 'system_design':
      return { kind: 'design', id: node.id }
    case 'system_design_version':
      return { kind: 'design', id: versionBase(node.id) }
    case 'task':
      return { kind: 'task', id: node.id }
    default:
      return undefined
  }
}

function isVersionNode(node: LineageNode) {
  return node.type === 'requirement_version' || node.type === 'system_design_version'
}

// Labels come from the lineage response; the identifier is the fallback so an
// entry never renders blank and never invents a title the API did not carry.
function entryLabel(node: LineageNode) {
  return node.label?.trim() || node.id
}

function versionBase(id: string) {
  const index = id.lastIndexOf(':v')
  return index > 0 ? id.slice(0, index) : id
}
