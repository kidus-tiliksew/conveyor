import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { Paperclip, X } from 'lucide-react'
import { createTask, fetchWorkers } from '../../lib/api'
import type { TaskMode } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useOperatorToken, useWorkspace } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Select, Textarea } from '../ui/input'
import { Sheet } from '../ui/sheet'

const modes: Array<{ mode: TaskMode; hint: string }> = [
  { mode: 'auto', hint: 'An enrolled healthy worker may claim and run this task.' },
  { mode: 'manual', hint: 'Only an operator-attached MCP agent claims work.' },
]

const descriptionScaffold = `Context — where this lives, links to prior work…

What should change —

Constraints & non-goals —

Acceptance ideas — how we'd know it works…`

// Task intake (spec §9): the dashboard is one source among github/cli/cron.
// A sheet instead of a page so intake happens over the board, with room for
// the rich triage context that saves bounce rounds downstream — structured
// description, base branch, execution policy, and artifact attachments.
export function TaskCreateSheet() {
  const token = useOperatorToken()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: workspace } = useWorkspace()
  const repos = workspace?.repos ?? []
  const workerHealth = useQuery({ queryKey: ['workers', token, workspace?.workspace], queryFn: () => fetchWorkers(token), enabled: Boolean(token && workspace?.workspace), refetchInterval: 5000 })
  const autoAvailable = workerHealth.data?.auto_available === true

  const [body, setBody] = useState('')
  const [repo, setRepo] = useState('')
  const [baseBranch, setBaseBranch] = useState('')
  const [mode, setMode] = useState<TaskMode | ''>('')
  const [specGate, setSpecGate] = useState<'default' | 'on' | 'off'>('default')
  const [mergeGate, setMergeGate] = useState<'default' | 'on' | 'off'>('default')
  const [files, setFiles] = useState<File[]>([])
  const fileInput = useRef<HTMLInputElement>(null)
  const intakeKey = useRef(crypto.randomUUID())
  const repoName = repo || repos[0]?.name || ''
  const close = () => void navigate({ to: '/' })

  const mutation = useMutation({
    mutationFn: async () => {
      const task = await createTask(token, {
        body: body.trim(),
        repo: repoName,
        ...(mode ? { mode } : {}),
        ...(specGate !== 'default' ? { spec_approval: specGate === 'on' } : {}),
        ...(mergeGate !== 'default' ? { merge_approval: mergeGate === 'on' } : {}),
        ...(baseBranch.trim() ? { base_branch: baseBranch.trim() } : {}),
      }, files, intakeKey.current)
      return task
    },
    onSuccess: (task) => {
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void navigate({ to: '/tasks/$taskId', params: { taskId: task.id } })
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
          <Textarea
            value={body}
            onChange={(event) => setBody(event.target.value)}
            placeholder={descriptionScaffold}
            className="min-h-56 leading-6"
          />
        </Field>

        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Repository">
            <Select value={repoName} onChange={(event) => setRepo(event.target.value)}>
              {repos.map((entry) => (
                <option key={entry.name} value={entry.name}>
                  {entry.name}
                </option>
              ))}
              {repos.length === 0 && <option value="">No repos configured</option>}
            </Select>
          </Field>
          <Field label="Base branch" hint={`Optional — defaults to ${repos.find((r) => r.name === repoName)?.base ?? 'the repo base'}.`}>
            <Input value={baseBranch} onChange={(event) => setBaseBranch(event.target.value)} placeholder={repos.find((r) => r.name === repoName)?.base ?? 'main'} className="font-mono" />
          </Field>
        </div>

        <Field label="Execution mode" hint="Leave Workspace default selected to use the persisted workspace policy.">
          <div className="grid gap-2 sm:grid-cols-3" role="radiogroup" aria-label="Execution mode">
            <button type="button" role="radio" aria-checked={mode === ''} onClick={() => setMode('')} className={cn('rounded-md border p-3 text-left transition-colors', mode === '' ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge')}><span className="text-sm font-semibold">Workspace default</span><span className="mt-0.5 block text-xs leading-5 text-muted">Falls back to Manual if Auto is unhealthy.</span></button>
            {modes.map((entry) => (
              <button
                key={entry.mode}
                type="button"
                role="radio"
                aria-checked={mode === entry.mode}
                disabled={entry.mode === 'auto' && !autoAvailable}
                onClick={() => setMode(entry.mode)}
                className={cn(
                  'rounded-md border p-3 text-left transition-colors',
                  entry.mode === 'auto' && !autoAvailable && 'cursor-not-allowed opacity-50',
                  mode === entry.mode ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge',
                )}
              >
                <span className={cn('font-mono text-sm font-semibold capitalize', mode === entry.mode ? 'text-primary' : 'text-foreground')}>
                  {entry.mode}
                </span>
                <span className="mt-0.5 block text-xs leading-5 text-muted">{entry.hint}</span>
              </button>
            ))}
          </div>
          {!autoAvailable && <p className="mt-2 text-xs text-muted">Auto is unavailable: {workerHealth.data?.auto_unavailable_reason ?? 'waiting for a live worker with healthy routed harnesses'}</p>}
        </Field>

        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Spec approval gate"><Select value={specGate} onChange={(event) => setSpecGate(event.target.value as typeof specGate)}><option value="default">Workspace default</option><option value="on">Human approval on</option><option value="off">Auto-approve off</option></Select></Field>
          <Field label="Merge approval gate"><Select value={mergeGate} onChange={(event) => setMergeGate(event.target.value as typeof mergeGate)}><option value="default">Workspace default</option><option value="on">Human approval on</option><option value="off">Auto-merge on green</option></Select></Field>
        </div>

        <Field label="Attachments" hint="Designs, specs, logs — uploaded as task artifacts for the triage and spec agents.">
          <input ref={fileInput} type="file" multiple className="hidden" onChange={(event) => { addFiles(event.target.files); event.target.value = '' }} />
          <Button variant="secondary" size="sm" onClick={() => fileInput.current?.click()}>
            <Paperclip />
            Attach files
          </Button>
          {files.length > 0 && (
            <ul className="mt-2 space-y-1">
              {files.map((file) => (
                <li key={`${file.name}:${file.size}`} className="flex items-center gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs">
                  <Paperclip className="size-3 shrink-0 text-faint" />
                  <span className="min-w-0 flex-1 truncate">{file.name}</span>
                  <span className="shrink-0 font-mono text-[11px] text-faint">{formatSize(file.size)}</span>
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

        {mutation.error != null && <p className="text-sm text-failure">{String(mutation.error)}</p>}
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

function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
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
