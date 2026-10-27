import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { BookOpenText, Download, ExternalLink, RotateCw, X } from 'lucide-react'
import { type ReactNode, useState } from 'react'
import { downloadLineageEvidence, fetchLineage, fetchLineageImpact } from '../../lib/api'
import { taskStateLabels } from '../../lib/contracts'
import { errorMessage } from '../../lib/errors'
import type {
  LineageGraph,
  LineageImpact,
  LineageLink,
  LineageNode,
  LineageNodeStatus,
  LineageNodeType,
} from '../../lib/types'
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
  // The read names the same workspace as its key and is cancelled with the
  // query, so a workspace switch never answers one key with another
  // workspace's walk.
  const { data, isLoading, error, refetch, isFetching } = useQuery({
    queryKey: ['lineage', workspace, type, id],
    queryFn: ({ signal }) => fetchLineage(workspace, type, id, signal),
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
        {error != null && !data && (
          <ReadFailure
            message={errorMessage(error, 'Could not load Knowledge explorer.')}
            retrying={isFetching}
            onRetry={() => void refetch()}
          />
        )}
        {data && <KnowledgeGroups graph={data} origin={{ type, id }} workspace={workspace} />}
      </div>
    </Sheet>
  )
}

function ReadFailure({ message, retrying, onRetry }: { message: string; retrying: boolean; onRetry: () => void }) {
  return (
    <div className="space-y-2">
      <p className="text-sm text-failure">{message}</p>
      <Button variant="secondary" size="sm" disabled={retrying} onClick={onRetry}>
        <RotateCw /> {retrying ? 'Retrying…' : 'Retry'}
      </Button>
    </div>
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
        // Root-relative grouping: entries the walk reached through another
        // task follow the record's own entries (component-web-document-surfaces).
        const own = groupedEntries.filter((entry) => !entry.via)
        const through = groupedEntries.filter((entry) => entry.via)
        const list = (items: LineageEntry[]) => (
          <ul className="mt-2 space-y-1.5">
            {items.map((entry) => (
              <li key={`${entry.kind}:${entry.id}`}>
                <KnowledgeEntry entry={entry} graph={graph} workspace={workspace} origin={origin} />
              </li>
            ))}
          </ul>
        )
        return (
          <section key={key} aria-label={title}>
            <h3 className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
              {title}
              <Badge variant="mono">{groupedEntries.length}</Badge>
            </h3>
            {own.length > 0 && list(own)}
            {through.length > 0 && (
              <section aria-label={`${title} through other tasks`} className="mt-3">
                <h4 className="text-[10px] font-medium uppercase tracking-[0.08em] text-faint">Through other tasks</h4>
                {list(through)}
              </section>
            )}
          </section>
        )
      })}
      <BoundedNotice graph={graph} />
    </div>
  )
}

// The walk is bounded server-side, so a partial answer says so and names the
// budget axis that stopped it rather than reading as the whole picture
// (AC-3.2; component-lineage).
function BoundedNotice({ graph }: { graph: LineageGraph }) {
  const omittedNodes = graph.omitted_nodes ?? 0
  const omittedLinks = graph.omitted_links ?? 0
  const reasons = graph.exhaustion_reasons ?? []
  if (!isBounded(graph)) return null
  const counts = [
    omittedNodes > 0 ? plural(omittedNodes, 'further record', 'further records') : '',
    omittedLinks > 0 ? plural(omittedLinks, 'further link', 'further links') : '',
  ].filter(Boolean)
  return (
    <div className="space-y-1 border-t border-border pt-3 text-xs text-muted" data-bounded="true">
      <p>This is a bounded view.</p>
      {reasons.length > 0 && (
        <ul aria-label="Exhausted limits" className="list-disc space-y-0.5 pl-4">
          {reasons.map((reason) => (
            <li key={reason}>{exhaustionText(reason, graph)}</li>
          ))}
        </ul>
      )}
      {counts.length > 0 && (
        <p>{`${counts.join(' and ')} ${omittedNodes + omittedLinks === 1 ? 'was' : 'were'} not read.`}</p>
      )}
    </div>
  )
}

function isBounded(graph: LineageGraph) {
  return (
    graph.truncated ||
    (graph.omitted_nodes ?? 0) > 0 ||
    (graph.omitted_links ?? 0) > 0 ||
    (graph.exhaustion_reasons?.length ?? 0) > 0
  )
}

function plural(count: number, one: string, many: string) {
  return `${count} ${count === 1 ? one : many}`
}

