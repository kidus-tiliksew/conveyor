import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useLocation } from '@tanstack/react-router'
import { CheckCircle2, Plus, Save, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import {
  useCanCreateWorkspaces,
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
import { VerificationKits } from '../components/workspace/verification-kits'
import { VerificationSettings } from '../components/workspace/verification-settings'
import { ConfigValidationError, fetchWorkspaceConfig, updateWorkspaceConfig } from '../lib/api'
import { githubSlug } from '../lib/repository'
import type { WorkspaceConfigDocument, WorkspaceConfigRepo } from '../lib/types'
import { cn } from '../lib/utils'
import { PageHeader } from '../components/ui/page-header'

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
    return [document.max_bounces, document.stage_timeouts, document.review, execution]
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
  const [draft, setDraftState] = useState<WorkspaceConfigDocument | null>(null)
  const [saved, setSaved] = useState('')
  useEffect(() => {
    setDraftState(query.data ? structuredClone(query.data.document) : null)
  }, [query.data, workspace])
  const save = useMutation({
    mutationFn: () => updateWorkspaceConfig(workspace, draft!, query.data!.version),
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
    <div className="flex h-full flex-col">
      <PageHeader title="Workspace" meta={draft && <Badge variant="mono">{draft.workspace}</Badge>}>
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
        <div className="max-w-2xl px-4 pb-24 pt-3 lg:px-6">
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
              <div
                role="tablist"
                className="flex gap-4 overflow-x-auto border-b border-border [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
              >
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
                        '-mb-px flex shrink-0 items-center gap-2 whitespace-nowrap border-b-2 pb-2.5 pt-1.5 text-[13px] font-medium transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary',
                        tab === entry.id
                          ? 'border-primary text-foreground'
                          : 'border-transparent text-muted hover:text-foreground',
                      )}
                    >
                      {entry.label}
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
                {tab === 'general' && (
                  <GeneralTab
                    draft={draft}
                    setDraft={setDraft}
                    errors={save.error instanceof ConfigValidationError ? save.error.fields : []}
                  />
                )}
                {tab === 'policy' && <PolicyTab draft={draft} setDraft={setDraft} />}
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
          {draft.repos.map((repo, index) => (
            <div
              key={index}
              data-testid="repository-row"
              className="grid items-start gap-3 rounded-md border border-border p-3 md:grid-cols-[1fr_1.4fr_1fr_auto_auto]"
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
                  <span className="mt-1 block break-all font-mono text-xs text-muted">
                    GitHub slug: {repo.github ?? githubSlug(repo.url)}
                  </span>
                )}
              </Field>
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <Switch
                    aria-label="Install Conveyor"
                    checked={repo.install_conveyor ?? true}
                    onChange={(checked) => updateRepo(index, { install_conveyor: checked })}
                  />
                  <span className="text-sm font-medium">Install Conveyor</span>
                </div>
                <p className="text-xs leading-5 text-muted">
                  The factory files a task to add agent instructions and Conveyor's skills, delivered as a pull request.
                </p>
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
              {errors
                .filter((error) => error.field === `repos[${index}]` || error.field.startsWith(`repos[${index}].`))
                .map((error) => (
                  <p key={error.field} role="alert" className="text-xs text-failure md:col-span-5">
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
        <CardContent className="space-y-4">
          <div className="space-y-3.5">
            <div className="flex items-start gap-3">
              <Switch
                aria-label="Plan approval"
                checked={draft.execution.spec_approval}
                onChange={(checked) => update({ execution: { ...draft.execution, spec_approval: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Pause for plan approval</p>
                <p className="text-xs leading-[1.45] text-muted">
                  Require operator approval before implementation starts
                </p>
              </div>
            </div>
            <div className="flex items-start gap-3">
              <Switch
                aria-label="Merge approval"
                checked={draft.execution.merge_approval}
                onChange={(checked) => update({ execution: { ...draft.execution, merge_approval: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Pause before merge</p>
                <p className="text-xs leading-[1.45] text-muted">
                  Require operator approval before the reviewed change lands
                </p>
              </div>
            </div>
            <div className="flex items-start gap-3">
              <Switch
                aria-label="Fail-fast review"
                checked={draft.review.fail_fast ?? false}
                onChange={(checked) => update({ review: { ...draft.review, fail_fast: checked } })}
              />
              <div>
                <p className="text-sm font-medium">Fail-fast review</p>
                <p className="text-xs leading-[1.45] text-muted">
                  End a review round on its first changes-requested verdict and cancel the seats still reviewing
                </p>
              </div>
            </div>
            <div className="flex items-start gap-3">
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
                <p className="text-xs leading-[1.45] text-muted">
                  Refuse a review seat claim that repeats the name another seat of the round reports. Names are
                  self-reported and never verified. New tasks only.
                </p>
              </div>
            </div>
          </div>
          <div className="flex flex-wrap gap-4 border-t border-border pt-4 [&_input]:w-28">
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
        <CardHeader>
          <CardTitle>Stage timeouts</CardTitle>
          <span className="text-xs text-faint">Each value is a positive duration such as 30m or 4h.</span>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-4 [&_input]:w-28">
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
        </CardContent>
      </Card>
    </div>
  )
}
