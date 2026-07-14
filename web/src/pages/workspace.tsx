import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, ExternalLink, Info, Plus, Save, Trash2 } from 'lucide-react'
import { useOperatorToken, useWorkspace } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input, Textarea } from '../components/ui/input'
import { Skeleton } from '../components/ui/skeleton'
import { ConfigValidationError, fetchWorkspaceConfig, updateWorkspaceConfig } from '../lib/api'
import { stageLabels } from '../lib/contracts'
import type { WorkspaceConfigDocument, WorkspaceConfigRepo, WorkspaceConfigRoute } from '../lib/types'

const stageOrder = ['triage', 'spec', 'implement', 'review', 'verify', 'gate', 'merge', 'monitor']

export function WorkspacePage() {
  const token = useOperatorToken()
  const queryClient = useQueryClient()
  const { data: workspace, isLoading, error } = useWorkspace()
  const configQuery = useQuery({
    queryKey: ['workspace-config', token],
    queryFn: () => fetchWorkspaceConfig(token),
    enabled: Boolean(token),
    retry: 1,
  })
  const [draft, setDraft] = useState<WorkspaceConfigDocument | null>(null)
  const [saved, setSaved] = useState<string>('')

  useEffect(() => {
    if (configQuery.data) setDraft(structuredClone(configQuery.data.document))
  }, [configQuery.data])

  const save = useMutation({
    mutationFn: () => updateWorkspaceConfig(token, draft!, configQuery.data!.version),
    onSuccess: (receipt) => {
      queryClient.setQueryData(['workspace-config', token], receipt)
      queryClient.invalidateQueries({ queryKey: ['workspace'] })
      setDraft(structuredClone(receipt.document))
      setSaved(`Recorded config.updated event ${receipt.event_id} by ${receipt.actor_id} · version ${receipt.version}`)
    },
  })
  const validation = save.error instanceof ConfigValidationError ? save.error : null
  const fieldError = (field: string) => validation?.fields.find((item) => item.field === field)?.message

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 py-8">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold tracking-tight">Workspace</h1>
            <p className="mt-1 text-sm text-muted">Repositories, sandbox environments, and per-stage routing.</p>
          </div>
          {draft && configQuery.data && (
            <Button disabled={!token || save.isPending} onClick={() => { setSaved(''); save.mutate() }}>
              <Save />{save.isPending ? 'Saving…' : `Save version ${configQuery.data.version}`}
            </Button>
          )}
        </div>

        {isLoading && <Skeleton className="mt-6 h-64" />}
        {error != null && <ErrorBox>Workspace snapshot unavailable: {String(error)}</ErrorBox>}
        {!token && workspace && (
          <div className="mt-6 flex items-start gap-2 rounded-lg border border-border bg-surface px-3 py-2.5 text-xs leading-5 text-muted">
            <Info className="mt-0.5 size-3.5 shrink-0" />
            <span>Set the operator token in Settings to edit the Postgres-backed workspace config. This unauthenticated snapshot remains read-only.</span>
          </div>
        )}
        {token && configQuery.isLoading && <Skeleton className="mt-6 h-64" />}
        {token && configQuery.error != null && <ErrorBox>Workspace config unavailable: {String(configQuery.error)}</ErrorBox>}
        {validation && <ErrorBox>{validation.message}</ErrorBox>}
        {save.error != null && !validation && <ErrorBox>{String(save.error)}</ErrorBox>}
        {saved && (
          <div className="mt-6 flex items-center gap-2 rounded-lg bg-success-soft px-3 py-2.5 text-sm text-success">
            <CheckCircle2 className="size-4" />{saved}
          </div>
        )}

        {draft ? (
          <Editor draft={draft} setDraft={setDraft} fieldError={fieldError} credentials={workspace?.credentials ?? []} />
        ) : workspace ? (
          <ReadOnlySnapshot workspace={workspace} />
        ) : null}
      </div>
    </div>
  )
}

