import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { MessageSquarePlus } from 'lucide-react'
import { useOperatorToken, useWorkspaceSelection } from '../components/app-shell'
import {
  PlanningChat,
  relativeDate,
  sessionGoalLabel,
  sessionStatusLabels,
} from '../components/planning/planning-chat'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Input, Select } from '../components/ui/input'
import {
  createPlanningSession,
  fetchPlanningSessions,
  fetchWorkspaceConfig,
} from '../lib/api'
import { errorMessage } from '../lib/errors'
import type { PlanningSessionGoal } from '../lib/types'

// A document-scoped session belongs beside its document, so this surface keeps
// only the goals that have no document context (spec §21.57 change 1).
const standaloneGoals: { value: PlanningSessionGoal; label: string }[] = [
  { value: 'open', label: 'Open exploration' },
  { value: 'blueprint', label: 'Blueprint' },
]

export function PlanningPage() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [selectedId, setSelectedId] = useState('')
  const restoredWorkspace = useRef('')
  const [title, setTitle] = useState('')
  const [model, setModel] = useState('')
  const [goal, setGoal] = useState<PlanningSessionGoal>('open')
  const { data: workspaceConfig } = useQuery({
    queryKey: ['workspace-config', token, workspace],
    queryFn: () => fetchWorkspaceConfig(token),
    enabled: Boolean(token && workspace),
  })
  const { data: sessions, isLoading, error: sessionsError } = useQuery({
    queryKey: ['planning-sessions', workspace], queryFn: fetchPlanningSessions, enabled: Boolean(workspace),
  })

  useEffect(() => {
    if (!workspace) return
    restoredWorkspace.current = workspace
    setSelectedId(localStorage.getItem(`conveyor-planning-session:${workspace}`) ?? '')
  }, [workspace])
  useEffect(() => {
    if (!sessions?.length || restoredWorkspace.current !== workspace) return
    if (!sessions.some((session) => session.id === selectedId)) setSelectedId(sessions[0].id)
  }, [sessions, selectedId, workspace])
  useEffect(() => {
    if (workspace && selectedId && restoredWorkspace.current === workspace) {
      localStorage.setItem(`conveyor-planning-session:${workspace}`, selectedId)
    }
  }, [selectedId, workspace])

  const configuredModels = workspaceConfig?.document.planning_models ?? []
  const defaultModel = workspaceConfig?.document.execution_settings.control_plane.planning?.model
    ?? workspaceConfig?.document.execution_settings.control_plane.triage.model
    ?? ''
  const modelOptions = configuredModels.length ? configuredModels : (defaultModel ? [defaultModel] : [])
  useEffect(() => {
    if (!workspaceConfig) return
    const next = configuredModels.includes(model) ? model : (configuredModels[0] ?? defaultModel)
    if (next !== model) setModel(next)
  }, [configuredModels, defaultModel, model, workspaceConfig])

  const create = useMutation({
    // An omitted title takes the goal-derived provisional one from the server,
    // which the produced artifact then replaces (spec §21.57 change 3).
    mutationFn: () => createPlanningSession(token, {
      title: title.trim() || undefined,
      goal,
      model: configuredModels.length ? model || undefined : undefined,
    }),
    onSuccess: (session) => {
      setTitle('')
      setSelectedId(session.id)
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  const selected = sessions?.find((session) => session.id === selectedId)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-b border-border px-6 py-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">Planning</h1>
          <p className="mt-0.5 text-xs text-muted">Open exploration and blueprints without a document. Requirement drafting lives beside the document, in Requirements.</p>
        </div>
        <form className="flex flex-wrap items-center gap-2" onSubmit={(event) => { event.preventDefault(); if (token) create.mutate() }}>
          <Input
            aria-label="Planning session title"
            className="w-56"
            placeholder="What are we planning?"
            value={title}
            onChange={(event) => setTitle(event.target.value)}
          />
          <Select
            aria-label="Planning goal"
            className="w-44"
            value={goal}
            onChange={(event) => setGoal(event.target.value as PlanningSessionGoal)}
          >
            {standaloneGoals.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </Select>
          <Select
            aria-label="Planning model"
            className="w-52 font-mono"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            disabled={!workspaceConfig || configuredModels.length === 0}
          >
            {modelOptions.map((candidate) => <option key={candidate} value={candidate}>{candidate}</option>)}
          </Select>
          <Button type="submit" disabled={!token || create.isPending}>
            <MessageSquarePlus /> {create.isPending ? 'Starting…' : 'New session'}
          </Button>
        </form>
        {create.error && <p className="basis-full text-xs text-failure">{errorMessage(create.error, 'Could not start this planning session.')}</p>}
      </header>
      <div className="flex min-h-0 flex-1">
        <aside className="w-72 shrink-0 overflow-y-auto border-r border-border bg-surface/40">
          <div className="px-4 py-3"><p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Sessions</p></div>
          {isLoading && <p className="px-4 text-xs text-muted">Restoring sessions…</p>}
          {sessionsError && <p className="px-4 text-xs text-failure">{errorMessage(sessionsError, 'Could not restore planning sessions.')}</p>}
          {sessions?.length === 0 && <p className="px-4 text-xs leading-5 text-muted">No planning sessions yet.</p>}
          <div className="divide-y divide-border">
            {sessions?.map((session) => (
              <button
                key={session.id}
                type="button"
                aria-current={selectedId === session.id ? 'true' : undefined}
                onClick={() => setSelectedId(session.id)}
                className={`block w-full px-4 py-3 text-left ${selectedId === session.id ? 'bg-primary-soft' : 'hover:bg-surface'}`}
              >
                <strong className="block truncate text-sm font-medium">{session.title || 'Untitled planning session'}</strong>
                <span className="mt-1 flex flex-wrap items-center gap-1.5">
                  <Badge variant={session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'}>
                    {sessionStatusLabels[session.status]}
                  </Badge>
                  <Badge variant="mono">{sessionGoalLabel(session)}</Badge>
                  <time className="ml-auto text-[10px] text-faint">{relativeDate(session.updated_at)}</time>
                </span>
              </button>
            ))}
          </div>
        </aside>
        <main className="min-w-0 flex-1">
          {selected
            ? <PlanningChat key={`${workspace}:${selected.id}`} summary={selected} token={token} workspace={workspace} />
            : <div className="grid h-full place-items-center"><p className="text-sm text-muted">Start or select a planning session.</p></div>}
        </main>
      </div>
    </div>
  )
}
