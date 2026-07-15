import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, Plus, Save, Trash2 } from 'lucide-react'
import { useOperatorToken, useWorkspace, useWorkspaceSelection } from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { ConfigValidationError, createWorkspace, fetchWorkspaceConfig, fetchWorkspaces, updateWorkspaceConfig } from '../lib/api'
import type { WorkspaceConfigDocument, WorkspaceConfigRepo, WorkspaceConfigRoute } from '../lib/types'

const stageOrder = ['triage', 'spec', 'implement', 'review']

export function WorkspacePage() {
  const token = useOperatorToken(); const queryClient = useQueryClient(); const { workspace, setWorkspace } = useWorkspaceSelection(); const { data: snapshot } = useWorkspace()
  const { data: workspaces } = useQuery({ queryKey: ['workspaces', token], queryFn: () => fetchWorkspaces(token), enabled: Boolean(token) })
  const query = useQuery({ queryKey: ['workspace-config', token, workspace], queryFn: () => fetchWorkspaceConfig(token), enabled: Boolean(token && workspace) })
  const [draft, setDraft] = useState<WorkspaceConfigDocument | null>(null); const [saved, setSaved] = useState('')
  const [newID, setNewID] = useState(''); const [newName, setNewName] = useState('')
  const [newDocument, setNewDocument] = useState('')
  const [showCreate, setShowCreate] = useState(false)
  useEffect(() => { setDraft(query.data ? structuredClone(query.data.document) : null) }, [query.data, workspace])
  const save = useMutation({ mutationFn: () => updateWorkspaceConfig(token, draft!, query.data!.version), onSuccess: (receipt) => { queryClient.setQueryData(['workspace-config', token, workspace], receipt); void queryClient.invalidateQueries({ queryKey: ['workspace', workspace] }); setDraft(structuredClone(receipt.document)); setSaved(`Recorded config.updated event ${receipt.event_id} · version ${receipt.version}`) } })
  const create = useMutation({ mutationFn: () => createWorkspace(token, { id: newID.trim(), name: newName.trim(), document: parseInitialDocument(newDocument) }), onSuccess: async (created) => { await queryClient.invalidateQueries({ queryKey: ['workspaces'] }); setWorkspace(created.id); setNewID(''); setNewName(''); setNewDocument(''); setShowCreate(false) } })
  return <div className="h-full overflow-y-auto"><div className="mx-auto max-w-5xl px-6 py-8">
    <div className="flex items-start justify-between"><div><h1 className="text-xl font-semibold">Workspace</h1><p className="mt-1 text-sm text-muted">Repositories and per-stage execution routing.</p></div><div className="flex gap-2">{token && workspace && <Button variant="secondary" onClick={() => setShowCreate((visible) => !visible)}><Plus />New workspace</Button>}{draft && <Button disabled={save.isPending} onClick={() => save.mutate()}><Save />Save</Button>}</div></div>
    {!token && <p className="mt-6 rounded-lg border border-border p-3 text-sm text-muted">Set the operator token in Settings to edit workspace configuration.</p>}
    {token && (!workspace || showCreate) && <Card className="mt-6"><CardHeader><CardTitle>{workspaces?.length ? 'Create another workspace' : 'Create your first workspace'}</CardTitle></CardHeader><CardContent className="grid gap-3 md:grid-cols-2"><Field label="Workspace ID"><Input value={newID} placeholder="engineering" onChange={(event) => setNewID(event.target.value.toLowerCase())} /></Field><Field label="Display name"><Input value={newName} placeholder="Engineering" onChange={(event) => setNewName(event.target.value)} /></Field><div className="md:col-span-2"><Field label="Initial configuration (optional JSON)"><textarea className="min-h-36 w-full rounded-lg border border-edge bg-background px-3 py-2 font-mono text-xs text-foreground placeholder:text-faint outline-none focus:border-primary" value={newDocument} placeholder={'{\n  "max_bounces": 3,\n  "work_order_queue_timeout": "48h",\n  "repos": []\n}'} onChange={(event) => setNewDocument(event.target.value)} /><span className="mt-1 block text-xs text-faint">All workspace document fields are accepted; omitted fields inherit deployment defaults.</span></Field></div><Button disabled={!newID.trim() || !newName.trim() || create.isPending} onClick={() => create.mutate()}><Plus />{create.isPending ? 'Creating…' : 'Create workspace'}</Button>{create.error && <p className="text-sm text-failure">{String(create.error)}</p>}</CardContent></Card>}
    {saved && <p className="mt-6 flex items-center gap-2 rounded-lg bg-success-soft p-3 text-sm text-success"><CheckCircle2 className="size-4" />{saved}</p>}
    {save.error && <p className="mt-6 rounded-lg bg-failure-soft p-3 text-sm text-failure">{save.error instanceof ConfigValidationError ? save.error.message : String(save.error)}</p>}
    {draft ? <Editor draft={draft} setDraft={setDraft} /> : snapshot ? <ReadOnly snapshot={snapshot} /> : null}
  </div></div>
}

