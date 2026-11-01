import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { GitMerge } from 'lucide-react'
import { useId, useRef, useState } from 'react'
import { fetchTaskActivity, postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, NextAction, NextActionChoice } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { choiceButtonClass, NextActionChoices } from '../inbox/choice-button'
import { Kbd, shortcutChoiceId, useShortcutChoice } from '../inbox/inbox-keyboard'
import { nextActionQueryKey, nextActionsQueryKey } from '../inbox/use-next-actions'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { withQuerySignal } from './use-task-detail'

type MergeReadiness = ActivityItem['merge_readiness']

// The server's choices for one next action, with the one-act merge at the
// merge_approval gate pulled out behind a confirm (DEC-64). The merge is a
// server choice like any other (DEC-61), but it is irreversible, so no
// surface offers it as a one-click button. The confirm names the reviewed head
// and pull request from the task's merge_readiness, read through the task
// detail query so the task page reuses its cache. Exactly one button is
// primary: the merge when the server offers it, otherwise the first choice
// that renders enabled (feature-operator-next-action NA-6).
// At merge_approval a status line above the choices states the merge
// readiness in plain words, and the choices take the grid layout: the merge
// button on its own full-width line, the other choices in two columns below
// it. Every other gate keeps the one wrapping row (component-web-task-surfaces,
// Next-action card).
export function GateChoices({
  taskId,
  action,
  onActed,
  onRestarted,
  routeVariant,
}: {
  taskId: string
  action: NextAction
  onActed?: () => void
  onRestarted?: () => void
  routeVariant?: TaskRouteVariant
}) {
  const { workspace } = useWorkspaceSelection()
  const ids = useId()
  const oneActMerge = action.kind === 'merge_approval'
  const mergeChoice = oneActMerge ? action.choices.find((choice) => choice.id === 'merge') : undefined
  const mergeRefusal = oneActMerge ? action.details?.refusals?.find((refusal) => refusal.choice === 'merge') : undefined
  const otherChoices = mergeChoice ? action.choices.filter((choice) => choice !== mergeChoice) : action.choices
  const atGate = Boolean(mergeChoice || mergeRefusal)
  const { data: detail } = useQuery({
    queryKey: taskDetailQueryKey(workspace, taskId),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchTaskActivity(workspace, taskId, signal)),
    enabled: Boolean(workspace && atGate),
  })
  const primary = shortcutChoiceId(action)
  const statusId = `${ids}-merge-status`
  // A merge the caller may not take keeps its reason in the reasons block; a
  // merge the server refuses is explained by the status line instead.
  const capabilityReason =
    mergeChoice && !mergeChoice.allowed
      ? {
          id: `${ids}-merge-reason`,
          label: mergeChoice.label,
          text: `Needs the ${mergeChoice.capability.replaceAll('_', ' ')} capability.`,
          choice: 'merge',
        }
      : undefined
  return (
    <NextActionChoices
      taskId={taskId}
      choices={otherChoices}
      details={action.details}
      onActed={() => onActed?.()}
      primaryId={primary}
      routeVariant={routeVariant}
      onRestarted={onRestarted}
      layout={atGate ? 'grid' : 'row'}
      leadingReason={capabilityReason}
      status={
        atGate && (
          <MergeStatusLine
            id={statusId}
            offered={Boolean(mergeChoice)}
            refusal={mergeRefusal?.text}
            readiness={detail?.merge_readiness}
            loaded={detail !== undefined}
          />
        )
      }
      leading={
        atGate && (
          <MergeAtGateAction
            taskId={taskId}
            choice={mergeChoice}
            reason={capabilityReason?.text ?? (mergeChoice ? '' : (mergeRefusal?.text ?? ''))}
            describedBy={capabilityReason ? capabilityReason.id : mergeChoice ? undefined : statusId}
            readiness={detail?.merge_readiness}
            primary={primary === 'merge'}
            onMerged={() => onActed?.()}
          />
        )
      }
    />
  )
}

