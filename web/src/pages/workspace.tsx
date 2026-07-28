import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { CheckCircle2, Plus, Save, Trash2 } from 'lucide-react'
import { useOperatorToken, useWorkspace, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { DropdownMenu, DropdownMenuItem } from '../components/ui/dropdown-menu'
import { Switch } from '../components/ui/switch'
import { Field } from '../components/workspace/field'
import { HarnessCard, latestProbe } from '../components/workspace/harness-card'
import { SetupCard } from '../components/workspace/setup-card'
import { ConfigValidationError, fetchWorkers, fetchWorkspaceConfig, getHarnessTemplates, issueWorkerPairing, revokeWorker, updateWorkspaceConfig } from '../lib/api'
import { cn } from '../lib/utils'
import type { HarnessTemplate, WorkerList, WorkspaceConfigDocument, WorkspaceConfigRepo, WorkspaceHarness } from '../lib/types'

type TabId = 'general' | 'execution' | 'harnesses' | 'workers'

const TABS: Array<{ id: TabId; label: string }> = [
  { id: 'general', label: 'General' },
  { id: 'execution', label: 'Execution' },
  { id: 'harnesses', label: 'Harnesses' },
  { id: 'workers', label: 'Workers' },
]

// Which tab owns a config section, for dirty markers and validation-error routing.
const TAB_SLICES: Record<Exclude<TabId, 'workers'>, (document: WorkspaceConfigDocument) => unknown> = {
  general: (document) => [document.max_bounces, document.work_order_queue_timeout, document.repos],
  execution: (document) => [document.execution, document.setups, document.default_setup],
  harnesses: (document) => [document.harnesses],
}

function tabForField(field: string): TabId {
  if (/^(max_bounces|work_order_queue_timeout|repos)/.test(field)) return 'general'
  if (/^harnesses/.test(field)) return 'harnesses'
  return 'execution'
}

export function WorkspacePage() {
  const token = useOperatorToken(); const queryClient = useQueryClient(); const { workspace } = useWorkspaceSelection(); const { data: snapshot } = useWorkspace()
  const query = useQuery({ queryKey: ['workspace-config', token, workspace], queryFn: () => fetchWorkspaceConfig(token), enabled: Boolean(token && workspace) })
  const templates = useQuery({ queryKey: ['harness-templates', token], queryFn: () => getHarnessTemplates(token), enabled: Boolean(token), retry: false })
  const workers = useQuery({ queryKey: ['workers', token, workspace], queryFn: () => fetchWorkers(token), enabled: Boolean(token && workspace), refetchInterval: 5000 })
  const [tab, setTab] = useState<TabId>('execution')
  const [draft, setDraftState] = useState<WorkspaceConfigDocument | null>(null)
  const [saved, setSaved] = useState(''); const [pairing, setPairing] = useState('')
  useEffect(() => { setDraftState(query.data ? structuredClone(query.data.document) : null) }, [query.data, workspace])
  const save = useMutation({ mutationFn: () => updateWorkspaceConfig(token, draft!, query.data!.version), onSuccess: (receipt) => { queryClient.setQueryData(['workspace-config', token, workspace], receipt); void queryClient.invalidateQueries({ queryKey: ['workspace', workspace] }); setDraftState(structuredClone(receipt.document)); setSaved(`Recorded config.updated event ${receipt.event_id} · version ${receipt.version}`) } })
  const pair = useMutation({ mutationFn: () => issueWorkerPairing(token), onSuccess: (result) => setPairing(`${result.pairing_token} (expires ${new Date(result.expires_at).toLocaleTimeString()})`) })
  const revoke = useMutation({ mutationFn: (id: string) => revokeWorker(token, id), onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['workers', token, workspace] }) })

  const setDraft = (value: WorkspaceConfigDocument) => { setDraftState(value); setSaved('') }
  const serverDocument = query.data?.document
  const dirtyTabs = useMemo(() => {
    if (!draft || !serverDocument) return [] as TabId[]
    return (Object.keys(TAB_SLICES) as Array<Exclude<TabId, 'workers'>>).filter((id) => JSON.stringify(TAB_SLICES[id](draft)) !== JSON.stringify(TAB_SLICES[id](serverDocument)))
  }, [draft, serverDocument])
  const errorTabs = useMemo(() => {
    if (!(save.error instanceof ConfigValidationError)) return [] as TabId[]
    return [...new Set(save.error.fields.map((field) => tabForField(field.field)))]
  }, [save.error])

  return <div className="h-full overflow-y-auto"><div className="mx-auto max-w-5xl px-6 pb-24 pt-8">
    <div className="flex items-start justify-between">
      <div className="flex items-baseline gap-3">
        <h1 className="text-xl font-semibold">Workspace</h1>
        {draft && <Badge variant="mono">{draft.workspace}</Badge>}
      </div>
      {token && <Link to="/workspaces/new"><Button variant="secondary" tabIndex={-1}><Plus />New workspace</Button></Link>}
    </div>
    {!token && <p className="mt-6 rounded-lg border border-border p-3 text-sm text-muted">Set the operator token in Settings to edit workspace configuration.</p>}
    {token && !workspace && <Card className="mt-6"><CardHeader><CardTitle>No workspace selected</CardTitle></CardHeader><CardContent><p className="text-sm text-muted">Pick a workspace from the left rail, or <Link to="/workspaces/new" className="text-primary hover:underline">create one</Link> to get started.</p></CardContent></Card>}

    {draft ? <>
      <div role="tablist" className="mt-6 flex gap-1 border-b border-border">
        {TABS.map((entry) => {
          const dirty = dirtyTabs.includes(entry.id); const errored = errorTabs.includes(entry.id)
          return <button key={entry.id} role="tab" aria-selected={tab === entry.id} onClick={() => setTab(entry.id)}
            className={cn('-mb-px flex items-center gap-2 border-b-2 px-3.5 py-2.5 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary', tab === entry.id ? 'border-primary text-foreground' : 'border-transparent text-muted hover:text-foreground')}>
            {entry.label}
            {entry.id === 'workers' && <span className="rounded-full bg-raised px-1.5 text-[11px] leading-4 text-muted">{workers.data?.workers.length ?? 0}</span>}
            {(dirty || errored) && <span title={errored ? 'Validation errors' : 'Unsaved changes'} className={cn('size-1.5 rounded-full', errored ? 'bg-failure' : 'bg-attention-dot')} />}
          </button>
        })}
      </div>

      <div className="pt-5">
        {tab === 'general' && <GeneralTab draft={draft} setDraft={setDraft} />}
        {tab === 'execution' && <ExecutionTab draft={draft} setDraft={setDraft} workerHealth={workers.data} />}
        {tab === 'harnesses' && <HarnessesTab draft={draft} setDraft={setDraft} workerHealth={workers.data} templates={templates.data?.templates ?? []} />}
        {tab === 'workers' && <WorkersTab data={workers.data} pairing={pairing} onPair={() => pair.mutate()} onRevoke={(id) => revoke.mutate(id)} />}
      </div>

      {(save.error || saved || dirtyTabs.length > 0) && <div className="pointer-events-none sticky bottom-4 mt-6 flex justify-center">
        <div className="pointer-events-auto w-full max-w-2xl space-y-2">
          {save.error ? <div className="rounded-lg border border-failure/30 bg-failure-soft p-3 text-sm text-failure shadow-lg"><p>{save.error instanceof ConfigValidationError ? save.error.message : String(save.error)}</p>{save.error instanceof ConfigValidationError && save.error.fields.map((field) => <p key={field.field} className="mt-1 font-mono text-xs">{field.field}: {field.message}</p>)}</div> : null}
          {saved && dirtyTabs.length === 0 && !save.error && <p className="flex items-center gap-2 rounded-lg border border-positive/30 bg-positive-soft p-3 text-sm text-positive shadow-lg"><CheckCircle2 className="size-4" />{saved}</p>}
          {dirtyTabs.length > 0 && <div className="flex items-center gap-3 rounded-lg border border-edge bg-card py-2 pl-4 pr-2 shadow-lg">
            <p className="text-sm"><span className="font-medium">Unsaved changes</span> <span className="text-faint">· {dirtyTabs.map((id) => TABS.find((entry) => entry.id === id)!.label).join(', ')}</span></p>
            <div className="ml-auto flex gap-2">
              <Button size="sm" variant="ghost" onClick={() => { setDraftState(structuredClone(serverDocument!)); save.reset(); setSaved('') }}>Discard</Button>
              <Button size="sm" disabled={save.isPending} onClick={() => save.mutate()}><Save />Save changes</Button>
            </div>
          </div>}
        </div>
      </div>}
    </> : snapshot ? <ReadOnly snapshot={snapshot} /> : null}
  </div></div>
}

