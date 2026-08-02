import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { ArrowRight, Check, Download, FileText, FileUp, GitBranch, MessageSquarePlus, Sparkles } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { LineageGraphCard } from '../components/lineage/lineage-graph-card'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import {
  confirmRequirementVersion,
  downloadArtifact,
  fetchRequirement,
  fetchRequirements,
  fetchRequirementVersions,
  uploadArtifact,
} from '../lib/api'
import { taskStateLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import type { RequirementVersion, RequirementView, TaskEvent } from '../lib/types'

const originLabels: Record<RequirementVersion['origin'], string> = {
  chat: 'Planning conversation',
  drift_amendment: 'Delivery drift',
  feature_migration: 'Migrated feature',
}

export function RequirementsPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const search = useSearch({ from: '/requirements' })
  const { data: requirements, isLoading, error } = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
  })
  const selectedId = search.requirement ?? ''

  useEffect(() => {
    if (!requirements?.length) return
    if (!requirements.some((item) => item.requirement.id === selectedId)) {
      void navigate({ to: '/requirements', search: { requirement: requirements[0].requirement.id }, replace: true })
    }
  }, [navigate, requirements, selectedId])
  const selected = requirements?.find((item) => item.requirement.id === selectedId)

  const selectRequirement = (requirement: string) => {
    void navigate({ to: '/requirements', search: { requirement }, replace: true })
  }
  const startPlanning = (requirementId?: string) => {
    if (requirementId) sessionStorage.setItem('conveyor-planning-requirement', requirementId)
    else sessionStorage.removeItem('conveyor-planning-requirement')
    void navigate({ to: '/planning' })
  }

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-7xl px-6 py-8">
        <header className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-xl font-semibold tracking-tight">Requirements</h1>
              <Badge variant="mono">{requirements?.length ?? 0}</Badge>
            </div>
            <p className="mt-1 max-w-2xl text-sm text-muted">Living intent documents, confirmed by an operator and connected to the blueprints that deliver them.</p>
          </div>
          <Button onClick={() => startPlanning()}><MessageSquarePlus /> New planning session</Button>
        </header>

        {!workspace && <EmptyMessage>Choose a workspace to open its requirement corpus.</EmptyMessage>}
        {isLoading && <EmptyMessage>Loading requirement documents…</EmptyMessage>}
        {error && <EmptyMessage tone="failure">{errorMessage(error, 'Could not load requirements.')}</EmptyMessage>}
        {requirements?.length === 0 && (
          <Card className="mt-8 border-dashed">
            <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
              <Sparkles className="size-7 text-primary" />
              <h2 className="mt-4 text-base font-semibold">Start with intent, not filing</h2>
              <p className="mt-2 max-w-md text-sm leading-6 text-muted">Describe what the system should do. Planning turns the conversation into a structured requirement for you to confirm.</p>
              <Button className="mt-5" onClick={() => startPlanning()}>Plan a requirement <ArrowRight /></Button>
            </CardContent>
          </Card>
        )}

        {requirements && requirements.length > 0 && (
          <div className="mt-7 grid min-h-[620px] gap-4 lg:grid-cols-[330px_minmax(0,1fr)]">
            <Card className="self-start overflow-hidden">
              <CardHeader><CardTitle>Living documents</CardTitle><span className="text-[11px] text-faint">Flat corpus</span></CardHeader>
              <div className="divide-y divide-border">
                {requirements.map((item) => (
                  <button
                    key={item.requirement.id}
                    type="button"
                    aria-current={selectedId === item.requirement.id ? 'true' : undefined}
                    onClick={() => selectRequirement(item.requirement.id)}
                    className={`block w-full px-4 py-3.5 text-left transition-colors ${selectedId === item.requirement.id ? 'bg-primary-soft' : 'hover:bg-surface'}`}
                  >
                    <span className="flex items-start gap-3">
                      <FileText className={`mt-0.5 size-4 shrink-0 ${selectedId === item.requirement.id ? 'text-primary' : 'text-faint'}`} />
                      <span className="min-w-0 flex-1">
                        <strong className="block truncate text-sm font-medium">{item.requirement.title}</strong>
                        <span className="mt-1 flex flex-wrap items-center gap-1.5">
                          {item.current_version
                            ? <Badge variant="positive"><Check /> Confirmed v{item.current_version.version}</Badge>
                            : <Badge variant="attention">Needs confirmation</Badge>}
                          <RequirementStateBadges item={item} compact />
                          {item.serving_blueprints.length > 0 && <Badge variant="mono">{item.serving_blueprints.length} blueprints</Badge>}
                        </span>
                      </span>
                    </span>
                  </button>
                ))}
              </div>
            </Card>
            {selected && <RequirementDetail key={selected.requirement.id} seed={selected} token={token} onPlan={() => startPlanning(selected.requirement.id)} />}
          </div>
        )}
      </div>
    </div>
  )
}

