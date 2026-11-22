import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'
import { Fragment, useEffect, useRef } from 'react'
import { CrateBriefingError, fetchCrateBriefing, generateCrateBriefing } from '../../lib/api'
import { crateBriefingQueryKey } from '../../lib/query-keys'
import type { CrateBriefingEntry, CrateBriefingView, CrateDetail, CrateTask } from '../../lib/types'
import { cn } from '../../lib/utils'
import { callerIdentityQueryOptions, useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Skeleton } from '../ui/skeleton'
import type { CrateEmphasis } from './crate-emphasis'

// The crate briefing (req-crate-insights REQ-5; component-crates invariants
// 33 and 35; DEC-144): one headline, a short account of where the delivery
// stands, and the tasks to look at first. It sits between the header with
// the line and the branch map. The server writes and stores the text and
// says whether it is current. The page shows it as plain text, with no
// generated marker, model name, or generation time (DEC-144(7)).
//
// The page asks for a briefing only as invariant 35 states: each opening
// decides once, on its first answered read, and posts only when the crate
// has a member, the user may generate one, and either no briefing is stored
// or the stored one is out of date and was generated more than 30 minutes
// before that read. Refetches never post again.

type Target = { workspace: string; crateId: string }

// A stored briefing is old once its generation time is more than this
// before the read that decides (component-crates invariant 35;
// req-crate-insights AC-5.2; DEC-144(4)(b), (c)).
export const briefingStaleAfterMs = 30 * 60 * 1000

// Whether a briefing generated at `generatedAt` is old at `readAt`, a time
// in milliseconds. Exactly 30 minutes is not old. An unreadable time is not
// old, so it never starts a request.
export function briefingOlderThanLimit(generatedAt: string, readAt: number) {
  const generated = Date.parse(generatedAt)
  return Number.isFinite(generated) && Number.isFinite(readAt) && readAt - generated > briefingStaleAfterMs
}

// A refused read is not tried again: a 4xx answer does not change on retry.
function briefingRetry(count: number, error: unknown) {
  if (error instanceof CrateBriefingError && error.status < 500) return false
  return count < 1
}

const toneDot: Record<string, string> = {
  attention: 'bg-attention-dot',
  info: 'bg-info',
  positive: 'bg-positive',
}

