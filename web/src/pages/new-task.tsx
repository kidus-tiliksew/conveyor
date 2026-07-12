import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { createTask } from '../lib/api'
import type { EscalationLevel } from '../lib/types'
import { useOperatorToken, useWorkspace } from '../components/app-shell'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input, Select, Textarea } from '../components/ui/input'

const levels: Array<{ level: EscalationLevel; hint: string }> = [
  { level: 'L0', hint: 'Fully automatic — auto-merge on green verification' },
  { level: 'L1', hint: 'Automatic with a one-click human approve' },
  { level: 'L2', hint: 'Human reviews the spec, then the PR' },
  { level: 'L3', hint: 'Human pairs interactively' },
]

// Task intake (spec §9): the dashboard is one source among github/cli/cron.
export function NewTaskPage() {
  const token = useOperatorToken()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: workspace } = useWorkspace()
  const repos = workspace?.repos ?? []

  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [repo, setRepo] = useState('')
  const [level, setLevel] = useState<EscalationLevel>('L2')
  const repoName = repo || repos[0]?.name || ''

  const mutation = useMutation({
    mutationFn: () => createTask(token, { title: title.trim(), body: body.trim(), repo: repoName, level }),
    onSuccess: (task) => {
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
      void navigate({ to: '/tasks/$taskId', params: { taskId: task.id } })
    },
  })

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-2xl px-6 py-8">
        <h1 className="text-xl font-semibold tracking-tight">New task</h1>
        <p className="mt-1 text-sm text-muted">
          Queue a unit of intended change. Triage classifies it and routes it through the pipeline.
        </p>

        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Task</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4 py-4">
            <Field label="Title">
              <Input
                value={title}
                onChange={(event) => setTitle(event.target.value)}
                placeholder="What should change?"
                maxLength={200}
              />
            </Field>
            <Field label="Description" hint="Free-form context; it becomes part of the triage and spec prompts.">
              <Textarea
                value={body}
                onChange={(event) => setBody(event.target.value)}
                placeholder="Symptoms, links, constraints, acceptance ideas…"
                className="min-h-32"
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
              <Field label="Escalation level" hint={levels.find((entry) => entry.level === level)?.hint}>
                <Select value={level} onChange={(event) => setLevel(event.target.value as EscalationLevel)}>
                  {levels.map((entry) => (
                    <option key={entry.level} value={entry.level}>
                      {entry.level}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            <div className="flex items-center justify-between gap-3 border-t border-border pt-4">
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
              <Button
                disabled={!token || !title.trim() || !repoName || mutation.isPending}
                onClick={() => mutation.mutate()}
              >
                {mutation.isPending ? 'Creating…' : 'Create task'}
              </Button>
            </div>
            {mutation.error != null && <p className="text-sm text-failure">{String(mutation.error)}</p>}
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-faint">{hint}</span>}
    </label>
  )
}
