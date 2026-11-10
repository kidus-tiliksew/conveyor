import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useLocation } from '@tanstack/react-router'
import { CheckCircle2, Plus, Save, Trash2 } from 'lucide-react'
import { useEffect, useId, useMemo, useState } from 'react'
import {
  useCanCreateWorkspaces,
  useWorkspace,
  useWorkspaceCapability,
  useWorkspaceSelection,
} from '../components/app-shell'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input, Select } from '../components/ui/input'
import { PageHeader } from '../components/ui/page-header'
import { Switch } from '../components/ui/switch'
import { Tabs, tabPanelProps } from '../components/ui/tabs'
import { Field } from '../components/workspace/field'
import { MembersSection } from '../components/workspace/members-section'
import { VerificationKits } from '../components/workspace/verification-kits'
import { VerificationSettings } from '../components/workspace/verification-settings'
import { ConfigValidationError, fetchWorkspaceConfig, updateWorkspaceConfig } from '../lib/api'
import { planStateLabels } from '../lib/contracts'
import { githubSlug } from '../lib/repository'
import type {
  ConfigWarning,
  ExecutionPolicy,
  RiskMode,
  RiskTier,
  RiskTierTable,
  WorkspaceConfigDocument,
  WorkspaceConfigRepo,
} from '../lib/types'
import { cn } from '../lib/utils'

// The selected tab is page state, never part of the address, so ordinary
// navigation to this page opens on Policy. Conveyor has no workers, so no tab,
// badge, or panel describes them (DEC-97(1)).
type TabId = 'general' | 'policy' | 'verification' | 'members'

// A link saved while the retired Workers tab was open carries `?tab=workers`
// and opens General as a bounded compatibility fallback. No other `tab` value
// selects a tab, and the page never writes one (component-web-dashboard,
// Workspace page).
function initialTab(searchStr: string): TabId {
  return new URLSearchParams(searchStr).get('tab') === 'workers' ? 'general' : 'policy'
}

const TABS: Array<{ id: TabId; label: string }> = [
  { id: 'general', label: 'General' },
  { id: 'policy', label: 'Policy' },
  { id: 'verification', label: 'Verification' },
  { id: 'members', label: 'Members' },
]

// Which tab owns a config section, for dirty markers and validation-error routing.
// The verify_stage switch lives in execution but belongs to the Verification
// tab, so the policy slice compares execution without it.
const TAB_SLICES: Record<Exclude<TabId, 'members'>, (document: WorkspaceConfigDocument) => unknown> = {
  general: (document) => [
    document.work_order_queue_timeout,
    document.repos,
    document.monitor,
    document.issue_publication?.enabled ?? false,
  ],
  policy: (document) => {
    const { verify_stage: _verify, ...execution } = document.execution
    return [document.max_bounces, document.stage_timeouts, document.review, execution, document.risk_policy ?? null]
  },
  verification: (document) => [document.execution.verify_stage ?? false],
}

function tabForField(field: string): TabId {
  if (/^(work_order_queue_timeout|repos|monitor|issue_publication)/.test(field)) return 'general'
  if (/^execution\.verify_stage/.test(field)) return 'verification'
  return 'policy'
}

