import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { CheckCircle2, Plus, Save, Trash2 } from 'lucide-react'
import {
  useDashboardSession,
  useWorkspace,
  useWorkspaceCapability,
  useWorkspaceSelection,
} from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { Switch } from '../components/ui/switch'
import { Field } from '../components/workspace/field'
import { MembersSection } from '../components/workspace/members-section'
import {
  ConfigValidationError,
  fetchWorkers,
  fetchWorkspaceConfig,
  issueWorkerPairing,
  revokeWorker,
  updateWorkspaceConfig,
} from '../lib/api'
import { cn } from '../lib/utils'
import type { WorkerList, WorkspaceConfigDocument, WorkspaceConfigRepo } from '../lib/types'

type TabId = 'general' | 'policy' | 'workers' | 'members'

const TABS: Array<{ id: TabId; label: string }> = [
  { id: 'general', label: 'General' },
  { id: 'policy', label: 'Policy' },
  { id: 'workers', label: 'Workers' },
  { id: 'members', label: 'Members' },
]

// Which tab owns a config section, for dirty markers and validation-error routing.
const TAB_SLICES: Record<Exclude<TabId, 'workers' | 'members'>, (document: WorkspaceConfigDocument) => unknown> = {
  general: (document) => [document.work_order_queue_timeout, document.repos, document.monitor],
  policy: (document) => [document.max_bounces, document.stage_timeouts, document.review, document.execution],
}

function tabForField(field: string): TabId {
  if (/^(work_order_queue_timeout|repos|monitor)/.test(field)) return 'general'
  return 'policy'
}