function parseInitialDocument(value: string): Partial<WorkspaceConfigDocument> | undefined {
  if (!value.trim()) return undefined
  const document: unknown = JSON.parse(value)
  if (!document || typeof document !== 'object' || Array.isArray(document)) throw new Error('Initial configuration must be a JSON object.')
  return document as Partial<WorkspaceConfigDocument>
}

function Editor({ draft, setDraft }: { draft: WorkspaceConfigDocument; setDraft: (value: WorkspaceConfigDocument) => void }) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateRepo = (index: number, change: Partial<WorkspaceConfigRepo>) => { const repos = [...draft.repos]; repos[index] = { ...repos[index], ...change }; update({ repos }) }
  const updateRoute = (stage: string, change: Partial<WorkspaceConfigRoute>) => update({ routing: { stages: { ...draft.routing.stages, [stage]: { ...draft.routing.stages[stage], ...change } } } })
  return <div className="mt-6 space-y-4">
    <Card><CardHeader><CardTitle>Workspace</CardTitle><Badge variant="mono">{draft.workspace}</Badge></CardHeader><CardContent className="space-y-4"><Field label="Maximum bounce rounds"><Input type="number" min={1} value={draft.max_bounces} onChange={(event) => update({ max_bounces: Number(event.target.value) })} /></Field><Field label="Work-order queue timeout"><Input value={draft.work_order_queue_timeout} onChange={(event) => update({ work_order_queue_timeout: event.target.value })} placeholder="24h" /></Field></CardContent></Card>
    <Card><CardHeader><CardTitle>Repositories</CardTitle><Button size="sm" variant="secondary" onClick={() => update({ repos: [...draft.repos, { name: '', url: '', base: 'main' }] })}><Plus />Add</Button></CardHeader><CardContent className="space-y-3">{draft.repos.map((repo, index) => <div key={index} className="grid gap-3 rounded-lg border border-border p-3 md:grid-cols-2"><Field label="Name"><Input value={repo.name} onChange={(event) => updateRepo(index, { name: event.target.value })} /></Field><Field label="URL"><Input value={repo.url} onChange={(event) => updateRepo(index, { url: event.target.value })} /></Field><Field label="GitHub slug"><Input value={repo.github ?? ''} onChange={(event) => updateRepo(index, { github: event.target.value })} /></Field><Field label="Base"><Input value={repo.base} onChange={(event) => updateRepo(index, { base: event.target.value })} /></Field><Button size="sm" variant="destructive" onClick={() => update({ repos: draft.repos.filter((_, i) => i !== index) })}><Trash2 />Remove</Button></div>)}</CardContent></Card>
    <Card><CardHeader><CardTitle>Stage routing</CardTitle><span className="text-xs text-faint">Triage/spec are fixed in-process; implement is fixed MCP.</span></CardHeader><CardContent className="space-y-3">{stageOrder.map((stage) => { const route = draft.routing.stages[stage]; if (!route) return null; return <div key={stage} className="grid gap-3 rounded-lg border border-border p-3 md:grid-cols-4"><Field label="Stage"><strong className="block py-2 text-sm">{stage}</strong></Field><Field label="Execution"><select className="h-9 w-full rounded-md border border-border bg-background px-2 text-sm" value={route.execution} disabled={stage !== 'review'} onChange={(event) => updateRoute(stage, { execution: event.target.value as 'mcp' | 'in_process' })}><option value="in_process">in_process</option><option value="mcp">mcp</option></select></Field><Field label="Model"><Input value={route.model} onChange={(event) => updateRoute(stage, { model: event.target.value })} /></Field><Field label="Timeout"><Input value={route.timeout} onChange={(event) => updateRoute(stage, { timeout: event.target.value })} /></Field></div> })}</CardContent></Card>
  </div>
}

function ReadOnly({ snapshot }: { snapshot: NonNullable<ReturnType<typeof useWorkspace>['data']> }) { return <div className="mt-6 space-y-4"><Card><CardHeader><CardTitle>{snapshot.workspace}</CardTitle></CardHeader><CardContent><p className="text-sm text-muted">{snapshot.repos?.length ?? 0} repositories · {snapshot.database} · max {snapshot.max_bounces} bounces</p></CardContent></Card></div> }
function Field({ label, children }: { label: string; children: React.ReactNode }) { return <label><span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>{children}</label> }