function RequirementDetail({ seed, token, onPlan }: { seed: RequirementView; token: string; onPlan: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const { data: item = seed, error: detailError } = useQuery({
    queryKey: ['requirement', workspace, seed.requirement.id],
    queryFn: () => fetchRequirement(seed.requirement.id),
    initialData: seed,
  })
  const { data: versions = [], error: versionsError } = useQuery({
    queryKey: ['requirement-versions', workspace, item.requirement.id],
    queryFn: () => fetchRequirementVersions(item.requirement.id),
  })
  const orderedVersions = useMemo(() => [...versions].sort((left, right) => right.version - left.version), [versions])
  const [selectedVersion, setSelectedVersion] = useState<number | null>(null)
  useEffect(() => {
    if (!orderedVersions.length) return
    if (!orderedVersions.some((version) => version.version === selectedVersion)) {
      setSelectedVersion(orderedVersions[0].version)
    }
  }, [orderedVersions, selectedVersion])
  const displayed = orderedVersions.find((version) => version.version === selectedVersion)
    ?? item.pending_versions.at(-1)
    ?? item.current_version
  const currentVersion = item.current_version?.version ?? 0
  const confirm = useMutation({
    mutationFn: (version: number) => confirmRequirementVersion(token, item.requirement.id, version, currentVersion),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['requirements', workspace] }),
        client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] }),
        client.invalidateQueries({ queryKey: ['requirement-versions', workspace, item.requirement.id] }),
      ])
    },
    onError: () => {
      void client.invalidateQueries({ queryKey: ['requirements', workspace] })
      void client.invalidateQueries({ queryKey: ['requirement', workspace, item.requirement.id] })
      void client.invalidateQueries({ queryKey: ['requirement-versions', workspace, item.requirement.id] })
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

  if (detailError) return <EmptyMessage tone="failure">{errorMessage(detailError, 'Could not load this requirement.')}</EmptyMessage>
  return (
    <div className="min-w-0 space-y-4">
      <Card>
        <CardHeader className="items-center">
          <div className="min-w-0">
            <p className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{item.requirement.slug}</p>
            <h2 className="mt-1 truncate text-lg font-semibold">{item.requirement.title}</h2>
          </div>
          <Button variant="secondary" size="sm" onClick={onPlan}><MessageSquarePlus /> Plan work</Button>
        </CardHeader>
        <CardContent className="space-y-4">
          <RequirementStateNotices item={item} />
          {versionsError && <p className="rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">{errorMessage(versionsError, 'Could not load version history.')}</p>}
          {orderedVersions.length > 0 && (
            <div>
              <p className="mb-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Version history</p>
			  <section className="flex flex-wrap gap-2" aria-label="Requirement versions">
                {orderedVersions.map((version) => (
                  <button
                    key={version.version}
                    type="button"
                    aria-pressed={displayed?.version === version.version}
                    onClick={() => setSelectedVersion(version.version)}
                    className={`rounded-md border px-2.5 py-2 text-left text-xs ${displayed?.version === version.version ? 'border-primary bg-primary-soft' : 'border-border hover:bg-surface'}`}
                  >
                    <span className="font-medium">Version {version.version}</span>
                    <span className="mt-1 flex gap-1">
                      <Badge variant={version.confirmed ? 'positive' : 'attention'}>{version.confirmed ? 'Confirmed' : 'Pending'}</Badge>
                      <Badge variant="mono">{originLabels[version.origin]}</Badge>
                    </span>
                  </button>
                ))}
			  </section>
            </div>
          )}
          {displayed ? (
            <>
              <div className="flex flex-wrap items-center gap-2 border-t border-border pt-4">
                <Badge variant={displayed.confirmed ? 'positive' : 'attention'}>{displayed.confirmed ? `Confirmed v${displayed.version}` : `Pending v${displayed.version}`}</Badge>
                <Badge variant="mono">{originLabels[displayed.origin]}</Badge>
                <time className="ml-auto text-[11px] text-faint">{formatDate(displayed.created_at)}</time>
              </div>
              <RequirementDocument version={displayed} />
              {!displayed.confirmed && item.current_version && <RequirementDiff current={item.current_version} pending={displayed} />}
              {!displayed.confirmed && (
                <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-attention/30 bg-attention-soft px-4 py-3">
                  <p className="text-xs leading-5 text-attention">This revision is not current intent until an operator confirms it.</p>
                  <Button
                    size="sm"
                    disabled={!token || confirm.isPending || !item.confirmation_eligible}
                    title={!item.confirmation_eligible ? 'Revise this migrated seed before confirming it.' : undefined}
                    onClick={() => confirm.mutate(displayed.version)}
                  >
                    <Check /> {confirm.isPending && confirm.variables === displayed.version ? 'Confirming…' : `Confirm version ${displayed.version}`}
                  </Button>
                  {!item.confirmation_eligible && <p className="basis-full text-xs text-muted">A migrated seed needs its first deliberate revision before it can be confirmed.</p>}
                  {confirm.error && confirm.variables === displayed.version && (
                    <p className="basis-full text-xs text-failure">{errorMessage(confirm.error, 'Could not confirm this version.')}</p>
                  )}
                </div>
              )}
            </>
          ) : <p className="text-sm text-muted">No requirement version has been proposed yet.</p>}
        </CardContent>
      </Card>

      <Card>
        <CardHeader><CardTitle>Serving blueprints</CardTitle><Badge variant="mono">{item.serving_blueprints.length}</Badge></CardHeader>
        <CardContent className="space-y-2">
          {item.serving_blueprints.length === 0 && <p className="text-sm text-muted">No blueprint has been planned in this requirement’s context yet.</p>}
          {item.serving_blueprints.map(({ task, spec }) => (
            <Link key={task.id} to="/blueprints/$taskId" params={{ taskId: task.id }} className="flex items-center gap-3 rounded-lg border border-border p-3 transition-colors hover:bg-surface">
              <GitBranch className="size-4 shrink-0 text-primary" />
              <span className="min-w-0 flex-1">
                <strong className="block truncate text-sm">{task.title}</strong>
                <span className="mt-1 flex gap-1.5">
                  <Badge variant="mono">{taskStateLabels[task.state] ?? humanize(task.state)}</Badge>
                  {spec && <Badge variant={spec.approved ? 'positive' : 'attention'}>Spec v{spec.version} {spec.approved ? 'approved' : 'at gate'}</Badge>}
                </span>
              </span>
              <ArrowRight className="size-4 text-faint" />
            </Link>
          ))}
        </CardContent>
      </Card>

      {item.planning_sessions.length > 0 && (
        <Card>
          <CardHeader><CardTitle>Planning provenance</CardTitle><Badge variant="mono">{item.planning_sessions.length}</Badge></CardHeader>
          <CardContent className="space-y-2">
            {item.planning_sessions.map((session) => (
              <div key={session.id} className="rounded-md border border-border p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <strong className="text-sm">{session.title || session.id}</strong>
                  {session.model && <Badge variant="mono">{session.model}{session.effort ? ` · ${session.effort}` : ''}</Badge>}
                  {session.exploration_output_tokens && <Badge variant="mono">{session.exploration_output_tokens.toLocaleString()} tokens/call</Badge>}
                </div>
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {Object.entries(session.pinned_revisions ?? {}).sort(([left], [right]) => left.localeCompare(right)).map(([repo, revision]) => (
                    <Badge key={repo} variant="mono">{repo}@{revision.slice(0, 12)}</Badge>
                  ))}
                </div>
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader><CardTitle>Context artifacts</CardTitle><Badge variant="mono">{item.artifacts.length}</Badge></CardHeader>
          <CardContent className="space-y-2">
            {item.artifacts.map((artifact) => (
			  <button type="button" key={`${artifact.id}-${artifact.role}`} onClick={() => void downloadArtifact(token, artifact)} className="flex w-full items-center gap-2 rounded-md border border-border p-2 text-left hover:bg-surface">
                <Download className="size-3.5 text-primary" /><span className="min-w-0 flex-1 truncate text-xs">{artifact.name}</span>
                <span className="font-mono text-[10px] text-faint">{artifact.size_bytes} B</span>
              </button>
            ))}
            <label className={`flex cursor-pointer items-center justify-center gap-2 rounded-md border border-dashed border-edge px-3 py-2 text-xs text-muted hover:bg-surface ${!token ? 'pointer-events-none opacity-40' : ''}`}>
              <FileUp className="size-4" /> {upload.isPending ? 'Uploading…' : 'Attach context'}
              <input className="hidden" type="file" disabled={!token || upload.isPending} onChange={(event) => {
                const file = event.target.files?.[0]
                if (file) upload.mutate(file)
                event.currentTarget.value = ''
              }} />
            </label>
            {upload.error && <p className="text-xs text-failure">{errorMessage(upload.error, 'Could not attach that file.')}</p>}
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Lineage</CardTitle><Badge variant="mono">{item.lineage.length}</Badge></CardHeader>
          <CardContent>
            {item.lineage.length === 0 && <p className="text-sm text-muted">Lineage appears as planning and delivery advance.</p>}
            {item.lineage.length > 0 && (
              <details>
                <summary className="cursor-pointer text-sm font-medium">Technical activity</summary>
                <ol className="mt-3 space-y-3">
                  {item.lineage.slice(-8).reverse().map((event) => (
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
      {item.lineage_graph && <LineageGraphCard graph={item.lineage_graph} title="Intent to delivery" />}
    </div>
  )
}

function RequirementDocument({ version }: { version: RequirementVersion }) {
  return (
    <div className="space-y-5">
      {stripStatementsFence(version.content) && <MarkdownProse>{stripStatementsFence(version.content)}</MarkdownProse>}
      {version.statements.length > 0 && (
        <section aria-label="Requirement statements">
          <h3 className="text-sm font-semibold">Requirement statements</h3>
          <ol className="mt-2 space-y-2">
            {version.statements.map((statement) => (
              <li key={statement.id} className="flex gap-3 rounded-md border border-border bg-surface px-3 py-2.5 text-sm">
                <Badge variant="mono">{statement.id}</Badge><span>{statement.statement}</span>
              </li>
            ))}
          </ol>
        </section>
      )}
    </div>
  )
}

function RequirementDiff({ current, pending }: { current: RequirementVersion; pending: RequirementVersion }) {
  const [currentLines, pendingLines] = lineChanges(documentText(current), documentText(pending))
  return (
    <details className="rounded-lg border border-border bg-surface/40" open>
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium">Compared with confirmed v{current.version}</summary>
      <div className="grid gap-px border-t border-border bg-border md:grid-cols-2">
        <div className="bg-card p-4"><p className="mb-2 text-xs font-medium text-failure">Current confirmed intent</p><pre className="whitespace-pre-wrap font-sans text-xs leading-5 text-muted">{currentLines.map((line, index) => <span key={`${index}-${line.text}`} className={line.changed ? 'block bg-failure-soft text-failure' : 'block'}>{line.text || ' '}</span>)}</pre></div>
        <div className="bg-card p-4"><p className="mb-2 text-xs font-medium text-positive">Pending revision</p><pre className="whitespace-pre-wrap font-sans text-xs leading-5">{pendingLines.map((line, index) => <span key={`${index}-${line.text}`} className={line.changed ? 'block bg-positive-soft text-positive' : 'block'}>{line.text || ' '}</span>)}</pre></div>
      </div>
    </details>
  )
}

function RequirementStateBadges({ item, compact = false }: { item: RequirementView; compact?: boolean }) {
  const shipped = item.shipped_past_intent
  const drift = item.staleness?.active_drift.length ?? 0
  return <>
    {shipped && <Badge variant="attention"><span title={shipped}>Code ahead of intent</span></Badge>}
    {drift > 0 && <Badge variant="attention">{drift} active drift</Badge>}
    {item.pending_versions.length > 0 && !item.migrated_seed && <Badge variant="attention">Revision pending</Badge>}
    {item.migrated_seed && <Badge variant="mono">Migrated seed</Badge>}
    {!compact && item.pending_versions.length === 0 && !shipped && drift === 0 && !item.migrated_seed && <Badge variant="positive">Intent aligned</Badge>}
  </>
}

function RequirementStateNotices({ item }: { item: RequirementView }) {
  const shipped = item.shipped_past_intent
  const activeDrift = item.staleness?.active_drift ?? []
  return (
	<section className="space-y-2" aria-label="Requirement alignment">
      {shipped && <p className="rounded-md border border-attention/30 bg-attention-soft px-3 py-2 text-xs text-attention">Code shipped past the confirmed intent. <span title={shipped}>Latest delivery: {shipped}.</span></p>}
      {activeDrift.length > 0 && <p className="rounded-md border border-attention/30 bg-attention-soft px-3 py-2 text-xs text-attention">{activeDrift.length} unreconciled repository change{activeDrift.length === 1 ? '' : 's'} affect this requirement through its delivery lineage.</p>}
      {item.pending_versions.length > 0 && !item.migrated_seed && <p className="rounded-md border border-attention/30 bg-attention-soft px-3 py-2 text-xs text-attention">A revision is pending operator confirmation.</p>}
      {item.migrated_seed && <p className="rounded-md border border-border bg-surface px-3 py-2 text-xs text-muted">This migrated seed is awaiting its first deliberate revision.</p>}
	</section>
  )
}

function stripStatementsFence(content: string) {
  return content.replace(/\n?```conveyor:requirements[\s\S]*?```\n?/g, '\n').trim()
}

function documentText(version: RequirementVersion) {
  const prose = stripStatementsFence(version.content)
  const statements = version.statements.map((statement) => `${statement.id}: ${statement.statement}`).join('\n')
  return [prose, statements].filter(Boolean).join('\n\n')
}

function lineChanges(current: string, pending: string) {
  const left = current.split('\n')
  const right = pending.split('\n')
  const common = Array.from({ length: left.length + 1 }, () => Array<number>(right.length + 1).fill(0))
  for (let i = left.length - 1; i >= 0; i--) {
    for (let j = right.length - 1; j >= 0; j--) {
      common[i][j] = left[i] === right[j] ? common[i + 1][j + 1] + 1 : Math.max(common[i + 1][j], common[i][j + 1])
    }
  }
  const unchangedLeft = new Set<number>()
  const unchangedRight = new Set<number>()
  let i = 0
  let j = 0
  while (i < left.length && j < right.length) {
    if (left[i] === right[j]) {
      unchangedLeft.add(i++)
      unchangedRight.add(j++)
    } else if (common[i + 1][j] >= common[i][j + 1]) i++
    else j++
  }
  return [
    left.map((text, index) => ({ text, changed: !unchangedLeft.has(index) })),
    right.map((text, index) => ({ text, changed: !unchangedRight.has(index) })),
  ]
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
  return <p className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}>{children}</p>
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}