export function WorkspacePage() {
  const token = useDashboardSession()
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  const queryClient = useQueryClient()
  const { workspace } = useWorkspaceSelection()
  const { data: snapshot } = useWorkspace()
  const query = useQuery({
    queryKey: ['workspace-config', token, workspace],
    queryFn: () => fetchWorkspaceConfig(),
    enabled: Boolean(token && workspace),
  })
  const workers = useQuery({
    queryKey: ['workers', token, workspace],
    queryFn: () => fetchWorkers(),
    enabled: Boolean(token && workspace),
    refetchInterval: () => (document.visibilityState === 'visible' ? 15_000 : false),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
  })
  const [tab, setTab] = useState<TabId>('policy')
  const [draft, setDraftState] = useState<WorkspaceConfigDocument | null>(null)
  const [saved, setSaved] = useState('')
  const [pairing, setPairing] = useState('')
  useEffect(() => {
    setDraftState(query.data ? structuredClone(query.data.document) : null)
  }, [query.data, workspace])
  const save = useMutation({
    mutationFn: () => updateWorkspaceConfig(draft!, query.data!.version),
    onSuccess: (receipt) => {
      queryClient.setQueryData(['workspace-config', token, workspace], receipt)
      void queryClient.invalidateQueries({ queryKey: ['workspace', workspace] })
      setDraftState(structuredClone(receipt.document))
      setSaved(`Recorded config.updated event ${receipt.event_id} · version ${receipt.version}`)
    },
  })
  const pair = useMutation({
    mutationFn: () => issueWorkerPairing(),
    onSuccess: (result) =>
      setPairing(`${result.pairing_token} (expires ${new Date(result.expires_at).toLocaleTimeString()})`),
  })
  const revoke = useMutation({
    mutationFn: (id: string) => revokeWorker(id),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['workers', token, workspace] }),
  })

  const setDraft = (value: WorkspaceConfigDocument) => {
    setDraftState(value)
    setSaved('')
  }
  const serverDocument = query.data?.document
  const dirtyTabs = useMemo(() => {
    if (!draft || !serverDocument) return [] as TabId[]
    return (Object.keys(TAB_SLICES) as Array<Exclude<TabId, 'workers' | 'members'>>).filter(
      (id) => JSON.stringify(TAB_SLICES[id](draft)) !== JSON.stringify(TAB_SLICES[id](serverDocument)),
    )
  }, [draft, serverDocument])
  const errorTabs = useMemo(() => {
    if (!(save.error instanceof ConfigValidationError)) return [] as TabId[]
    return [...new Set(save.error.fields.map((field) => tabForField(field.field)))]
  }, [save.error])

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-5xl px-6 pb-24 pt-8">
        <div className="flex items-start justify-between">
          <div className="flex items-baseline gap-3">
            <h1 className="text-xl font-semibold">Workspace</h1>
            {draft && <Badge variant="mono">{draft.workspace}</Badge>}
          </div>
          {canManageWorkspace && (
            <Link to="/workspaces/new">
              <Button variant="secondary" tabIndex={-1}>
                <Plus />
                New workspace
              </Button>
            </Link>
          )}
        </div>
        {!workspace && (
          <Card className="mt-6">
            <CardHeader>
              <CardTitle>No workspace selected</CardTitle>
            </CardHeader>
            <CardContent>
              <p className="text-sm text-muted">
                Pick a workspace from the left rail, or{' '}
                <Link to="/workspaces/new" className="text-primary hover:underline">
                  create one
                </Link>{' '}
                to get started.
              </p>
            </CardContent>
          </Card>
        )}

        {draft && canManageWorkspace ? (
          <>
            <div role="tablist" className="mt-6 flex gap-1 border-b border-border">
              {TABS.map((entry) => {
                const dirty = dirtyTabs.includes(entry.id)
                const errored = errorTabs.includes(entry.id)
                return (
                  <button
                    type="button"
                    key={entry.id}
                    role="tab"
                    aria-selected={tab === entry.id}
                    onClick={() => setTab(entry.id)}
                    className={cn(
                      '-mb-px flex items-center gap-2 border-b-2 px-3.5 py-2.5 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary',
                      tab === entry.id
                        ? 'border-primary text-foreground'
                        : 'border-transparent text-muted hover:text-foreground',
                    )}
                  >
                    {entry.label}
                    {entry.id === 'workers' && (
                      <span className="rounded-full bg-raised px-1.5 text-[11px] leading-4 text-muted">
                        {workers.data?.workers.length ?? 0}
                      </span>
                    )}
                    {(dirty || errored) && (
                      <span
                        title={errored ? 'Validation errors' : 'Unsaved changes'}
                        className={cn('size-1.5 rounded-full', errored ? 'bg-failure' : 'bg-attention-dot')}
                      />
                    )}
                  </button>
                )
              })}
            </div>

            <div className="pt-5">
              {tab === 'general' && <GeneralTab draft={draft} setDraft={setDraft} />}
              {tab === 'policy' && <PolicyTab draft={draft} setDraft={setDraft} />}
              {tab === 'workers' && (
                <WorkersTab
                  data={workers.data}
                  pairing={pairing}
                  onPair={() => pair.mutate()}
                  onRevoke={(id) => revoke.mutate(id)}
                />
              )}
              {tab === 'members' && <MembersSection />}
            </div>

            {(save.error || saved || dirtyTabs.length > 0) && (
              <div className="pointer-events-none sticky bottom-4 mt-6 flex justify-center">
                <div className="pointer-events-auto w-full max-w-2xl space-y-2">
                  {save.error ? (
                    <div className="rounded-lg border border-failure/30 bg-failure-soft p-3 text-sm text-failure shadow-lg">
                      <p>{save.error instanceof ConfigValidationError ? save.error.message : String(save.error)}</p>
                      {save.error instanceof ConfigValidationError &&
                        save.error.fields.map((field) => (
                          <p key={field.field} className="mt-1 font-mono text-xs">
                            {field.field}: {field.message}
                          </p>
                        ))}
                    </div>
                  ) : null}
                  {saved && dirtyTabs.length === 0 && !save.error && (
                    <p className="flex items-center gap-2 rounded-lg border border-positive/30 bg-positive-soft p-3 text-sm text-positive shadow-lg">
                      <CheckCircle2 className="size-4" />
                      {saved}
                    </p>
                  )}
                  {dirtyTabs.length > 0 && (
                    <div className="flex items-center gap-3 rounded-lg border border-edge bg-card py-2 pl-4 pr-2 shadow-lg">
                      <p className="text-sm">
                        <span className="font-medium">Unsaved changes</span>{' '}
                        <span className="text-faint">
                          · {dirtyTabs.map((id) => TABS.find((entry) => entry.id === id)!.label).join(', ')}
                        </span>
                      </p>
                      <div className="ml-auto flex gap-2">
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => {
                            setDraftState(structuredClone(serverDocument!))
                            save.reset()
                            setSaved('')
                          }}
                        >
                          Discard
                        </Button>
                        <Button size="sm" disabled={save.isPending} onClick={() => save.mutate()}>
                          <Save />
                          Save changes
                        </Button>
                      </div>
                    </div>
                  )}
                </div>
              </div>
            )}
          </>
        ) : snapshot ? (
          // Configuration is an operator surface, so a member reaching this page
          // gets the read-only summary. The member list belongs beside it: every
          // member may see who else is here, without the management controls.
          <>
            <ReadOnly snapshot={snapshot} />
            <div className="mt-4">
              <MembersSection />
            </div>
          </>
        ) : null}
      </div>
    </div>
  )
}