export function WorkspacePage() {
  const canManageWorkspace = useWorkspaceCapability('manage_workspace')
  // Creating another workspace follows the caller's own right; editing the
  // selected workspace keeps manage_workspace (component-web-dashboard).
  const canCreateWorkspaces = useCanCreateWorkspaces()
  const queryClient = useQueryClient()
  const { workspace } = useWorkspaceSelection()
  const { data: snapshot } = useWorkspace()
  const query = useQuery({
    queryKey: ['workspace-config', workspace],
    queryFn: () => fetchWorkspaceConfig(workspace),
    enabled: Boolean(workspace),
  })
  const searchStr = useLocation({ select: (location) => location.searchStr })
  const [tab, setTab] = useState<TabId>(() => initialTab(searchStr))
  const tabsId = useId()
  const [draft, setDraftState] = useState<WorkspaceConfigDocument | null>(null)
  const [saved, setSaved] = useState('')
  useEffect(() => {
    setDraftState(query.data ? structuredClone(query.data.document) : null)
  }, [query.data, workspace])
  const save = useMutation({
    mutationFn: () => updateWorkspaceConfig(workspace, withoutLegacyPlanKey(draft!), query.data!.version),
    onSuccess: (receipt) => {
      queryClient.setQueryData(['workspace-config', workspace], receipt)
      void queryClient.invalidateQueries({ queryKey: ['workspace', workspace] })
      setDraftState(structuredClone(receipt.document))
      setSaved(`Recorded config.updated event ${receipt.event_id} · version ${receipt.version}`)
    },
  })

  const setDraft = (value: WorkspaceConfigDocument) => {
    setDraftState(value)
    setSaved('')
  }
  const serverDocument = query.data?.document
  const dirtyTabs = useMemo(() => {
    if (!draft || !serverDocument) return [] as TabId[]
    return (Object.keys(TAB_SLICES) as Array<Exclude<TabId, 'members'>>).filter(
      (id) => JSON.stringify(TAB_SLICES[id](draft)) !== JSON.stringify(TAB_SLICES[id](serverDocument)),
    )
  }, [draft, serverDocument])
  const errorTabs = useMemo(() => {
    if (!(save.error instanceof ConfigValidationError)) return [] as TabId[]
    return [...new Set(save.error.fields.map((field) => tabForField(field.field)))]
  }, [save.error])

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader title="Workspace" badge={draft && <Badge variant="mono">{draft.workspace}</Badge>}>
        {canCreateWorkspaces && (
          <Link to="/workspaces/new">
            <Button variant="secondary" size="sm" tabIndex={-1}>
              <Plus />
              New workspace
            </Button>
          </Link>
        )}
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-5xl px-4 pb-24 pt-6 sm:px-6">
          {!workspace && (
            <Card className="mt-6">
              <CardHeader>
                <CardTitle>No workspace selected</CardTitle>
              </CardHeader>
              <CardContent>
                {canCreateWorkspaces ? (
                  <p className="text-sm text-muted">
                    Pick a workspace from the left rail, or{' '}
                    <Link to="/workspaces/new" className="text-primary hover:underline">
                      create one
                    </Link>{' '}
                    to get started.
                  </p>
                ) : (
                  <p className="text-sm text-muted">Pick a workspace from the left rail to get started.</p>
                )}
              </CardContent>
            </Card>
          )}

          {draft && canManageWorkspace ? (
            <>
              {/* When the tabs exceed a phone width, the row scrolls inside itself. */}
              <Tabs
                id={tabsId}
                aria-label="Workspace settings"
                items={TABS.map((entry) => {
                  const dirty = dirtyTabs.includes(entry.id)
                  const errored = errorTabs.includes(entry.id)
                  return {
                    id: entry.id,
                    label: (
                      <>
                        {entry.label}
                        {(dirty || errored) && (
                          <span
                            title={errored ? 'Validation errors' : 'Unsaved changes'}
                            className={cn('ml-0.5 size-1.5 rounded-full', errored ? 'bg-failure' : 'bg-attention-dot')}
                          />
                        )}
                      </>
                    ),
                  }
                })}
                value={tab}
                onChange={setTab}
              />

              <div {...tabPanelProps(tabsId, tab)} className="pt-5">
                {tab === 'general' && (
                  <GeneralTab
                    draft={draft}
                    setDraft={setDraft}
                    errors={save.error instanceof ConfigValidationError ? save.error.fields : []}
                  />
                )}
                {tab === 'policy' && (
                  <PolicyTab
                    draft={draft}
                    setDraft={setDraft}
                    errors={save.error instanceof ConfigValidationError ? save.error.fields : []}
                    warnings={query.data?.warnings ?? []}
                    riskChanged={
                      JSON.stringify(draft.risk_policy ?? null) !== JSON.stringify(serverDocument?.risk_policy ?? null)
                    }
                  />
                )}
                {tab === 'verification' && (
                  <div className="space-y-4">
                    <VerificationSettings draft={draft} setDraft={setDraft} />
                    <VerificationKits key={workspace} workspace={workspace} />
                  </div>
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
              <ReadOnly snapshot={snapshot} configuration={query.data?.document} />
              {workspace && (
                <div className="mt-4">
                  <VerificationKits key={workspace} workspace={workspace} />
                </div>
              )}
              <div className="mt-4">
                <MembersSection />
              </div>
            </>
          ) : null}
        </div>
      </div>
    </div>
  )
}

function monitorDraft(
  draft: WorkspaceConfigDocument,
  patch: Partial<NonNullable<WorkspaceConfigDocument['monitor']>>,
): NonNullable<WorkspaceConfigDocument['monitor']> {
  return {
    enabled: draft.monitor?.enabled ?? false,
    repositories: draft.monitor?.repositories ?? [],
    poll_interval: draft.monitor?.poll_interval ?? '1m',
    startup_window: draft.monitor?.startup_window ?? '24h',
    merge_deferral_bound: draft.monitor?.merge_deferral_bound,
    ...patch,
  }
}

function GeneralTab({
  draft,
  setDraft,
  errors,
}: {
  errors: Array<{ field: string; message: string }>
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
}) {
  const { workspace } = useWorkspaceSelection()
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
            hint="How long a work order may wait in the queue before it is marked stalled."
          >
            <Input
              className="max-w-48"
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
            onClick={() =>
              update({ repos: [...draft.repos, { name: '', url: '', base: 'main', install_conveyor: true }] })
            }
          >
            <Plus />
            Add repository
          </Button>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-xs text-muted">
            Install Conveyor files a task to add agent instructions and Conveyor's skills, delivered as a pull request.
          </p>
          {draft.repos.map((repo, index) => (
            <div
              key={index}
              data-testid="repository-row"
              className="grid items-start gap-3 border-t border-border py-3 first:border-t-0 first:pt-0 md:grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)_6.5rem_auto]"
            >
              <Field label="Name">
                <Input value={repo.name} onChange={(event) => updateRepo(index, { name: event.target.value })} />
              </Field>
              <Field label="URL">
                <Input
                  aria-label="URL"
                  value={repo.url}
                  onChange={(event) => updateRepo(index, { url: event.target.value, github: undefined })}
                />
                {githubSlug(repo.url) && (
                  <span className="mt-1 block truncate text-xs text-faint">
                    GitHub slug: {repo.github ?? githubSlug(repo.url)}
                  </span>
                )}
              </Field>
              <Field label="Base">
                <Input value={repo.base} onChange={(event) => updateRepo(index, { base: event.target.value })} />
              </Field>
              <Button
                size="icon"
                variant="ghost"
                className="self-end hover:text-failure"
                aria-label={`Remove repository ${repo.name || index + 1}`}
                onClick={() => update({ repos: draft.repos.filter((_, i) => i !== index) })}
              >
                <Trash2 />
              </Button>
              <div className="space-y-1 md:col-span-4">
                <div className="flex items-center gap-2">
                  <Switch
                    aria-label="Install Conveyor"
                    checked={repo.install_conveyor ?? true}
                    onChange={(checked) => updateRepo(index, { install_conveyor: checked })}
                  />
                  <span className="text-sm font-medium">Install Conveyor</span>
                </div>
                {repo.install_task && (
                  <div
                    className="flex flex-wrap items-center gap-2 border-l border-border pl-2 text-xs"
                    title={`Install task ${repo.install_task.id}: ${repo.install_task.state}`}
                  >
                    <span className="text-muted">Install task</span>
                    <Link
                      to="/w/$workspaceId/tasks/$taskId"
                      params={{ workspaceId: workspace, taskId: repo.install_task.id }}
                      className="font-mono text-primary hover:underline"
                    >
                      {repo.install_task.id}
                    </Link>
                    <Badge>{repo.install_task.state}</Badge>
                  </div>
                )}
              </div>
              {errors
                .filter((error) => error.field === `repos[${index}]` || error.field.startsWith(`repos[${index}].`))
                .map((error) => (
                  <p key={error.field} role="alert" className="text-xs text-failure md:col-span-4">
                    {error.message}
                  </p>
                ))}
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
                monitor: monitorDraft(draft, { enabled }),
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
                    monitor: monitorDraft(draft, { poll_interval: event.target.value }),
                  })
                }
              />
            </Field>
            <Field label="Startup reconciliation window">
              <Input
                value={draft.monitor?.startup_window ?? '24h'}
                onChange={(event) =>
                  update({
                    monitor: monitorDraft(draft, { startup_window: event.target.value }),
                  })
                }
              />
            </Field>
            <Field
              label="Merge deferral bound"
              hint="How long to skip direct pushes and external merges while a Conveyor merge is in flight. Leave empty to derive min(10m, startup window − poll − 5s)."
            >
              <Input
                value={draft.monitor?.merge_deferral_bound ?? ''}
                placeholder="auto"
                onChange={(event) =>
                  update({
                    monitor: monitorDraft(draft, { merge_deferral_bound: event.target.value || undefined }),
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
                        const repositories = event.target.checked
                          ? [...(draft.monitor?.repositories ?? []), repo.name]
                          : (draft.monitor?.repositories ?? []).filter((name) => name !== repo.name)
                        update({
                          monitor: monitorDraft(draft, { repositories }),
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
      <Card>
        <CardHeader>
          <CardTitle>Issue publication</CardTitle>
          <Switch
            aria-label="Publish GitHub issues for approved plans"
            checked={draft.issue_publication?.enabled ?? false}
            onChange={(enabled) => update({ issue_publication: { ...draft.issue_publication, enabled } })}
          />
        </CardHeader>
        <CardContent>
          <p className="text-xs leading-5 text-muted">
            Publish GitHub issues for approved plans. Only plans approved after this is turned on get an issue. Turning
            it off creates no further issues and leaves published ones unchanged.
          </p>
        </CardContent>
      </Card>
    </div>
  )
}

// The Policy tab binds execution.default_plan_state and never the retired
// execution.spec_approval key (component-web-dashboard invariant 33;
// req-task-plan-state AC-2.1, AC-3.5; DEC-125(7)).
const defaultPlanStates = ['needed', 'not_required'] as const

// The value the configuration read serves. A document that states neither key
// takes the server's own default, Planning needed (component-runtime
// invariant 39); the legacy alias is read only when the key is absent.
function servedDefaultPlanState(execution: ExecutionPolicy): ExecutionPolicy['default_plan_state'] {
  if (execution.default_plan_state) return execution.default_plan_state
  const legacy = (execution as { spec_approval?: unknown }).spec_approval
  return legacy === false ? 'not_required' : 'needed'
}

// A save writes default_plan_state and never spec_approval, whatever the read
// carried.
function withoutLegacyPlanKey(document: WorkspaceConfigDocument): WorkspaceConfigDocument {
  const { spec_approval: _legacy, ...execution } = document.execution as ExecutionPolicy & { spec_approval?: unknown }
  return { ...document, execution: { ...execution, default_plan_state: servedDefaultPlanState(document.execution) } }
}

function PolicyTab({
  draft,
  setDraft,
  errors,
  warnings,
  riskChanged,
}: {
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
  errors: Array<{ field: string; message: string }>
  warnings: ConfigWarning[]
  riskChanged: boolean
}) {
  const update = (change: Partial<WorkspaceConfigDocument>) => setDraft({ ...draft, ...change })
  const updateStageTimeout = (stage: 'spec' | 'implement' | 'review', value: string) =>
    update({ stage_timeouts: { ...draft.stage_timeouts, [stage]: value } })
  const seatCount = draft.review.seats.length
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex-wrap">
          <CardTitle>Pipeline policy</CardTitle>
          <span className="text-xs text-faint">Defaults are frozen onto each task at intake.</span>
        </CardHeader>
        <CardContent className="grid gap-x-6 gap-y-5 md:grid-cols-2">
          <div className="space-y-3">
            <div className="max-w-xs">
              <Field
                label="Default plan state"
                hint="The plan state a new task takes when it is filed with neither a plan state nor a plan. Existing tasks keep theirs."
              >
                <Select
                  aria-label="Default plan state"
                  value={servedDefaultPlanState(draft.execution)}
                  onChange={(event) =>
                    update({
                      execution: {
                        ...draft.execution,
                        default_plan_state: event.target.value as ExecutionPolicy['default_plan_state'],
                      },
                    })
                  }
                >
                  {defaultPlanStates.map((state) => (
                    <option key={state} value={state}>
                      {planStateLabels[state]}
                    </option>
                  ))}
                </Select>
              </Field>
              <p className="mt-1 text-xs text-faint">New tasks only. No existing task's plan state changes.</p>
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
                aria-label="Fail-fast review"
                checked={draft.review.fail_fast ?? false}
                onChange={(checked) => update({ review: { ...draft.review, fail_fast: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Fail-fast review</p>
                <p className="text-xs text-faint">
                  End a review round on its first changes-requested verdict and cancel the seats still reviewing
                </p>
              </div>
            </div>
            <div className="flex items-center gap-3">
              <Switch
                aria-label="Distinct seat models"
                checked={draft.review.distinct_models ?? false}
                onChange={(checked) => {
                  // Off omits the key, matching the fetched document, so the draft reads as unchanged.
                  const { distinct_models: _off, ...review } = draft.review
                  update({ review: checked ? { ...review, distinct_models: true } : review })
                }}
              />
              <div>
                <p className="text-sm font-medium">Distinct seat models</p>
                <p className="text-xs text-faint">
                  Refuse a review seat claim that repeats the name another seat of the round reports. Names are
                  self-reported and never verified. New tasks only.
                </p>
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
                  update({ review: { ...draft.review, seats: Array.from({ length: count }, () => ({})) } })
                }}
              />
            </Field>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-wrap">
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

      <RiskPolicyCard
        draft={draft}
        setDraft={setDraft}
        errors={errors.filter((error) => error.field.startsWith('risk_policy'))}
        // A warning describes the saved policy, so an unsaved edit hides it
        // until the next save answers again.
        warnings={riskChanged ? [] : warnings.filter((warning) => warning.field === 'risk_policy')}
      />
    </div>
  )
}

const riskModes: Array<{ value: RiskMode; label: string }> = [
  { value: 'off', label: 'Off' },
  { value: 'record', label: 'Record' },
  { value: 'enforce', label: 'Enforce' },
]

const riskTiers: Array<{ value: RiskTier; label: string }> = [
  { value: 'low', label: 'Low' },
  { value: 'medium', label: 'Medium' },
  { value: 'high', label: 'High' },
]

// The table a policy turned on without a stated table uses
// (req-risk-based-review AC-1.3).
const defaultRiskTiers: RiskTierTable = {
  low: { agent_reviews: 1, human_approvals: 0 },
  medium: { agent_reviews: 1, human_approvals: 1 },
  high: { agent_reviews: 2, human_approvals: 2 },
}

const riskModeHints: Record<RiskMode, string> = {
  off: 'No task gets a risk tier.',
  record: 'Each new task gets a risk tier. Reviews, approvals, and merges do not change.',
  enforce: 'Each new task gets a risk tier, and its tier sets the reviews and approvals it needs.',
}

// The workspace risk policy (req-risk-based-review REQ-1). A workspace with no
// policy reads as Off, and the draft keeps the key absent until an operator
// chooses a mode, so an unrelated save does not write it.
function RiskPolicyCard({
  draft,
  setDraft,
  errors,
  warnings,
}: {
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
  errors: Array<{ field: string; message: string }>
  warnings: ConfigWarning[]
}) {
  const policy = draft.risk_policy
  const mode: RiskMode = policy?.mode ?? 'off'
  const tiers = policy?.tiers ?? defaultRiskTiers
  const setMode = (next: RiskMode) => {
    if (!policy && next === 'off') return
    setDraft({ ...draft, risk_policy: { mode: next, tiers: structuredClone(tiers) } })
  }
  const setCount = (tier: RiskTier, key: 'agent_reviews' | 'human_approvals', value: string) => {
    setDraft({
      ...draft,
      risk_policy: { mode, tiers: { ...tiers, [tier]: { ...tiers[tier], [key]: Number(value) } } },
    })
  }
  return (
    <Card>
      <CardHeader className="flex-wrap">
        <CardTitle>Risk review</CardTitle>
        <span className="text-xs text-faint">Frozen onto each task at intake. Existing tasks keep theirs.</span>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="max-w-xs">
          <Field label="Risk mode">
            <Select aria-label="Risk mode" value={mode} onChange={(event) => setMode(event.target.value as RiskMode)}>
              {riskModes.map((entry) => (
                <option key={entry.value} value={entry.value}>
                  {entry.label}
                </option>
              ))}
            </Select>
          </Field>
          <p className="mt-1 text-xs text-faint">{riskModeHints[mode]}</p>
        </div>
        {mode !== 'off' && (
          <div className="space-y-2">
            <p className="text-xs text-faint">
              Each tier needs at least one agent review. No number can be lower than the same number in the tier below.
            </p>
            <table className="w-full max-w-lg text-sm" aria-label="Risk tier requirements">
              <thead>
                <tr className="text-left text-xs text-muted">
                  <th scope="col" className="py-1 pr-3 font-medium">
                    Tier
                  </th>
                  <th scope="col" className="py-1 pr-3 font-medium">
                    Agent reviews
                  </th>
                  <th scope="col" className="py-1 font-medium">
                    Human approvals
                  </th>
                </tr>
              </thead>
              <tbody>
                {riskTiers.map((tier) => (
                  <tr key={tier.value} className="border-t border-border">
                    <th scope="row" className="py-2 pr-3 text-left font-medium">
                      {tier.label}
                    </th>
                    <td className="py-2 pr-3">
                      <Input
                        aria-label={`${tier.label} agent reviews`}
                        type="number"
                        min={1}
                        step={1}
                        className="max-w-24"
                        value={tiers[tier.value].agent_reviews}
                        onChange={(event) => setCount(tier.value, 'agent_reviews', event.target.value)}
                      />
                    </td>
                    <td className="py-2">
                      <Input
                        aria-label={`${tier.label} human approvals`}
                        type="number"
                        min={0}
                        step={1}
                        className="max-w-24"
                        value={tiers[tier.value].human_approvals}
                        onChange={(event) => setCount(tier.value, 'human_approvals', event.target.value)}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {errors.map((error) => (
          <p key={error.field + error.message} role="alert" className="text-xs text-failure">
            {error.message}
          </p>
        ))}
        {warnings.map((warning) => (
          <p
            key={warning.code}
            role="status"
            className="rounded-md border border-attention/30 bg-attention-soft p-2 text-xs text-attention"
          >
            {warning.message}
          </p>
        ))}
      </CardContent>
    </Card>
  )
}
function ReadOnly({
  snapshot,
  configuration,
}: {
  snapshot: NonNullable<ReturnType<typeof useWorkspace>['data']>
  configuration?: WorkspaceConfigDocument
}) {
  return (
    <div className="mt-6 space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>{snapshot.workspace}</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted">
            {snapshot.repos?.length ?? 0} repositories · {snapshot.database} · check-in after {snapshot.max_bounces}{' '}
            review rounds ·{' '}
            {snapshot.recovery?.automatic_recoveries === 0
              ? 'off'
              : `${snapshot.recovery?.automatic_recoveries ?? 2} automatic recoveries`}{' '}
            / {snapshot.recovery?.window ?? '24h'}
          </p>
          {/* view_workspace serves the configuration, so every member sees the
              setting it governs (req-delivery-and-forge AC-8.5). */}
          {configuration && (
            <p className="mt-2 text-sm text-muted">
              GitHub issue publication for approved plans:{' '}
              <span className="font-medium text-foreground">
                {configuration.issue_publication?.enabled ? 'on' : 'off'}
              </span>
            </p>
          )}
          {configuration && (
            <p className="mt-2 text-sm text-muted">
              Risk mode: <span className="font-medium text-foreground">{configuration.risk_policy?.mode ?? 'off'}</span>
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
