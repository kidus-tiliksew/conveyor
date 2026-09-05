import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { Archive, Check, Clock, ExternalLink, History, Layers, RotateCcw, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { ArchiveDocumentDialog, type SuccessorCandidate } from '../components/documents/archive-document-dialog'
import { type AttentionItem, AttentionSurface } from '../components/documents/attention-surface'
import { compareDocuments, type DocumentSort, type DocumentSortDirection } from '../components/documents/document-sort'
import {
  DocumentTree,
  DocumentTreeGroup,
  DocumentTreeItem,
  DocumentTreeNote,
  DocumentTreeToolbar,
} from '../components/documents/document-tree'
import { DriftResolutionForm } from '../components/documents/drift-resolution-form'
import { SuccessorLinks } from '../components/documents/successor-links'
import { VersionDiff } from '../components/documents/version-diff'
import { VersionDismissDialog } from '../components/documents/version-dismiss-dialog'
import { MoreDocumentEvents, useDocumentEvents } from '../components/documents/document-events'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  archiveSystemDesign,
  confirmSystemDesignVersion,
  dismissDecisionSupersessionSweep,
  dismissSystemDesignVersion,
  fetchDecisions,
  fetchRequirements,
  fetchSystemDesign,
  fetchSystemDesigns,
  resolveDecision,
  restoreSystemDesign,
  SystemDesignConflictError,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type {
  Decision,
  DecisionSupersessionSweepEntry,
  SystemDesignSummary,
  SystemDesignVersion,
  SystemDesignView,
} from '../lib/types'

const originLabels: Record<SystemDesignVersion['origin'], string> = {
  planning_session: 'Written in a planning conversation',
  implementation_deliberation: 'Proposed while implementing a task',
  operator: 'Written by an operator',
}

/**
 * System Design is a category tree beside a document canvas. The canvas is the
 * hero: the confirmed guide reads
 * first, its history and its diffs sit under it as collapsed detail, and the
 * one attention surface above it carries every signal that needs an operator.
 * The assistant column is withdrawn from presentation while in-product
 * planning is parked — its components and every
 * propose→confirm route stay exactly as they are.
 */
export function SystemDesignPage() {
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const client = useQueryClient()
  const search = useSearch({ from: '/system-design' })
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<DocumentSort>('updated')
  const [direction, setDirection] = useState<DocumentSortDirection>('descending')
  const designs = useQuery({
    queryKey: ['system-designs', workspace, { includeArchived: true }],
    queryFn: () => fetchSystemDesigns({ includeArchived: true }),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const decisions = useQuery({
    queryKey: ['decisions', workspace],
    queryFn: fetchDecisions,
    enabled: Boolean(workspace),
  })
  const resolve = useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'confirm' | 'dismiss' }) => resolveDecision(id, action),
    onSettled: () => void client.invalidateQueries({ queryKey: ['decisions', workspace] }),
  })
  const dismissSweep = useMutation({
    mutationFn: ({ decisionId, entry }: { decisionId: string; entry: DecisionSupersessionSweepEntry }) =>
      dismissDecisionSupersessionSweep(decisionId, entry.document_tier, entry.document_id),
    onSettled: () => void client.invalidateQueries({ queryKey: ['decisions', workspace] }),
  })
  const selected = designs.data?.find((item) => item.document.id === search.document)
  const detail = useQuery({
    queryKey: ['system-design', workspace, selected?.document.id],
    queryFn: () => fetchSystemDesign(selected?.document.id ?? ''),
    enabled: Boolean(workspace && selected?.document.id),
    staleTime: 60_000,
  })
  useEffect(() => {
    if (!decisions.data?.length || !window.location.hash) return
    const anchor = window.location.hash.slice(1)
    requestAnimationFrame(() => document.getElementById(anchor)?.scrollIntoView({ block: 'start' }))
  }, [decisions.data])
  useEffect(() => {
    if (!designs.data?.length || selected) return
    const fallback = designs.data.find((item) => !item.document.archived) ?? designs.data[0]
    void navigate({
      to: '/system-design',
      search: { document: fallback.document.id },
      replace: true,
    })
  }, [designs.data, navigate, selected])
  const grouped = useMemo(() => {
    const groups = new Map<string, SystemDesignSummary[]>()
    const needle = query.trim().toLocaleLowerCase()
    for (const item of (designs.data ?? []).filter(
      (candidate) => !candidate.document.archived && candidate.document.title.toLocaleLowerCase().includes(needle),
    )) {
      const list = groups.get(item.document.category) ?? []
      list.push(item)
      groups.set(item.document.category, list)
    }
    return [...groups.entries()].map(
      ([category, items]) =>
        [
          category,
          items.sort((left, right) => compareDocuments(left.document, right.document, sort, direction)),
        ] as const,
    )
  }, [designs.data, direction, query, sort])
  const archived = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase()
    return (designs.data ?? [])
      .filter((item) => item.document.archived && item.document.title.toLocaleLowerCase().includes(needle))
      .sort((left, right) => compareDocuments(left.document, right.document, sort, direction))
  }, [designs.data, direction, query, sort])
  const archivedCount = (designs.data ?? []).filter((item) => item.document.archived).length
  const liveCount = (designs.data ?? []).length - archivedCount
  const settledDecisions = (decisions.data ?? []).filter((decision) => decision.status !== 'proposed')
  // Decisions are workspace-wide, so they are voiced on whichever document is
  // open — the same place they were surfaced before.
  const decisionItems: AttentionItem[] = (decisions.data ?? [])
    .filter((decision) => decision.status === 'proposed')
    .map((decision) => ({
      id: `decision-${decision.id}`,
      anchor: `decision-${decision.id.toLowerCase()}`,
      title: `${decision.id} is waiting for your call`,
      detail: (
        <>
          <span className="block font-medium text-foreground">{decision.statement}</span>
          {decision.context}
        </>
      ),
      action: canConfirm ? (
        <>
          <Button
            size="sm"
            disabled={resolve.isPending}
            onClick={() => resolve.mutate({ id: decision.id, action: 'confirm' })}
          >
            <Check />
            {resolve.isPending && resolve.variables?.id === decision.id && resolve.variables.action === 'confirm'
              ? 'Confirming…'
              : 'Confirm'}
          </Button>
          <Button
            size="sm"
            variant="destructive"
            disabled={resolve.isPending}
            onClick={() => resolve.mutate({ id: decision.id, action: 'dismiss' })}
          >
            <X />
            {resolve.isPending && resolve.variables?.id === decision.id && resolve.variables.action === 'dismiss'
              ? 'Dismissing…'
              : 'Dismiss'}
          </Button>
        </>
      ) : undefined,
      error: resolve.error ? errorMessage(resolve.error, 'Could not resolve this decision.') : undefined,
    }))

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 items-center gap-3 border-b border-border px-6 py-4">
        <span className="flex size-8 items-center justify-center rounded-lg bg-primary-soft text-primary">
          <Layers className="size-4" />
        </span>
        <div className="min-w-0">
          <h1 className="text-lg font-semibold tracking-tight">System Design</h1>
          <p className="mt-0.5 text-xs text-muted">How this system works today, written down and confirmed.</p>
        </div>
      </header>
      <div className="flex min-h-0 flex-1">
        <DocumentTree>
          <DocumentTreeToolbar
            searchLabel="Search System Design"
            sortLabel="Sort System Design by"
            query={query}
            onQueryChange={setQuery}
            options={[
              { value: 'updated', label: 'Updated', initialDirection: 'descending' },
              { value: 'created', label: 'Created', initialDirection: 'descending' },
              { value: 'name', label: 'Name', initialDirection: 'ascending' },
            ]}
            sort={sort}
            direction={direction}
            onSortChange={(nextSort, nextDirection) => {
              setSort(nextSort as DocumentSort)
              setDirection(nextDirection)
            }}
          />
          {grouped.map(([category, items]) => (
            <DocumentTreeGroup key={category} label={category}>
              {items.map((item) => (
                <DocumentTreeItem
                  key={item.document.id}
                  label={item.document.title}
                  meta={
                    item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined
                  }
                  title={
                    item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined
                  }
                  attentionCount={item.drift_count + item.pending_version_count}
                  selected={selected?.document.id === item.document.id}
                  onClick={() =>
                    void navigate({ to: '/system-design', search: { document: item.document.id }, replace: true })
                  }
                />
              ))}
            </DocumentTreeGroup>
          ))}
          {archivedCount > 0 && (
            <DocumentTreeGroup label="Archived" collapsible defaultOpen={false}>
              {archived.map((item) => (
                <DocumentTreeItem
                  key={item.document.id}
                  label={item.document.title}
                  meta={
                    item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined
                  }
                  title={
                    item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined
                  }
                  tooltip={<SuccessorLinks ids={item.document.superseded_by} compact />}
                  selected={selected?.document.id === item.document.id}
                  onClick={() =>
                    void navigate({ to: '/system-design', search: { document: item.document.id }, replace: true })
                  }
                />
              ))}
              {archived.length === 0 && <DocumentTreeNote>No archived documents match your search.</DocumentTreeNote>}
            </DocumentTreeGroup>
          )}
          {designs.isLoading && <DocumentTreeNote>Loading documents…</DocumentTreeNote>}
          {designs.error && (
            <DocumentTreeNote>{errorMessage(designs.error, 'Could not load these documents.')}</DocumentTreeNote>
          )}
          {!designs.isLoading && !designs.data?.length && (
            <DocumentTreeNote>Nothing written down yet.</DocumentTreeNote>
          )}
          {liveCount > 0 && grouped.length === 0 && (
            <DocumentTreeNote>No documents match your search.</DocumentTreeNote>
          )}
        </DocumentTree>
        <main className="min-w-0 flex-1 overflow-y-auto">
          {detail.data ? (
            <DesignCanvas
              key={detail.data.document.id}
              item={detail.data}
              workspace={workspace}
              decisionItems={decisionItems}
              settledDecisions={settledDecisions}
              decisionsLoading={decisions.isLoading}
              decisionsError={decisions.error}
              canConfirmDecisions={canConfirm}
              dismissSweep={dismissSweep}
            />
          ) : selected && detail.isLoading ? (
            <div className="px-8 py-8 text-sm text-muted">Loading document…</div>
          ) : selected && detail.error ? (
            <div className="px-8 py-8 text-sm text-failure">
              {errorMessage(detail.error, 'Could not load this System Design document.')}
            </div>
          ) : (
            <div className="mx-auto max-w-2xl px-6 py-16">
              <Card className="rounded-xl border-dashed">
                <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
                  <span className="mb-3 flex size-10 items-center justify-center rounded-full bg-primary-soft text-primary">
                    <Layers className="size-5" />
                  </span>
                  <h2 className="text-base font-semibold">Write down how this system works</h2>
                  <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                    Each document describes one part of the system and names the code it covers. Propose a version, then
                    confirm the one the team should follow.
                  </p>
                </CardContent>
              </Card>
              {decisionItems.length > 0 && (
                <div className="mt-10">
                  <AttentionSurface items={decisionItems} />
                </div>
              )}
            </div>
          )}
        </main>
      </div>
    </div>
  )
}