function Editor({ draft, setDraft, fieldError, credentials }: {
  draft: WorkspaceConfigDocument
  setDraft: (next: WorkspaceConfigDocument) => void
  fieldError: (field: string) => string | undefined
  credentials: NonNullable<ReturnType<typeof useWorkspace>['data']>['credentials']
}) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateRepo = (index: number, change: Partial<WorkspaceConfigRepo>) => {
    const repos = [...draft.repos]
    repos[index] = { ...repos[index], ...change }
    update({ repos })
  }
  const updateRoute = (stage: string, change: Partial<WorkspaceConfigRoute>) => update({
    routing: { ...draft.routing, stages: { ...draft.routing.stages, [stage]: { ...draft.routing.stages[stage], ...change } } },
  })

  return (
    <div className="mt-6 space-y-4">
      <Card>
        <CardHeader><CardTitle>Workspace basics</CardTitle><Badge variant="mono">{draft.workspace}</Badge></CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-2">
          <Field label="Base sandbox image" error={fieldError('image')}>
            <Input value={draft.image} onChange={(event) => update({ image: event.target.value })} />
          </Field>
          <Field label="Maximum bounce rounds" error={fieldError('max_bounces')}>
            <Input type="number" min={1} value={draft.max_bounces} onChange={(event) => update({ max_bounces: Number(event.target.value) })} />
          </Field>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Repositories & environments</CardTitle>
          <Button size="sm" variant="secondary" onClick={() => update({ repos: [...draft.repos, emptyRepo(draft.image)] })}><Plus />Add repository</Button>
        </CardHeader>
        <CardContent className="space-y-4">
          {fieldError('repos') && <p className="text-xs text-failure">{fieldError('repos')}</p>}
          {draft.repos.map((repo, index) => (
            <div key={`${index}-${repo.name}`} className="rounded-lg border border-border p-4">
              <div className="grid gap-3 md:grid-cols-2">
                <Field label="Name"><Input value={repo.name} onChange={(event) => updateRepo(index, { name: event.target.value })} /></Field>
                <Field label="Repository URL"><Input value={repo.url} onChange={(event) => updateRepo(index, { url: event.target.value })} /></Field>
                <Field label="GitHub slug"><Input placeholder="owner/repo" value={repo.github ?? ''} onChange={(event) => updateRepo(index, { github: event.target.value })} /></Field>
                <Field label="Base branch"><Input value={repo.base} onChange={(event) => updateRepo(index, { base: event.target.value })} /></Field>
                <Field label="Sandbox image"><Input value={repo.image} onChange={(event) => updateRepo(index, { image: event.target.value })} /></Field>
                <Field label="Secret-set references" hint="One secretref:// reference per line; values never enter config.">
                  <Textarea value={(repo.secret_refs ?? []).join('\n')} onChange={(event) => updateRepo(index, { secret_refs: lines(event.target.value) })} />
                </Field>
                <Field label="Allowed command prefixes" hint="One command prefix per line.">
                  <Textarea value={commandsText(repo.tool_policy.allowed_commands)} onChange={(event) => updateRepo(index, { tool_policy: { ...repo.tool_policy, allowed_commands: commands(event.target.value) } })} />
                </Field>
                <Field label="Denied command prefixes" hint="Deny wins when prefixes overlap.">
                  <Textarea value={commandsText(repo.tool_policy.denied_commands)} onChange={(event) => updateRepo(index, { tool_policy: { ...repo.tool_policy, denied_commands: commands(event.target.value) } })} />
                </Field>
              </div>
              <div className="mt-3 flex justify-end"><Button size="sm" variant="destructive" onClick={() => update({ repos: draft.repos.filter((_, repoIndex) => repoIndex !== index) })}><Trash2 />Remove</Button></div>
            </div>
          ))}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Stage routing</CardTitle>
          <span className="text-xs text-faint">Applies to the next dispatch</span>
          {firstMissingStage(draft.routing.stages) && <Button size="sm" variant="secondary" onClick={() => {
            const stage = firstMissingStage(draft.routing.stages)!
            updateRoute(stage, { harnesses: ['codex', 'claude-code'], model_tier: '', budget_usd: 0, timeout: '2h' })
          }}><Plus />Add stage route</Button>}
        </CardHeader>
        <CardContent className="space-y-3">
          {fieldError('routing') && <p className="text-xs text-failure">{fieldError('routing')}</p>}
          {orderedStages(draft.routing.stages).map(([stage, route]) => (
            <div key={stage} className="grid gap-3 rounded-lg border border-border p-3 md:grid-cols-[1fr_1.5fr_1fr_0.7fr_0.8fr]">
              <Field label="Stage"><div className="h-9 py-2 text-sm font-medium">{stageLabels[stage] ?? stage}</div></Field>
              <Field label="Harness order"><Input value={route.harnesses.join(', ')} onChange={(event) => updateRoute(stage, { harnesses: event.target.value.split(',').map((item) => item.trim()).filter(Boolean) })} /></Field>
              <Field label="Model tier"><Input value={route.model_tier ?? ''} onChange={(event) => updateRoute(stage, { model_tier: event.target.value })} /></Field>
              <Field label="Budget USD"><Input type="number" min={0} step="0.01" value={route.budget_usd} onChange={(event) => updateRoute(stage, { budget_usd: Number(event.target.value) })} /></Field>
              <Field label="Timeout"><Input value={route.timeout} onChange={(event) => updateRoute(stage, { timeout: event.target.value })} /></Field>
            </div>
          ))}
        </CardContent>
      </Card>

      <CredentialPool credentials={credentials ?? []} />
    </div>
  )
}

