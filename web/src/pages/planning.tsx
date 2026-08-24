import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { MessageSquarePlus } from 'lucide-react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { PlanningChat, relativeDate, sessionStatusLabels } from '../components/planning/planning-chat'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Select } from '../components/ui/input'
import { createPlanningSession, decidePlanningBundle, fetchPlanningBundles, fetchPlanningSessions } from '../lib/api'
import { sessionGoalLabel, sessionGoalLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import type { PlanningSessionGoal } from '../lib/types'

// Task fan-out now comes from reviewable delivery bundles. Blueprint remains
// a historical document label, not a planning-session creation goal.
const standaloneGoals: PlanningSessionGoal[] = ['open', 'bundle']

export function PlanningPage() {
  const { workspace } = useWorkspaceSelection()
  const canPropose = useWorkspaceCapability('propose_documents')
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const client = useQueryClient()
  const [selectedId, setSelectedId] = useState('')
  const restoredWorkspace = useRef('')
  const [goal, setGoal] = useState<PlanningSessionGoal>('open')
  const {
    data: sessions,
    isLoading,
    error: sessionsError,
  } = useQuery({
    queryKey: ['planning-sessions', workspace],
    queryFn: fetchPlanningSessions,
    enabled: Boolean(workspace),
  })
  const { data: bundles } = useQuery({
    queryKey: ['planning-bundles', workspace],
    queryFn: fetchPlanningBundles,
    enabled: Boolean(workspace),
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

  const create = useMutation({
    // No title is sent: the server names the session from its goal, and the
    // artifact it produces renames it.
    mutationFn: () => createPlanningSession({ goal }),
    onSuccess: (session) => {
      setSelectedId(session.id)
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  const selected = sessions?.find((session) => session.id === selectedId)
  const selectedBundle = bundles?.find((bundle) => bundle.id === selected?.produced_bundle_id)
  const decideBundle = useMutation({
    mutationFn: (decision: 'approve' | 'reject') => decidePlanningBundle(selectedBundle!.id, decision),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['planning-bundles', workspace] })
      void client.invalidateQueries({ queryKey: ['tasks', workspace] })
      void client.invalidateQueries({ queryKey: ['activity', workspace] })
    },
  })

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-b border-border px-6 py-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">Planning</h1>
          <p className="mt-0.5 text-xs text-muted">
            Explore freely or propose a delivery bundle. Requirement drafting lives beside the document in Requirements.
          </p>
        </div>
        {canPropose && (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault()
              create.mutate()
            }}
          >
            <Select
              aria-label="Planning goal"
              className="w-44"
              value={goal}
              onChange={(event) => setGoal(event.target.value as PlanningSessionGoal)}
            >
              {standaloneGoals.map((option) => (
                <option key={option} value={option}>
                  {sessionGoalLabels[option]}
                </option>
              ))}
            </Select>
            <Button type="submit" disabled={create.isPending}>
              <MessageSquarePlus /> {create.isPending ? 'Starting…' : 'New session'}
            </Button>
          </form>
        )}
        {canPropose && create.error && (
          <p className="basis-full text-xs text-failure">
            {errorMessage(create.error, 'Could not start this planning session.')}
          </p>
        )}
      </header>
      <div className="flex min-h-0 flex-1">
        <aside className="w-72 shrink-0 overflow-y-auto border-r border-border bg-surface/40">
          <div className="px-4 py-3">
            <p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Sessions</p>
          </div>
          {isLoading && <p className="px-4 text-xs text-muted">Restoring sessions…</p>}
          {sessionsError && (
            <p className="px-4 text-xs text-failure">
              {errorMessage(sessionsError, 'Could not restore planning sessions.')}
            </p>
          )}
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
                <strong className="block truncate text-sm font-medium">
                  {session.title || 'Untitled planning session'}
                </strong>
                <span className="mt-1 flex flex-wrap items-center gap-1.5">
                  <Badge
                    variant={
                      session.status === 'active' ? 'accent' : session.status === 'finalized' ? 'positive' : 'default'
                    }
                  >
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
          {selected ? (
            <div className="flex h-full min-h-0 flex-col">
              <PlanningChat
                key={`${workspace}:${selected.id}`}
                summary={selected}
                workspace={workspace}
                onFinalized={() => {
                  void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
                  void client.invalidateQueries({ queryKey: ['planning-bundles', workspace] })
                }}
              />
              {selectedBundle && (
                <section
                  className="max-h-[45%] shrink-0 overflow-y-auto border-t border-border bg-surface/40 px-6 py-4"
                  aria-label="Planning bundle preview"
                >
                  <div className="flex items-start justify-between gap-4">
                    <div>
                      <p className="text-[10px] font-semibold uppercase tracking-wider text-faint">
                        Delivery bundle · {selectedBundle.status}
                      </p>
                      <h2 className="mt-1 text-base font-semibold">{selectedBundle.title}</h2>
                      <p className="mt-1 text-xs text-muted">
                        Approving creates the task set. Document revisions remain pending their own confirmation.
                      </p>
                    </div>
                    {canConfirm && selectedBundle.status === 'pending' && (
                      <div className="flex gap-2">
                        <Button
                          variant="secondary"
                          disabled={decideBundle.isPending}
                          onClick={() => decideBundle.mutate('reject')}
                        >
                          Reject
                        </Button>
                        <Button disabled={decideBundle.isPending} onClick={() => decideBundle.mutate('approve')}>
                          Approve task set
                        </Button>
                      </div>
                    )}
                  </div>
                  <div className="mt-4 grid gap-4 lg:grid-cols-2">
                    <div>
                      <h3 className="text-xs font-semibold uppercase tracking-wider text-muted">Pending documents</h3>
                      <ul className="mt-2 space-y-2">
                        {selectedBundle.documents.map((document) => (
                          <li
                            key={`${document.kind}:${document.id}:${document.version ?? 0}`}
                            className="rounded-md border border-border bg-background p-3 text-sm"
                          >
                            <strong>{document.title || document.id}</strong>
                            <p className="mt-1 font-mono text-xs text-faint">
                              {document.kind} · {document.id}
                              {document.version ? ` v${document.version}` : ''} · pending
                            </p>
                          </li>
                        ))}
                      </ul>
                    </div>
                    <div>
                      <h3 className="text-xs font-semibold uppercase tracking-wider text-muted">Task set</h3>
                      <ol className="mt-2 space-y-2">
                        {selectedBundle.tasks.map((task) => (
                          <li
                            key={task.member_id}
                            className="rounded-md border border-border bg-background p-3 text-sm"
                          >
                            <strong>{task.title}</strong>
                            <p className="mt-1 whitespace-pre-wrap text-xs text-muted">{task.body}</p>
                            <p className="mt-2 font-mono text-[10px] text-faint">
                              {task.repo} · depends on: {task.depends_on?.join(', ') || 'none'} · context:{' '}
                              {[
                                ...(task.context?.requirement_ids ?? []),
                                ...(task.context?.system_design_ids ?? []),
                              ].join(', ') || 'none'}
                            </p>
                          </li>
                        ))}
                      </ol>
                    </div>
                  </div>
                  {decideBundle.error && (
                    <p className="mt-3 text-xs text-failure">
                      {errorMessage(decideBundle.error, 'Could not resolve this bundle.')}
                    </p>
                  )}
                </section>
              )}
            </div>
          ) : (
            <div className="grid h-full place-items-center">
              <p className="text-sm text-muted">Start or select a planning session.</p>
            </div>
          )}
        </main>
      </div>
    </div>
  )
}