function DesignCanvas({
  item,
  workspace,
  decisionItems,
  settledDecisions,
  decisionsLoading,
  decisionsError,
  canConfirmDecisions,
  dismissSweep,
}: {
  item: SystemDesignView
  workspace: string
  decisionItems: AttentionItem[]
  settledDecisions: Decision[]
  decisionsLoading: boolean
  decisionsError: Error | null
  canConfirmDecisions: boolean
  dismissSweep: ReturnType<
    typeof useMutation<
      DecisionSupersessionSweepEntry,
      Error,
      { decisionId: string; entry: DecisionSupersessionSweepEntry }
    >
  >
}) {
  const history = useDocumentEvents('system_design', item.document.id, item)
  const client = useQueryClient()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const displayed = item.current_version ?? item.pending_versions[0] ?? item.versions[item.versions.length - 1]
  const [dismissTarget, setDismissTarget] = useState<SystemDesignVersion | null>(null)
  const [archiveDialogOpen, setArchiveDialogOpen] = useState(false)
  const { data: successorRequirements = [] } = useQuery({
    queryKey: ['requirements', workspace, { includeArchived: false }],
    queryFn: () => fetchRequirements(),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const { data: successorDesigns = [] } = useQuery({
    queryKey: ['system-designs', workspace, { includeArchived: false }],
    queryFn: () => fetchSystemDesigns(),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const successorCandidates = useMemo<SuccessorCandidate[]>(
    () => [
      ...successorRequirements
        .filter((candidate) => !candidate.requirement.archived)
        .map((candidate) => ({
          id: candidate.requirement.id,
          title: candidate.requirement.title,
          kind: 'requirement' as const,
        })),
      ...successorDesigns
        .filter((candidate) => candidate.document.id !== item.document.id && !candidate.document.archived)
        .map((candidate) => ({
          id: candidate.document.id,
          title: candidate.document.title,
          kind: 'system_design' as const,
        })),
    ],
    [item.document.id, successorDesigns, successorRequirements],
  )
  const confirm = useMutation({
    mutationFn: (version: number) =>
      confirmSystemDesignVersion(item.document.id, version, item.document.current_version ?? 0),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
      void client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] })
    },
    onError: (error) => {
      if (error instanceof SystemDesignConflictError) {
        void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
        void client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] })
      }
    },
  })
  const dismiss = useMutation({
    mutationFn: (version: number) => dismissSystemDesignVersion(item.document.id, version),
    onSuccess: () => setDismissTarget(null),
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
        client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] }),
        client.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
        client.invalidateQueries({ queryKey: ['activity', workspace] }),
        client.invalidateQueries({ queryKey: ['task', workspace] }),
      ])
    },
  })
  const archive = useMutation({
    mutationFn: (supersededBy: string[] = []) =>
      item.document.archived
        ? restoreSystemDesign(item.document.id)
        : archiveSystemDesign(item.document.id, supersededBy),
    onSuccess: () => setArchiveDialogOpen(false),
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
        client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] }),
        client.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
      ])
    },
  })
  const pending = item.pending_versions.at(-1)
  const deliveryConsultations = history.events.filter(
    (event) => event.kind === 'system_design.consulted' && event.payload?.consultation === 'delivery_no_revision',
  )
  const archiveActivity = history.events.filter(
    (event) => event.kind === 'system_design.archived' || event.kind === 'system_design.restored',
  )

  // Every signal below is already produced by unchanged machinery; the surface
  // only decides where it is voiced. Nothing here re-derives state (AC-1.1).
  const attention: AttentionItem[] = [
    ...item.drift.map((entry) => ({
      id: `drift-${entry.id}`,
      title: `Code changed in ${entry.repository} without a matching update here`,
      detail: (
        <>
          {(entry.matching_paths ?? []).join(', ') || 'No file list was recorded for this change.'}
          <span className="ml-1 text-faint">· seen {formatDate(entry.detected_at)}</span>
        </>
      ),
      action: (
        <>
          {entry.source_url && (
            <a
              className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
              href={entry.source_url}
              target="_blank"
              rel="noreferrer"
            >
              Open the change <ExternalLink className="size-3" />
            </a>
          )}
          {canManageWorkspace && (
            <DriftResolutionForm
              drift={entry}
              surface="system_design"
              workspace={workspace}
              onResolved={() =>
                Promise.all([
                  client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
                  client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] }),
                  client.invalidateQueries({ queryKey: ['requirements', workspace] }),
                  client.invalidateQueries({ queryKey: ['requirement', workspace] }),
                ])
              }
            />
          )}
        </>
      ),
    })),
    ...item.pending_versions.map((version) => ({
      id: `pending-${version.version}`,
      title: `Version ${version.version} is waiting for you`,
      detail: (
        <>
          {originLabels[version.origin]} · {formatDate(version.created_at)}
          {item.pending_versions.length > 1 && '. Confirming a later version drops the earlier ones.'}
        </>
      ),
      action: canConfirm ? (
        <>
          <Button disabled={confirm.isPending || dismiss.isPending} onClick={() => confirm.mutate(version.version)}>
            <Check />
            {confirm.isPending && confirm.variables === version.version
              ? 'Confirming…'
              : `Confirm version ${version.version}`}
          </Button>
          <Button
            variant="destructive"
            disabled={confirm.isPending || dismiss.isPending}
            onClick={() => setDismissTarget(version)}
          >
            <X /> Dismiss
          </Button>
        </>
      ) : undefined,
      error:
        confirm.error && confirm.variables === version.version
          ? errorMessage(confirm.error, 'Could not confirm this version.')
          : undefined,
    })),
    ...decisionItems,
  ]

  return (
    <article className="mx-auto max-w-4xl px-8 py-8">
      {dismissTarget && (
        <VersionDismissDialog
          documentTitle={item.document.title}
          version={dismissTarget.version}
          pending={dismiss.isPending}
          error={dismiss.error ? errorMessage(dismiss.error, 'Could not dismiss this version.') : undefined}
          onCancel={() => setDismissTarget(null)}
          onConfirm={() => dismiss.mutate(dismissTarget.version)}
        />
      )}
      {archiveDialogOpen && (
        <ArchiveDocumentDialog
          documentTitle={item.document.title}
          candidates={successorCandidates}
          pending={archive.isPending}
          error={
            archive.error ? errorMessage(archive.error, 'Could not archive this System Design document.') : undefined
          }
          onCancel={() => setArchiveDialogOpen(false)}
          onConfirm={(supersededBy) => archive.mutate(supersededBy)}
        />
      )}
      <header className="mb-8 flex items-start gap-4 border-b border-border pb-6">
        <div className="min-w-0 flex-1">
          <span
            className="inline-flex items-center rounded-md border border-border bg-surface px-2 py-0.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint"
            title="The group this document belongs to"
          >
            {item.document.category}
          </span>
          <h2 className="mt-3 text-[28px] font-semibold leading-tight tracking-tight text-balance">
            {item.document.title}
          </h2>
          {displayed ? (
            <div className="mt-3 flex flex-wrap items-center gap-1.5">
              <Badge variant="mono">v{displayed.version}</Badge>
              <Badge variant={displayed.confirmed ? 'positive' : 'accent'}>
                {displayed.confirmed ? 'Confirmed' : 'Proposed'}
              </Badge>
              {item.document.archived && (
                <Badge
                  variant="outline"
                  title={
                    item.document.archived_by && item.document.archived_at
                      ? `Archived by ${item.document.archived_by} on ${formatDate(item.document.archived_at)}`
                      : 'Archived'
                  }
                >
                  Archived
                </Badge>
              )}
              <span className="inline-flex items-center gap-1 text-xs text-faint">
                <Clock className="size-3" />
                {formatDate(displayed.created_at)}
              </span>
            </div>
          ) : (
            <p className="mt-1.5 text-xs text-muted">No version has been written yet.</p>
          )}
        </div>
        {/* The document's corner affordance (REQ-3): the code, work, and
            evidence this guide governs, on demand. */}
        <div className="flex shrink-0 items-center gap-2">
          {canConfirm && (
            <Button
              size="sm"
              variant={item.document.archived ? 'secondary' : 'destructive'}
              disabled={archive.isPending}
              onClick={() => (item.document.archived ? archive.mutate([]) : setArchiveDialogOpen(true))}
            >
              {item.document.archived ? <RotateCcw /> : <Archive />}
              {archive.isPending
                ? item.document.archived
                  ? 'Restoring…'
                  : 'Archiving…'
                : item.document.archived
                  ? 'Restore'
                  : 'Archive'}
            </Button>
          )}
          <LineageExplorer type="system_design" id={item.document.id} />
        </div>
      </header>

      {archive.error && item.document.archived && (
        <p className="mb-3 rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
          {errorMessage(
            archive.error,
            `Could not ${item.document.archived ? 'restore' : 'archive'} this System Design document.`,
          )}
        </p>
      )}
      {item.document.archived ? (
        <section
          aria-label="Needs your attention"
          className="rounded-lg border border-border bg-surface/40 px-4 py-3 text-sm text-muted"
        >
          <p>This System Design document is archived.</p>
          <SuccessorLinks ids={item.document.superseded_by} />
        </section>
      ) : (
        <AttentionSurface items={attention} />
      )}

      <section className="mt-8">
        {displayed && <MarkdownProse>{displayed.content}</MarkdownProse>}
        {displayed && displayed.governs.length > 0 && (
          <div className="mt-8 flex flex-wrap items-center gap-2 border-t border-border pt-4">
            <span className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Covers</span>
            {displayed.governs.flatMap((scope) =>
              scope.paths.map((path) => (
                <Badge key={`${scope.repository}:${path}`} variant="mono" title="Code this document describes">
                  {scope.repository}:{path}
                </Badge>
              )),
            )}
          </div>
        )}
      </section>

      {pending && item.current_version && <DesignDiff current={item.current_version} pending={pending} />}

      {deliveryConsultations.length > 0 && (
        <section className="mt-8 border-t border-border pt-5" aria-label="Delivery history">
          <h3 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            <History className="size-3.5" /> Delivery history
          </h3>
          <ol className="mt-3 divide-y divide-border rounded-md border border-border">
            {deliveryConsultations.map((event) => {
              const taskID = String(event.payload?.delivery_task_id ?? '')
              const mergeSHA = String(event.payload?.merge_head_sha ?? '')
              const version = Number(event.payload?.version ?? 0)
              return (
                <li key={event.id} className="px-3 py-3 text-xs text-muted">
                  <p className="font-medium text-foreground">{systemDesignEventLabel(event.kind)}</p>
                  <p className="mt-1">
                    {version > 0 && <>Pinned version {version} · </>}
                    task <span className="font-mono">{taskID}</span>
                    {mergeSHA && (
                      <>
                        {' '}
                        · merge <span className="font-mono">{mergeSHA}</span>
                      </>
                    )}
                  </p>
                  <time className="mt-1 block text-[10px] text-faint">{formatDate(event.at)}</time>
                </li>
              )
            })}
          </ol>
        </section>
      )}

      {archiveActivity.length > 0 && (
        <section className="mt-8 border-t border-border pt-5" aria-label="Archive activity">
          <h3 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            <History className="size-3.5" /> Archive activity
          </h3>
          <ol className="mt-3 divide-y divide-border rounded-md border border-border">
            {archiveActivity.map((event) => (
              <li key={event.id} className="px-3 py-3 text-xs text-muted">
                <p className="font-medium text-foreground">{systemDesignEventLabel(event.kind)}</p>
                <time className="mt-1 block text-[10px] text-faint">{formatDate(event.at)}</time>
              </li>
            ))}
          </ol>
        </section>
      )}

      <section className="mt-4" aria-label="Document activity">
        <span className="mr-2 text-xs text-muted">{history.total} activity events</span>
        <MoreDocumentEvents history={history} />
      </section>

      {item.versions.length > 0 && (
        <details className="mt-8 border-t border-border pt-5">
          <summary className="flex cursor-pointer items-center gap-1.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            <History className="size-3.5" /> Version history
            <span className="text-faint">({item.versions.length})</span>
          </summary>
          <ol className="mt-3 divide-y divide-border rounded-md border border-border">
            {item.versions.map((version) => (
              <li key={version.version} className="px-3 py-2 text-xs">
                <details>
                  <summary className="flex cursor-pointer flex-wrap items-center gap-2">
                    <Badge variant="mono">v{version.version}</Badge>
                    <Badge variant={version.confirmed ? 'positive' : version.dismissed ? 'default' : 'accent'}>
                      {version.confirmed ? 'Confirmed' : version.dismissed ? 'Dismissed' : 'Proposed'}
                    </Badge>
                    {version.dismissed && version.dismissed_by && version.dismissed_at && (
                      <span className="text-faint">
                        Dismissed by {version.dismissed_by} on {formatDate(version.dismissed_at)}
                      </span>
                    )}
                    <span className="ml-auto font-medium text-primary hover:underline">Read version</span>
                  </summary>
                  <div className="mt-2 rounded-md bg-surface p-4">
                    <MarkdownProse>{version.content}</MarkdownProse>
                  </div>
                </details>
              </li>
            ))}
          </ol>
        </details>
      )}

      <section className="mt-10 border-t border-border pt-8" aria-label="Settled decisions">
        {decisionsLoading ? (
          <p className="text-sm text-muted">Loading decisions…</p>
        ) : decisionsError ? (
          <p className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
            {errorMessage(decisionsError, 'Could not load decisions.')}
          </p>
        ) : settledDecisions.length === 0 ? (
          <p className="text-sm text-muted">No settled decisions yet.</p>
        ) : (
          <Card className="rounded-lg">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <span className="flex size-5 items-center justify-center rounded-md bg-primary-soft text-primary">
                  <Check className="size-3" />
                </span>
                Settled decisions
              </CardTitle>
              <Badge variant="mono">{settledDecisions.length}</Badge>
            </CardHeader>
            <CardContent className="space-y-2">
              {settledDecisions.map((decision) => (
                <article
                  id={`decision-${decision.id.toLowerCase()}`}
                  key={decision.id}
                  className="scroll-mt-6 rounded-lg border border-border p-3"
                >
                  <div className="flex gap-2">
                    <Badge variant="mono">{decision.id}</Badge>
                    <Badge variant={decision.status === 'confirmed' ? 'positive' : 'default'}>{decision.status}</Badge>
                  </div>
                  <p className="mt-2 text-sm font-medium">{decision.statement}</p>
                  <p className="mt-1 text-xs text-muted">{decision.context}</p>
                  {(decision.supersedes || decision.superseded_by) && (
                    <p className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
                      {decision.supersedes && (
                        <span>
                          Supersedes <DecisionLink id={decision.supersedes} />
                        </span>
                      )}
                      {decision.superseded_by && (
                        <span>
                          Superseded by <DecisionLink id={decision.superseded_by} />
                        </span>
                      )}
                    </p>
                  )}
                  {decision.supersedes && decision.sweep && !decision.sweep.clean && (
                    <details className="mt-3 rounded-md border border-attention/40 bg-attention-soft/40 p-3">
                      <summary className="flex cursor-pointer flex-wrap items-center gap-2 text-xs font-medium">
                        <Badge variant="attention">
                          {decision.sweep.entries.filter((entry) => entry.status === 'open').length} open
                        </Badge>
                        documents still citing {decision.supersedes}
                      </summary>
                      <p className="mt-2 text-xs text-muted">
                        These signals manage operator attention. They do not block delivery, review, or merge.
                      </p>
                      <ul
                        className="mt-2 divide-y divide-border"
                        aria-label={`Documents still citing ${decision.supersedes}`}
                      >
                        {decision.sweep.entries.map((entry) => {
                          const pending =
                            dismissSweep.isPending &&
                            dismissSweep.variables?.decisionId === decision.id &&
                            dismissSweep.variables.entry.document_tier === entry.document_tier &&
                            dismissSweep.variables.entry.document_id === entry.document_id
                          const failed = dismissSweep.error && dismissSweep.variables?.entry === entry
                          return (
                            <li key={`${entry.document_tier}:${entry.document_id}`} className="py-2 text-xs">
                              <div className="flex flex-wrap items-center gap-2">
                                <SweepDocumentLink entry={entry} />
                                <Badge>{sweepTierLabel(entry.document_tier)}</Badge>
                                <Badge variant={entry.status === 'open' ? 'attention' : 'default'}>
                                  {entry.status}
                                </Badge>
                                <time className="text-faint" title={new Date(entry.detected_at).toLocaleString()}>
                                  Detected {formatDate(entry.detected_at)}
                                </time>
                                {entry.status === 'open' && canConfirmDecisions && (
                                  <Button
                                    className="ml-auto"
                                    size="sm"
                                    variant="secondary"
                                    disabled={dismissSweep.isPending}
                                    onClick={() => dismissSweep.mutate({ decisionId: decision.id, entry })}
                                  >
                                    {pending ? 'Dismissing…' : 'Dismiss'}
                                  </Button>
                                )}
                              </div>
                              {failed && (
                                <p className="mt-1 text-failure">
                                  {errorMessage(dismissSweep.error, 'Could not dismiss this signal.')}
                                </p>
                              )}
                            </li>
                          )
                        })}
                      </ul>
                    </details>
                  )}
                  {decision.status === 'confirmed' && decision.confirmed_by && decision.confirmed_at && (
                    <p className="mt-2 flex items-center gap-1 text-xs text-muted">
                      <Clock className="size-3" /> Confirmed by {decision.confirmed_by} on{' '}
                      {formatDate(decision.confirmed_at)}
                    </p>
                  )}
                  {decision.status === 'dismissed' && decision.dismissed_by && decision.dismissed_at && (
                    <p className="mt-2 flex items-center gap-1 text-xs text-muted">
                      <Clock className="size-3" /> Dismissed by {decision.dismissed_by} on{' '}
                      {formatDate(decision.dismissed_at)}
                    </p>
                  )}
                </article>
              ))}
            </CardContent>
          </Card>
        )}
      </section>
    </article>
  )
}

