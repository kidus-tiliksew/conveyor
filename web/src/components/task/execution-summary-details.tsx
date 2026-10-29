import { Link } from '@tanstack/react-router'
import { AlertTriangle, CircleDashed } from 'lucide-react'
import { useLayoutEffect, useRef } from 'react'
import { type CurrentExecutionState, dependencyRelationLabel } from '../../lib/activity'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { WorkOrderActivitySnapshot } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Disclosure } from '../ui/disclosure'

// The current order's execution state as the next-action card prints it
// (component-web-task-surfaces, Next-action card). Every line comes from
// deriveCurrentExecutionState, and its `facts` flags decide which lines carry
// information beyond the server's summary; nothing here compares sentences.

export function ExecutionStatusLine({ state }: { state: CurrentExecutionState }) {
  const paused = state.status === 'paused'
  return (
    <p className="flex items-center gap-1.5 text-xs font-semibold text-muted">
      {paused ? (
        <AlertTriangle className="size-3.5 shrink-0 text-attention" aria-hidden />
      ) : (
        <CircleDashed className="size-3.5 shrink-0 animate-pulse text-primary" aria-hidden />
      )}
      <span data-execution-status={state.status}>
        {state.stage} · {paused ? 'Paused — needs attention' : 'Progressing'}
      </span>
    </p>
  )
}

// The state's own title, printed only where it adds to the summary — a
// checkpoint's question label, or the cause of a pause.
export function ExecutionTitle({ state }: { state: CurrentExecutionState }) {
  if (!state.facts.title) return null
  return (
    <p
      id="current-execution-title"
      className={cn('text-sm font-medium', state.status === 'paused' ? 'text-attention' : 'text-foreground')}
    >
      {state.title}
    </p>
  )
}

export function ExecutionFactRows({
  state,
  routeVariant,
}: {
  state: CurrentExecutionState
  routeVariant: TaskRouteVariant
}) {
  const { workspace } = useWorkspaceSelection()
  const { facts } = state
  const nextAction = facts.nextAction ? state.nextAction : undefined
  if (!facts.blocker && !facts.retry && !nextAction) return null
  const relatedRoute = relatedTaskRoute(routeVariant)
  const blockerDecision = state.blocker.match(/\bDEC-[1-9][0-9]*\b/)?.[0]
  return (
    <dl className="grid gap-2 text-xs leading-5 sm:grid-cols-3">
      {facts.blocker && (
        <div>
          <dt className="font-medium text-foreground">Current blocker</dt>
          <dd className="text-muted">
            {state.blockingDependencies?.length ? (
              <span className="flex flex-col items-start">
                {state.blockingDependencies.map((dependency) => (
                  <Link
                    key={dependency.id}
                    to={relatedRoute}
                    params={{ workspaceId: workspace, taskId: dependency.id }}
                    className="text-primary hover:underline"
                  >
                    {dependency.title || dependency.id} ·{' '}
                    {dependencyRelationLabel(
                      dependency.state,
                      true,
                      state.unsatisfiableDependencyIDs?.includes(dependency.id) === true,
                    )}
                  </Link>
                ))}
              </span>
            ) : (
              <DecisionBlockerText blocker={state.blocker} decisionID={blockerDecision} />
            )}
          </dd>
        </div>
      )}
      {facts.retry && (
        <div>
          <dt className="font-medium text-foreground">Retry</dt>
          <dd className="text-muted">{state.retry}</dd>
        </div>
      )}
      {nextAction && (
        <div>
          <dt className="font-medium text-foreground">What to do next</dt>
          <dd className="text-muted">{nextAction}</dd>
        </div>
      )}
    </dl>
  )
}

export function RecentActivity({ snapshot }: { snapshot: WorkOrderActivitySnapshot }) {
  const outputRef = useRef<HTMLPreElement>(null)

  useLayoutEffect(() => {
    if (outputRef.current) outputRef.current.scrollTop = outputRef.current.scrollHeight
  }, [snapshot.content])

  return (
    <section
      aria-label="Client machine activity"
      className="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-2 border-t border-border/60 pt-2.5"
    >
      <details className="contents [&::details-content]:col-span-2">
        <summary
          aria-label="Client machine activity"
          className="flex cursor-pointer flex-wrap items-baseline gap-2 rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
        >
          <h3 className="text-xs font-medium text-foreground">Client machine activity</h3>
        </summary>
        <pre
          ref={outputRef}
          className="mt-1.5 max-h-40 overflow-auto whitespace-pre-wrap rounded border border-border bg-background p-2 font-mono text-[11px] leading-5 text-foreground"
        >
          {snapshot.content}
        </pre>
      </details>
      {/* Inspecting the capture time must not toggle the output summary. */}
      <Disclosure
        className="col-start-2 row-start-1 justify-self-start"
        triggerClassName="text-[11px] text-faint"
        content={absoluteTime(snapshot.captured_at)}
      >
        <time dateTime={snapshot.captured_at}>Captured {absoluteTime(snapshot.captured_at)}</time>
      </Disclosure>
    </section>
  )
}

function DecisionBlockerText({ blocker, decisionID }: { blocker: string; decisionID?: string }) {
  const { workspace } = useWorkspaceSelection()
  if (!decisionID) return blocker
  const [before, ...after] = blocker.split(decisionID)
  return (
    <>
      {before}
      <Link
        to="/w/$workspaceId/decisions/$decisionId"
        params={{ workspaceId: workspace, decisionId: decisionID }}
        className="text-primary hover:underline"
        aria-label={`Open ${decisionID}`}
      >
        {decisionID}
      </Link>
      {after.join(decisionID)}
    </>
  )
}