function exhaustionText(reason: string, graph: LineageGraph) {
  const budget = graph.budget
  const omissions = graph.omissions
  const limit = (value: number | undefined, unit: string) => (value ? ` of ${plural(value, unit, `${unit}s`)}` : '')
  switch (reason) {
    case 'depth':
      return `Depth limit${limit(budget?.max_depth, 'step')} reached${
        omissions?.depth_nodes
          ? `: at least ${plural(omissions.depth_nodes, 'record lies', 'records lie')} deeper.`
          : '.'
      }`
    case 'nodes':
      return `Record limit${limit(budget?.max_nodes, 'record')} reached${
        omissions?.node_cap_nodes
          ? `: at least ${plural(omissions.node_cap_nodes, 'more record was', 'more records were')} not read.`
          : '.'
      }`
    case 'fanout':
      return `Per-record link limit${limit(budget?.max_fanout, 'link')} reached${
        omissions?.fanout_nodes
          ? `: ${plural(omissions.fanout_nodes, 'record has', 'records have')} more links than were read.`
          : '.'
      }`
    case 'links':
      return `Link limit${limit(budget?.max_links, 'link')} reached${
        omissions?.links ? `: ${plural(omissions.links, 'link was', 'links were')} dropped.` : '.'
      }`
    default:
      return `Limit reached: ${reason}.`
  }
}

function KnowledgeEntry({
  entry,
  graph,
  workspace,
  origin,
}: {
  entry: LineageEntry
  graph: LineageGraph
  workspace: string
  origin: Pick<LineageNode, 'type' | 'id'>
}) {
  const versions = entry.versions.map((version) => `v${version.version}`).join(', ')
  const newest = entry.versions[entry.versions.length - 1]
  const versionNote =
    entry.versions.length === 0 || entry.recordReturned
      ? undefined
      : versionSelectable.has(entry.kind)
        ? `Opens v${newest.version}`
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
        {entry.archived && <Badge>Archived</Badge>}
        {entry.current && <Badge variant="accent">Current</Badge>}
      </span>
      {entry.detail && <span className="block text-[11px] text-muted">{entry.detail}</span>}
      {entry.via && <span className="block text-[11px] text-muted">via {entry.via.label}</span>}
      {versionNote && <span className="block text-[11px] text-muted">{versionNote}</span>}
    </>
  )
  const card = entry.current ? (
    <span
      aria-current="true"
      className="block rounded-md border border-primary bg-primary-soft px-3 py-2 text-xs text-primary"
    >
      {body}
    </span>
  ) : (
    <EntryLink destination={entryDestination(entry, graph)} workspace={workspace} body={body} />
  )
  const impactRoot = entry.current && (origin.type === 'requirement' || origin.type === 'system_design')
  if (entry.versions.length === 0 && !impactRoot) return card
  return (
    <div>
      {card}
      {entry.versions.length > 0 && <VersionRows entry={entry} workspace={workspace} />}
      {impactRoot && (
        <ImpactView workspace={workspace} type={origin.type as 'requirement' | 'system_design'} id={origin.id} />
      )}
    </div>
  )
}

const versionStatusLabels: Record<string, string> = {
  current: 'current',
  superseded: 'superseded',
  proposed: 'proposed',
  dismissed: 'dismissed',
}

function versionStatusText(status?: string) {
  if (!status) return 'status not returned'
  return versionStatusLabels[status] ?? status.replaceAll('_', ' ')
}

function taskStateText(state?: string) {
  if (!state) return 'state not returned'
  return taskStateLabels[state] ?? state.replaceAll('_', ' ')
}

// Each returned version is its own row with its returned status, and lists
// the tasks a returned serves or governs link from that version names
// (component-web-document-surfaces). No version-to-task relationship is
// inferred (AC-3.2).
function VersionRows({ entry, workspace }: { entry: LineageEntry; workspace: string }) {
  return (
    <ul aria-label={`Versions of ${entry.label}`} className="ml-3 mt-1 space-y-1 border-l border-border pl-3">
      {[...entry.versions].reverse().map((version) => {
        const status = versionStatusText(version.status?.version_status)
        const label = (
          <>
            <span className="font-mono">v{version.version}</span> <span className="text-muted">{status}</span>
          </>
        )
        const target = versionDestination(entry, version.version)
        return (
          <li key={version.version} data-version={version.version} className="text-[11px]">
            {target ? (
              <EntryLink destination={target} workspace={workspace} body={label} className={versionClassName} />
            ) : (
              <span className={versionClassName}>{label}</span>
            )}
            {version.tasks.length > 0 && (
              <ul aria-label={`Tasks on v${version.version}`} className="mt-0.5 space-y-0.5 pl-2">
                {version.tasks.map((task) => (
                  <li key={task.id} className="flex items-center gap-2">
                    <Link
                      to="/tasks/$taskId/full"
                      params={{ taskId: task.id }}
                      className="min-w-0 flex-1 truncate text-foreground hover:underline"
                      title={task.id}
                    >
                      {task.label}
                    </Link>
                    <span className="shrink-0 text-muted">{taskStateText(task.state)}</span>
                  </li>
                ))}
              </ul>
            )}
          </li>
        )
      })}
    </ul>
  )
}