function systemDesignEventLabel(kind: string) {
  const labels: Record<string, string> = {
    'system_design.consulted': 'Consulted at delivery — no revision warranted',
    'system_design.archived': 'System Design document archived',
    'system_design.restored': 'System Design document restored',
  }
  return labels[kind] ?? kind.replaceAll('_', ' ').replaceAll('.', ' ')
}

function DecisionLink({ id }: { id: string }) {
  return (
    <a href={`#decision-${id.toLowerCase()}`} className="font-medium text-primary hover:underline">
      {id}
    </a>
  )
}

function SweepDocumentLink({ entry }: { entry: DecisionSupersessionSweepEntry }) {
  const className = 'font-medium text-primary hover:underline'
  if (entry.document_tier === 'system_design')
    return (
      <Link to="/system-design" search={{ document: entry.document_id }} className={className}>
        {entry.document_id}
      </Link>
    )
  if (entry.document_tier === 'requirement')
    return (
      <Link to="/requirements" search={{ requirement: entry.document_id }} className={className}>
        {entry.document_id}
      </Link>
    )
  return (
    <Link to="/requirements" hash={`reference-${entry.document_id}-v0`} className={className}>
      {entry.document_id}
    </Link>
  )
}

function sweepTierLabel(tier: DecisionSupersessionSweepEntry['document_tier']) {
  if (tier === 'system_design') return 'System Design'
  if (tier === 'reference_document') return 'Reference document'
  return 'Requirement'
}

// The pending-version comparison, presented exactly like the requirement
// diff: one details block, confirmed on the left in failure red, proposed on
// the right in positive green. Bounded because design documents run long.
function DesignDiff({ current, pending }: { current: SystemDesignVersion; pending: SystemDesignVersion }) {
  return (
    <details className="mt-6 rounded-lg border border-border bg-surface/40" open>
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium">
        Compared with confirmed v{current.version}
      </summary>
      <section aria-label="Pending version diff">
        <VersionDiff
          left={{
            content: current.content,
            label: 'Confirmed today',
            labelClassName: 'mb-2 text-xs font-medium text-failure',
            preClassName: 'whitespace-pre-wrap font-sans text-xs leading-5 text-muted',
          }}
          right={{
            content: pending.content,
            label: 'Proposed',
            labelClassName: 'mb-2 text-xs font-medium text-positive',
          }}
          bounded
        />
      </section>
    </details>
  )
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(value))
}
