import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { GitMerge } from 'lucide-react'
import { useRef, useState } from 'react'
import { fetchTaskActivity, postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, NextAction, NextActionChoice } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { NextActionChoices } from '../inbox/choice-button'
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
// detail query so the task page reuses its cache. The merge button and the
// other choices share one wrapping row with one primary button: the first
// choice that renders enabled, which for merge_approval is the merge
// (feature-operator-next-action NA-6; component-web-task-surfaces).
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
  const oneActMerge = action.kind === 'merge_approval'
  const mergeChoice = oneActMerge ? action.choices.find((choice) => choice.id === 'merge') : undefined
  const mergeRefusal = oneActMerge ? action.details?.refusals?.find((refusal) => refusal.choice === 'merge') : undefined
  const otherChoices = mergeChoice ? action.choices.filter((choice) => choice !== mergeChoice) : action.choices
  const { data: detail } = useQuery({
    queryKey: taskDetailQueryKey(workspace, taskId),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchTaskActivity(workspace, taskId, signal)),
    enabled: Boolean(workspace && mergeChoice),
  })
  const primary = shortcutChoiceId(action)
  return (
    <NextActionChoices
      taskId={taskId}
      choices={otherChoices}
      details={action.details}
      onActed={() => onActed?.()}
      primaryId={primary}
      routeVariant={routeVariant}
      onRestarted={onRestarted}
      leading={
        (mergeChoice || mergeRefusal) && (
          <MergeAtGateAction
            taskId={taskId}
            choice={mergeChoice}
            refusal={mergeRefusal?.text}
            readiness={detail?.merge_readiness}
            primary={primary === 'merge'}
            onMerged={() => onActed?.()}
          />
        )
      }
    />
  )
}

// Whether the act may be taken, the reason it may not, and its endpoint are the
// server's choice; the browser adds only the confirm, which names the reviewed
// head and the pull request the act merges.
function MergeAtGateAction({
  taskId,
  choice,
  refusal,
  readiness,
  primary,
  onMerged,
}: {
  taskId: string
  choice: NextActionChoice | undefined
  refusal: string | undefined
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
  const reason = !choice
    ? (refusal ?? '')
    : !choice.allowed
      ? `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
      : ''
  const head = readiness?.head_sha ?? ''
  const pullRequest = readiness?.number ? `#${readiness.number}` : 'the pull request'
  const merge = (target: NextActionChoice) => {
    if (inFlight.current) return
    inFlight.current = true
    mutation.mutate(target)
  }

  return (
    <span className="inline-flex max-w-full min-w-0 flex-col gap-0.5">
      <span className="inline-flex max-w-full min-w-0 items-center gap-1.5">
        <Button
          size="sm"
          variant={primary && reason === '' ? 'default' : 'secondary'}
          data-merge-at-gate
          data-shortcut={keyed && reason === '' ? 'c' : undefined}
          title={choice ? `${choice.effect} (${choice.method} ${choice.path})` : undefined}
          disabled={mutation.isPending || reason !== ''}
          onClick={() => {
            mutation.reset()
            setConfirming(true)
          }}
        >
          <GitMerge className="size-4" />
          {choice?.label ?? 'Merge'}
        </Button>
        {keyed && reason === '' && <Kbd>c</Kbd>}
      </span>
      {reason && <span className="text-[11px] text-muted">{reason}</span>}
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