const versionClassName = 'inline-flex items-center gap-1 rounded px-1 py-0.5 hover:bg-raised'

function versionDestination(entry: LineageEntry, version: number): Destination | undefined {
  switch (entry.kind) {
    case 'requirement':
      return { type: 'requirement', id: entry.id, target: version }
    case 'system_design':
      return { type: 'design', id: entry.id, target: version }
    case 'reference_document':
      return { type: 'reference', hash: `reference-${entry.id}-v${version}` }
    default:
      // The blueprint detail surface has no version selector.
      return undefined
  }
}

const impactReasonLabels: Record<string, string> = {
  versions: 'the version limit',
  tasks: 'the task limit',
  version_edges: 'the version-edge limit',
  merged_ranges: 'the merged-range limit',
}

// The impact read's "merged on an older version" view, offered on requirement
// and System Design detail and read only when asked for (component-lineage).
function ImpactView({ workspace, type, id }: { workspace: string; type: 'requirement' | 'system_design'; id: string }) {
  const [requested, setRequested] = useState(false)
  const { data, error, isLoading, isFetching, refetch } = useQuery({
    queryKey: ['lineage-impact', workspace, type, id],
    queryFn: ({ signal }) => fetchLineageImpact(workspace, type, id, signal),
    enabled: requested,
  })
  if (!requested) {
    return (
      <Button variant="ghost" size="sm" className="mt-1" onClick={() => setRequested(true)}>
        Show work merged on an older version
      </Button>
    )
  }
  return (
    <section aria-label="Merged on an older version" className="ml-3 mt-2 border-l border-border pl-3 text-[11px]">
      <h4 className="text-[10px] font-medium uppercase tracking-[0.08em] text-faint">Merged on an older version</h4>
      {isLoading && (
        <p role="status" className="text-muted">
          Reading merged work…
        </p>
      )}
      {error != null && !data && (
        <ReadFailure
          message={errorMessage(error, 'Could not read merged work.')}
          retrying={isFetching}
          onRetry={() => void refetch()}
        />
      )}
      {data && <ImpactItems impact={data} />}
    </section>
  )
}

function ImpactItems({ impact }: { impact: LineageImpact }) {
  const reasons = impact.exhaustion_reasons ?? []
  return (
    <>
      {impact.items.length === 0 ? (
        <p className="mt-1 text-muted">No merged work was built on an older version.</p>
      ) : (
        <ul className="mt-1 space-y-0.5">
          {impact.items.map((item) => (
            <li key={`${item.task.id}:${item.version ?? ''}:${item.created_by_event_id}`} className="flex gap-2">
              <Link
                to="/tasks/$taskId/full"
                params={{ taskId: item.task.id }}
                className="min-w-0 flex-1 truncate text-foreground hover:underline"
                title={item.task.id}
              >
                {item.task.label?.trim() || item.task.id}
              </Link>
              <span className="shrink-0 text-muted">
                {item.version ? `v${item.version} ${versionStatusText(item.version_status)}` : 'version not recorded'}
                {' · '}
                {taskStateText(item.task_state)}
              </span>
            </li>
          ))}
        </ul>
      )}
      {impact.total > impact.items.length && (
        <p className="mt-1 text-muted">{`Showing ${impact.items.length} of ${impact.total}.`}</p>
      )}
      {impact.truncated && (
        <p className="mt-1 text-muted">
          {`The impact read was bounded by ${
            reasons.length > 0
              ? reasons.map((reason) => impactReasonLabels[reason] ?? reason).join(' and ')
              : 'its budget'
          }.`}
        </p>
      )}
    </>
  )
}

const entryClassName =
  'relative block w-full rounded-md border border-border bg-surface px-3 py-2 text-left text-xs transition-colors hover:border-edge hover:bg-raised'

