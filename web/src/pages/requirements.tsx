import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import {
  ArrowRight,
  Check,
  Clock,
  Download,
  ExternalLink,
  FileText,
  FileUp,
  History,
  ListChecks,
  Paperclip,
  Search,
  Sparkles,
  Trash2,
} from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { AttentionSurface, type AttentionItem } from '../components/documents/attention-surface'
import { DriftResolutionForm } from '../components/documents/drift-resolution-form'
import { VersionDiff } from '../components/documents/version-diff'
import {
  DocumentTree,
  DocumentTreeGroup,
  DocumentTreeItem,
  DocumentTreeNote,
} from '../components/documents/document-tree'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  confirmRequirementVersion,
  acknowledgeRequirementStaleness,
  createRequirementStalenessFollowUp,
  fetchCheckpointContextCandidates,
  downloadArtifact,
  fetchRequirement,
  fetchRequirements,
  fetchRequirementVersions,
  fetchReferenceDocuments,
  fetchReferenceDocumentVersions,
  uploadReferenceDocument,
  deleteReferenceDocument,
  uploadArtifact,
  updateTaskContext,
} from '../lib/api'
import { sessionGoalLabel, taskStateLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import type {
  ReferenceDocument,
  ReferenceDocumentVersion,
  RequirementDerivation,
  RequirementVersion,
  RequirementView,
  TaskEvent,
  CheckpointContextCandidate,
} from '../lib/types'
import { Dialog } from '../components/ui/dialog'
import { Input, Select } from '../components/ui/input'

const originLabels: Record<RequirementVersion['origin'], string> = {
  chat: 'Written in a planning conversation',
  drift_amendment: 'Written from a delivery change',
  feature_migration: 'Carried over from the old feature list',
  operator: 'Written by an operator',
}

const referenceAnchor = /^reference-(.+)-v(\d+)$/
type RequirementSort = 'name' | 'created' | 'updated'
type SortDirection = 'ascending' | 'descending'

function timestamp(value: string) {
  const parsed = Date.parse(value)
  return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed
}

function compareText(left: string, right: string) {
  return left.localeCompare(right, 'en', { sensitivity: 'base' })
}

function compareTimestamp(left: string, right: string) {
  const leftTime = timestamp(left)
  const rightTime = timestamp(right)
  return leftTime === rightTime ? 0 : leftTime < rightTime ? -1 : 1
}

export function visibleRequirements(
  requirements: RequirementView[],
  query: string,
  sort: RequirementSort,
  direction: SortDirection,
) {
  const needle = query.trim().toLocaleLowerCase()
  const multiplier = direction === 'ascending' ? 1 : -1
  return requirements
    .filter((item) => item.requirement.title.toLocaleLowerCase().includes(needle))
    .sort((left, right) => {
      const comparison =
        sort === 'name'
          ? compareText(left.requirement.title, right.requirement.title)
          : compareTimestamp(
              left.requirement[sort === 'created' ? 'created_at' : 'updated_at'],
              right.requirement[sort === 'created' ? 'created_at' : 'updated_at'],
            )
      return comparison === 0 ? compareText(left.requirement.id, right.requirement.id) : comparison * multiplier
    })
}

function requirementAttentionCount(item: RequirementView) {
  return (
    item.pending_versions.length +
    Number(Boolean(item.staleness?.delivery_after_intent)) +
    Number(Boolean(item.staleness?.partial_evaluation)) +
    (item.staleness?.active_drift?.length ?? 0)
  )
}

/**
 * Requirements is a document tree beside a document canvas. The tree groups
 * the product overviews apart from
 * the requirement corpus; whichever document is selected becomes the canvas,
 * with its history and diffs collapsed underneath. Detailed machinery signals
 * and actions remain in the canvas attention surface; the tree may carry the
 * compact aggregate approved for the Requirements list. The
 * assistant column is withdrawn while in-product planning is parked — nothing
 * about propose→confirm changes: revisions still
 * arrive as proposed versions an operator confirms here.
 */
export function RequirementsPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const client = useQueryClient()
  const search = useSearch({ from: '/requirements' })
  const {
    data: requirements,
    isLoading,
    error,
  } = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
  })
  const { data: overviews = [] } = useQuery({
    queryKey: ['reference-documents', workspace],
    queryFn: fetchReferenceDocuments,
    enabled: Boolean(workspace),
  })
  const selectedId = search.requirement ?? ''
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<RequirementSort>('name')
  const [direction, setDirection] = useState<SortDirection>('ascending')
  const visible = useMemo(
    () => visibleRequirements(requirements ?? [], query, sort, direction),
    [direction, query, requirements, sort],
  )

  // Overviews open on the same canvas as requirements. Their selection lives
  // in the existing `#reference-<id>-v<n>` anchor rather than a new query key,
  // so the links proposals already carry keep working unchanged.
  const [openOverview, setOpenOverview] = useState<{ id: string; version?: number } | undefined>(() =>
    readOverviewAnchor(window.location.hash),
  )
  useEffect(() => {
    const follow = () => {
      const target = readOverviewAnchor(window.location.hash)
      if (target) setOpenOverview(target)
    }
    window.addEventListener('hashchange', follow)
    return () => window.removeEventListener('hashchange', follow)
  }, [])

  useEffect(() => {
    if (!requirements?.length) return
    if (requirements.some((item) => item.requirement.id === selectedId)) return
    void navigate({
      to: '/requirements',
      search: { requirement: requirements[0].requirement.id },
      replace: true,
    })
  }, [navigate, requirements, selectedId])
  const selected = requirements?.find((item) => item.requirement.id === selectedId)
  const openOverviewDocument = overviews.find((document) => document.id === openOverview?.id)

  const selectRequirement = (requirement: string) => {
    setOpenOverview(undefined)
    void navigate({ to: '/requirements', search: { requirement }, replace: true })
  }

  const upload = useMutation({
    mutationFn: ({ file, id }: { file: File; id?: string }) => uploadReferenceDocument(token, file, id),
    onSuccess: () => client.invalidateQueries({ queryKey: ['reference-documents', workspace] }),
  })
  const remove = useMutation({
    mutationFn: (id: string) => deleteReferenceDocument(token, id),
    onSuccess: () => {
      setOpenOverview(undefined)
      return client.invalidateQueries({ queryKey: ['reference-documents', workspace] })
    },
  })

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 items-center gap-3 border-b border-border px-6 py-4">
        <span className="flex size-8 items-center justify-center rounded-lg bg-primary-soft text-primary">
          <ListChecks className="size-4" />
        </span>
        <div className="min-w-0">
          <h1 className="text-lg font-semibold tracking-tight">Requirements</h1>
          <p className="mt-0.5 text-xs text-muted">What this product should do, written down and confirmed.</p>
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <DocumentTree>
          <DocumentTreeGroup label="Product overviews">
            {overviews.map((document) => (
              <DocumentTreeItem
                key={document.id}
                label={document.name}
                selected={openOverview?.id === document.id}
                onClick={() => setOpenOverview({ id: document.id })}
              />
            ))}
            {overviews.length === 0 && (
              <DocumentTreeNote>Add a product overview, personas, or a glossary.</DocumentTreeNote>
            )}
            <div className="px-2 pt-2">
              <label className="flex cursor-pointer items-center justify-center gap-2 rounded-md border border-dashed border-edge px-2 py-2 text-xs font-medium text-muted transition-colors hover:border-primary/40 hover:bg-surface hover:text-primary">
                <FileUp className="size-3.5" /> {upload.isPending ? 'Uploading…' : 'Add Markdown'}
                <input
                  className="hidden"
                  type="file"
                  accept=".md,.markdown,text/markdown"
                  disabled={!token || upload.isPending}
                  onChange={(event) => {
                    const file = event.target.files?.[0]
                    if (file) upload.mutate({ file })
                    event.currentTarget.value = ''
                  }}
                />
              </label>
              {upload.error && (
                <p className="mt-2 text-xs text-failure">
                  {errorMessage(upload.error, 'Could not add that overview.')}
                </p>
              )}
            </div>
          </DocumentTreeGroup>

          <DocumentTreeGroup label="Requirements">
            <div className="mb-2 space-y-2 px-2">
              <label className="relative block" htmlFor="requirement-search">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-faint" />
                <Input
                  id="requirement-search"
                  type="search"
                  aria-label="Search requirements"
                  placeholder="Search requirements"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  className="h-8 pl-8 text-xs"
                />
              </label>
              <div className="grid grid-cols-[1fr_auto] gap-1.5">
                <Select
                  aria-label="Sort requirements by"
                  value={sort}
                  onChange={(event) => setSort(event.target.value as RequirementSort)}
                  className="[&_select]:h-8 [&_select]:py-1 [&_select]:text-xs"
                >
                  <option value="name">Name</option>
                  <option value="created">Created</option>
                  <option value="updated">Updated</option>
                </Select>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  aria-label={`Sort ${direction}`}
                  title={`Sort ${direction}`}
                  onClick={() => setDirection((value) => (value === 'ascending' ? 'descending' : 'ascending'))}
                  className="h-8 min-w-14 px-2 text-xs"
                >
                  {direction === 'ascending' ? 'Asc' : 'Desc'}
                </Button>
              </div>
            </div>
            {visible.map((item) => (
              <DocumentTreeItem
                key={item.requirement.id}
                label={item.requirement.title}
                attentionCount={requirementAttentionCount(item)}
                selected={!openOverview && selectedId === item.requirement.id}
                onClick={() => selectRequirement(item.requirement.id)}
              />
            ))}
            {requirements?.length === 0 && <DocumentTreeNote>No requirements yet.</DocumentTreeNote>}
            {Boolean(requirements?.length) && visible.length === 0 && (
              <DocumentTreeNote>No requirements match your search.</DocumentTreeNote>
            )}
          </DocumentTreeGroup>
        </DocumentTree>

        <section aria-label="Requirement document" className="min-w-0 flex-1 overflow-y-auto">
          <div className="mx-auto max-w-4xl px-8 py-8">
            {!workspace && <EmptyMessage>Choose a workspace to open its requirements.</EmptyMessage>}
            {isLoading && <EmptyMessage>Loading requirements…</EmptyMessage>}
            {error && <EmptyMessage tone="failure">{errorMessage(error, 'Could not load requirements.')}</EmptyMessage>}
            {openOverviewDocument ? (
              <OverviewCanvas
                key={openOverviewDocument.id}
                document={openOverviewDocument}
                workspace={workspace}
                token={token}
                initialVersion={openOverview?.version}
                upload={(file) => upload.mutate({ file, id: openOverviewDocument.id })}
                uploading={upload.isPending}
                remove={() => remove.mutate(openOverviewDocument.id)}
                removing={remove.isPending}
              />
            ) : (
              <>
                {requirements?.length === 0 && (
                  <Card className="mt-2 rounded-xl border-dashed">
                    <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
                      <span className="mb-3 flex size-10 items-center justify-center rounded-full bg-primary-soft text-primary">
                        <ListChecks className="size-5" />
                      </span>
                      <h2 className="text-base font-semibold">Nothing written down yet</h2>
                      <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                        Requirements say what this product should do. Each one arrives as a proposed version for you to
                        read and confirm here.
                      </p>
                    </CardContent>
                  </Card>
                )}
                {selected && <RequirementCanvas key={selected.requirement.id} seed={selected} token={token} />}
              </>
            )}
          </div>
        </section>
      </div>
    </div>
  )
}

