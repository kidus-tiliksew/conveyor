import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { Check, Package, Paperclip, Plus, X } from 'lucide-react'
import { useRef, useState } from 'react'
import {
  createTask,
  fetchCrate,
  fetchOpenTasks,
  fetchRequirements,
  fetchSystemDesigns,
  TaskIntakeError,
} from '../../lib/api'
import { planStateLabels } from '../../lib/contracts'
import { crateQueryKey } from '../../lib/query-keys'
import type { CrateDetail, PlanState } from '../../lib/types'
import { cn, formatBytes } from '../../lib/utils'
import { useWorkspace, useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import type { BoardSearch } from '../board/board-search'
import { Button } from '../ui/button'
import { Input, Select, Textarea } from '../ui/input'
import { MarkdownEditor } from '../ui/markdown-editor'
import { Sheet } from '../ui/sheet'
import { EmptyRepositoryNotice } from '../workspace/empty-repository-notice'
import { type ContextGroup, TaskContextPicker } from './task-context-picker'

const descriptionScaffold = 'Enter a description ...'

const DEPENDENCY_RESULT_LIMIT = 20

// The two plan states intake accepts by name (req-task-plan-state AC-2.1).
type IntakePlanState = Extract<PlanState, 'needed' | 'not_required'>
const intakePlanStates: readonly IntakePlanState[] = ['needed', 'not_required']

// A plan's base commit is a full commit ID: 40 lowercase hexadecimal
// characters (req-task-plan-state AC-2.5; component-plan-state invariant 2).
const fullCommitID = /^[0-9a-f]{40}$/

// Task intake: the dashboard is one source among github/cli/cron.
// A sheet instead of a page so intake happens over the board, with room for
// the rich triage context that saves bounce rounds downstream — structured
// description, base branch, policy overrides, and artifact attachments.
export function TaskCreateSheet() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: workspace } = useWorkspace()
  const { workspace: workspaceId } = useWorkspaceSelection()
  const repos = workspace?.repos ?? []
  // Intake opened from a crate's page names that crate (component-crates
  // invariant 17; req-crates AC-3.2).
  const { crate: crateId } = useSearch({ strict: false }) as BoardSearch
  const crate = useQuery({
    queryKey: crateQueryKey(workspaceId, crateId ?? ''),
    queryFn: () => fetchCrate(workspaceId, crateId ?? ''),
    enabled: Boolean(workspaceId && crateId),
  })
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const tasks = useQuery({
    queryKey: ['tasks', workspaceId, 'dependency-candidates'],
    queryFn: () => fetchOpenTasks(workspaceId),
    enabled: advancedOpen && Boolean(workspaceId),
  })
  const requirements = useQuery({
    queryKey: ['requirements', workspaceId],
    queryFn: () => fetchRequirements(workspaceId),
    enabled: Boolean(workspaceId),
    staleTime: 60_000,
  })
  const designs = useQuery({
    queryKey: ['system-designs', workspaceId],
    queryFn: () => fetchSystemDesigns(workspaceId),
    enabled: Boolean(workspaceId),
    staleTime: 60_000,
  })

  const [body, setBody] = useState('')
  const [repo, setRepo] = useState('')
  const [baseBranch, setBaseBranch] = useState('')
  // Empty until the operator picks a plan state: an unchanged select sends
  // none, so the server applies the workspace default (component-web-task-surfaces
  // invariant 63).
  const [chosenPlanState, setChosenPlanState] = useState<IntakePlanState | ''>('')
  // What the select showed when the operator first changed it: the served
  // default, or '' while the workspace read had stated none. A choice is sent
  // whenever it differs from that value, so a state picked before the default
  // arrived is kept even if the default later matches it.
  const [openedOn, setOpenedOn] = useState<IntakePlanState | '' | null>(null)
  const [plan, setPlan] = useState('')
  const [planBaseCommit, setPlanBaseCommit] = useState('')
  const [approvePlan, setApprovePlan] = useState(false)
  const canOperateGates = useWorkspaceCapability('operate_gates')
  const [mergeGate, setMergeGate] = useState<'default' | 'on' | 'off'>('default')
  const [dependsOn, setDependsOn] = useState<string[]>([])
  const [requirementIds, setRequirementIds] = useState<string[]>([])
  const [systemDesignIds, setSystemDesignIds] = useState<string[]>([])
  const [dependencySearch, setDependencySearch] = useState('')
  const [files, setFiles] = useState<File[]>([])
  const fileInput = useRef<HTMLInputElement>(null)
  const intakeKey = useRef(crypto.randomUUID())
  const repoName = repo || repos[0]?.name || ''
  const defaultPlanState = workspace?.default_plan_state
  const shownPlanState = chosenPlanState || defaultPlanState || ''
  // A plan sends no plan state (AC-2.6). The select sends one only when the
  // operator moved it off the value it opened on.
  const hasPlan = plan.trim() !== ''
  const sentPlanState =
    !hasPlan && chosenPlanState && chosenPlanState !== (openedOn ?? defaultPlanState ?? '')
      ? chosenPlanState
      : undefined
  const baseCommit = planBaseCommit.trim()
  const baseCommitError =
    hasPlan && baseCommit && !fullCommitID.test(baseCommit)
      ? 'Base commit must be a full commit ID of 40 lowercase hexadecimal characters.'
      : ''
  const approving = hasPlan && canOperateGates && approvePlan
  const resultingPlanState: PlanState | undefined = hasPlan
    ? approving
      ? 'completed'
      : 'awaiting_approval'
    : chosenPlanState || defaultPlanState
  // Intake is the Board's `create` parameter, so closing drops it and leaves
  // whichever view the Board was showing.
  // Opened from a crate, closing returns to that crate's page.
  const close = () =>
    crateId
      ? void navigate({ to: '/w/$workspaceId/crates/$crateId', params: { workspaceId, crateId } })
      : void navigate({
          to: '/w/$workspaceId/',
          params: { workspaceId },
          search: ({ create: _create, crate: _crate, ...prev }) => prev,
        })
  // A closed crate takes no new task (req-crates AC-2.1), so the form sends no
  // request that names it. "Not in a crate" creates the task without it.
  const crateClosed = Boolean(crateId) && crate.data?.state === 'closed'
  const leaveCrate = () =>
    void navigate({ to: '/w/$workspaceId/', params: { workspaceId }, search: ({ crate: _crate, ...prev }) => prev })

  const mutation = useMutation({
    mutationFn: async () => {
      const task = await createTask(
        workspaceId,
        {
          body: body.trim(),
          repo: repoName,
          ...(sentPlanState ? { plan_state: sentPlanState } : {}),
          ...(hasPlan ? { plan } : {}),
          ...(hasPlan && baseCommit ? { plan_base_commit: baseCommit } : {}),
          ...(approving ? { plan_approved: true } : {}),
          ...(mergeGate !== 'default' ? { merge_approval: mergeGate === 'on' } : {}),
          ...(baseBranch.trim() ? { base_branch: baseBranch.trim() } : {}),
          ...(dependsOn.length ? { depends_on: dependsOn } : {}),
          ...(requirementIds.length ? { requirement_ids: requirementIds } : {}),
          ...(systemDesignIds.length ? { system_design_ids: systemDesignIds } : {}),
          ...(crateId ? { crate_id: crateId } : {}),
        },
        files,
        intakeKey.current,
      )
      return task
    },
    onSuccess: (task) => {
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      if (crateId) void queryClient.invalidateQueries({ queryKey: ['crates', workspaceId] })
      // The new task opens in the Board's sheet over the view intake came from.
      void navigate({
        to: '/w/$workspaceId/tasks/$taskId',
        params: { workspaceId, taskId: task.id },
        search: ({ create: _create, crate: _crate, ...prev }) => prev,
      })
    },
    onError: (error) => {
      if (error instanceof TaskIntakeError && error.code === 'invalid_dependencies') setAdvancedOpen(true)
    },
  })

  const addFiles = (picked: FileList | null) => {
    // Snapshot immediately: the input is cleared right after this handler,
    // which empties the live FileList before React runs the state updater.
    const list = picked ? Array.from(picked) : []
    if (list.length === 0) return
    setFiles((current) => {
      const seen = new Set(current.map((f) => `${f.name}:${f.size}`))
      return [...current, ...list.filter((f) => !seen.has(`${f.name}:${f.size}`))]
    })
  }
  // Only confirmed documents are attachable, and the two tiers stay separate
  // groups so the create payload still carries distinct ID arrays. From a
  // crate, its declared requirements lead as their own group, and none is
  // selected until the operator picks it (req-crates AC-3.1, AC-3.2).
  const declared = crate.data?.requirements ?? []
  const declaredIds = new Set(declared.map((item) => item.id))
  const contextGroups: ContextGroup[] = [
    ...(declared.length > 0
      ? [
          {
            key: 'crate-requirements',
            label: `Declared by ${crate.data?.name}`,
            options: declared.map((item) => ({ id: item.id, title: item.title })),
            selected: requirementIds.filter((id) => declaredIds.has(id)),
            onChange: (ids: string[]) =>
              setRequirementIds([...requirementIds.filter((id) => !declaredIds.has(id)), ...ids]),
          },
        ]
      : []),
    {
      key: 'requirements',
      label: declared.length > 0 ? 'Other requirements' : 'Requirements',
      options: (requirements.data ?? [])
        .filter((item) => item.current_version != null && !declaredIds.has(item.requirement.id))
        .map((item) => ({ id: item.requirement.id, title: item.requirement.title })),
      selected: requirementIds.filter((id) => !declaredIds.has(id)),
      onChange: (ids: string[]) => setRequirementIds([...requirementIds.filter((id) => declaredIds.has(id)), ...ids]),
    },
    {
      key: 'system-design',
      label: 'System Design',
      options: (designs.data ?? [])
        .filter((item) => item.current_version != null)
        .map((item) => ({ id: item.document.id, title: item.document.title })),
      selected: systemDesignIds,
      onChange: setSystemDesignIds,
    },
  ]
  const matchingDependencyOptions = (tasks.data ?? []).filter((task) => {
    if (task.state === 'merged' || task.state === 'closed') return false
    const query = dependencySearch.trim().toLowerCase()
    return !query || task.id.toLowerCase().includes(query) || task.title.toLowerCase().includes(query)
  })
  const dependencyOptions = matchingDependencyOptions.slice(0, DEPENDENCY_RESULT_LIMIT)
  const dependencyError =
    mutation.error instanceof TaskIntakeError && mutation.error.code === 'invalid_dependencies'
      ? mutation.error.message
      : ''
  const dependencyStatus = tasks.isPending
    ? 'Loading dependency candidates…'
    : tasks.error != null
      ? 'Could not load dependency candidates. Check your access and try again.'
      : matchingDependencyOptions.length === 0
        ? 'No matching open tasks.'
        : matchingDependencyOptions.length > DEPENDENCY_RESULT_LIMIT
          ? `Showing ${DEPENDENCY_RESULT_LIMIT} of ${matchingDependencyOptions.length} matching open tasks. Narrow your search to see more.`
          : `${matchingDependencyOptions.length} matching open ${matchingDependencyOptions.length === 1 ? 'task' : 'tasks'}.`

  return (
    <Sheet onClose={close} label="New task">
      <header className="flex shrink-0 items-center justify-between border-b border-border px-5 py-3">
        <div>
          <h2 className="text-base font-semibold tracking-tight">New task</h2>
          <p className="text-xs text-muted">Queue a unit of intended change. Triage classifies and routes it.</p>
        </div>
        <Button variant="ghost" size="icon" aria-label="Close" onClick={close}>
          <X />
        </Button>
      </header>

      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-5 py-4">
        <EmptyRepositoryNotice />
        {crateId && (
          <CrateIntakeNotice
            crateId={crateId}
            crate={crate.data}
            loading={crate.isPending}
            failed={crate.error != null}
            selected={requirementIds}
            onToggle={(id) =>
              setRequirementIds((current) =>
                current.includes(id) ? current.filter((value) => value !== id) : [...current, id],
              )
            }
            onLeave={leaveCrate}
          />
        )}
        <Field
          label="Description"
          hint="AI generates the task title from this context, which also becomes the triage and plan prompt."
        >
          <MarkdownEditor value={body} onChange={setBody} placeholder={descriptionScaffold} />
        </Field>

        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Repository">
            <Select aria-label="Repository" value={repoName} onChange={(event) => setRepo(event.target.value)}>
              {repos.map((entry) => (
                <option key={entry.name} value={entry.name}>
                  {entry.name}
                </option>
              ))}
              {repos.length === 0 && <option value="">No repos configured</option>}
            </Select>
          </Field>
          <Field
            label="Base branch"
            hint={`Optional — defaults to ${repos.find((r) => r.name === repoName)?.base ?? 'the repo base'}.`}
          >
            <Input
              value={baseBranch}
              onChange={(event) => setBaseBranch(event.target.value)}
              placeholder={repos.find((r) => r.name === repoName)?.base ?? 'main'}
              className="font-mono"
            />
          </Field>
        </div>

        <TaskContextPicker
          label="Context"
          hint="Optional — attach the confirmed requirements this work should satisfy and the system design that governs it."
          loading={requirements.isPending || designs.isPending}
          groups={contextGroups}
        />

        <details
          open={advancedOpen}
          onToggle={(event) => setAdvancedOpen(event.currentTarget.open)}
          className="rounded-md border border-border p-3"
        >
          <summary className="cursor-pointer text-sm font-medium">Advanced options</summary>
          <div className="mt-3">
            <Field label="Depends on" hint="Optional — this task stays queued until every selected task is merged.">
              <Input
                aria-label="Search dependency tasks"
                aria-controls="dependency-results"
                aria-describedby="dependency-results-status"
                value={dependencySearch}
                onChange={(event) => setDependencySearch(event.target.value)}
                placeholder="Search open tasks by title or ID"
              />
              {dependsOn.length > 0 && (
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {dependsOn.map((id) => {
                    const selected = (tasks.data ?? []).find((task) => task.id === id)
                    return (
                      <button
                        key={id}
                        type="button"
                        aria-label={`Remove dependency ${selected?.title ?? id}`}
                        onClick={() => setDependsOn((current) => current.filter((value) => value !== id))}
                        className="inline-flex items-center gap-1 rounded-full border border-border bg-surface px-2 py-1 text-xs"
                      >
                        <span className="max-w-48 truncate">{selected?.title ?? id}</span>
                        <span className="font-mono text-faint">{id}</span>
                        <X className="size-3" />
                      </button>
                    )
                  })}
                </div>
              )}
              <p id="dependency-results-status" aria-live="polite" className="mt-2 text-xs text-faint">
                {dependencyStatus}
              </p>
              <div id="dependency-results" className="mt-1 max-h-40 space-y-1 overflow-y-auto">
                {!tasks.isPending &&
                  tasks.error == null &&
                  dependencyOptions.map((task) => (
                    <label
                      key={task.id}
                      className="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-xs hover:bg-surface"
                    >
                      <input
                        type="checkbox"
                        checked={dependsOn.includes(task.id)}
                        onChange={(event) =>
                          setDependsOn((current) =>
                            event.target.checked ? [...current, task.id] : current.filter((value) => value !== task.id),
                          )
                        }
                      />
                      <span className="min-w-0 flex-1 truncate">{task.title}</span>
                      <span className="shrink-0 text-faint">{task.repo}</span>
                      <span className="shrink-0 font-mono text-faint">{task.id}</span>
                    </label>
                  ))}
              </div>
              {dependencyError && <p className="mt-2 text-xs text-failure">{dependencyError}</p>}
            </Field>
          </div>
        </details>

        {/* A task carries no hold or execution reservation (DEC-125(3)), and a
            filed task waits in the queue for an agent session with no
            availability warning (req-task-lifecycle-and-queue AC-9.4). */}

        {/* Intake plan fields (component-web-task-surfaces invariant 63;
            req-task-plan-state AC-2.1 to AC-2.6). The server validates the
            plan, and a refusal shows its message with nothing recorded. */}
        <section aria-label="Plan" className="space-y-3">
          <Field
            label="Plan (optional)"
            hint="A Markdown execution plan filed with the task. It becomes plan version 1 and waits for approval."
          >
            <Textarea
              aria-label="Plan"
              value={plan}
              onChange={(event) => setPlan(event.target.value)}
              placeholder="## Approach …"
              className="min-h-24 font-mono text-xs"
            />
          </Field>
          <div className="grid gap-4 md:grid-cols-2">
            <Field
              label="Plan state"
              hint={hasPlan ? 'A filed plan sets the plan state.' : 'Optional — new tasks take the workspace default.'}
            >
              <Select
                aria-label="Plan state"
                value={shownPlanState}
                disabled={hasPlan}
                onChange={(event) => {
                  if (openedOn === null) setOpenedOn(defaultPlanState ?? '')
                  setChosenPlanState(event.target.value as IntakePlanState | '')
                }}
              >
                {/* Until the read states the default, choosing it again sends
                    no plan_state. */}
                {!defaultPlanState && <option value="">Workspace default</option>}
                {intakePlanStates.map((state) => (
                  <option key={state} value={state}>
                    {planStateLabels[state]}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Base commit" hint="Optional — the full commit ID the plan was read against.">
              <Input
                aria-label="Base commit"
                value={planBaseCommit}
                disabled={!hasPlan}
                aria-invalid={baseCommitError ? true : undefined}
                onChange={(event) => setPlanBaseCommit(event.target.value)}
                placeholder="40-character commit ID"
                className="font-mono"
              />
              {baseCommitError && (
                <span role="alert" className="mt-1 block text-xs text-failure">
                  {baseCommitError}
                </span>
              )}
            </Field>
          </div>
          {hasPlan && canOperateGates && (
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={approvePlan} onChange={(event) => setApprovePlan(event.target.checked)} />
              Approve this plan
            </label>
          )}
        </section>

        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Merge approval gate">
            <Select value={mergeGate} onChange={(event) => setMergeGate(event.target.value as typeof mergeGate)}>
              <option value="default">Workspace default</option>
              <option value="on">Human approval on</option>
              <option value="off">Auto-merge on green</option>
            </Select>
          </Field>
        </div>

        <Field
          label="Attachments"
          hint="Designs, plans, logs — uploaded as task artifacts for the triage and plan agents."
        >
          <input
            ref={fileInput}
            type="file"
            multiple
            className="hidden"
            onChange={(event) => {
              addFiles(event.target.files)
              event.target.value = ''
            }}
          />
          <Button variant="secondary" size="sm" onClick={() => fileInput.current?.click()}>
            <Paperclip />
            Attach files
          </Button>
          {files.length > 0 && (
            <ul className="mt-2 space-y-1">
              {files.map((file) => (
                <li
                  key={`${file.name}:${file.size}`}
                  className="flex items-center gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs"
                >
                  <Paperclip className="size-3 shrink-0 text-faint" />
                  <span className="min-w-0 flex-1 truncate">{file.name}</span>
                  <span className="shrink-0 font-mono text-[11px] text-faint">{formatBytes(file.size)}</span>
                  <button
                    type="button"
                    aria-label={`Remove ${file.name}`}
                    className="shrink-0 text-faint hover:text-foreground"
                    onClick={() => setFiles((current) => current.filter((f) => f !== file))}
                  >
                    <X className="size-3.5" />
                  </button>
                </li>
              ))}
            </ul>
          )}
        </Field>

        {mutation.error != null && !dependencyError && <p className="text-sm text-failure">{String(mutation.error)}</p>}
      </div>

      <footer className="shrink-0 space-y-2 border-t border-border px-5 py-3">
        {/* The plan state the task will take, stated above Create
            (component-web-task-surfaces invariant 63). */}
        <p className="text-xs text-muted" data-resulting-plan-state={resultingPlanState ?? 'default'}>
          {resultingPlanState
            ? `This task will start at: ${planStateLabels[resultingPlanState]}.`
            : 'This task will start at the workspace default plan state.'}
        </p>
        <div className="flex items-center justify-between gap-3">
          <p className="text-xs text-faint">Dispatches after every attachment is stored.</p>
          <Button
            disabled={
              !body.trim() ||
              !repoName ||
              repos.length === 0 ||
              mutation.isPending ||
              baseCommitError !== '' ||
              crateClosed
            }
            onClick={() => mutation.mutate()}
          >
            {mutation.isPending ? 'Creating…' : 'Create task'}
          </Button>
        </div>
      </footer>
    </Sheet>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div>
      <span className="mb-1 block text-xs font-medium text-muted">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-faint">{hint}</span>}
    </div>
  )
}

// Names the crate a new task joins, and lists the requirements the crate
// declares so the operator can pick the ones this task serves. None starts
// picked (req-crates AC-3.2).
function CrateIntakeNotice({
  crateId,
  crate,
  loading,
  failed,
  selected,
  onToggle,
  onLeave,
}: {
  crateId: string
  crate?: CrateDetail
  loading: boolean
  failed: boolean
  selected: string[]
  onToggle: (id: string) => void
  onLeave: () => void
}) {
  const declared = crate?.requirements ?? []
  return (
    <section
      aria-label="Crate"
      data-intake-crate={crateId}
      className="rounded-lg border border-primary/25 bg-primary-soft/40 px-3.5 py-3"
    >
      <div className="flex items-start gap-2.5">
        <span className="mt-0.5 grid size-7 shrink-0 place-items-center rounded-md bg-primary-soft text-primary">
          <Package aria-hidden className="size-3.5" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-xs text-muted">This task joins the crate</p>
          <p className="truncate text-sm font-medium" data-intake-crate-name>
            {crate?.name ?? (loading ? 'Loading…' : crateId)}
          </p>
        </div>
        <Button
          variant="ghost"
          size="sm"
          className="-mr-1.5 shrink-0"
          aria-label="Create this task outside the crate"
          title="Create this task outside the crate"
          onClick={onLeave}
        >
          <X />
          <span className="max-sm:sr-only">Not in a crate</span>
        </Button>
      </div>
      {failed && (
        <p className="mt-2 text-xs text-failure">This crate did not load. The task can still join it by ID.</p>
      )}
      {crate?.state === 'closed' && (
        <p className="mt-2 text-xs text-failure">
          This crate is closed, so a new task cannot join it. Choose "Not in a crate" to create the task without it.
        </p>
      )}
      {declared.length > 0 && (
        <div className="mt-3 border-t border-primary/15 pt-2.5">
          <p className="text-xs text-muted">
            Pick the crate's requirements this task serves. None is attached unless you pick it.
          </p>
          <ul aria-label="Requirements the crate declares" className="mt-2 flex flex-wrap gap-1.5">
            {declared.map((item) => {
              const picked = selected.includes(item.id)
              return (
                <li key={item.id} className="min-w-0 max-w-full">
                  <button
                    type="button"
                    aria-pressed={picked}
                    title={item.id}
                    onClick={() => onToggle(item.id)}
                    className={cn(
                      'inline-flex max-w-full items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors',
                      picked
                        ? 'border-primary bg-primary text-primary-foreground'
                        : 'border-border bg-card text-foreground hover:border-edge',
                    )}
                  >
                    {picked ? (
                      <Check aria-hidden className="size-3 shrink-0" />
                    ) : (
                      <Plus aria-hidden className="size-3 shrink-0 text-faint" />
                    )}
                    <span className="truncate">{item.title}</span>
                    {item.task_count === 0 && (
                      <span className={cn('shrink-0', picked ? 'opacity-80' : 'text-attention')}>· uncovered</span>
                    )}
                  </button>
                </li>
              )
            })}
          </ul>
        </div>
      )}
    </section>
  )
}