function GeneralTab({ draft, setDraft }: { draft: WorkspaceConfigDocument; setDraft: (value: WorkspaceConfigDocument) => void }) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateRepo = (index: number, change: Partial<WorkspaceConfigRepo>) => { const repos = [...draft.repos]; repos[index] = { ...repos[index], ...change }; update({ repos }) }
  return <div className="space-y-4">
    <Card>
      <CardHeader><CardTitle>Basics</CardTitle></CardHeader>
      <CardContent className="grid gap-3 md:grid-cols-2">
        <Field label="Review rounds before check-in" hint="Unsupervised implement–review rounds before the task pauses for a human look. The window resets after each human intervention.">
          <Input type="number" min={1} value={draft.max_bounces} onChange={(event) => update({ max_bounces: Number(event.target.value) })} />
        </Field>
        <Field label="Work-order queue timeout" hint="How long a task may wait for a worker before it is marked stalled.">
          <Input value={draft.work_order_queue_timeout} onChange={(event) => update({ work_order_queue_timeout: event.target.value })} placeholder="24h" />
        </Field>
      </CardContent>
    </Card>
    <Card>
      <CardHeader><CardTitle>Repositories</CardTitle><Button size="sm" variant="secondary" onClick={() => update({ repos: [...draft.repos, { name: '', url: '', base: 'main' }] })}><Plus />Add repository</Button></CardHeader>
      <CardContent className="space-y-3">
        {draft.repos.map((repo, index) => <div key={index} className="grid items-end gap-3 rounded-md border border-border p-3 md:grid-cols-[1fr_1.4fr_1fr_auto_auto]">
          <Field label="Name"><Input value={repo.name} onChange={(event) => updateRepo(index, { name: event.target.value })} /></Field>
          <Field label="URL"><Input value={repo.url} onChange={(event) => updateRepo(index, { url: event.target.value })} /></Field>
          <Field label="GitHub slug"><Input value={repo.github ?? ''} onChange={(event) => updateRepo(index, { github: event.target.value })} /></Field>
          <Field label="Base"><Input className="w-24" value={repo.base} onChange={(event) => updateRepo(index, { base: event.target.value })} /></Field>
          <Button size="icon" variant="ghost" className="mb-0.5 hover:text-failure" aria-label={`Remove repository ${repo.name || index + 1}`} onClick={() => update({ repos: draft.repos.filter((_, i) => i !== index) })}><Trash2 /></Button>
        </div>)}
        {draft.repos.length === 0 && <p className="text-sm text-faint">No repositories yet.</p>}
      </CardContent>
    </Card>
  </div>
}