function readOverviewAnchor(hash: string) {
  const match = referenceAnchor.exec(hash.replace(/^#/, ''))
  return match ? { id: match[1], version: Number(match[2]) } : undefined
}

/**
 * A product overview reads on the same canvas as a requirement: the Markdown
 * is the hero, and its version history, comparison, and file actions sit under
 * it. Overviews are uploaded source material, so they
 * carry no machinery signals and no attention surface.
 */
function OverviewCanvas({
  document,
  workspace,
  token,
  initialVersion,
  upload,
  uploading,
  remove,
  removing,
}: {
  document: ReferenceDocument
  workspace: string
  token: string
  initialVersion?: number
  upload: (file: File) => void
  uploading: boolean
  remove: () => void
  removing: boolean
}) {
  const [selectedVersion, setSelectedVersion] = useState(initialVersion ?? document.current_version)
  useEffect(
    () => setSelectedVersion(initialVersion ?? document.current_version),
    [initialVersion, document.id, document.current_version],
  )
  const {
    data: versions = [],
    isLoading,
    error,
  } = useQuery({
    queryKey: ['reference-document-versions', workspace, document.id, document.current_version],
    queryFn: () => fetchReferenceDocumentVersions(document.id),
  })
  const selected = versions.find((version) => version.version === selectedVersion) ?? versions.at(-1)
  const prior = selected?.supersedes_version
    ? versions.find((version) => version.version === selected.supersedes_version)
    : undefined
  return (
    <article id={selected ? `reference-${document.id}-v${selected.version}` : undefined} className="scroll-mt-6">
      <header className="mb-8 flex items-start justify-between gap-4 border-b border-border pb-6">
        <div className="min-w-0">
          <span className="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2 py-0.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">
            <FileText className="size-3" /> Product overview
          </span>
          <h2 className="mt-3 text-[28px] font-semibold leading-tight tracking-tight text-balance">{document.name}</h2>
          <div className="mt-3 flex flex-wrap items-center gap-1.5">
            <Badge variant="mono">v{document.current_version}</Badge>
            <Badge variant="outline">Reference material</Badge>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <label
            className={`inline-flex h-8 cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-md border border-edge bg-background px-2.5 text-xs font-medium text-foreground transition-colors hover:bg-surface ${!token || uploading ? 'pointer-events-none opacity-40' : ''}`}
          >
            <FileUp className="size-3.5" /> {uploading ? 'Uploading…' : 'Re-upload'}
            <input
              className="hidden"
              type="file"
              accept=".md,.markdown,text/markdown"
              disabled={!token || uploading}
              onChange={(event) => {
                const file = event.target.files?.[0]
                if (file) upload(file)
                event.currentTarget.value = ''
              }}
            />
          </label>
          <Button
            variant="destructive"
            size="sm"
            disabled={!token || removing}
            onClick={() => {
              if (window.confirm(`Delete ${document.name}? Its saved versions will no longer appear here.`)) remove()
            }}
          >
            <Trash2 className="size-3.5" /> Delete
          </Button>
        </div>
      </header>
      {isLoading && <p className="text-sm text-muted">Loading version history…</p>}
      {error && <p className="text-sm text-failure">{errorMessage(error, 'Could not load version history.')}</p>}
      {selected && <MarkdownProse>{selected.content}</MarkdownProse>}
      {versions.length > 1 && (
        <label
          className="mt-8 block max-w-xs border-t border-border pt-4 text-xs font-medium"
          htmlFor="overview-version"
        >
          Read version
          <select
            id="overview-version"
            className="mt-1 h-8 w-full rounded-md border border-border bg-card px-2 text-xs"
            value={selected?.version ?? ''}
            onChange={(event) => setSelectedVersion(Number(event.target.value))}
          >
            {[...versions].reverse().map((version) => (
              <option key={version.version} value={version.version}>
                v{version.version} · {version.filename}
              </option>
            ))}
          </select>
        </label>
      )}
      {prior && selected && <OverviewDiff prior={prior} current={selected} />}
    </article>
  )
}

function OverviewDiff({ prior, current }: { prior: ReferenceDocumentVersion; current: ReferenceDocumentVersion }) {
  const [open, setOpen] = useState(false)
  return (
    <details
      className="mt-4 rounded-lg border border-border bg-surface/40"
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium">Compared with v{prior.version}</summary>
      <VersionDiff
        left={{
          content: prior.content,
          paneClassName: 'bg-card',
          preClassName: 'max-h-72 overflow-auto whitespace-pre-wrap p-3 text-[11px]',
        }}
        right={{
          content: current.content,
          paneClassName: 'bg-card',
          preClassName: 'max-h-72 overflow-auto whitespace-pre-wrap p-3 text-[11px]',
        }}
        bounded
        enabled={open}
      />
    </details>
  )
}

function RequirementCanvas({ seed, token }: { seed: RequirementView; token: string }) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const { data: item = seed, error: detailError } = useQuery({
    queryKey: ['requirement', workspace, seed.requirement.id],
    queryFn: () => fetchRequirement(seed.requirement.id),
    initialData: seed,
  })
  const servingTasks = item.serving_tasks ?? []
  const { data: versions = [], error: versionsError } = useQuery({
    queryKey: ['requirement-versions', workspace, item.requirement.id],
    queryFn: () => fetchRequirementVersions(item.requirement.id),
  })
  const orderedVersions = useMemo(() => [...versions].sort((left, right) => right.version - left.version), [versions])
  const [selectedVersion, setSelectedVersion] = useState<number | null>(null)
  // A newly proposed revision has to become the displayed version. Only
  // resetting when the selection disappears is not enough — the older version
  // is still in the refetched list, so the canvas would keep showing confirmed
  // intent while a revision waits.
  const highestSeen = useRef<number | null>(null)
  useEffect(() => {
    if (!orderedVersions.length) return
    const latest = orderedVersions[0].version
    const arrived = highestSeen.current === null || latest > highestSeen.current
    highestSeen.current = latest
    if (arrived || !orderedVersions.some((version) => version.version === selectedVersion)) {
      setSelectedVersion(latest)
    }
  }, [orderedVersions, selectedVersion])
  const displayed =
    orderedVersions.find((version) => version.version === selectedVersion) ??
    item.pending_versions.at(-1) ??
    item.current_version
  const { data: referenceDocuments = [] } = useQuery({
    queryKey: ['reference-documents', workspace],
    queryFn: fetchReferenceDocuments,
    enabled: Boolean(workspace),
  })
  useEffect(() => {
    if (!displayed || !/^#(?:req|ac)-/i.test(window.location.hash)) return
    const id = window.location.hash.slice(1).toLowerCase()
    requestAnimationFrame(() => documentGlobalByID(id)?.scrollIntoView())
  }, [displayed])
  const currentVersion = item.current_version?.version ?? 0
  const [attachmentOffer, setAttachmentOffer] = useState<number | null>(null)
  const confirm = useMutation({
    mutationFn: (version: number) => confirmRequirementVersion(token, item.requirement.id, version, currentVersion),
    onSuccess: ({ version }) => setAttachmentOffer(version.version),
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['requirements', workspace] }),
        client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] }),
        client.invalidateQueries({ queryKey: ['requirement-versions', workspace, item.requirement.id] }),
      ])
    },
  })
  const upload = useMutation({
    mutationFn: (file: File) => uploadArtifact(token, file, undefined, item.requirement.id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['requirements', workspace] })
      void client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] })
      void client.invalidateQueries({ queryKey: ['artifacts', workspace] })
    },
  })
  const refreshRequirement = async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: ['requirements', workspace] }),
      client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] }),
      client.invalidateQueries({ queryKey: ['tasks', workspace] }),
    ])
  }
  const acknowledgeStaleness = useMutation({
    mutationFn: (signalID: string) => acknowledgeRequirementStaleness(token, item.requirement.id, signalID),
    onSettled: refreshRequirement,
  })
  const fileStalenessFollowUp = useMutation({
    mutationFn: (signalID: string) => createRequirementStalenessFollowUp(token, item.requirement.id, signalID),
    onSettled: refreshRequirement,
  })

  if (detailError)
    return <EmptyMessage tone="failure">{errorMessage(detailError, 'Could not load this requirement.')}</EmptyMessage>

  const deliveries = item.staleness?.deliveries ?? []
  const suspectDeliveries = deliveries.filter((delivery) => delivery.needs_attention)
  const routineDeliveries = deliveries.filter((delivery) => !delivery.needs_attention)
  // Every entry below already exists as machinery state; this surface only
  // decides where it is voiced, and it is the only place it is voiced (AC-1.2).
  const attention: AttentionItem[] = [
    ...suspectDeliveries.map((delivery) => ({
      id: `staleness-${delivery.task_id}-${delivery.at}`,
      title: `${delivery.label} may have moved past the confirmed intent`,
      detail: (
        <div title={`${delivery.label} delivered ${formatDate(delivery.at)}`}>
          <p>{delivery.reasons.join(' · ')}</p>
          <p className="mt-1 font-mono text-[10px] text-faint">
            Task {delivery.task_id} · {delivery.event_kind} #{delivery.delivery_event_id}
          </p>
          {(delivery.pinned_version || delivery.current_version) && (
            <p className="text-[10px] text-faint">
              Served v{delivery.pinned_version || 'unknown'} · current at delivery v
              {delivery.current_version || 'unknown'}
            </p>
          )}
        </div>
      ),
      action: (
        <>
          <Link
            to="/tasks/$taskId"
            params={{ taskId: delivery.task_id }}
            className="text-xs font-medium text-primary hover:underline"
          >
            Inspect task
          </Link>
          {delivery.url && (
            <a
              className="text-xs font-medium text-primary hover:underline"
              href={delivery.url}
              target="_blank"
              rel="noreferrer"
            >
              Open PR <ExternalLink className="inline size-3" />
            </a>
          )}
          <Button
            size="sm"
            variant="secondary"
            disabled={!token || fileStalenessFollowUp.isPending || acknowledgeStaleness.isPending}
            onClick={() => fileStalenessFollowUp.mutate(delivery.signal_id)}
          >
            File a task
          </Button>
          <Button
            size="sm"
            variant="ghost"
            disabled={!token || acknowledgeStaleness.isPending || fileStalenessFollowUp.isPending}
            onClick={() => acknowledgeStaleness.mutate(delivery.signal_id)}
          >
            Dismiss
          </Button>
        </>
      ),
      error:
        acknowledgeStaleness.variables === delivery.signal_id && acknowledgeStaleness.error
          ? errorMessage(acknowledgeStaleness.error, 'Could not dismiss this signal.')
          : fileStalenessFollowUp.variables === delivery.signal_id && fileStalenessFollowUp.error
            ? errorMessage(fileStalenessFollowUp.error, 'Could not file a follow-up task.')
            : undefined,
    })),
    // A truncated lineage walk cannot prove the absence of newer delivery, so
    // partial evaluation is voiced rather than reported as alignment. It is a
    // machinery signal like any other and belongs here.
    ...(item.staleness?.partial_evaluation
      ? [
          {
            id: 'staleness-partial',
            title: 'Staleness could only be partially evaluated',
            detail: <>The bounded delivery lineage was truncated, so newer delivery may not be reflected here.</>,
          },
        ]
      : []),
    ...(item.staleness?.active_drift ?? []).map((entry) => ({
      id: `drift-${entry.id}`,
      title: `Code changed in ${entry.repository} without reaching this document`,
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
          <DriftResolutionForm
            drift={entry}
            surface="requirement"
            token={token}
            workspace={workspace}
            requirementID={item.requirement.id}
            onResolved={() =>
              Promise.all([
                client.invalidateQueries({ queryKey: ['requirements', workspace] }),
                client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] }),
                client.invalidateQueries({ queryKey: ['requirement-versions', workspace, item.requirement.id] }),
              ])
            }
          />
        </>
      ),
    })),
    ...item.pending_versions.map((version) => ({
      id: `pending-${version.version}`,
      title: `Version ${version.version} is waiting for you`,
      detail: (
        <>
          {originLabels[version.origin]} · {formatDate(version.created_at)}
          {!item.confirmation_eligible && (
            <span className="block">
              A migrated seed needs its first deliberate revision before it can be confirmed.
            </span>
          )}
          {version.derived_from && (
            <RequirementDerivationNotice derivation={version.derived_from} documents={referenceDocuments} />
          )}
        </>
      ),
      action: (
        <Button
          disabled={!token || confirm.isPending || !item.confirmation_eligible}
          title={!item.confirmation_eligible ? 'Revise this migrated seed before confirming it.' : undefined}
          onClick={() => confirm.mutate(version.version)}
        >
          <Check />
          {confirm.isPending && confirm.variables === version.version
            ? 'Confirming…'
            : `Confirm version ${version.version}`}
        </Button>
      ),
      error:
        confirm.error && confirm.variables === version.version
          ? errorMessage(confirm.error, 'Could not confirm this version.')
          : undefined,
    })),
  ]

  return (
    <div className="min-w-0">
      {attachmentOffer != null && (
        <CheckpointContextOffer
          requirementId={item.requirement.id}
          requirementTitle={item.requirement.title}
          token={token}
          onClose={() => setAttachmentOffer(null)}
        />
      )}
      <header className="mb-8 flex items-start gap-4 border-b border-border pb-6">
        <div className="min-w-0 flex-1">
          <span className="inline-flex items-center rounded-md border border-border bg-surface px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-faint">
            {item.requirement.slug}
          </span>
          <h2 className="mt-3 text-[28px] font-semibold leading-tight tracking-tight text-balance">
            {item.requirement.title}
          </h2>
          {displayed && (
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
          )}
        </div>
        {/* The document's corner affordance (REQ-3): what this intent reaches
            in work, delivery, and evidence, on demand. */}
        <LineageExplorer type="requirement" id={item.requirement.id} />
      </header>

      <AttentionSurface items={attention} />

      <div className="mt-8">
        {displayed ? (
          <RequirementDocument version={displayed} />
        ) : (
          <p className="text-sm text-muted">Nothing has been proposed for this requirement yet.</p>
        )}
      </div>

      {versionsError && (
        <p className="mt-4 rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
          {errorMessage(versionsError, 'Could not load version history.')}
        </p>
      )}

      {displayed && !displayed.confirmed && item.current_version && (
        <RequirementDiff current={item.current_version} pending={displayed} />
      )}

      {orderedVersions.length > 0 && (
        <section className="mt-8 border-t border-border pt-5" aria-label="Requirement versions">
          <h3 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            <History className="size-3.5" /> Version history
          </h3>
          <div className="mt-3 flex flex-wrap gap-2">
            {orderedVersions.map((version) => {
              const active = displayed?.version === version.version
              return (
                <button
                  key={version.version}
                  type="button"
                  aria-pressed={active}
                  aria-current={active ? 'true' : undefined}
                  onClick={() => setSelectedVersion(version.version)}
                  className={`rounded-full border px-3 py-1.5 text-left text-xs transition-colors ${active ? 'border-primary bg-primary text-primary-foreground shadow-sm' : 'border-border hover:border-edge hover:bg-surface'}`}
                >
                  <span className="font-medium">v{version.version}</span>
                  <span className={`ml-1.5 text-[11px] ${active ? 'text-primary-foreground/75' : 'text-faint'}`}>
                    {version.confirmed ? 'Confirmed' : 'Proposed'} · {originLabels[version.origin]}
                  </span>
                </button>
              )
            })}
          </div>
        </section>
      )}

      <div className="mt-10 space-y-4 border-t border-border pt-8">
        <h3 className="text-xs font-semibold uppercase tracking-[0.12em] text-faint">Delivery &amp; history</h3>
        {/*
          Delivery is task-centric: `serves` sits on the task itself (spec
          §21.58 change 6). Blueprints are retained below as the historical
          lens over records planned before the noun was retired, so they stay
          readable without being mislabelled as newly planned work.
        */}
        <Card className="rounded-lg">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <span className="flex size-5 items-center justify-center rounded-md bg-primary-soft text-primary">
                <ListChecks className="size-3" />
              </span>
              Delivery
            </CardTitle>
            <Badge variant="mono">{servingTasks.length}</Badge>
          </CardHeader>
          <CardContent className="space-y-2">
            {servingTasks.length === 0 && (
              <p className="text-sm text-muted">No work has been planned against this requirement yet.</p>
            )}
            {servingTasks.map((task) => (
              <Link
                key={task.id}
                to="/tasks/$taskId/full"
                params={{ taskId: task.id }}
                className="flex items-center gap-3 rounded-lg border border-border p-3 transition-colors hover:border-edge hover:bg-surface"
              >
                <span className="min-w-0 flex-1">
                  <strong className="block truncate text-sm">{task.title}</strong>
                  <span className="mt-1 flex gap-1.5">
                    <Badge variant="mono">{taskStateLabels[task.state] ?? humanize(task.state)}</Badge>
                  </span>
                </span>
                <ArrowRight className="size-4 shrink-0 text-faint" />
              </Link>
            ))}
          </CardContent>
        </Card>

        {item.serving_blueprints.length > 0 && (
          <Card className="rounded-lg">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <span className="flex size-5 items-center justify-center rounded-md bg-surface text-muted">
                  <History className="size-3" />
                </span>
                Delivery before blueprints retired
              </CardTitle>
              <Badge variant="mono">{item.serving_blueprints.length}</Badge>
            </CardHeader>
            <CardContent className="space-y-2">
              {item.serving_blueprints.map(({ task, spec }) => (
                <Link
                  key={task.id}
                  to="/blueprints/$taskId"
                  params={{ taskId: task.id }}
                  className="flex items-center gap-3 rounded-lg border border-border p-3 transition-colors hover:border-edge hover:bg-surface"
                >
                  <span className="min-w-0 flex-1">
                    <strong className="block truncate text-sm">{task.title}</strong>
                    <span className="mt-1 flex gap-1.5">
                      <Badge variant="mono">{taskStateLabels[task.state] ?? humanize(task.state)}</Badge>
                      {spec && (
                        <Badge variant="mono">
                          Plan v{spec.version} {spec.approved ? 'approved' : 'awaiting approval'}
                        </Badge>
                      )}
                    </span>
                  </span>
                  <ArrowRight className="size-4 shrink-0 text-faint" />
                </Link>
              ))}
            </CardContent>
          </Card>
        )}

        {item.planning_sessions.length > 0 && (
          <Card className="rounded-lg">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <span className="flex size-5 items-center justify-center rounded-md bg-primary-soft text-primary">
                  <Sparkles className="size-3" />
                </span>
                How this was written
              </CardTitle>
              <Badge variant="mono">{item.planning_sessions.length}</Badge>
            </CardHeader>
            <CardContent className="space-y-2">
              {item.planning_sessions.map((session) => (
                <div key={session.id} className="rounded-md border border-border p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <strong className="text-sm">{session.title || session.id}</strong>
                    <Badge variant="accent">{sessionGoalLabel(session)}</Badge>
                    {session.model && (
                      <Badge variant="mono" title="The model and effort this conversation ran with">
                        {session.model}
                        {session.effort ? ` · ${session.effort}` : ''}
                      </Badge>
                    )}
                    {session.exploration_output_tokens && (
                      <Badge variant="mono" title="Reading budget for each repository lookup">
                        {session.exploration_output_tokens.toLocaleString()} tokens/call
                      </Badge>
                    )}
                  </div>
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    {Object.entries(session.pinned_revisions ?? {})
                      .sort(([left], [right]) => left.localeCompare(right))
                      .map(([repo, revision]) => (
                        <Badge key={repo} variant="mono" title="The exact code this conversation read">
                          {repo}@{revision.slice(0, 12)}
                        </Badge>
                      ))}
                  </div>
                </div>
              ))}
            </CardContent>
          </Card>
        )}

        <div className="grid gap-4 xl:grid-cols-2">
          <Card className="rounded-lg">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <span className="flex size-5 items-center justify-center rounded-md bg-primary-soft text-primary">
                  <Paperclip className="size-3" />
                </span>
                Attached files
              </CardTitle>
              <Badge variant="mono">{item.artifacts.length}</Badge>
            </CardHeader>
            <CardContent className="space-y-2">
              {item.artifacts.map((artifact) => (
                <button
                  type="button"
                  key={`${artifact.id}-${artifact.role}`}
                  onClick={() => void downloadArtifact(token, artifact)}
                  className="flex w-full items-center gap-2 rounded-md border border-border p-2 text-left transition-colors hover:border-edge hover:bg-surface"
                >
                  <Download className="size-3.5 shrink-0 text-primary" />
                  <span className="min-w-0 flex-1 truncate text-xs">{artifact.name}</span>
                  <span className="font-mono text-[10px] text-faint">{artifact.size_bytes} B</span>
                </button>
              ))}
              <label
                className={`flex cursor-pointer items-center justify-center gap-2 rounded-md border border-dashed border-edge px-3 py-2 text-xs font-medium text-muted transition-colors hover:border-primary/40 hover:bg-surface hover:text-primary ${!token ? 'pointer-events-none opacity-40' : ''}`}
              >
                <FileUp className="size-4" /> {upload.isPending ? 'Uploading…' : 'Attach context'}
                <input
                  className="hidden"
                  type="file"
                  disabled={!token || upload.isPending}
                  onChange={(event) => {
                    const file = event.target.files?.[0]
                    if (file) upload.mutate(file)
                    event.currentTarget.value = ''
                  }}
                />
              </label>
              {upload.error && (
                <p className="text-xs text-failure">{errorMessage(upload.error, 'Could not attach that file.')}</p>
              )}
            </CardContent>
          </Card>
          <Card className="rounded-lg">
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <span className="flex size-5 items-center justify-center rounded-md bg-surface text-muted">
                  <Clock className="size-3" />
                </span>
                Activity
              </CardTitle>
              <Badge variant="mono">{item.lineage.length}</Badge>
            </CardHeader>
            <CardContent>
              {routineDeliveries.length > 0 && (
                <section aria-label="Delivery activity" className="mb-4 space-y-2">
                  <p className="text-xs font-medium text-muted">Delivery activity</p>
                  <ul className="space-y-2">
                    {routineDeliveries.map((delivery) => (
                      <li key={`${delivery.task_id}-${delivery.at}`} className="flex items-start gap-2 text-xs">
                        <span className="mt-1.5 size-1.5 shrink-0 rounded-full bg-edge" />
                        <span className="min-w-0 flex-1">
                          <Link
                            to="/tasks/$taskId"
                            params={{ taskId: delivery.task_id }}
                            className="font-medium hover:underline"
                          >
                            {delivery.label}
                          </Link>
                          <span className="block text-[10px] text-faint">Delivered {formatDate(delivery.at)}</span>
                          {delivery.follow_up && (
                            <Link
                              to="/tasks/$taskId"
                              params={{ taskId: delivery.follow_up.task_id }}
                              className="block text-[10px] font-medium text-primary hover:underline"
                            >
                              Follow-up: {delivery.follow_up.title}
                            </Link>
                          )}
                        </span>
                      </li>
                    ))}
                  </ul>
                </section>
              )}
              {item.lineage.length === 0 && routineDeliveries.length === 0 && (
                <p className="text-sm text-muted">Activity appears as planning and delivery advance.</p>
              )}
              {item.lineage.length > 0 && (
                <details>
                  <summary className="cursor-pointer text-sm font-medium">Technical activity</summary>
                  <ol className="mt-3 space-y-3">
                    {item.lineage
                      .slice(-8)
                      .reverse()
                      .map((event) => (
                        <li key={`${event.id}-${event.kind}`} className="flex gap-2 text-xs">
                          <span className="mt-1.5 size-1.5 shrink-0 rounded-full bg-edge" />
                          <span className="min-w-0 flex-1">
                            <strong className="font-medium">{eventLabel(event)}</strong>
                            <span className="mt-0.5 flex items-center gap-2">
                              <time className="text-[10px] text-faint">{formatDate(event.at)}</time>
                              {event.payload?.backfilled === true && <Badge variant="mono">Backfilled</Badge>}
                            </span>
                          </span>
                        </li>
                      ))}
                  </ol>
                </details>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

function CheckpointContextOffer({
  requirementId,
  requirementTitle,
  token,
  onClose,
}: {
  requirementId: string
  requirementTitle: string
  token: string
  onClose: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [selected, setSelected] = useState<string[]>([])
  const candidates = useQuery({
    queryKey: ['checkpoint-context-candidates', workspace, requirementId],
    queryFn: () => fetchCheckpointContextCandidates(requirementId),
  })
  useEffect(() => {
    if (candidates.data)
      setSelected((current) => current.filter((id) => candidates.data.some((task) => task.id === id)))
  }, [candidates.data])
  const attach = useMutation({
    mutationFn: async () => {
      const results = await Promise.allSettled(
        selected.map((taskId) =>
          updateTaskContext(token, taskId, { add: { requirement_ids: [requirementId] }, remove: {} }),
        ),
      )
      const failed = results.filter((result) => result.status === 'rejected')
      if (failed.length > 0) throw new Error(`Could not attach context to ${failed.length} selected task(s).`)
    },
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['checkpoint-context-candidates', workspace, requirementId] }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        ...selected.map((taskId) => client.invalidateQueries({ queryKey: ['task', taskId] })),
      ])
    },
    onSuccess: onClose,
  })
  if (candidates.isLoading) return null
  if (candidates.error)
    return (
      <Dialog label="Attach confirmed requirement" onClose={onClose}>
        <div className="space-y-4 px-5 py-4">
          <h2 className="font-semibold">Requirement confirmed</h2>
          <p className="text-sm text-failure">Could not check checkpoint-paused tasks for missing context.</p>
          <div className="flex justify-end">
            <Button variant="secondary" onClick={onClose}>
              Dismiss
            </Button>
          </div>
        </div>
      </Dialog>
    )
  if (!candidates.data?.length) return null
  return (
    <Dialog label="Attach confirmed requirement" onClose={() => !attach.isPending && onClose()}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="font-semibold">Attach this requirement to paused tasks?</h2>
        <p className="mt-1 text-sm leading-6 text-muted">
          {requirementTitle} was confirmed. These tasks are paused at an operator checkpoint and cannot see it yet.
        </p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <div className="space-y-2">
          {candidates.data.map((task: CheckpointContextCandidate) => (
            <label key={task.id} className="flex cursor-pointer items-start gap-2 rounded border border-border p-3">
              <input
                type="checkbox"
                className="mt-0.5 size-4 accent-primary"
                checked={selected.includes(task.id)}
                onChange={(event) =>
                  setSelected((current) =>
                    event.target.checked ? [...current, task.id] : current.filter((id) => id !== task.id),
                  )
                }
              />
              <span className="min-w-0 text-sm">
                <span className="block font-medium">{task.title}</span>
                <span className="font-mono text-xs text-faint">{task.id}</span>
              </span>
            </label>
          ))}
        </div>
        {attach.error && <p className="text-sm text-failure">{String(attach.error)}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={attach.isPending}>
            Dismiss
          </Button>
          <Button onClick={() => attach.mutate()} disabled={selected.length === 0 || attach.isPending}>
            {attach.isPending ? 'Attaching…' : `Attach to ${selected.length} task${selected.length === 1 ? '' : 's'}`}
          </Button>
        </div>
      </div>
    </Dialog>
  )
}

function RequirementDerivationNotice({
  derivation,
  documents,
}: {
  derivation: RequirementDerivation
  documents: ReferenceDocument[]
}) {
  const source = documents.find((document) => document.id === derivation.document_id)
  const anchor = `reference-${derivation.document_id}-v${derivation.version}`
  return (
    <span className="block" title="The exact saved source this proposal was written from.">
      Source:{' '}
      <a className="font-medium text-primary underline-offset-2 hover:underline" href={`#${anchor}`}>
        {source?.name ?? derivation.document_id} · version {derivation.version}
      </a>{' '}
      · section {derivation.section_anchor}
    </span>
  )
}

function documentGlobalByID(id: string) {
  return window.document.getElementById(id)
}

function RequirementDocument({ version }: { version: RequirementVersion }) {
  return (
    <div className="space-y-8">
      {stripStatementsFence(version.content) && <MarkdownProse>{stripStatementsFence(version.content)}</MarkdownProse>}
      {version.statements.length > 0 && (
        <section aria-label="Requirement statements">
          <h3 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
            <ListChecks className="size-3.5" /> What this requires
          </h3>
          <ol className="mt-4 space-y-3">
            {version.statements.map((statement, index) => (
              <li
                key={statement.id}
                id={statement.id.toLowerCase()}
                className="scroll-mt-6 overflow-hidden rounded-lg border border-border bg-card transition-colors hover:border-edge"
              >
                <div className="flex gap-3 p-4">
                  <a
                    href={`#${statement.id.toLowerCase()}`}
                    aria-label={`Link to ${statement.id}`}
                    className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary-soft font-mono text-[11px] font-semibold text-primary"
                    title={statement.id}
                  >
                    {index + 1}
                  </a>
                  <div className="min-w-0 flex-1">
                    <p className="text-sm leading-6 text-balance">{statement.statement}</p>
                    {statement.user_story && (
                      <p className="mt-3 border-l-2 border-edge pl-3 text-xs leading-5 text-muted italic">
                        As {statement.user_story.as_a}, I want {statement.user_story.i_want}, so that{' '}
                        {statement.user_story.so_that}.
                      </p>
                    )}
                  </div>
                </div>
                {(statement.acceptance_criteria?.length ?? 0) > 0 && (
                  <ol className="space-y-2.5 border-t border-border bg-surface/40 px-4 py-3">
                    {statement.acceptance_criteria?.map((criterion, criterionIndex) => (
                      <li key={criterion.id} id={criterion.id.toLowerCase()} className="scroll-mt-6 flex gap-3 text-xs">
                        <a
                          href={`#${criterion.id.toLowerCase()}`}
                          aria-label={`Link to ${criterion.id}`}
                          className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full border border-edge font-mono text-[9px] font-semibold text-muted hover:border-primary hover:text-primary"
                          title={criterion.id}
                        >
                          {criterionIndex + 1}
                        </a>
                        <span className="leading-5">{criterion.statement}</span>
                      </li>
                    ))}
                  </ol>
                )}
              </li>
            ))}
          </ol>
        </section>
      )}
    </div>
  )
}

function RequirementDiff({ current, pending }: { current: RequirementVersion; pending: RequirementVersion }) {
  return (
    <details className="mt-6 rounded-lg border border-border bg-surface/40" open>
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium">
        Compared with confirmed v{current.version}
      </summary>
      <VersionDiff
        left={{
          content: documentText(current),
          label: 'Confirmed today',
          labelClassName: 'mb-2 text-xs font-medium text-failure',
          preClassName: 'whitespace-pre-wrap font-sans text-xs leading-5 text-muted',
        }}
        right={{
          content: documentText(pending),
          label: 'Proposed',
          labelClassName: 'mb-2 text-xs font-medium text-positive',
        }}
      />
    </details>
  )
}

function stripStatementsFence(content: string) {
  return content.replace(/\n?```conveyor:requirements[\s\S]*?```\n?/g, '\n').trim()
}

function documentText(version: RequirementVersion) {
  const prose = stripStatementsFence(version.content)
  const statements = version.statements
    .flatMap((statement) => [
      `${statement.id}: ${statement.statement}`,
      ...(statement.user_story
        ? [
            `  As ${statement.user_story.as_a}, I want ${statement.user_story.i_want}, so that ${statement.user_story.so_that}.`,
          ]
        : []),
      ...(statement.acceptance_criteria ?? []).map((criterion) => `  ${criterion.id}: ${criterion.statement}`),
    ])
    .join('\n')
  return [prose, statements].filter(Boolean).join('\n\n')
}

function eventLabel(event: TaskEvent) {
  const labels: Record<string, string> = {
    'requirement.created': 'Requirement created',
    'requirement.version_proposed': 'Revision proposed',
    'requirement.version_confirmed': 'Revision confirmed',
    'merge.confirmed': 'Delivery merged',
    'merge.reconciled': 'Merged delivery reconciled',
  }
  return labels[event.kind] ?? humanize(event.kind.replaceAll('.', ' '))
}

function humanize(value: string) {
  const text = value.replaceAll('_', ' ').trim()
  return text ? text[0].toUpperCase() + text.slice(1) : 'Unknown'
}

function EmptyMessage({ children, tone = 'muted' }: { children: string; tone?: 'muted' | 'failure' }) {
  return (
    <p
      className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}
    >
      {children}
    </p>
  )
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}