function GeneralTab({
  draft,
  setDraft,
}: {
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
}) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateRepo = (index: number, change: Partial<WorkspaceConfigRepo>) => {
    const repos = [...draft.repos]
    repos[index] = { ...repos[index], ...change }
    update({ repos })
  }
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Basics</CardTitle>
        </CardHeader>
        <CardContent>
          <Field
            label="Work-order queue timeout"
            hint="How long a task may wait for a worker before it is marked stalled."
          >
            <Input
              value={draft.work_order_queue_timeout}
              onChange={(event) => update({ work_order_queue_timeout: event.target.value })}
              placeholder="24h"
            />
          </Field>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Repositories</CardTitle>
          <Button
            size="sm"
            variant="secondary"
            onClick={() => update({ repos: [...draft.repos, { name: '', url: '', base: 'main' }] })}
          >
            <Plus />
            Add repository
          </Button>
        </CardHeader>
        <CardContent className="space-y-3">
          {draft.repos.map((repo, index) => (
            <div
              key={index}
              className="grid items-end gap-3 rounded-md border border-border p-3 md:grid-cols-[1fr_1.4fr_1fr_auto_auto]"
            >
              <Field label="Name">
                <Input value={repo.name} onChange={(event) => updateRepo(index, { name: event.target.value })} />
              </Field>
              <Field label="URL">
                <Input value={repo.url} onChange={(event) => updateRepo(index, { url: event.target.value })} />
              </Field>
              <Field label="GitHub slug">
                <Input
                  value={repo.github ?? ''}
                  onChange={(event) => updateRepo(index, { github: event.target.value })}
                />
              </Field>
              <Field label="Base">
                <Input
                  className="w-24"
                  value={repo.base}
                  onChange={(event) => updateRepo(index, { base: event.target.value })}
                />
              </Field>
              <Button
                size="icon"
                variant="ghost"
                className="mb-0.5 hover:text-failure"
                aria-label={`Remove repository ${repo.name || index + 1}`}
                onClick={() => update({ repos: draft.repos.filter((_, i) => i !== index) })}
              >
                <Trash2 />
              </Button>
            </div>
          ))}
          {draft.repos.length === 0 && <p className="text-sm text-faint">No repositories yet.</p>}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Repository monitor</CardTitle>
          <Switch
            aria-label="Enable repository monitor"
            checked={draft.monitor?.enabled ?? false}
            onChange={(enabled) =>
              update({
                monitor: {
                  enabled,
                  repositories: draft.monitor?.repositories ?? [],
                  poll_interval: draft.monitor?.poll_interval ?? '1m',
                  startup_window: draft.monitor?.startup_window ?? '24h',
                },
              })
            }
          />
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-xs leading-5 text-muted">
            Observe selected GitHub repositories for post-merge failures and changes outside Conveyor. Signals always
            create ordinary gated tasks; the monitor cannot implement, review, merge, or deploy them.
          </p>
          <div className="grid gap-3 md:grid-cols-2">
            <Field label="Poll interval">
              <Input
                value={draft.monitor?.poll_interval ?? '1m'}
                onChange={(event) =>
                  update({
                    monitor: {
                      enabled: draft.monitor?.enabled ?? false,
                      repositories: draft.monitor?.repositories ?? [],
                      poll_interval: event.target.value,
                      startup_window: draft.monitor?.startup_window ?? '24h',
                    },
                  })
                }
              />
            </Field>
            <Field label="Startup reconciliation window">
              <Input
                value={draft.monitor?.startup_window ?? '24h'}
                onChange={(event) =>
                  update({
                    monitor: {
                      enabled: draft.monitor?.enabled ?? false,
                      repositories: draft.monitor?.repositories ?? [],
                      poll_interval: draft.monitor?.poll_interval ?? '1m',
                      startup_window: event.target.value,
                    },
                  })
                }
              />
            </Field>
          </div>
          <div>
            <p className="mb-2 text-xs font-medium text-muted">Monitored repositories</p>
            <div className="flex flex-wrap gap-2">
              {draft.repos.map((repo) => {
                const selected = draft.monitor?.repositories.includes(repo.name) ?? false
                return (
                  <label
                    key={repo.name}
                    className="flex items-center gap-2 rounded border border-border px-3 py-2 text-xs"
                  >
                    <input
                      type="checkbox"
                      checked={selected}
                      onChange={(event) => {
                        const current = draft.monitor ?? {
                          enabled: false,
                          repositories: [],
                          poll_interval: '1m',
                          startup_window: '24h',
                        }
                        update({
                          monitor: {
                            ...current,
                            repositories: event.target.checked
                              ? [...current.repositories, repo.name]
                              : current.repositories.filter((name) => name !== repo.name),
                          },
                        })
                      }}
                    />
                    {repo.name || 'Unnamed repository'}
                  </label>
                )
              })}
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

