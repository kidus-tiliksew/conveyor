import { useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { Paperclip, X } from 'lucide-react'
import { createTask, uploadArtifact } from '../../lib/api'
import type { EscalationLevel, Task } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useOperatorToken, useWorkspace } from '../app-shell'
import { Button } from '../ui/button'
import { Input, Select, Textarea } from '../ui/input'
import { Sheet } from '../ui/sheet'

const levels: Array<{ level: EscalationLevel; hint: string }> = [
  { level: 'L0', hint: 'Fully automatic — auto-merge on green verification' },
  { level: 'L1', hint: 'Automatic with a one-click human approve' },
  { level: 'L2', hint: 'Human reviews the spec, then the PR' },
  { level: 'L3', hint: 'Human pairs interactively' },
]

const descriptionScaffold = `Context — where this lives, links to prior work…

What should change —

Constraints & non-goals —

Acceptance ideas — how we'd know it works…`

// Task intake (spec §9): the dashboard is one source among github/cli/cron.
// A sheet instead of a page so intake happens over the board, with room for
// the rich triage context that saves bounce rounds downstream — structured
// description, base branch, escalation choice, and artifact attachments.
export function TaskCreateSheet() {
  const token = useOperatorToken()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: workspace } = useWorkspace()
  const repos = workspace?.repos ?? []

  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [repo, setRepo] = useState('')
  const [baseBranch, setBaseBranch] = useState('')
  const [level, setLevel] = useState<EscalationLevel>('L2')
  const [files, setFiles] = useState<File[]>([])
  const [uploadFailures, setUploadFailures] = useState<{ task: Task; errors: string[] } | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const repoName = repo || repos[0]?.name || ''
  const close = () => void navigate({ to: '/' })

  const mutation = useMutation({
    mutationFn: async () => {
      const task = await createTask(token, {
        title: title.trim(),
        body: body.trim(),
        repo: repoName,
        level,
        ...(baseBranch.trim() ? { base_branch: baseBranch.trim() } : {}),
      })
      // Attachments ride the existing artifacts API (§21.4), linked to the
      // task so triage and spec agents can pull them as context.
      const errors: string[] = []
      for (const file of files) {
        try {
          await uploadArtifact(token, file, task.id)
        } catch (error) {
          errors.push(`${file.name}: ${String(error)}`)
        }
      }
      return { task, errors }
    },
    onSuccess: ({ task, errors }) => {
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      if (errors.length > 0) {
        setUploadFailures({ task, errors })
      } else {
        void navigate({ to: '/tasks/$taskId', params: { taskId: task.id } })
      }
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
        <Field label="Title">
          <Input value={title} onChange={(event) => setTitle(event.target.value)} placeholder="What should change?" maxLength={200} />
        </Field>

        <Field
          label="Description"
          hint="Becomes the triage and spec prompt — richer context here saves bounce rounds."
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

        <Field label="Escalation level">
          <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label="Escalation level">
            {levels.map((entry) => (
              <button
                key={entry.level}
                type="button"
                role="radio"
                aria-checked={level === entry.level}
                onClick={() => setLevel(entry.level)}
                className={cn(
                  'rounded-md border p-3 text-left transition-colors',
                  level === entry.level ? 'border-primary bg-primary-soft/40' : 'border-border hover:border-edge',
                )}
              >
                <span className={cn('font-mono text-sm font-semibold', level === entry.level ? 'text-primary' : 'text-foreground')}>
                  {entry.level}
                </span>
                <span className="mt-0.5 block text-xs leading-5 text-muted">{entry.hint}</span>
              </button>
            ))}
          </div>
        </Field>

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
        {uploadFailures && (
          <div className="rounded-md border border-attention-dot/40 bg-attention-soft p-3 text-xs leading-5">
            <p className="font-semibold text-attention">Task created, but some attachments failed:</p>
            <ul className="mt-1 list-inside list-disc text-attention/90">
              {uploadFailures.errors.map((entry) => (
                <li key={entry}>{entry}</li>
              ))}
            </ul>
            <Link
              to="/tasks/$taskId"
              params={{ taskId: uploadFailures.task.id }}
              className="mt-2 inline-block font-medium text-primary hover:underline"
            >
              Continue to the task →
            </Link>
          </div>
        )}
      </div>

      <footer className="flex shrink-0 items-center justify-between gap-3 border-t border-border px-5 py-3">
        <p className="text-xs text-faint">
          {token ? (
            'Dispatches immediately after creation.'
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
        <Button disabled={!token || !title.trim() || !repoName || mutation.isPending} onClick={() => mutation.mutate()}>
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