function ExecutionTab({ draft, setDraft, workerHealth }: { draft: WorkspaceConfigDocument; setDraft: (value: WorkspaceConfigDocument) => void; workerHealth?: WorkerList }) {
  // Collapsed by default; expansion is keyed by index so renames keep it open.
  const [expanded, setExpanded] = useState<Record<number, boolean>>({})
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  // Per-setup serviceability is advisory (spec §21.31): it explains whether
  // the worker can serve a setup, and never gates anything.
  const workerReadyFor = (name: string) => workerHealth?.setup_serviceability?.[name]?.auto_available ?? workerHealth?.auto_available === true
  const uniqueName = (base: string) => { let name = base; let suffix = 2; while (draft.setups.some((setup) => setup.name === name)) name = `${base}-${suffix++}`; return name }
  const addSetup = () => {
    const base = draft.setups.find((setup) => setup.name === draft.default_setup) ?? draft.setups[0]
    const copy = structuredClone(base); let suffix = 1
    while (draft.setups.some((setup) => setup.name === `setup-${suffix}`)) suffix++
    copy.name = `setup-${suffix}`
    update({ setups: [...draft.setups, copy] }); setExpanded({ ...expanded, [draft.setups.length]: true })
  }
  const duplicateSetup = (index: number) => {
    const copy = structuredClone(draft.setups[index]); copy.name = uniqueName(`${copy.name}-copy`)
    update({ setups: [...draft.setups, copy] }); setExpanded({ ...expanded, [draft.setups.length]: true })
  }
  const deleteSetup = (index: number) => {
    const removed = draft.setups[index]
    const setups = draft.setups.filter((_, i) => i !== index)
    const defaultSetup = draft.default_setup === removed.name ? setups[0].name : draft.default_setup
    const projected = setups.find((setup) => setup.name === defaultSetup)!
    setDraft({ ...draft, setups, default_setup: defaultSetup, execution_settings: projected.execution_settings, review: projected.review })
    setExpanded({})
  }
  return <div className="space-y-4">
    <Card>
      <CardHeader><CardTitle>Policy</CardTitle><span className="text-xs text-faint">Dispatch worker · confinement none · authentication BYOA.</span></CardHeader>
      <CardContent className="grid gap-x-6 gap-y-4 md:grid-cols-2">
        <div className="space-y-3">
          <div className="flex items-center gap-3"><Switch aria-label="Spec approval" checked={draft.execution.spec_approval} onChange={(checked) => update({ execution: { ...draft.execution, spec_approval: checked } })} /><div><p className="text-sm font-medium">Pause for spec approval</p><p className="text-xs text-faint">You approve the spec before implementation starts</p></div></div>
          <div className="flex items-center gap-3"><Switch aria-label="Merge approval" checked={draft.execution.merge_approval} onChange={(checked) => update({ execution: { ...draft.execution, merge_approval: checked } })} /><div><p className="text-sm font-medium">Pause before merge</p><p className="text-xs text-faint">You approve the final PR before it merges</p></div></div>
          <div className="flex items-center gap-3"><Switch aria-label="Require verification evidence" checked={draft.execution.require_verification_evidence} onChange={(checked) => update({ execution: { ...draft.execution, require_verification_evidence: checked } })} /><div><p className="text-sm font-medium">Require verification evidence</p><p className="text-xs text-faint">Refuse review submission until the task has an eligible screenshot or short recording</p></div></div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Parallel implementations"><Input type="number" min={1} value={draft.execution.implement_concurrency} onChange={(event) => update({ execution: { ...draft.execution, implement_concurrency: Number(event.target.value) } })} /></Field>
          <Field label="Reserved review slots" hint="Worker capacity held back so reviews never wait behind implementations."><Input type="number" min={1} value={draft.execution.review_concurrency} onChange={(event) => update({ execution: { ...draft.execution, review_concurrency: Number(event.target.value) } })} /></Field>
          <div className="col-span-2"><Field label="First activity timeout" hint="Maximum time a launched harness may remain completely silent before worker recovery."><Input value={draft.execution.first_activity_timeout} onChange={(event) => update({ execution: { ...draft.execution, first_activity_timeout: event.target.value } })} /></Field></div>
        </div>
      </CardContent>
    </Card>

    <div>
      <div className="mb-3 flex items-center gap-3">
        <div><h3 className="text-sm font-semibold">Execution setups</h3><p className="text-xs text-faint">Chosen at task intake and frozen on each task</p></div>
        <Button size="sm" className="ml-auto" onClick={addSetup}><Plus />New setup</Button>
      </div>
      <div className="space-y-3">
        {draft.setups.map((setup, index) => <SetupCard key={index} document={draft} setup={setup} index={index}
          expanded={Boolean(expanded[index])} onToggle={() => setExpanded({ ...expanded, [index]: !expanded[index] })}
          workerReady={workerReadyFor(setup.name)} workerReason={workerHealth?.setup_serviceability?.[setup.name]?.auto_unavailable_reason ?? workerHealth?.auto_unavailable_reason}
          setDraft={setDraft} onDuplicate={() => duplicateSetup(index)} onDelete={() => deleteSetup(index)} />)}
      </div>
    </div>
  </div>
}