function PolicyTab({
  draft,
  setDraft,
}: {
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
}) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateStageTimeout = (stage: 'spec' | 'implement' | 'review', value: string) =>
    update({ stage_timeouts: { ...draft.stage_timeouts, [stage]: value } })
  const seatCount = draft.review.seats.length
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Pipeline policy</CardTitle>
          <span className="text-xs text-faint">Defaults are frozen onto each task at intake.</span>
        </CardHeader>
        <CardContent className="grid gap-x-6 gap-y-5 md:grid-cols-2">
          <div className="space-y-3">
            <div className="flex items-center gap-3">
              <Switch
                aria-label="Plan approval"
                checked={draft.execution.spec_approval}
                onChange={(checked) => update({ execution: { ...draft.execution, spec_approval: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Pause for plan approval</p>
                <p className="text-xs text-faint">Require operator approval before implementation starts</p>
              </div>
            </div>
            <div className="flex items-center gap-3">
              <Switch
                aria-label="Merge approval"
                checked={draft.execution.merge_approval}
                onChange={(checked) => update({ execution: { ...draft.execution, merge_approval: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Pause before merge</p>
                <p className="text-xs text-faint">Require operator approval before the reviewed change lands</p>
              </div>
            </div>
            <div className="flex items-center gap-3">
              <Switch
                aria-label="Require verification evidence"
                checked={draft.execution.require_verification_evidence}
                onChange={(checked) =>
                  update({ execution: { ...draft.execution, require_verification_evidence: checked } })
                }
              />
              <div>
                <p className="text-sm font-medium">Require verification evidence</p>
                <p className="text-xs text-faint">Require an eligible screenshot or short recording before review</p>
              </div>
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field
              label="Review rounds before check-in"
              hint="Unsupervised implement-review rounds before the task pauses for an operator."
            >
              <Input
                type="number"
                min={1}
                value={draft.max_bounces}
                onChange={(event) => update({ max_bounces: Number(event.target.value) })}
              />
            </Field>
            <Field label="Review seats" hint="Independent verdicts required in each review round.">
              <Input
                aria-label="Review seats"
                type="number"
                min={1}
                value={seatCount}
                onChange={(event) => {
                  const count = Math.max(1, Number(event.target.value) || 1)
                  update({ review: { seats: Array.from({ length: count }, () => ({})) } })
                }}
              />
            </Field>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Stage timeouts</CardTitle>
          <span className="text-xs text-faint">Each value is a positive duration such as 30m or 4h.</span>
        </CardHeader>
        <CardContent className="grid gap-3 md:grid-cols-3">
          <Field label="Plan">
            <Input
              aria-label="Plan stage timeout"
              value={draft.stage_timeouts.spec}
              onChange={(event) => updateStageTimeout('spec', event.target.value)}
            />
          </Field>
          <Field label="Implementation">
            <Input
              aria-label="Implementation stage timeout"
              value={draft.stage_timeouts.implement}
              onChange={(event) => updateStageTimeout('implement', event.target.value)}
            />
          </Field>
          <Field label="Review">
            <Input
              aria-label="Review stage timeout"
              value={draft.stage_timeouts.review}
              onChange={(event) => updateStageTimeout('review', event.target.value)}
            />
          </Field>
        </CardContent>
      </Card>
    </div>
  )
}
function WorkersTab({
  data,
  pairing,
  onPair,
  onRevoke,
}: {
  data?: WorkerList
  pairing: string
  onPair: () => void
  onRevoke: (id: string) => void
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Workers</CardTitle>
        <div className="flex items-center gap-2">
          <Badge variant={!data?.worker_expected || data.worker_available ? 'positive' : 'attention'}>
            {!data?.worker_expected
              ? 'Pull claiming'
              : data.worker_available
                ? 'Worker available'
                : 'Worker unavailable'}
          </Badge>
          <Button size="sm" variant="secondary" onClick={onPair}>
            Issue pairing token
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {pairing && (
          <p className="break-all rounded-md border border-border bg-surface p-2 font-mono text-xs">{pairing}</p>
        )}
        {data?.worker_expected && !data.worker_available && (
          <div className="rounded-md border border-attention/40 bg-attention-soft p-3 text-xs leading-5 text-muted">
            <p>{data?.worker_unavailable_reason}</p>
            <p>
              Restart an enrolled worker with its saved credential; ordinary restarts do not require a new pairing
              token.
            </p>
          </div>
        )}
        {(data?.rate_limits?.length ?? 0) > 0 && (
          <div className="rounded-md border border-border bg-surface p-3">
            <p className="text-xs font-semibold uppercase tracking-wider text-faint">Provider rate limits</p>
            <div className="mt-2 space-y-2">
              {data!.rate_limits!.map((entry) => {
                const values = [
                  entry.rate_limit.remaining != null && entry.rate_limit.limit != null
                    ? `${entry.rate_limit.remaining} of ${entry.rate_limit.limit} remaining`
                    : undefined,
                  entry.rate_limit.reset_at
                    ? `resets ${new Date(entry.rate_limit.reset_at).toLocaleString()}`
                    : undefined,
                  `observed ${new Date(entry.observed_at).toLocaleString()}`,
                ]
                  .filter(Boolean)
                  .join(' · ')
                return (
                  <div
                    key={`${entry.harness}-${entry.model ?? ''}`}
                    className="flex flex-wrap items-baseline gap-x-2 text-xs"
                  >
                    <span className="font-mono font-medium">
                      {entry.harness}
                      {entry.model ? ` / ${entry.model}` : ''}
                    </span>
                    <Badge variant="mono">{entry.rate_limit.status}</Badge>
                    <span className="text-muted">{values}</span>
                  </div>
                )
              })}
            </div>
            <p className="mt-2 text-[11px] text-faint">
              Self-reported telemetry only; Conveyor does not use it to gate or route work.
            </p>
          </div>
        )}
        {(data?.workers ?? []).map((worker) => (
          <div key={worker.id} className="rounded-md border border-border p-3">
            <div className="flex items-center justify-between">
              <div>
                <p className="text-sm font-medium">{worker.name}</p>
                <p className="font-mono text-xs text-faint">{worker.id}</p>
                <p className="text-xs text-muted">
                  {worker.last_seen_at
                    ? `Last heartbeat ${new Date(worker.last_seen_at).toLocaleString()}`
                    : 'No heartbeat recorded'}
                  {worker.lease_expires_at && new Date(worker.lease_expires_at) <= new Date() ? ' · disconnected' : ''}
                </p>
              </div>
              <Button size="sm" variant="destructive" onClick={() => onRevoke(worker.id)}>
                Revoke
              </Button>
            </div>
            <div className="mt-2 flex flex-wrap gap-1">
              {(worker.probes ?? []).map((probe) => (
                <Badge key={`${probe.harness}-${probe.checked_at}`} variant={probe.healthy ? 'positive' : 'attention'}>
                  {probe.harness}: {probe.healthy ? 'healthy' : 'unhealthy'}
                </Badge>
              ))}
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
  )
}

function ReadOnly({ snapshot }: { snapshot: NonNullable<ReturnType<typeof useWorkspace>['data']> }) {
  return (
    <div className="mt-6 space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>{snapshot.workspace}</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted">
            {snapshot.repos?.length ?? 0} repositories · {snapshot.database} · check-in after {snapshot.max_bounces}{' '}
            review rounds
          </p>
        </CardContent>
      </Card>
    </div>
  )
}