export function CrateBriefingSection({
  crate,
  emphasis,
  onSelect,
}: {
  crate: CrateDetail
  emphasis: CrateEmphasis
  onSelect: (id: string) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const members = crate.tasks.length
  const key = crateBriefingQueryKey(workspace, crate.id)
  // The read lives under the crate prefix, so a workspace change refreshes
  // it with the other crate reads (component-crates invariant 18). Each
  // opening reads it again even when a fresh copy is cached, because the
  // generation decision waits for an answer that arrives in this opening.
  const read = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchCrateBriefing(workspace, crate.id, signal),
    enabled: Boolean(workspace) && members > 0,
    refetchOnMount: 'always',
    retry: briefingRetry,
  })

  // One request at a time. The flag is set before the request starts, so a
  // second click before the page renders again sends nothing.
  const posting = useRef(false)
  // Each result lands on the key of the workspace and crate that asked. A
  // 409 means the server has nothing to do: the page reads the briefing
  // again and shows no notice.
  const generation = useMutation({
    mutationFn: ({ workspace, crateId }: Target) => generateCrateBriefing(workspace, crateId),
    retry: false,
    onSuccess: async (view, target) => {
      const targetKey = crateBriefingQueryKey(target.workspace, target.crateId)
      queryClient.setQueryData<CrateBriefingView>(targetKey, view)
      // The answer says whether the briefing matches the crate as it stood
      // when generation started. The crate can change while the model runs,
      // so the page reads the briefing again: the read compares it with the
      // crate as it stands now (component-crates invariant 35). This also
      // replaces any read that started before the answer.
      await queryClient.invalidateQueries({ queryKey: targetKey, exact: true })
    },
    onError: (error, target) => {
      if (error instanceof CrateBriefingError && error.status === 409)
        void queryClient.invalidateQueries({
          queryKey: crateBriefingQueryKey(target.workspace, target.crateId),
          exact: true,
        })
    },
    onSettled: () => {
      posting.current = false
    },
  })
  const post = () => {
    if (posting.current) return
    posting.current = true
    generation.mutate({ workspace, crateId: crate.id })
  }

  // Regenerate is an affordance, so it follows component-web-dashboard
  // invariant 11: it shows only when a successful identity read lists
  // create_tasks, and a failed refetch over an earlier success denies. It
  // also needs this opening's latest briefing read to have succeeded with
  // can_generate: a copy cached by an earlier visit, or a view that a failed
  // refetch left on screen, keeps its text but grants no control
  // (req-crate-insights AC-5.5). The server authorizes the POST again.
  const mayCreate = useWorkspaceCapability('create_tasks')
  const identity = useQuery({ ...callerIdentityQueryOptions(workspace), enabled: Boolean(workspace) })
  const view = read.data
  const answered = read.isFetchedAfterMount && read.status === 'success'
  const canGenerate = answered && mayCreate && view?.can_generate === true

  // The decision of this opening, keyed by workspace and crate. It is taken
  // once, from the first successful read answered after the section mounted
  // and that read's time: a copy cached by an earlier visit does not count.
  // When that read answers before the identity read, the decision waits for
  // the identity read and still uses the first read. It is used up before any
  // request, so later refetches, a 409, a failed request, and StrictMode's
  // repeated effects never post again. An automatic request needs both the
  // identity read's create_tasks and the view's can_generate.
  const decided = useRef<string | null>(null)
  const firstRead = useRef<{ opening: string; view: CrateBriefingView; readAt: number } | null>(null)
  const opening = `${workspace}\n${crate.id}`
  useEffect(() => {
    if (decided.current === opening) return
    if (answered && view && firstRead.current?.opening !== opening)
      firstRead.current = { opening, view, readAt: read.dataUpdatedAt }
    const first = firstRead.current
    if (!first || first.opening !== opening || identity.status === 'pending') return
    decided.current = opening
    const stored = first.view.briefing
    const wanted =
      stored === null || (stored.current === false && briefingOlderThanLimit(stored.generated_at, first.readAt))
    if (wanted && first.view.can_generate && mayCreate && members > 0) post()
  }, [answered, view, read.dataUpdatedAt, opening, members, identity.status, mayCreate])

  // A crate with no member has no briefing (feature-crate-insights, Rollout
  // and closure).
  if (members === 0) return null

  const failure =
    generation.error != null && !(generation.error instanceof CrateBriefingError && generation.error.status === 409)
      ? generation.error
      : null
  const busy = generation.isPending
  const briefing = view?.briefing ?? null
  const byId = new Map(crate.tasks.map((task) => [task.id, task]))

  return (
    <section
      aria-label="Briefing"
      data-crate-briefing
      className="min-w-0 rounded-lg border border-border bg-card px-5 py-4 sm:px-6"
    >
      <style>{briefingMotion}</style>
      {!view && read.isPending && (
        <div className="space-y-2" aria-busy="true" data-briefing-loading>
          <Skeleton className="h-5 w-3/5" />
          <Skeleton className="h-3.5 w-full" />
          <Skeleton className="h-3.5 w-4/5" />
        </div>
      )}

      {!view && read.error != null && (
        <p
          role="alert"
          data-briefing-read-failed
          className="rounded-md bg-raised px-3 py-2 text-xs leading-5 text-muted"
        >
          The briefing did not load: {errorMessage(read.error)}
        </p>
      )}

      {view && !briefing && (
        <>
          {busy ? (
            <div className="space-y-2" role="status" aria-label="Writing the briefing" data-briefing-generating>
              <p className="text-xs text-faint">
                Reading {members} {members === 1 ? 'task' : 'tasks'} and their activity…
              </p>
              <Skeleton className="h-5 w-3/5" />
              <Skeleton className="h-3.5 w-full" />
              <Skeleton className="h-3.5 w-4/5" />
            </div>
          ) : failure ? null : (
            <p data-briefing-none className="text-sm text-muted">
              {view.can_generate && mayCreate
                ? 'No briefing yet.'
                : 'No briefing yet. A user who can create tasks can generate one.'}
            </p>
          )}
        </>
      )}

      {briefing && (
        <BriefingBody
          briefing={briefing}
          byId={byId}
          canGenerate={canGenerate}
          busy={busy}
          onRegenerate={post}
          emphasis={emphasis}
          onSelect={onSelect}
        />
      )}

      {/* Regenerating over a stored briefing only dims the text, so a
          screen reader hears the request through this status. */}
      {briefing && busy && (
        <p role="status" className="sr-only" data-briefing-regenerating>
          Writing a new briefing…
        </p>
      )}

      {/* A read that fails after an earlier answer keeps that answer on
          screen (a copy cached by an earlier visit, or the last refetch).
          The page still says that this read failed, so the stored text and
          its Regenerate state are not taken as the crate as it stands now. */}
      {view && read.error != null && (
        <p
          role="alert"
          data-briefing-read-failed
          className="mt-3 rounded-md bg-raised px-3 py-2 text-xs leading-5 text-muted"
        >
          The briefing did not refresh: {errorMessage(read.error)}
        </p>
      )}

      {failure && (
        <p
          role="alert"
          data-briefing-failed
          className={cn(
            'rounded-md bg-raised px-3 py-2 text-xs leading-5 text-muted',
            (briefing || (view && read.error != null)) && 'mt-3',
          )}
        >
          {failureText(failure)}
        </p>
      )}
    </section>
  )
}