function EntryLink({
  destination,
  workspace,
  body,
  className = entryClassName,
}: {
  destination: Destination
  workspace: string
  body: ReactNode
  className?: string
}) {
  switch (destination.type) {
    case 'task':
      return (
        <Link to="/tasks/$taskId/full" params={{ taskId: destination.taskId }} className={className}>
          {body}
        </Link>
      )
    case 'blueprint':
      return (
        <Link to="/blueprints/$taskId" params={{ taskId: destination.taskId }} className={className}>
          {body}
        </Link>
      )
    case 'planning':
      return (
        <Link to="/planning" search={{ session: destination.session }} className={className}>
          {body}
        </Link>
      )
    case 'requirement':
      return (
        <Link
          to="/requirements"
          search={{ requirement: destination.id, ...(destination.target ? { target: destination.target } : {}) }}
          className={className}
        >
          {body}
        </Link>
      )
    case 'design':
      return (
        <Link
          to="/system-design"
          search={{ document: destination.id, ...(destination.target ? { target: destination.target } : {}) }}
          className={className}
        >
          {body}
        </Link>
      )
    case 'reference':
      return (
        <Link to="/requirements" hash={destination.hash} className={className}>
          {body}
        </Link>
      )
    case 'decision':
      return (
        <Link to="/system-design" hash={destination.hash} className={className}>
          {body}
        </Link>
      )
    case 'external':
      return (
        <a href={destination.href} target="_blank" rel="noopener noreferrer" className={className}>
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
  reference_document: { group: 'documents', label: 'Reference document' },
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

interface VersionRow {
  version: number
  status?: LineageNodeStatus
  // Tasks named by a returned serves or governs link from this version.
  tasks: Array<{ id: string; label: string; state?: string }>
}

interface LineageEntry {
  kind: EntryKind
  id: string
  label: string
  current: boolean
  // The walk returned the durable record node itself, not only versions.
  recordReturned: boolean
  versions: VersionRow[]
  archived: boolean
  // Secondary text from the returned status, such as a verdict's round and seat.
  detail?: string
  // The nearest other task the walk passed through to reach this entry.
  via?: { id: string; label: string }
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

const nodeKey = (node: Pick<LineageNode, 'type' | 'id'>) => `${node.type}:${node.id}`

// The nearest task other than the root that the returned edge path passes
// through before it reaches the node, or undefined when the node is the
// root's own. A node without a returned path counts as the root's own.
function viaTask(node: LineageNode, origin: Pick<LineageNode, 'type' | 'id'>): string | undefined {
  const path = node.edge_path ?? []
  let current: Pick<LineageNode, 'type' | 'id'> = origin
  const passed: Array<Pick<LineageNode, 'type' | 'id'>> = []
  for (const link of path) {
    current =
      link.src_type === current.type && link.src_id === current.id
        ? { type: link.dst_type, id: link.dst_id }
        : { type: link.src_type, id: link.src_id }
    passed.push(current)
  }
  passed.pop()
  const task = passed.reverse().find((step) => step.type === 'task' && nodeKey(step) !== nodeKey(origin))
  return task?.id
}

const verdictOutcomeLabels: Record<string, string> = {
  approve: 'Approved',
  approved: 'Approved',
  changes_requested: 'Changes requested',
}

function verdictText(status?: LineageNodeStatus) {
  if (!status?.outcome) return undefined
  const outcome = verdictOutcomeLabels[status.outcome] ?? status.outcome.replaceAll('_', ' ')
  const place = [
    status.review_round ? `Review round ${status.review_round}` : '',
    status.review_seat ? `seat ${status.review_seat}` : '',
  ]
    .filter(Boolean)
    .join(', ')
  return { label: outcome, detail: place || undefined }
}

function lineageEntries(graph: LineageGraph, origin: Pick<LineageNode, 'type' | 'id'>): LineageEntry[] {
  const originIdentity = entryIdentity(origin)
  const returned = new Map<string, LineageNode>()
  // Walk nodes come first: they carry depth, path, and status, which the
  // copy in roots may not.
  for (const node of [...graph.nodes, ...graph.roots]) {
    if (!returned.has(nodeKey(node))) returned.set(nodeKey(node), node)
  }
  const links = graph.links ?? []
  type Draft = LineageEntry & {
    recordLabel?: string
    versionLabel?: string
    shallowest?: { depth: number; via?: string }
    ownNode: boolean
  }
  const entries = new Map<string, Draft>()
  for (const node of returned.values()) {
    if (!(node.type in versionedKinds) && !(node.type in kindInfo)) continue
    const identity = entryIdentity(node)
    const key = `${identity.kind}:${identity.id}`
    const entry: Draft = entries.get(key) ?? {
      kind: identity.kind,
      id: identity.id,
      label: identity.id,
      current: originIdentity.kind === identity.kind && originIdentity.id === identity.id,
      recordReturned: false,
      versions: [],
      archived: false,
      ownNode: false,
    }
    const label = node.label?.trim()
    if (node.status?.archived) entry.archived = true
    if (identity.version === undefined) {
      entry.recordReturned = true
      if (label) entry.recordLabel = label
      if (identity.kind === 'verdict') {
        const verdict = verdictText(node.status)
        if (verdict) {
          entry.recordLabel = verdict.label
          entry.detail = verdict.detail
        }
      }
    } else {
      if (!entry.versions.some((version) => version.version === identity.version)) {
        const tasks = links
          .filter(
            (link) =>
              link.src_type === node.type &&
              link.src_id === node.id &&
              link.dst_type === 'task' &&
              (link.kind === 'serves' || link.kind === 'governs'),
          )
          .map((link) => link.dst_id)
          .filter((taskId, index, all) => all.indexOf(taskId) === index)
          .map((taskId) => {
            const task = returned.get(nodeKey({ type: 'task', id: taskId }))
            return { id: taskId, label: task?.label?.trim() || taskId, state: task?.status?.state }
          })
          .sort((left, right) => left.label.localeCompare(right.label) || left.id.localeCompare(right.id))
        entry.versions.push({ version: identity.version, status: node.status, tasks })
      }
      if (label && !entry.versionLabel) entry.versionLabel = label
    }
    // Root-relative grouping reads the returned depth and edge path of the
    // entry's shallowest node; any node the root reaches without passing
    // through another task makes the entry the root's own.
    const via = viaTask(node, origin)
    if (via === undefined) entry.ownNode = true
    const depth = node.depth ?? 0
    if (!entry.shallowest || depth < entry.shallowest.depth) entry.shallowest = { depth, via }
    entries.set(key, entry)
  }
  return [...entries.values()]
    .map(({ recordLabel, versionLabel, shallowest, ownNode, ...entry }) => {
      const viaId = entry.current || ownNode ? undefined : shallowest?.via
      const viaNode = viaId ? returned.get(nodeKey({ type: 'task', id: viaId })) : undefined
      return {
        ...entry,
        // Labels come from the response; the identifier is the fallback so an
        // entry never renders blank and never invents a title.
        label: recordLabel ?? versionLabel ?? entry.id,
        versions: [...entry.versions].sort((left, right) => left.version - right.version),
        via: viaId ? { id: viaId, label: viaNode?.label?.trim() || viaId } : undefined,
      }
    })
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
  const newest = entry.versions[entry.versions.length - 1]?.version
  const target = entry.recordReturned ? undefined : newest
  // A truncated walk may have dropped the link a destination needs, so the
  // entry says the budget may have omitted it rather than that it does not
  // exist (component-web-document-surfaces).
  const bounded = isBounded(graph)
  const missing = (unbounded: string, omitted: string): Destination => ({
    type: 'unavailable',
    reason: bounded ? `This bounded view may have omitted ${omitted}.` : unbounded,
  })
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
        : missing(
            'No returned link names the planning session that produced this bundle.',
            'the link that names the planning session that produced this bundle',
          )
    }
    case 'work_order': {
      const taskId = returnedSource(links, 'task', 'work_order', entry.id, 'dispatches')
      return taskId
        ? { type: 'task', taskId }
        : missing(
            'No returned link names the task that dispatched this work order.',
            'the link that names the task that dispatched this work order',
          )
    }
    case 'verdict': {
      const order = returnedSource(links, 'work_order', 'verdict', entry.id, 'produced_verdict')
      const taskId = order ? returnedSource(links, 'task', 'work_order', order, 'dispatches') : undefined
      return taskId
        ? { type: 'task', taskId }
        : missing(
            'No returned links name the work order and task behind this verdict.',
            'the links that name the work order and task behind this verdict',
          )
    }
    case 'requirement':
      return { type: 'requirement', id: entry.id, target }
    case 'system_design':
      return { type: 'design', id: entry.id, target }
    case 'reference_document':
      return newest
        ? { type: 'reference', hash: `reference-${entry.id}-v${newest}` }
        : missing(
            'No returned version of this reference document can be opened.',
            'the versions of this reference document',
          )
    case 'decision':
      return { type: 'decision', hash: `decision-${entry.id.toLowerCase()}` }
    case 'repository_path': {
      const governing = returnedSource(links, 'system_design_version', 'repository_path', entry.id, 'governs')
      const design = governing ? entryIdentity({ type: 'system_design_version', id: governing }) : undefined
      return design
        ? { type: 'design', id: design.id, target: design.version }
        : missing(
            'No returned System Design version governs this path.',
            'the link that names the System Design version governing this path',
          )
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
