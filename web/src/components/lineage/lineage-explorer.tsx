import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { BookOpenText, Download, ExternalLink, X } from 'lucide-react'
import { downloadLineageEvidence, fetchLineage } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { LineageGraph, LineageLink, LineageNode, LineageNodeType } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'

/**
 * A corner affordance on task, requirement, and System Design detail opens the
 * records related to the current one, grouped as work, documents, delivery,
 * and evidence (`req-document-operating-surfaces` AC-3.1).
 *
 * Everything it shows comes from one read of the canonical lineage API. The
 * panel groups and orders that bounded walk, and resolves each entry's
 * destination only from the entry's own canonical identity or from a link the
 * same response returned. It presents no relationship of its own (AC-3.2).
 */
export function LineageExplorer({ type, id }: { type: LineageNodeType; id: string }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      {/* Below sm the trigger keeps its name but shows only the icon, so a
          long record title truncates beside it instead of being overdrawn by
          label text that cannot wrap (component-web-dashboard). */}
      <Button variant="ghost" size="sm" className="shrink-0" onClick={() => setOpen(true)}>
        <BookOpenText /> <span className="max-sm:sr-only">Knowledge explorer</span>
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
    <Sheet onClose={onClose} label="Knowledge explorer" width="w-full md:w-[26rem]">
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
        {data && <KnowledgeGroups graph={data} origin={{ type, id }} workspace={workspace} />}
      </div>
    </Sheet>
  )
}

