import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { ArrowLeft, MessageSquarePlus, MessagesSquare } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useWorkspaceCapability, useWorkspaceSelection } from '../components/app-shell'
import { PlanningChat, relativeDate, sessionStatusLabels } from '../components/planning/planning-chat'
import { Button } from '../components/ui/button'
import { Select } from '../components/ui/input'
import { MarkdownProse, withoutLeadingTitle } from '../components/ui/markdown-prose'
import { PageHeader } from '../components/ui/page-header'
import { createPlanningSession, decidePlanningBundle, fetchPlanningBundles, fetchPlanningSessions } from '../lib/api'
import { sessionGoalLabel, sessionGoalLabels } from '../lib/contracts'
import { errorMessage } from '../lib/errors'
import type { PlanningSession, PlanningSessionGoal } from '../lib/types'
import { cn } from '../lib/utils'

// Task fan-out now comes from reviewable delivery bundles. Blueprint remains
// a historical document label, not a planning-session creation goal.
const standaloneGoals: PlanningSessionGoal[] = ['open', 'bundle']

// What tells apart sessions that share a title: the document they draft from or
// produced, else the end of the session's own identifier.
function sessionMark(session: PlanningSession) {
  return (
    session.requirement_context_id ||
    session.system_design_context_id ||
    session.produced_requirement_id ||
    session.produced_system_design_id ||
    session.id.split('-').at(-1)
  )
}

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
    queryFn: () => fetchPlanningSessions(workspace),
    enabled: Boolean(workspace),
  })
  const { data: bundles } = useQuery({
    queryKey: ['planning-bundles', workspace],
    queryFn: () => fetchPlanningBundles(workspace),
    enabled: Boolean(workspace),
  })

  // A `session` deep link (the lineage explorer's destination) wins over the
  // session restored for this workspace.
  const { session: linkedSession } = useSearch({ from: '/w/$workspaceId/planning' })
  const navigate = useNavigate({ from: '/w/$workspaceId/planning' })
  useEffect(() => {
    if (!workspace) return
    restoredWorkspace.current = workspace
    setSelectedId(linkedSession || (localStorage.getItem(`conveyor-planning-session:${workspace}`) ?? ''))
  }, [workspace, linkedSession])
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
    mutationFn: () => createPlanningSession(workspace, { goal }),
    onSuccess: (session) => {
      client.setQueryData<PlanningSession[]>(['planning-sessions', workspace], (current) => [
        session,
        ...(current ?? []),
      ])
      selectSession(session.id)
      void client.invalidateQueries({ queryKey: ['planning-sessions', workspace] })
    },
  })
  // Selecting writes `session`, as the Inbox writes `selected`: below md that
  // names the conversation view, and Back returns to the list (AC-2.8's pattern).
  const selectSession = (id: string) => {
    setSelectedId(id)
    void navigate({ search: (current) => ({ ...current, session: id }), resetScroll: false })
  }
  const selected = sessions?.find((session) => session.id === selectedId)
  const conversationView = Boolean(linkedSession && sessions?.some((session) => session.id === linkedSession))
  const titleCounts = new Map<string, number>()
  for (const session of sessions ?? []) {
    const title = session.title || ''
    titleCounts.set(title, (titleCounts.get(title) ?? 0) + 1)
  }
  const selectedBundle = bundles?.find((bundle) => bundle.id === selected?.produced_bundle_id)
  const decideBundle = useMutation({
    mutationFn: (decision: 'approve' | 'reject') => decidePlanningBundle(workspace, selectedBundle!.id, decision),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['planning-bundles', workspace] })
      void client.invalidateQueries({ queryKey: ['tasks', workspace] })
      void client.invalidateQueries({ queryKey: ['activity', workspace] })
    },
  })

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        title="Planning"
        description="Explore freely or propose a delivery bundle. Requirement drafting lives beside the document in Requirements."
        className="max-sm:[&>div:last-child]:ml-0 max-sm:[&>div:last-child]:w-full"
      >
        {canPropose && (
          <form
            className={cn('flex items-center gap-2 max-sm:w-full', conversationView && 'max-md:hidden')}
            onSubmit={(event) => {
              event.preventDefault()
              create.mutate()
            }}
          >
            <Select
              aria-label="Planning goal"
              className="w-44 max-sm:w-full"
              value={goal}
              onChange={(event) => setGoal(event.target.value as PlanningSessionGoal)}
            >
              {standaloneGoals.map((option) => (
                <option key={option} value={option}>
                  {sessionGoalLabels[option]}
                </option>
              ))}
            </Select>
            <Button type="submit" className="shrink-0" disabled={create.isPending}>
              <MessageSquarePlus /> {create.isPending ? 'Starting…' : 'New session'}
            </Button>
          </form>
        )}
        {canPropose && create.error && (
          <p className="basis-full text-xs text-failure">
            {errorMessage(create.error, 'Could not start this planning session.')}
          </p>
        )}
      </PageHeader>
      <div className="flex min-h-0 flex-1">
        <aside
          className={cn(
            'min-w-0 flex-1 overflow-y-auto bg-surface/40 md:w-72 md:flex-none md:shrink-0 md:border-r md:border-border',
            conversationView && 'max-md:hidden',
          )}
        >
          <div className="px-4 py-3">
            <p className="text-xs font-medium text-muted">Sessions</p>
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
                onClick={() => selectSession(session.id)}
                className={cn(
                  'block w-full px-4 py-3 text-left',
                  selectedId === session.id ? 'bg-raised' : 'hover:bg-surface',
                )}
              >
                <strong className="block truncate text-sm font-medium">
                  {session.title || 'Untitled planning session'}
                </strong>
                <span className="mt-1 flex min-w-0 items-center gap-1.5 text-xs text-muted">
                  <span className="min-w-0 truncate">
                    <span>{sessionStatusLabels[session.status]}</span> · <span>{sessionGoalLabel(session)}</span>
                  </span>
                  {(titleCounts.get(session.title || '') ?? 0) > 1 && (
                    <span data-session-mark className="min-w-0 truncate font-mono text-faint">
                      {sessionMark(session)}
                    </span>
                  )}
                  <time className="ml-auto shrink-0 text-faint">{relativeDate(session.updated_at)}</time>
                </span>
              </button>
            ))}
          </div>
        </aside>
        <main className={cn('flex min-w-0 flex-1 flex-col', !conversationView && 'max-md:hidden')}>
          <Link
            to="/w/$workspaceId/planning"
            params={{ workspaceId: workspace }}
            search={(current) => ({ ...current, session: undefined })}
            resetScroll={false}
            data-planning-back
            className="inline-flex shrink-0 items-center gap-1 border-b border-border px-4 py-2 text-xs font-medium text-primary hover:underline pointer-coarse:min-h-10 md:hidden"
          >
            <ArrowLeft className="size-3.5" aria-hidden />
            Back to sessions
          </Link>
          {selected ? (
            <div className="flex min-h-0 flex-1 flex-col">
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
                  className="max-h-[45%] shrink-0 overflow-y-auto border-t border-border bg-surface/40 px-4 py-4 md:px-6"
                  aria-label="Planning bundle preview"
                >
                  <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-3">
                    <div>
                      <p className="text-xs font-medium text-muted">Delivery bundle · {selectedBundle.status}</p>
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
                      <h3 className="text-xs font-medium text-muted">Pending documents</h3>
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
                      <h3 className="text-xs font-medium text-muted">Task set</h3>
                      <ol className="mt-2 space-y-2">
                        {selectedBundle.tasks.map((task) => (
                          <li
                            key={task.member_id}
                            className="rounded-md border border-border bg-background p-3 text-sm"
                          >
                            <strong>{task.title}</strong>
                            <MarkdownProse className="mt-1 text-xs text-muted">
                              {withoutLeadingTitle(task.body, task.title)}
                            </MarkdownProse>
                            <p className="mt-2 break-words font-mono text-xs text-faint">
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
            <div className="grid min-h-0 flex-1 place-items-center p-6">
              <div className="flex max-w-sm flex-col items-center rounded-xl border border-dashed border-border px-8 py-14 text-center">
                <MessagesSquare aria-hidden="true" className="size-5 text-faint" />
                <h2 className="mt-3 text-base font-semibold tracking-tight">No session open</h2>
                <p className="mt-1 text-sm text-muted">Start or select a planning session.</p>
              </div>
            </div>
          )}
        </main>
      </div>
    </div>
  )
}