function BriefingBody({
  briefing,
  byId,
  canGenerate,
  busy,
  onRegenerate,
  emphasis,
  onSelect,
}: {
  briefing: NonNullable<CrateBriefingView['briefing']>
  byId: Map<string, CrateTask>
  canGenerate: boolean
  busy: boolean
  onRegenerate: () => void
  emphasis: CrateEmphasis
  onSelect: (id: string) => void
}) {
  // Only a current member is a row: a stored briefing can name a task that
  // has since left the crate, and that task gets no link or emphasis.
  const seen = new Set<string>()
  const rows: Array<{ entry: CrateBriefingEntry; task: CrateTask }> = []
  for (const entry of briefing.look_first ?? []) {
    const task = byId.get(entry.task_id)
    if (!task || seen.has(task.id)) continue
    seen.add(task.id)
    rows.push({ entry, task })
  }
  const words = briefing.headline.split(' ')
  return (
    <div className={cn('grid gap-x-8 gap-y-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,22rem)]', busy && 'opacity-60')}>
      <div className="min-w-0">
        {/* Plain text: the generated words are never rendered as markup. */}
        <p
          key={briefing.generated_at}
          data-briefing-headline
          className="text-lg font-semibold leading-snug tracking-tight text-foreground"
        >
          {/* The space between two words sits outside their boxes: a space
              at the end of an inline block does not show. */}
          {words.map((word, index) => (
            <Fragment key={index}>
              {index > 0 && ' '}
              <span className="crate-briefing-word inline-block" style={{ animationDelay: `${index * 45}ms` }}>
                {word}
              </span>
            </Fragment>
          ))}
        </p>
        <p data-briefing-account className="mt-1.5 max-w-[68ch] text-sm leading-relaxed text-markdown">
          {briefing.account}
        </p>
      </div>
      {(rows.length > 0 || canGenerate) && (
        <div className="min-w-0">
          {/* The heading and Regenerate share one row, level with the
              headline. With no row to list, Regenerate sits alone at the
              right of it. */}
          <div className="mb-1 flex min-h-6 items-center justify-between gap-2" data-briefing-heading-row>
            {rows.length > 0 && (
              <h3 className="text-[11px] font-medium uppercase tracking-wide text-faint">Look at first</h3>
            )}
            {canGenerate && (
              <Button
                size="sm"
                variant="outline"
                className="ml-auto h-6 shrink-0 px-2 text-[11px] [&_svg]:size-3"
                data-briefing-regenerate
                // Enabled only for a briefing that is out of date, and not
                // while a request runs (component-crates invariant 35).
                disabled={busy || briefing.current}
                title={briefing.current ? 'The briefing is current.' : undefined}
                onClick={onRegenerate}
              >
                <RefreshCw className={cn(busy && 'animate-spin')} />
                Regenerate
              </Button>
            )}
          </div>
          {rows.length > 0 && (
            <ul className="-mx-2" aria-label="Look at first">
              {rows.map(({ entry, task }) => {
                const target = { task: task.id }
                const emphasized = emphasis.emphasizedId === task.id
                return (
                  <li key={task.id}>
                    {/* Pointing at a row emphasizes its task in the line and
                        the branch map and marks its row in the task list, as
                        the map's lines do (req-crate-insights REQ-3). */}
                    <button
                      type="button"
                      data-briefing-task={task.id}
                      data-briefing-emphasized={emphasized || undefined}
                      onClick={() => onSelect(task.id)}
                      onMouseEnter={() => emphasis.emphasize(task.id)}
                      onMouseLeave={() => emphasis.emphasize(null)}
                      onFocus={() => emphasis.focus(target)}
                      onBlur={() => emphasis.blur(target)}
                      className={cn(
                        'flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-raised focus-visible:bg-raised focus-visible:outline-none',
                        emphasized && 'bg-raised',
                      )}
                    >
                      <span
                        aria-hidden
                        data-tone={entry.tone}
                        className={cn('mt-[7px] size-1.5 shrink-0 rounded-full', toneDot[entry.tone] ?? 'bg-info')}
                      />
                      <span className="min-w-0">
                        <span className="block truncate text-sm font-medium text-foreground" title={task.title}>
                          {task.title}
                        </span>
                        <span className="block text-xs leading-snug text-muted" data-briefing-reason>
                          {entry.reason}
                        </span>
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

// Names the failure with the server's code and its redacted detail.
function failureText(error: Error) {
  if (error instanceof CrateBriefingError) {
    const code = error.code ?? `HTTP ${error.status}`
    return `The briefing could not be generated (${code}): ${error.detail || error.message}`
  }
  return `The briefing could not be generated: ${error.message}`
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}

// The headline's words come in one after another. Under reduced motion they
// do not move.
const briefingMotion = `
@keyframes crate-briefing-word-in {
  from { opacity: 0; transform: translateY(0.35em); filter: blur(3px); }
  to { opacity: 1; transform: none; filter: none; }
}
.crate-briefing-word { animation: crate-briefing-word-in 420ms cubic-bezier(0.2, 0.7, 0.2, 1) both; }
@media (prefers-reduced-motion: reduce) {
  .crate-briefing-word { animation: none; }
}
`
