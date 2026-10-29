import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { Archive, Check, Clock, ExternalLink, History, Layers, RotateCcw, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { ArchiveDocumentDialog, type SuccessorCandidate } from '../components/documents/archive-document-dialog'
import { type AttentionItem, AttentionSurface } from '../components/documents/attention-surface'
import { MoreDocumentEvents, useDocumentEvents } from '../components/documents/document-events'
import {
  archivedByDismissal,
  DocumentReview,
  dismissalArchiveConsequence,
  dismissalArchiveDescription,
  type ReviewSearch,
  reviewVersionLabel,
  selectedReviewVersion,
} from '../components/documents/document-review'
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
import { VersionDismissDialog } from '../components/documents/version-dismiss-dialog'
import { VersionReviseDialog } from '../components/documents/version-revise-dialog'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  archiveSystemDesign,
  confirmSystemDesignVersion,
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
import { decisionsQueryKey } from '../lib/query-keys'
import type { SystemDesignSummary, SystemDesignVersion, SystemDesignView } from '../lib/types'

const originLabels: Record<SystemDesignVersion['origin'], string> = {
  planning_session: 'Written in a planning conversation',
  implementation_deliberation: 'Proposed while implementing a task',
  operator: 'Written by an operator',
}

/**
 * System Design is a category tree beside a document canvas. The canvas is the
 * ordinary reading surface. Changes and History share its header and the one
 * attention surface carries every signal that needs an operator
 * (component-web-document-surfaces; req-document-operating-surfaces REQ-1/REQ-2).
 * The assistant column is withdrawn from presentation while in-product
 * planning is parked — its components and every
 * propose→confirm route stay exactly as they are.
 */
export function SystemDesignPage() {
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const client = useQueryClient()
  const search = useSearch({ from: '/w/$workspaceId/system-design' })
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<DocumentSort>('updated')
  const [direction, setDirection] = useState<DocumentSortDirection>('descending')
  const designs = useQuery({
    queryKey: ['system-designs', workspace, { includeArchived: true }],
    queryFn: () => fetchSystemDesigns(workspace, { includeArchived: true }),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  // Only proposed decisions are voiced here; confirmed, superseded, and
  // dismissed decisions live on the Decisions surface
  // (req-document-operating-surfaces AC-8.10).
  const decisions = useQuery({
    queryKey: decisionsQueryKey(workspace),
    queryFn: ({ signal }) => fetchDecisions(workspace, signal),
    enabled: Boolean(workspace),
  })
  const resolve = useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'confirm' | 'dismiss' }) =>
      resolveDecision(workspace, id, action),
    onSettled: () => void client.invalidateQueries({ queryKey: decisionsQueryKey(workspace) }),
  })
  const selected = designs.data?.find((item) => item.document.id === search.document)
  const detail = useQuery({
    queryKey: ['system-design', workspace, selected?.document.id],
    queryFn: ({ signal }) => fetchSystemDesign(selected?.document.id ?? '', workspace, signal),
    enabled: Boolean(workspace && selected?.document.id),
    staleTime: 60_000,
  })
  useEffect(() => {
    if (!designs.data?.length || selected) return
    const fallback = designs.data.find((item) => !item.document.archived) ?? designs.data[0]
    void navigate({
      to: '/w/$workspaceId/system-design',
      params: { workspaceId: workspace },
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
      <header className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-3 lg:px-6 lg:py-4">
        <span className="flex size-8 items-center justify-center rounded-lg bg-primary-soft text-primary">
          <Layers className="size-4" />
        </span>
        <div className="min-w-0">
          <h1 className="text-lg font-semibold tracking-tight">System Design</h1>
          <p className="mt-0.5 text-xs text-muted">How this system works today, written down and confirmed.</p>
        </div>
      </header>
      <div className="flex min-h-0 flex-1 flex-col lg:flex-row">
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
                    !item.current_version
                      ? 'not yet confirmed'
                      : item.document.superseded_by?.length
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
                    void navigate({
                      to: '/w/$workspaceId/system-design',
                      params: { workspaceId: workspace },
                      search: { document: item.document.id },
                      replace: true,
                    })
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
                    dismissalArchiveDescription(item.document) ??
                    (item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined)
                  }
                  title={
                    dismissalArchiveDescription(item.document) ??
                    (item.document.superseded_by?.length
                      ? `Superseded by ${item.document.superseded_by.join(', ')}`
                      : undefined)
                  }
                  tooltip={
                    <>
                      <p className="max-w-36 whitespace-pre-wrap break-words">
                        {dismissalArchiveDescription(item.document)}
                      </p>
                      <SuccessorLinks ids={item.document.superseded_by} compact />
                    </>
                  }
                  selected={selected?.document.id === item.document.id}
                  onClick={() =>
                    void navigate({
                      to: '/w/$workspaceId/system-design',
                      params: { workspaceId: workspace },
                      search: { document: item.document.id },
                      replace: true,
                    })
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
              key={`${workspace}:${detail.data.document.id}:${search.tab}:${search.base}:${search.target}`}
              search={search}
              item={detail.data}
              workspace={workspace}
              decisionItems={decisionItems}
            />
          ) : selected && detail.isLoading ? (
            <div className="px-4 py-6 text-sm text-muted sm:px-8 sm:py-8">Loading document…</div>
          ) : selected && detail.error ? (
            <div className="px-4 py-6 text-sm text-failure sm:px-8 sm:py-8">
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
  search,
  item,
  workspace,
  decisionItems,
}: {
  search: ReviewSearch & { document?: string; session?: string }
  item: SystemDesignView
  workspace: string
  decisionItems: AttentionItem[]
}) {
  const history = useDocumentEvents('system_design', item.document.id, item)
  const client = useQueryClient()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const navigate = useNavigate()
  const onReviewChange = (next: ReviewSearch) =>
    navigate({
      to: '/w/$workspaceId/system-design',
      params: { workspaceId: workspace },
      search: { ...search, ...next, base: next.base, target: next.target },
    })
  const reviewVersions = [
    ...new Map(
      [...item.versions, ...item.pending_versions, ...(item.current_version ? [item.current_version] : [])].map((v) => [
        v.version,
        v,
      ]),
    ).values(),
  ]
  const displayed = selectedReviewVersion(reviewVersions, item.current_version, search)
  const [dismissTarget, setDismissTarget] = useState<SystemDesignVersion | null>(null)
  const canPropose = useWorkspaceCapability('propose_documents')
  const [reviseTarget, setReviseTarget] = useState<SystemDesignVersion | null>(null)
  const [archiveDialogOpen, setArchiveDialogOpen] = useState(false)
  const { data: successorRequirements = [] } = useQuery({
    queryKey: ['requirements', workspace, { includeArchived: false }],
    queryFn: () => fetchRequirements(workspace),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const { data: successorDesigns = [] } = useQuery({
    queryKey: ['system-designs', workspace, { includeArchived: false }],
    queryFn: () => fetchSystemDesigns(workspace),
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
      confirmSystemDesignVersion(workspace, item.document.id, version, item.document.current_version ?? 0),
    onSettled: () => {
      void client.invalidateQueries({ queryKey: ['pending-proposals', workspace] })
      void client.invalidateQueries({ queryKey: ['activity', workspace] })
      void client.invalidateQueries({ queryKey: ['task', workspace] })
    },
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
    mutationFn: ({ version, note }: { version: number; note: string }) =>
      dismissSystemDesignVersion(workspace, item.document.id, version, note),
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
        ? restoreSystemDesign(workspace, item.document.id)
        : archiveSystemDesign(workspace, item.document.id, supersededBy),
    onSuccess: () => setArchiveDialogOpen(false),
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['system-designs', workspace] }),
        client.invalidateQueries({ queryKey: ['system-design', workspace, item.document.id] }),
        client.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
      ])
    },
  })
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
      title: `${item.document.title} · Version ${version.version} is waiting for you`,
      detail: (
        <>
          {!item.current_version && (
            <span className="block">
              This document has no confirmed version.
              {item.pending_versions.length === 1 && ` ${dismissalArchiveConsequence}`}
            </span>
          )}
          {originLabels[version.origin]} · {formatDate(version.created_at)}
          {item.pending_versions.length > 1 && '. Confirming a later version drops the earlier ones.'}
        </>
      ),
      action: (
        <>
          <Button
            variant="secondary"
            onClick={() =>
              onReviewChange({ tab: 'changes', target: version.version, base: item.current_version?.version ?? 0 })
            }
          >
            Review changes · v{version.version}
          </Button>
          {canConfirm &&
          ((search.tab === 'changes' && displayed?.version === version.version) ||
            ((!search.tab || search.tab === 'document') &&
              (search.target === undefined || displayed?.version === version.version))) ? (
            <>
              <Button disabled={confirm.isPending || dismiss.isPending} onClick={() => confirm.mutate(version.version)}>
                <Check />
                {confirm.isPending && confirm.variables === version.version
                  ? 'Confirming…'
                  : `Confirm version ${version.version}`}
              </Button>
              {canPropose && (
                <Button
                  variant="secondary"
                  disabled={confirm.isPending || dismiss.isPending}
                  onClick={() => setReviseTarget(version)}
                >
                  Revise
                </Button>
              )}
              <Button
                variant="destructive"
                disabled={confirm.isPending || dismiss.isPending}
                onClick={() => setDismissTarget(version)}
              >
                <X /> Dismiss
              </Button>
            </>
          ) : null}
        </>
      ),
      error:
        confirm.error && confirm.variables === version.version
          ? errorMessage(confirm.error, 'Could not confirm this version.')
          : undefined,
    })),
    ...decisionItems,
  ]

  return (
    <article className="mx-auto max-w-4xl px-4 py-6 sm:px-6 lg:px-8 lg:py-8">
      {!item.document.archived &&
        reviseTarget &&
        item.pending_versions.some((v) => v.version === reviseTarget.version) && (
          <VersionReviseDialog
            target={{
              id: item.document.id,
              title: item.document.title,
              tier: 'system_design',
              version: reviseTarget.version,
            }}
            onClose={() => setReviseTarget(null)}
          />
        )}
      {!item.document.archived &&
        dismissTarget &&
        item.pending_versions.some((v) => v.version === dismissTarget.version) && (
          <VersionDismissDialog
            documentTitle={item.document.title}
            version={dismissTarget.version}
            archivesDocument={!item.current_version && item.pending_versions.length === 1}
            pending={dismiss.isPending}
            error={dismiss.error ? errorMessage(dismiss.error, 'Could not dismiss this version.') : undefined}
            onCancel={() => setDismissTarget(null)}
            onConfirm={(note) => dismiss.mutate({ version: dismissTarget.version, note })}
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
      <header className="mb-8 flex flex-wrap items-start gap-4 border-b border-border pb-6">
        <div className="min-w-[14rem] flex-1">
          <span
            className="inline-flex items-center rounded-md border border-border bg-surface px-2 py-0.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint"
            title="The group this document belongs to"
          >
            {item.document.category}
          </span>
          <h2 className="mt-3 text-[28px] font-semibold leading-tight tracking-tight text-balance">
            {item.document.title}
          </h2>
          {!item.current_version && <p className="mt-2 text-sm text-muted">No confirmed version</p>}
          {displayed ? (
            <div className="mt-3 flex flex-wrap items-center gap-1.5">
              <Badge variant="mono">{reviewVersionLabel(displayed, Boolean(item.current_version))}</Badge>
              {displayed.confirmed && <Badge variant="positive">Confirmed</Badge>}
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
              <span className="inline-flex items-center gap-1 whitespace-nowrap text-xs text-faint">
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
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          {canConfirm && !archivedByDismissal(item.document) && (
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
      <DocumentReview
        search={search}
        onChange={onReviewChange}
        versions={reviewVersions}
        current={item.current_version}
        attention={
          <>
            {item.document.archived ? (
              <section
                aria-label="Needs your attention"
                className="rounded-lg border border-border bg-surface/40 px-4 py-3 text-sm text-muted"
              >
                <p className="whitespace-pre-wrap break-words">
                  {dismissalArchiveDescription(item.document) ?? 'This System Design document is archived.'}
                </p>
                <SuccessorLinks ids={item.document.superseded_by} />
              </section>
            ) : (
              <AttentionSurface
                items={
                  search.tab === 'changes'
                    ? [...attention].sort(
                        (a, b) =>
                          Number(b.id === `pending-${displayed?.version}`) -
                          Number(a.id === `pending-${displayed?.version}`),
                      )
                    : attention
                }
              />
            )}
          </>
        }
        document={
          <>
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
          </>
        }
        history={
          <>
            {' '}
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
          </>
        }
      />
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

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(value))
}
