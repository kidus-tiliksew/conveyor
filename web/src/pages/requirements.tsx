import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { ArrowRight, Check, Download, FileText, FileUp, GitBranch, MessageSquarePlus, Sparkles } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { MarkdownProse } from '../components/ui/markdown-prose'
import { confirmRequirementVersion, downloadArtifact, fetchRequirements, uploadArtifact } from '../lib/api'
import type { RequirementView } from '../lib/types'

export function RequirementsPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const navigate = useNavigate()
  const client = useQueryClient()
  const { data: requirements, isLoading, error } = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: fetchRequirements,
    enabled: Boolean(workspace),
  })
  const [selectedId, setSelectedId] = useState('')
  useEffect(() => {
    if (!requirements?.length) return
    if (!requirements.some((item) => item.requirement.id === selectedId)) {
      setSelectedId(requirements[0].requirement.id)
    }
  }, [requirements, selectedId])
  const selected = requirements?.find((item) => item.requirement.id === selectedId)

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
            <p className="mt-1 max-w-2xl text-sm text-muted">
              Living intent documents, confirmed by an operator and connected to the blueprints that deliver them.
            </p>
          </div>
          <Button onClick={() => startPlanning()}>
            <MessageSquarePlus /> New planning session
          </Button>
        </header>

        {!workspace && <EmptyMessage>Choose a workspace to open its requirement corpus.</EmptyMessage>}
        {isLoading && <EmptyMessage>Loading requirement documents…</EmptyMessage>}
        {error && <EmptyMessage tone="failure">{String(error)}</EmptyMessage>}
        {requirements?.length === 0 && (
          <Card className="mt-8 border-dashed">
            <CardContent className="flex min-h-56 flex-col items-center justify-center text-center">
              <Sparkles className="size-7 text-primary" />
              <h2 className="mt-4 text-base font-semibold">Start with intent, not filing</h2>
              <p className="mt-2 max-w-md text-sm leading-6 text-muted">
                Describe what the system should do. Planning turns the conversation into a structured requirement for you to confirm.
              </p>
              <Button className="mt-5" onClick={() => startPlanning()}>Plan a requirement <ArrowRight /></Button>
            </CardContent>
          </Card>
        )}

        {requirements && requirements.length > 0 && (
          <div className="mt-7 grid min-h-[620px] gap-4 lg:grid-cols-[330px_minmax(0,1fr)]">
            <Card className="self-start overflow-hidden">
              <CardHeader>
                <CardTitle>Living documents</CardTitle>
                <span className="text-[11px] text-faint">Flat corpus</span>
              </CardHeader>
              <div className="divide-y divide-border">
                {requirements.map((item) => (
                  <button
                    key={item.requirement.id}
                    type="button"
                    onClick={() => setSelectedId(item.requirement.id)}
                    className={`block w-full px-4 py-3.5 text-left transition-colors ${
                      selectedId === item.requirement.id ? 'bg-primary-soft' : 'hover:bg-surface'
                    }`}
                  >
                    <span className="flex items-start gap-3">
                      <FileText className={`mt-0.5 size-4 shrink-0 ${selectedId === item.requirement.id ? 'text-primary' : 'text-faint'}`} />
                      <span className="min-w-0 flex-1">
                        <strong className="block truncate text-sm font-medium">{item.requirement.title}</strong>
                        <span className="mt-1 flex flex-wrap items-center gap-1.5">
                          {item.current_version
                            ? <Badge variant="positive"><Check /> Confirmed v{item.current_version.version}</Badge>
                            : <Badge variant="attention">Needs confirmation</Badge>}
                          {item.stale && item.current_version && <Badge variant="attention">Stale</Badge>}
                          {item.serving_blueprints.length > 0 && <Badge variant="mono">{item.serving_blueprints.length} blueprints</Badge>}
                        </span>
                      </span>
                    </span>
                  </button>
                ))}
              </div>
            </Card>
            {selected && (
              <RequirementDetail
                item={selected}
                token={token}
                onPlan={() => startPlanning(selected.requirement.id)}
                onChanged={() => void client.invalidateQueries({ queryKey: ['requirements', workspace] })}
              />
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function RequirementDetail({
  item,
  token,
  onPlan,
  onChanged,
}: {
  item: RequirementView
  token: string
  onPlan: () => void
  onChanged: () => void
}) {
  const client = useQueryClient()
  const latestPending = item.pending_versions.at(-1)
  const confirm = useMutation({
    mutationFn: (version: number) => confirmRequirementVersion(token, item.requirement.id, version),
    onSuccess: onChanged,
  })
  const upload = useMutation({
    mutationFn: (file: File) => uploadArtifact(token, file, undefined, item.requirement.id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['requirements'] })
      void client.invalidateQueries({ queryKey: ['artifacts'] })
    },
  })
  const displayed = latestPending ?? item.current_version

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
        <CardContent>
          {displayed ? (
            <>
              <div className="mb-4 flex flex-wrap items-center gap-2">
                <Badge variant={displayed.confirmed ? 'positive' : 'attention'}>
                  {displayed.confirmed ? `Confirmed v${displayed.version}` : `Proposed v${displayed.version}`}
                </Badge>
                <Badge variant="mono">{displayed.origin.replaceAll('_', ' ')}</Badge>
                <time className="ml-auto text-[11px] text-faint">{formatDate(displayed.created_at)}</time>
              </div>
              <MarkdownProse>{displayed.content}</MarkdownProse>
              {!displayed.confirmed && (
                <div className="mt-5 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-attention/30 bg-attention-soft px-4 py-3">
                  <p className="text-xs leading-5 text-attention">
                    This revision is not current intent until an operator confirms it.
                  </p>
                  <Button
                    size="sm"
                    disabled={!token || confirm.isPending}
                    onClick={() => confirm.mutate(displayed.version)}
                  >
                    <Check /> {confirm.isPending ? 'Confirming…' : `Confirm version ${displayed.version}`}
                  </Button>
                  {confirm.error && <p className="basis-full text-xs text-failure">{String(confirm.error)}</p>}
                </div>
              )}
            </>
          ) : (
            <p className="text-sm text-muted">No requirement version has been proposed yet.</p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader><CardTitle>Serving blueprints</CardTitle><Badge variant="mono">{item.serving_blueprints.length}</Badge></CardHeader>
        <CardContent className="space-y-2">
          {item.serving_blueprints.length === 0 && <p className="text-sm text-muted">No blueprint has been planned in this requirement’s context yet.</p>}
          {item.serving_blueprints.map(({ task, spec }) => (
            <Link
              key={task.id}
              to="/tasks/$taskId"
              params={{ taskId: task.id }}
              className="flex items-center gap-3 rounded-lg border border-border p-3 transition-colors hover:bg-surface"
            >
              <GitBranch className="size-4 shrink-0 text-primary" />
              <span className="min-w-0 flex-1">
                <strong className="block truncate text-sm">{task.title}</strong>
                <span className="mt-1 flex gap-1.5">
                  <Badge variant="mono">{task.state.replaceAll('_', ' ')}</Badge>
                  {spec && <Badge variant={spec.approved ? 'positive' : 'attention'}>Spec v{spec.version} {spec.approved ? 'approved' : 'at gate'}</Badge>}
                </span>
              </span>
              <ArrowRight className="size-4 text-faint" />
            </Link>
          ))}
        </CardContent>
      </Card>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader><CardTitle>Context artifacts</CardTitle><Badge variant="mono">{item.artifacts.length}</Badge></CardHeader>
          <CardContent className="space-y-2">
            {item.artifacts.map((artifact) => (
              <button key={`${artifact.id}-${artifact.role}`} onClick={() => void downloadArtifact(token, artifact)} className="flex w-full items-center gap-2 rounded-md border border-border p-2 text-left hover:bg-surface">
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
            {upload.error && <p className="text-xs text-failure">{String(upload.error)}</p>}
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Lineage</CardTitle><Badge variant="mono">{item.lineage.length}</Badge></CardHeader>
          <CardContent>
            {item.lineage.length === 0 && <p className="text-sm text-muted">Lineage appears as planning and delivery advance.</p>}
            <ol className="space-y-3">
              {item.lineage.slice(-8).reverse().map((event) => (
                <li key={`${event.id}-${event.kind}`} className="flex gap-2 text-xs">
                  <span className="mt-1.5 size-1.5 shrink-0 rounded-full bg-edge" />
                  <span className="min-w-0 flex-1">
                    <strong className="font-medium">{event.kind.replaceAll('.', ' · ')}</strong>
                    <time className="mt-0.5 block text-[10px] text-faint">{formatDate(event.at)}</time>
                  </span>
                </li>
              ))}
            </ol>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function EmptyMessage({ children, tone = 'muted' }: { children: string; tone?: 'muted' | 'failure' }) {
  return <p className={`mt-8 rounded-md border border-border p-4 text-sm ${tone === 'failure' ? 'text-failure' : 'text-muted'}`}>{children}</p>
}

function formatDate(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}