type MergeTone = 'positive' | 'neutral' | 'attention'

const mergeToneClass: Record<MergeTone, string> = {
  positive: 'bg-positive-soft text-positive',
  neutral: 'border border-border bg-surface text-muted',
  attention: 'bg-attention-soft text-attention',
}

const mergeDotClass: Record<MergeTone, string> = {
  positive: 'bg-positive',
  neutral: 'bg-faint',
  attention: 'bg-attention',
}

// The merge readiness in plain words. The server refuses the merge with
// `merge readiness <STATE>` (DEC-64(3)); the known states map to a sentence
// here, and any other refusal prints verbatim. The state is read from the task
// detail's merge_readiness, and a refusal without one is the server's unread
// peek, UNKNOWN.
export function mergeStatus({
  offered,
  refusal,
  readiness,
  loaded,
}: {
  offered: boolean
  refusal: string | undefined
  readiness: MergeReadiness
  loaded: boolean
}): { tone: MergeTone; state: string; lead: string; text: string; head?: string } | null {
  const number = readiness?.number ? `#${readiness.number}` : ''
  const pr = number || 'the pull request'
  const Pr = number || 'The pull request'
  if (offered) {
    const head = readiness?.head_sha
    return head
      ? { tone: 'positive', state: 'MERGEABLE', lead: 'Ready to merge', text: ' at reviewed head ', head }
      : { tone: 'positive', state: 'MERGEABLE', lead: 'Ready to merge.', text: '' }
  }
  if (!refusal) return null
  if (!loaded) return { tone: 'neutral', state: 'LOADING', lead: 'Checking merge readiness…', text: '' }
  const state = readiness?.state ?? 'UNKNOWN'
  switch (state) {
    case 'UNKNOWN':
      return {
        tone: 'neutral',
        state,
        lead: 'Merge not available yet.',
        text: ` GitHub hasn't reported whether ${pr} can merge.`,
      }
    case 'CONFLICTING':
      return { tone: 'attention', state, lead: "Can't merge.", text: ` ${Pr} conflicts with its base branch.` }
    case 'STALE':
      return { tone: 'attention', state, lead: "Can't merge.", text: ` ${Pr} moved since review.` }
    default:
      return { tone: 'attention', state, lead: '', text: refusal }
  }
}

function MergeStatusLine({
  id,
  offered,
  refusal,
  readiness,
  loaded,
}: {
  id: string
  offered: boolean
  refusal: string | undefined
  readiness: MergeReadiness
  loaded: boolean
}) {
  const status = mergeStatus({ offered, refusal, readiness, loaded })
  if (!status) return null
  const number = readiness?.number ? `#${readiness.number}` : 'the pull request'
  return (
    <p
      id={id}
      data-merge-status={status.state}
      className={cn(
        'flex max-w-md items-start gap-2 rounded-lg px-2.5 py-2 text-xs leading-5',
        mergeToneClass[status.tone],
      )}
    >
      <span aria-hidden="true" className={cn('mt-[7px] size-1.5 shrink-0 rounded-full', mergeDotClass[status.tone])} />
      <span className="min-w-0">
        {status.lead && <span className="font-semibold">{status.lead}</span>}
        {status.text}
        {status.head && (
          <>
            <code className="font-mono text-foreground" title={status.head}>
              {status.head.slice(0, 7)}
            </code>
            .
          </>
        )}
        {readiness?.url && (
          <>
            {' '}
            <a className="underline underline-offset-2" href={readiness.url} target="_blank" rel="noreferrer">
              Open {number}
            </a>
          </>
        )}
      </span>
    </p>
  )
}