function ReadOnlySnapshot({ workspace }: { workspace: NonNullable<ReturnType<typeof useWorkspace>['data']> }) {
  return (
    <div className="mt-6 space-y-4">
      <Card><CardHeader><CardTitle>Overview</CardTitle></CardHeader><CardContent className="grid grid-cols-2 gap-4 text-sm md:grid-cols-4"><Fact label="Workspace" value={workspace.workspace} /><Fact label="Base image" value={workspace.image} /><Fact label="Database" value={workspace.database} /><Fact label="Max bounces" value={String(workspace.max_bounces)} /></CardContent></Card>
      <Card><CardHeader><CardTitle>Repositories & environments</CardTitle></CardHeader><CardContent className="space-y-2">{(workspace.repos ?? []).map((repo) => <div key={repo.name} className="flex items-center gap-2 rounded-lg border border-border p-3 text-sm"><strong>{repo.name}</strong><Badge variant="mono">{repo.base}</Badge>{repo.github && <a className="ml-auto inline-flex items-center gap-1 text-primary" href={`https://github.com/${repo.github}`} target="_blank" rel="noreferrer">{repo.github}<ExternalLink className="size-3" /></a>}</div>)}</CardContent></Card>
      <CredentialPool credentials={workspace.credentials ?? []} />
    </div>
  )
}

function CredentialPool({ credentials }: { credentials: Array<{ id: string; harness: string; vendor: string; kind: string; owner_kind: string; owner_id: string }> }) {
  return <Card><CardHeader><CardTitle>Credential pool</CardTitle><Badge>{credentials.length} · file-managed</Badge></CardHeader><CardContent className="space-y-2">{credentials.map((credential) => <div key={credential.id} className="flex flex-wrap items-center gap-2 rounded-lg border border-border px-3 py-2"><span className="font-mono text-xs">{credential.id}</span><Badge variant="accent">{credential.harness}</Badge><Badge>{credential.vendor}</Badge><span className="ml-auto text-xs text-faint">{credential.owner_kind} · {credential.owner_id}</span></div>)}{credentials.length === 0 && <p className="text-sm text-muted">No file-managed credentials configured.</p>}</CardContent></Card>
}

function Field({ label, hint, error, children }: { label: string; hint?: string; error?: string; children: React.ReactNode }) {
  return <label className="block"><span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>{children}{hint && <span className="mt-1 block text-xs text-faint">{hint}</span>}{error && <span className="mt-1 block text-xs text-failure">{error}</span>}</label>
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return <div><dt className="text-[11px] uppercase tracking-wider text-faint">{label}</dt><dd className="mt-0.5 truncate">{value}</dd></div>
}

function ErrorBox({ children }: { children: React.ReactNode }) {
  return <p className="mt-6 rounded-lg bg-failure-soft p-3 text-sm text-failure">{children}</p>
}

function emptyRepo(image: string): WorkspaceConfigRepo {
  return { name: '', url: '', base: 'main', image, secret_refs: [], tool_policy: {} }
}

function lines(value: string) { return value.split('\n').map((item) => item.trim()).filter(Boolean) }
function commands(value: string) { return lines(value).map((line) => line.split(/\s+/)) }
function commandsText(value?: string[][]) { return (value ?? []).map((command) => command.join(' ')).join('\n') }
function orderedStages(stages: Record<string, WorkspaceConfigRoute>) {
  return Object.entries(stages).sort(([left], [right]) => {
    const leftIndex = stageOrder.indexOf(left)
    const rightIndex = stageOrder.indexOf(right)
    return (leftIndex < 0 ? stageOrder.length : leftIndex) - (rightIndex < 0 ? stageOrder.length : rightIndex) || left.localeCompare(right)
  })
}
function firstMissingStage(stages: Record<string, WorkspaceConfigRoute>) { return stageOrder.find((stage) => !(stage in stages)) }
