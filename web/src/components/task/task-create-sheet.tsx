import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { Paperclip, X } from 'lucide-react'
import {
  createTask,
  fetchRequirements,
  fetchSystemDesigns,
  fetchTasks,
  fetchWorkers,
  TaskIntakeError,
} from '../../lib/api'
import { formatBytes } from '../../lib/utils'
import { useOperatorToken, useWorkspace } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Select } from '../ui/input'
import { MarkdownEditor } from '../ui/markdown-editor'
import { Sheet } from '../ui/sheet'
import { Switch } from '../ui/switch'
import { type ContextGroup, TaskContextPicker } from './task-context-picker'

const descriptionScaffold = 'Enter a description ...'

const DEPENDENCY_RESULT_LIMIT = 20

// Task intake: the dashboard is one source among github/cli/cron.
// A sheet instead of a page so intake happens over the board, with room for
// the rich triage context that saves bounce rounds downstream — structured
// description, base branch, execution policy, and artifact attachments.
export function TaskCreateSheet({
  onClose,
  onCreated,
}: {
  onClose?: () => void
  onCreated?: (taskId: string) => void
} = {}) {
  const token = useOperatorToken()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: workspace } = useWorkspace()
  const repos = workspace?.repos ?? []
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const workerHealth = useQuery({
    queryKey: ['workers', token, workspace?.workspace],
    queryFn: () => fetchWorkers(token),
    enabled: Boolean(token && workspace?.workspace),
    refetchInterval: 5000,
  })
  const setups = workspace?.setups ?? []
  const tasks = useQuery({
    queryKey: ['tasks', workspace?.workspace, 'dependency-candidates'],
    queryFn: fetchTasks,
    enabled: advancedOpen && Boolean(workspace?.workspace),
  })
  const requirements = useQuery({
    queryKey: ['requirements', workspace?.workspace, 'task-intake'],
    queryFn: fetchRequirements,
    enabled: Boolean(token && workspace?.workspace),
  })
  const designs = useQuery({
    queryKey: ['system-designs', workspace?.workspace, 'task-intake'],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(token && workspace?.workspace),
  })

  const [body, setBody] = useState('')
  const [repo, setRepo] = useState('')
  const [baseBranch, setBaseBranch] = useState('')
  const [hold, setHold] = useState(false)
  const [setup, setSetup] = useState('')
  const [specGate, setSpecGate] = useState<'default' | 'on' | 'off'>('default')
  const [mergeGate, setMergeGate] = useState<'default' | 'on' | 'off'>('default')
  const [dependsOn, setDependsOn] = useState<string[]>([])
  const [requirementIds, setRequirementIds] = useState<string[]>([])
  const [systemDesignIds, setSystemDesignIds] = useState<string[]>([])
  const [dependencySearch, setDependencySearch] = useState('')
  const [files, setFiles] = useState<File[]>([])
  const fileInput = useRef<HTMLInputElement>(null)
  const intakeKey = useRef(crypto.randomUUID())
  const repoName = repo || repos[0]?.name || ''
  const setupName = setup || workspace?.default_setup || setups[0]?.name || ''
  const selectedSetup = setups.find((entry) => entry.name === setupName)
  const setupHealth = workerHealth.data?.setup_serviceability?.[setupName]
  const autoAvailable = setupHealth?.auto_available ?? workerHealth.data?.auto_available === true
  // Hosts may keep their own surface mounted while sharing the complete intake
  // composition. The Tasks list remains the default for legacy `/new` links
  // and its own create action.
  const close = onClose ?? (() => void navigate({ to: '/tasks', search: {} }))

  const mutation = useMutation({
    mutationFn: async () => {
      const task = await createTask(
        token,
        {
          body: body.trim(),
          repo: repoName,
          ...(setupName ? { setup: setupName } : {}),
          ...(hold ? { hold } : {}),
          ...(specGate !== 'default' ? { spec_approval: specGate === 'on' } : {}),
          ...(mergeGate !== 'default' ? { merge_approval: mergeGate === 'on' } : {}),
          ...(baseBranch.trim() ? { base_branch: baseBranch.trim() } : {}),
          ...(dependsOn.length ? { depends_on: dependsOn } : {}),
          ...(requirementIds.length ? { requirement_ids: requirementIds } : {}),
          ...(systemDesignIds.length ? { system_design_ids: systemDesignIds } : {}),
        },
        files,
        intakeKey.current,
      )
      return task
    },
    onSuccess: (task) => {
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void queryClient.invalidateQueries({ queryKey: ['task-operations'] })
      if (onCreated) {
        onCreated(task.id)
      } else {
        // The default host opens the new task in the list's own panel.
        void navigate({ to: '/tasks', search: { task: task.id } })
      }
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
  // groups so the create payload still carries distinct ID arrays.
  const contextGroups: ContextGroup[] = [
    {
      key: 'requirements',
      label: 'Requirements',
      options: (requirements.data ?? [])
        .filter((item) => item.current_version != null)
        .map((item) => ({ id: item.requirement.id, title: item.requirement.title })),
      selected: requirementIds,
      onChange: setRequirementIds,
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
        <Field
          label="Description"
          hint="AI generates the task title from this context, which also becomes the triage and spec prompt."
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

        <Field
          label="Execution setup"
          hint="Choose a prepared execution contract; its details are captured on this task."
        >
          <Select aria-label="Execution setup" value={setupName} onChange={(event) => setSetup(event.target.value)}>
            {setups.map((entry) => (
              <option key={entry.name} value={entry.name}>
                {entry.name}
                {entry.name === workspace?.default_setup ? ' (default)' : ''}
              </option>
            ))}
          </Select>
          {selectedSetup && (
            <details className="mt-2 text-xs text-muted">
              <summary className="cursor-pointer">Composition</summary>
              <p className="mt-1 font-mono">
                Implement: {selectedSetup.execution_settings.implementation.harness} ·{' '}
                {selectedSetup.execution_settings.implementation.model || 'harness default'}
              </p>
              <p className="font-mono">
                Review:{' '}
                {selectedSetup.review.seats
                  .map(
                    (seat) =>
                      `${seat.harness || selectedSetup.execution_settings.review.fallback_harness || 'in-process'} / ${seat.model}`,
                  )
                  .join(', ')}
              </p>
            </details>
          )}
        </Field>

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

        {/* Per-task hold: reservation from the worker, not a mode. */}
        <Field label="Hold">
          <div className="flex items-center gap-3 rounded-md border border-border p-3">
            <Switch aria-label="Hold for hands-on work" checked={hold} onChange={setHold} />
            <div>
              <p className="text-sm font-medium">Hold for hands-on work</p>
              <p className="text-xs leading-5 text-muted">
                Your worker won't claim this task; you attach an agent and claim it yourself. You can release the hold
                at any time.
              </p>
            </div>
          </div>
          {!autoAvailable && !hold && (
            <p className="mt-2 text-xs text-attention">
              No worker can run {setupName || 'this setup'} right now —{' '}
              {setupHealth?.auto_unavailable_reason ??
                workerHealth.data?.auto_unavailable_reason ??
                'waiting for a live worker with healthy routed harnesses'}
              . The task will queue until a worker is available or you claim it manually.
            </p>
          )}
        </Field>

        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Plan approval gate">
            <Select value={specGate} onChange={(event) => setSpecGate(event.target.value as typeof specGate)}>
              <option value="default">Workspace default</option>
              <option value="on">Human approval on</option>
              <option value="off">Auto-approve off</option>
            </Select>
          </Field>
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
          hint="Designs, specs, logs — uploaded as task artifacts for the triage and spec agents."
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

      <footer className="flex shrink-0 items-center justify-between gap-3 border-t border-border px-5 py-3">
        <p className="text-xs text-faint">
          {token ? (
            'Dispatches after every attachment is stored.'
          ) : (
            <>
              Requires the operator token — set it in{' '}
              <Link to="/settings" className="text-primary hover:underline">
                Settings
              </Link>
              .
            </>
          )}
        </p>
        <Button disabled={!token || !body.trim() || !repoName || mutation.isPending} onClick={() => mutation.mutate()}>
          {mutation.isPending ? 'Creating…' : 'Create task'}
        </Button>
      </footer>
    </Sheet>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div>
      <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-faint">{hint}</span>}
    </div>
  )
}