// Whether the act may be taken, the reason it may not, and its endpoint are the
// server's choice; the browser adds only the confirm, which names the reviewed
// head and the pull request the act merges.
function MergeAtGateAction({
  taskId,
  choice,
  reason,
  describedBy,
  readiness,
  primary,
  onMerged,
}: {
  taskId: string
  choice: NextActionChoice | undefined
  reason: string
  describedBy: string | undefined
  readiness: MergeReadiness
  primary: boolean
  onMerged: () => void
}) {
  // The Inbox binds `c` to the merge button when it is the decision's primary.
  const keyed = useShortcutChoice() === 'merge'
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  // isPending lands a render after the click, so a double click would post
  // twice; the ref closes that window.
  const inFlight = useRef(false)
  const mutation = useMutation({
    onMutate: (): MutationWorkspace => ({ workspace }),
    mutationFn: (merge: NextActionChoice) => postNextActionChoice(workspace, merge, undefined),
    onSuccess: () => {
      setConfirming(false)
      onMerged()
    },
    onSettled: async (_result, _error, _variables, context) => {
      inFlight.current = false
      const acted = context?.workspace ?? workspace
      await Promise.all([
        client.invalidateQueries({ queryKey: nextActionsQueryKey(acted) }),
        client.invalidateQueries({ queryKey: nextActionQueryKey(acted, taskId) }),
        client.invalidateQueries({ queryKey: ['activity'] }),
        client.invalidateQueries({ queryKey: taskDetailQueryKey(acted, taskId) }),
      ])
    },
  })
  const head = readiness?.head_sha ?? ''
  const pullRequest = readiness?.number ? `#${readiness.number}` : 'the pull request'
  const merge = (target: NextActionChoice) => {
    if (inFlight.current) return
    inFlight.current = true
    mutation.mutate(target)
  }

  return (
    <span className="flex w-full min-w-0 flex-col gap-0.5">
      <span className="flex w-full min-w-0 items-stretch gap-1.5">
        <Button
          size="sm"
          className={`${choiceButtonClass} flex-1 justify-center`}
          variant={primary && reason === '' ? 'default' : 'secondary'}
          data-merge-at-gate
          data-shortcut={keyed && reason === '' ? 'c' : undefined}
          title={choice ? `${choice.effect} (${choice.method} ${choice.path})` : undefined}
          aria-describedby={describedBy}
          disabled={mutation.isPending || reason !== ''}
          onClick={() => {
            mutation.reset()
            setConfirming(true)
          }}
        >
          <GitMerge className="size-4" />
          {choice?.label ?? 'Merge'}
        </Button>
        {keyed && reason === '' && <Kbd className="self-center">c</Kbd>}
      </span>
      {!confirming && mutation.error != null && (
        <span className="text-xs text-failure">{errorMessage(mutation.error, 'Could not merge.')}</span>
      )}
      {confirming && choice && (
        <Dialog label="Confirm merge" onClose={() => !mutation.isPending && setConfirming(false)}>
          <div className="space-y-3 p-4">
            <h2 className="text-sm font-semibold">Merge {pullRequest}?</h2>
            {head ? (
              <p className="text-sm leading-6 text-muted">
                This approves and merges {pullRequest} at the reviewed head{' '}
                <code className="font-mono text-foreground" title={head}>
                  {head.slice(0, 7)}
                </code>{' '}
                in one act, under your name.
                {readiness?.url && (
                  <>
                    {' '}
                    <a className="underline" href={readiness.url} target="_blank" rel="noreferrer">
                      Open {pullRequest}
                    </a>
                  </>
                )}
              </p>
            ) : (
              <p className="text-sm leading-6 text-muted">Loading the reviewed head…</p>
            )}
            {mutation.error != null && (
              <p className="text-xs text-failure">{errorMessage(mutation.error, 'Could not merge.')}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button size="sm" variant="secondary" disabled={mutation.isPending} onClick={() => setConfirming(false)}>
                Cancel
              </Button>
              <Button
                size="sm"
                data-confirm-merge
                data-endpoint={`${choice.method} ${choice.path}`}
                disabled={mutation.isPending || !head}
                onClick={() => merge(choice)}
              >
                {mutation.isPending ? 'Merging…' : `Merge ${pullRequest}`}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </span>
  )
}