function HarnessesTab({ draft, setDraft, workerHealth, templates }: { draft: WorkspaceConfigDocument; setDraft: (value: WorkspaceConfigDocument) => void; workerHealth?: WorkerList; templates: HarnessTemplate[] }) {
  const [expanded, setExpanded] = useState<Record<number, boolean>>({})
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateHarness = (index: number, change: Partial<WorkspaceHarness>) => { const harnesses = [...draft.harnesses]; harnesses[index] = { ...harnesses[index], ...change }; update({ harnesses }) }
  const updateEffortArgs = (index: number, effort: 'low' | 'medium' | 'high', value: string[]) => { const effortArgs = { ...draft.harnesses[index].effort_args }; if (value.length === 0) delete effortArgs[effort]; else effortArgs[effort] = value; updateHarness(index, { effort_args: Object.keys(effortArgs).length ? effortArgs : undefined }) }
  const uniqueName = (base: string) => { let name = base; let suffix = 2; while (draft.harnesses.some((harness) => harness.name === name)) name = `${base}-${suffix++}`; return name }
  const addHarness = (template?: HarnessTemplate) => {
    const harness: WorkspaceHarness = template
      ? { ...structuredClone(template.harness), name: uniqueName(template.id) }
      : { name: '', mcp_transport: 'json_file', command: [], probe_command: [], probe_timeout: '10s', stall_timeout: '10m' }
    update({ harnesses: [...draft.harnesses, harness] })
    setExpanded({ ...expanded, [draft.harnesses.length]: true })
  }
  return <div>
    <div className="mb-3 flex items-center gap-3">
      <div><h3 className="text-sm font-semibold">Harnesses</h3><p className="text-xs text-faint">How the worker launches each coding agent</p></div>
      <DropdownMenu label="Add harness" className="ml-auto">
        {templates.map((template) => <DropdownMenuItem key={template.id} onSelect={() => addHarness(template)}>
          <span className="text-sm font-medium">{template.label}</span>
          <span className="text-xs text-faint">{template.description}</span>
        </DropdownMenuItem>)}
        <DropdownMenuItem onSelect={() => addHarness()}>
          <span className="text-sm font-medium">Custom</span>
          <span className="text-xs text-faint">Start with a blank harness</span>
        </DropdownMenuItem>
      </DropdownMenu>
    </div>
    <div className="space-y-3">
      {draft.harnesses.map((harness, index) => <HarnessCard key={index} harness={harness} index={index}
        expanded={Boolean(expanded[index])} onToggle={() => setExpanded({ ...expanded, [index]: !expanded[index] })}
        probe={latestProbe(workerHealth, harness.name)}
        onChange={(change) => updateHarness(index, change)} onEffortChange={(effort, value) => updateEffortArgs(index, effort, value)}
        onRemove={() => { update({ harnesses: draft.harnesses.filter((_, i) => i !== index) }); setExpanded({}) }} />)}
      {draft.harnesses.length === 0 && <p className="text-sm text-faint">No harnesses yet. Add one to route implementation and review work to a coding agent.</p>}
    </div>
  </div>
}

