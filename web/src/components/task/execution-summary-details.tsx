import { Link } from '@tanstack/react-router'
import { AlertTriangle, CircleDashed, Hourglass } from 'lucide-react'
import { type CurrentExecutionState, dependencyRelationLabel } from '../../lib/activity'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'

// The current order's execution state as the next-action card prints it
// (component-web-task-surfaces, Next-action card). Every line comes from
// deriveCurrentExecutionState, and its `facts` flags decide which lines carry
// information beyond the server's summary; nothing here compares sentences.

const executionStatusWords: Record<CurrentExecutionState['status'], string> = {
  progressing: 'Progressing',
  waiting: 'Waiting',
  paused: 'Paused — needs attention',
}

export function ExecutionStatusLine({ state }: { state: CurrentExecutionState }) {
  return (
    <p className="flex items-center gap-1.5 text-xs font-medium text-muted">
      {state.status === 'paused' ? (
        <AlertTriangle className="size-3.5 shrink-0 text-attention" aria-hidden />
      ) : state.status === 'waiting' ? (
        <Hourglass className="size-3.5 shrink-0 text-faint" aria-hidden />
      ) : (
        <CircleDashed className="size-3.5 shrink-0 animate-pulse text-primary" aria-hidden />
      )}
      <span data-execution-status={state.status}>
        {state.stage} · {executionStatusWords[state.status]}
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
    <dl className="space-y-2 text-xs leading-5">
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
        <div className="flex flex-wrap gap-x-1.5">
          <dt className="font-medium text-foreground after:content-[':']">Retry</dt>
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
