import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { Check, Clock, ExternalLink, History, Layers, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
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
import { VersionDiff } from '../components/documents/version-diff'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  confirmSystemDesignVersion,
  fetchDecisions,
  fetchSystemDesign,
  fetchSystemDesigns,
  resolveDecision,
  SystemDesignConflictError,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type { Decision, SystemDesignSummary, SystemDesignVersion, SystemDesignView } from '../lib/types'

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
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
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
    void navigate({
      to: '/system-design',
      search: { document: designs.data[0].document.id },
      replace: true,
    })
  }, [designs.data, navigate, selected])
  const grouped = useMemo(() => {
    const groups = new Map<string, SystemDesignSummary[]>()
    const needle = query.trim().toLocaleLowerCase()
    for (const item of (designs.data ?? []).filter((candidate) =>
      candidate.document.title.toLocaleLowerCase().includes(needle),
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
                  attentionCount={item.drift_count + item.pending_version_count}
                  selected={selected?.document.id === item.document.id}
                  onClick={() =>
                    void navigate({ to: '/system-design', search: { document: item.document.id }, replace: true })
                  }
                />
              ))}
            </DocumentTreeGroup>
          ))}
          {designs.isLoading && <DocumentTreeNote>Loading documents…</DocumentTreeNote>}
          {designs.error && (
            <DocumentTreeNote>{errorMessage(designs.error, 'Could not load these documents.')}</DocumentTreeNote>
          )}
          {!designs.isLoading && !designs.data?.length && (
            <DocumentTreeNote>Nothing written down yet.</DocumentTreeNote>
          )}
          {Boolean(designs.data?.length) && grouped.length === 0 && (
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
            />
          ) : selected && detail.isLoading ? (
            <div className="px-8 py-8 text-sm text-muted">Loading document…</div>
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
}: {
  item: SystemDesignView
  workspace: string
  decisionItems: AttentionItem[]
  settledDecisions: Decision[]
}) {
  const client = useQueryClient()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const displayed = item.current_version ?? item.pending_versions[0] ?? item.versions[item.versions.length - 1]
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
  const pending = item.pending_versions.at(-1)
  const deliveryConsultations = item.lineage.filter(
    (event) => event.kind === 'system_design.consulted' && event.payload?.consultation === 'delivery_no_revision',
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
        <Button disabled={confirm.isPending} onClick={() => confirm.mutate(version.version)}>
          <Check />
          {confirm.isPending && confirm.variables === version.version
            ? 'Confirming…'
            : `Confirm version ${version.version}`}
        </Button>
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
        <LineageExplorer type="system_design" id={item.document.id} />
      </header>

      <AttentionSurface items={attention} />

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
                  <p className="font-medium text-foreground">Consulted at delivery — no revision warranted</p>
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
                </li>
              )
            })}
          </ol>
        </section>
      )}

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

      {settledDecisions.length > 0 && (
        <section className="mt-10 border-t border-border pt-8" aria-label="Settled decisions">
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
        </section>
      )}
    </article>
  )
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