function WorkersTab({ data, pairing, onPair, onRevoke }: { data?: WorkerList; pairing: string; onPair: () => void; onRevoke: (id: string) => void }) {
  return <Card>
    <CardHeader><CardTitle>Workers</CardTitle><div className="flex items-center gap-2"><Badge variant={data?.auto_available ? 'positive' : 'attention'}>{data?.auto_available ? 'Auto available' : 'Auto unavailable'}</Badge><Button size="sm" variant="secondary" onClick={onPair}>Issue pairing token</Button></div></CardHeader>
    <CardContent className="space-y-3">
      {pairing && <p className="break-all rounded-md border border-border bg-surface p-2 font-mono text-xs">{pairing}</p>}
      {!data?.auto_available && <div className="rounded-md border border-attention/40 bg-attention-soft p-3 text-xs leading-5 text-muted"><p>{data?.auto_unavailable_reason}</p><p>Restart an enrolled worker with its saved credential; ordinary restarts do not require a new pairing token.</p></div>}
      {(data?.rate_limits?.length ?? 0) > 0 && <div className="rounded-md border border-border bg-surface p-3">
        <p className="text-xs font-semibold uppercase tracking-wider text-faint">Provider rate limits</p>
        <div className="mt-2 space-y-2">{data!.rate_limits!.map((entry) => {
          const values = [
            entry.rate_limit.remaining != null && entry.rate_limit.limit != null ? `${entry.rate_limit.remaining} of ${entry.rate_limit.limit} remaining` : undefined,
            entry.rate_limit.reset_at ? `resets ${new Date(entry.rate_limit.reset_at).toLocaleString()}` : undefined,
            `observed ${new Date(entry.observed_at).toLocaleString()}`,
          ].filter(Boolean).join(' · ')
          return <div key={`${entry.harness}-${entry.model ?? ''}`} className="flex flex-wrap items-baseline gap-x-2 text-xs">
            <span className="font-mono font-medium">{entry.harness}{entry.model ? ` / ${entry.model}` : ''}</span>
            <Badge variant="mono">{entry.rate_limit.status}</Badge>
            <span className="text-muted">{values}</span>
          </div>
        })}</div>
        <p className="mt-2 text-[11px] text-faint">Self-reported telemetry only; Conveyor does not use it to gate or route work.</p>
      </div>}
      {(data?.workers ?? []).map((worker) => <div key={worker.id} className="rounded-md border border-border p-3">
        <div className="flex items-center justify-between">
          <div><p className="text-sm font-medium">{worker.name}</p><p className="font-mono text-xs text-faint">{worker.id}</p><p className="text-xs text-muted">{worker.last_seen_at ? `Last heartbeat ${new Date(worker.last_seen_at).toLocaleString()}` : 'No heartbeat recorded'}{worker.lease_expires_at && new Date(worker.lease_expires_at) <= new Date() ? ' · disconnected' : ''}</p></div>
          <Button size="sm" variant="destructive" onClick={() => onRevoke(worker.id)}>Revoke</Button>
        </div>
        <div className="mt-2 flex flex-wrap gap-1">{(worker.probes ?? []).map((probe) => <Badge key={`${probe.harness}-${probe.checked_at}`} variant={probe.healthy ? 'positive' : 'attention'}>{probe.harness}: {probe.healthy ? 'healthy' : 'unhealthy'}</Badge>)}</div>
      </div>)}
    </CardContent>
  </Card>
}

function ReadOnly({ snapshot }: { snapshot: NonNullable<ReturnType<typeof useWorkspace>['data']> }) { return <div className="mt-6 space-y-4"><Card><CardHeader><CardTitle>{snapshot.workspace}</CardTitle></CardHeader><CardContent><p className="text-sm text-muted">{snapshot.repos?.length ?? 0} repositories · {snapshot.database} · check-in after {snapshot.max_bounces} review rounds</p></CardContent></Card></div> }