function KnowledgeGroups({
  graph,
  origin,
  workspace,
}: {
  graph: LineageGraph
  origin: Pick<LineageNode, 'type' | 'id'>
  workspace: string
}) {
  const entries = lineageEntries(graph, origin)
  const relatedCount = entries.filter((entry) => !entry.current).length
  const omitted = (graph.omitted_nodes ?? 0) + (graph.omitted_links ?? 0)
  return (
    <div className="space-y-6">
      {relatedCount === 0 && (
        <p className="text-sm text-muted">
          No related work, documents, delivery, or evidence are linked to this record yet.
        </p>
      )}
      {groupOrder.map(({ key, title }) => {
        const groupedEntries = entries.filter((entry) => kindInfo[entry.kind].group === key)
        if (groupedEntries.length === 0) return null
        return (
          <section key={key} aria-label={title}>
            <h3 className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
              {title}
              <Badge variant="mono">{groupedEntries.length}</Badge>
            </h3>
            <ul className="mt-2 space-y-1.5">
              {groupedEntries.map((entry) => (
                <li key={`${entry.kind}:${entry.id}`}>
                  <KnowledgeEntry entry={entry} graph={graph} workspace={workspace} />
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

function KnowledgeEntry({ entry, graph, workspace }: { entry: LineageEntry; graph: LineageGraph; workspace: string }) {
  const versions = entry.versions.map((version) => `v${version}`).join(', ')
  const versionNote =
    entry.versions.length === 0
      ? undefined
      : entry.recordReturned
        ? `Versions ${versions}`
        : versionSelectable.has(entry.kind)
          ? `Opens v${entry.versions[entry.versions.length - 1]}`
          : // The blueprint detail page has no version selector and shows its
            // governing version, so the entry cannot promise the returned one.
            `Returned ${versions}; opens the blueprint record without selecting a version`
  const body = (
    <>
      <span className={`block text-[10px] uppercase tracking-wide ${entry.current ? 'text-primary/70' : 'text-faint'}`}>
        {kindInfo[entry.kind].label}
      </span>
      <span className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate" title={entry.id}>
          {entry.label}
        </span>
        {entry.current && <Badge variant="accent">Current</Badge>}
      </span>
      {versionNote && <span className="block text-[11px] text-muted">{versionNote}</span>}
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
  const destination = entryDestination(entry, graph)
  return <EntryLink destination={destination} workspace={workspace} body={body} />
}

const entryClassName =
  'relative block w-full rounded-md border border-border bg-surface px-3 py-2 text-left text-xs transition-colors hover:border-edge hover:bg-raised'

function EntryLink({ destination, workspace, body }: { destination: Destination; workspace: string; body: ReactNode }) {
  switch (destination.type) {
    case 'task':
      return (
        <Link to="/tasks/$taskId/full" params={{ taskId: destination.taskId }} className={entryClassName}>
          {body}
        </Link>
      )
    case 'blueprint':
      return (
        <Link to="/blueprints/$taskId" params={{ taskId: destination.taskId }} className={entryClassName}>
          {body}
        </Link>
      )
    case 'planning':
      return (
        <Link to="/planning" search={{ session: destination.session }} className={entryClassName}>
          {body}
        </Link>
      )
    case 'requirement':
      return (
        <Link
          to="/requirements"
          search={{ requirement: destination.id, ...(destination.target ? { target: destination.target } : {}) }}
          className={entryClassName}
        >
          {body}
        </Link>
      )
    case 'design':
      return (
        <Link
          to="/system-design"
          search={{ document: destination.id, ...(destination.target ? { target: destination.target } : {}) }}
          className={entryClassName}
        >
          {body}
        </Link>
      )
    case 'reference':
      return (
        <Link to="/requirements" hash={destination.hash} className={entryClassName}>
          {body}
        </Link>
      )
    case 'decision':
      return (
        <Link to="/system-design" hash={destination.hash} className={entryClassName}>
          {body}
        </Link>
      )
    case 'external':
      return (
        <a href={destination.href} target="_blank" rel="noopener noreferrer" className={entryClassName}>
          {body}
          <span className="mt-0.5 flex items-center gap-1 text-[11px] text-muted">
            <ExternalLink className="size-3" aria-hidden="true" /> Opens on GitHub
          </span>
        </a>
      )
    case 'evidence':
      return <EvidenceEntry artifactId={destination.artifactId} workspace={workspace} body={body} />
    case 'unavailable':
      return (
        <span className="block rounded-md border border-dashed border-border px-3 py-2 text-xs" data-destination="none">
          {body}
          <span className="mt-0.5 block text-[11px] text-muted">{destination.reason}</span>
        </span>
      )
  }
}

// Evidence is an artifact behind the authenticated API, so the entry downloads
// it through a same-origin read that names the workspace explicitly.
function EvidenceEntry({ artifactId, workspace, body }: { artifactId: string; workspace: string; body: ReactNode }) {
  const [failure, setFailure] = useState('')
  const [pending, setPending] = useState(false)
  return (
    <div>
      <button
        type="button"
        className={entryClassName}
        disabled={pending}
        onClick={() => {
          setFailure('')
          setPending(true)
          downloadLineageEvidence(workspace, artifactId)
            .catch((error: unknown) => setFailure(errorMessage(error, 'Could not download this evidence.')))
            .finally(() => setPending(false))
        }}
      >
        {body}
        <span className="mt-0.5 flex items-center gap-1 text-[11px] text-muted">
          <Download className="size-3" aria-hidden="true" /> {pending ? 'Downloading…' : 'Download evidence'}
        </span>
      </button>
      {failure && <p className="mt-1 text-[11px] text-failure">{failure}</p>}
    </div>
  )
}

type GroupKey = 'work' | 'documents' | 'delivery' | 'evidence'

const groupOrder: Array<{ key: GroupKey; title: string }> = [
  { key: 'work', title: 'Work' },
  { key: 'documents', title: 'Documents' },
  { key: 'delivery', title: 'Delivery' },
  { key: 'evidence', title: 'Evidence' },
]

type EntryKind =
  | 'task'
  | 'blueprint'
  | 'planning_session'
  | 'planning_bundle'
  | 'work_order'
  | 'requirement'
  | 'system_design'
  | 'reference_document'
  | 'decision'
  | 'repository_path'
  | 'pull_request'
  | 'commit_range'
  | 'verdict'
  | 'evidence'

// Key order is the order entries take inside their group.
const kindInfo: Record<EntryKind, { group: GroupKey; label: string }> = {
  task: { group: 'work', label: 'Task' },
  blueprint: { group: 'work', label: 'Blueprint' },
  planning_session: { group: 'work', label: 'Planning session' },
  planning_bundle: { group: 'work', label: 'Planning bundle' },
  work_order: { group: 'work', label: 'Work order' },
  requirement: { group: 'documents', label: 'Requirement' },
  system_design: { group: 'documents', label: 'System Design' },
  reference_document: { group: 'documents', label: 'Product overview' },
  decision: { group: 'documents', label: 'Decision' },
  repository_path: { group: 'documents', label: 'Repository path' },
  pull_request: { group: 'delivery', label: 'Pull request' },
  commit_range: { group: 'delivery', label: 'Commit range' },
  verdict: { group: 'delivery', label: 'Review verdict' },
  evidence: { group: 'evidence', label: 'Evidence' },
}
const kindOrder = Object.keys(kindInfo) as EntryKind[]

// Immutable versions fold into their record through the canonical
// `<id>:v<n>` lineage identity (conveyor:internal/core/lineage.go).
const versionedKinds: Partial<Record<LineageNodeType, EntryKind>> = {
  requirement_version: 'requirement',
  system_design_version: 'system_design',
  reference_document_version: 'reference_document',
  blueprint_version: 'blueprint',
}

interface LineageEntry {
  kind: EntryKind
  id: string
  label: string
  current: boolean
  // The walk returned the durable record node itself, not only versions.
  recordReturned: boolean
  versions: number[]
}

type Destination =
  | { type: 'task'; taskId: string }
  | { type: 'blueprint'; taskId: string }
  | { type: 'planning'; session: string }
  | { type: 'requirement'; id: string; target?: number }
  | { type: 'design'; id: string; target?: number }
  | { type: 'reference'; hash: string }
  | { type: 'decision'; hash: string }
  | { type: 'external'; href: string }
  | { type: 'evidence'; artifactId: string }
  | { type: 'unavailable'; reason: string }

// Kinds whose destination surface can select a specific returned version.
const versionSelectable = new Set<EntryKind>(['requirement', 'system_design', 'reference_document'])

const versionIdentity = /^(.+):v([1-9]\d*)$/

function entryIdentity(node: Pick<LineageNode, 'type' | 'id'>): { kind: EntryKind; id: string; version?: number } {
  const folded = versionedKinds[node.type]
  if (folded) {
    const match = versionIdentity.exec(node.id)
    return match ? { kind: folded, id: match[1], version: Number(match[2]) } : { kind: folded, id: node.id }
  }
  return { kind: node.type as EntryKind, id: node.id }
}

function lineageEntries(graph: LineageGraph, origin: Pick<LineageNode, 'type' | 'id'>): LineageEntry[] {
  const originIdentity = entryIdentity(origin)
  const entries = new Map<string, LineageEntry & { recordLabel?: string; versionLabel?: string }>()
  for (const node of [...graph.roots, ...graph.nodes]) {
    if (!(node.type in versionedKinds) && !(node.type in kindInfo)) continue
    const identity = entryIdentity(node)
    const key = `${identity.kind}:${identity.id}`
    const entry = entries.get(key) ?? {
      kind: identity.kind,
      id: identity.id,
      label: identity.id,
      current: originIdentity.kind === identity.kind && originIdentity.id === identity.id,
      recordReturned: false,
      versions: [],
    }
    const label = node.label?.trim()
    if (identity.version === undefined) {
      entry.recordReturned = true
      if (label) entry.recordLabel = label
    } else {
      if (!entry.versions.includes(identity.version)) entry.versions.push(identity.version)
      if (label && !entry.versionLabel) entry.versionLabel = label
    }
    entries.set(key, entry)
  }
  return [...entries.values()]
    .map(({ recordLabel, versionLabel, ...entry }) => ({
      ...entry,
      // Labels come from the response; the identifier is the fallback so an
      // entry never renders blank and never invents a title.
      label: recordLabel ?? versionLabel ?? entry.id,
      versions: [...entry.versions].sort((left, right) => left - right),
    }))
    .sort(
      (left, right) =>
        groupOrder.findIndex((group) => group.key === kindInfo[left.kind].group) -
          groupOrder.findIndex((group) => group.key === kindInfo[right.kind].group) ||
        kindOrder.indexOf(left.kind) - kindOrder.indexOf(right.kind) ||
        (left.current === right.current ? 0 : left.current ? 1 : -1) ||
        left.label.localeCompare(right.label) ||
        left.id.localeCompare(right.id),
    )
}

// Ownership comes only from links this response returned, never from an ID
// prefix: a work order or verdict ID is opaque to the graph.
function returnedSource(
  links: LineageLink[],
  srcType: LineageNodeType,
  dstType: LineageNodeType,
  dstId: string,
  kind: string,
) {
  return links
    .filter(
      (link) => link.src_type === srcType && link.dst_type === dstType && link.dst_id === dstId && link.kind === kind,
    )
    .map((link) => link.src_id)
    .sort()[0]
}

const forgeSegment = '([A-Za-z0-9_.-]+)'
const pullRequestIdentity = new RegExp(`^${forgeSegment}/${forgeSegment}#([1-9]\\d{0,9})$`)
const commitRangeIdentity = new RegExp(`^${forgeSegment}/${forgeSegment}@([0-9a-fA-F]{7,64})\\.\\.([0-9a-fA-F]{7,64})$`)

function safeForgeRepository(owner: string, repository: string) {
  return ![owner, repository].some((segment) => segment === '.' || segment === '..')
}

function entryDestination(entry: LineageEntry, graph: LineageGraph): Destination {
  const links = graph.links ?? []
  const newest = entry.versions[entry.versions.length - 1]
  const target = entry.recordReturned ? undefined : newest
  switch (entry.kind) {
    case 'task':
      return { type: 'task', taskId: entry.id }
    case 'blueprint':
      return { type: 'blueprint', taskId: entry.id }
    case 'planning_session':
      return { type: 'planning', session: entry.id }
    case 'planning_bundle': {
      const session = returnedSource(links, 'planning_session', 'planning_bundle', entry.id, 'produced_bundle')
      return session
        ? { type: 'planning', session }
        : { type: 'unavailable', reason: 'No returned link names the planning session that produced this bundle.' }
    }
    case 'work_order': {
      const taskId = returnedSource(links, 'task', 'work_order', entry.id, 'dispatches')
      return taskId
        ? { type: 'task', taskId }
        : { type: 'unavailable', reason: 'No returned link names the task that dispatched this work order.' }
    }
    case 'verdict': {
      const order = returnedSource(links, 'work_order', 'verdict', entry.id, 'produced_verdict')
      const taskId = order ? returnedSource(links, 'task', 'work_order', order, 'dispatches') : undefined
      return taskId
        ? { type: 'task', taskId }
        : { type: 'unavailable', reason: 'No returned links name the work order and task behind this verdict.' }
    }
    case 'requirement':
      return { type: 'requirement', id: entry.id, target }
    case 'system_design':
      return { type: 'design', id: entry.id, target }
    case 'reference_document':
      return newest
        ? { type: 'reference', hash: `reference-${entry.id}-v${newest}` }
        : { type: 'unavailable', reason: 'No returned version of this product overview can be opened.' }
    case 'decision':
      return { type: 'decision', hash: `decision-${entry.id.toLowerCase()}` }
    case 'repository_path': {
      const governing = returnedSource(links, 'system_design_version', 'repository_path', entry.id, 'governs')
      const design = governing ? entryIdentity({ type: 'system_design_version', id: governing }) : undefined
      return design
        ? { type: 'design', id: design.id, target: design.version }
        : { type: 'unavailable', reason: 'No returned System Design version governs this path.' }
    }
    case 'pull_request': {
      const match = pullRequestIdentity.exec(entry.id)
      if (!match || !safeForgeRepository(match[1], match[2]))
        return { type: 'unavailable', reason: 'Not a canonical owner/repository#number identity, so no link is built.' }
      return {
        type: 'external',
        href: `https://github.com/${encodeURIComponent(match[1])}/${encodeURIComponent(match[2])}/pull/${match[3]}`,
      }
    }
    case 'commit_range': {
      const match = commitRangeIdentity.exec(entry.id)
      if (!match || !safeForgeRepository(match[1], match[2]))
        return {
          type: 'unavailable',
          reason: 'Not a canonical owner/repository@base..head identity, so no link is built.',
        }
      return {
        type: 'external',
        href: `https://github.com/${encodeURIComponent(match[1])}/${encodeURIComponent(match[2])}/compare/${match[3]}...${match[4]}`,
      }
    }
    case 'evidence':
      return { type: 'evidence', artifactId: entry.id }
  }
}
