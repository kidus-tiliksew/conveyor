import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { GitMerge } from 'lucide-react'
import { useId, useRef, useState } from 'react'
import { fetchTaskActivity, GateMutationError, postNextActionChoice } from '../../lib/api'
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
import { CheckStatusList } from './next-action-checks'
import { riskApprovalText } from './risk-tier'
import { withQuerySignal } from './use-task-detail'

type MergeReadiness = ActivityItem['merge_readiness']

// The server's choices for one next action, with the merge pulled out behind a
// confirm wherever the server offers it: the one-act merge at the
// merge_approval gate (DEC-64), and the merge of the approved head at
// merge_execution and check_hold. The merge is a server choice like any other
// (DEC-61), but it is irreversible, so no surface offers it as a one-click
// button. The confirm names the head and pull request from the task's
// merge_readiness, read through the task detail query so the task page reuses
// its cache; at check_hold it also lists the checks that hold the merge
// (component-next-action, The statement; DEC-108(6)). Exactly one button is
// primary: the merge when the server offers it, otherwise the first choice
// that renders enabled (component-web-task-surfaces).
// At merge_approval a status line above the choices states the merge
// readiness in plain words. On a phone the choices stack, with the merge button
// on its own full-width line and the other choices in two columns below it;
// from `sm` up they share the one row every other gate uses
// (component-web-task-surfaces, Next-action card). `footer` keeps every kind in
// one action row with the merge at its natural width, for the Inbox detail's
// footer.
export function GateChoices({
  taskId,
  action,
  onActed,
  onRestarted,
  routeVariant,
  footer = false,
}: {
  taskId: string
  action: NextAction
  onActed?: () => void
  onRestarted?: () => void
  routeVariant?: TaskRouteVariant
  footer?: boolean
}) {
  const { workspace } = useWorkspaceSelection()
  const ids = useId()
  const oneActMerge = action.kind === 'merge_approval'
  const confirmedMerge = mergeKinds.has(action.kind)
  const mergeChoice = confirmedMerge ? action.choices.find((choice) => choice.id === 'merge') : undefined
  const mergeRefusal = oneActMerge ? action.details?.refusals?.find((refusal) => refusal.choice === 'merge') : undefined
  const otherChoices = mergeChoice ? action.choices.filter((choice) => choice !== mergeChoice) : action.choices
  // The status line and the phone grid belong to the merge_approval gate; the
  // merge at merge_execution and check_hold keeps the row every other kind uses.
  const atGate = oneActMerge && Boolean(mergeChoice || mergeRefusal)
  const leadsWithMerge = atGate || Boolean(mergeChoice)
  const {
    data: detail,
    error: detailError,
    refetch: refetchDetail,
    isFetching: detailFetching,
  } = useQuery({
    queryKey: taskDetailQueryKey(workspace, taskId),
    queryFn: (context) => withQuerySignal(context, (signal) => fetchTaskActivity(workspace, taskId, signal)),
    enabled: Boolean(workspace && leadsWithMerge),
  })
  const primary = shortcutChoiceId(action)
  const statusId = `${ids}-merge-status`
  // An enforcing task states its approvals beside the merge control, so the
  // person sees how many approvals remain before acting (req-risk-based-review
  // AC-8.2, AC-11.1).
  const approvals = leadsWithMerge && detail?.risk ? riskApprovalText(detail.risk) : null
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
      layout={footer ? 'footer' : atGate ? 'grid' : 'row'}
      leadingReason={capabilityReason}
      status={
        (atGate || approvals) && (
          <>
            {approvals && (
              <p data-approval-count className="text-xs leading-5 text-foreground/85">
                {approvals}
              </p>
            )}
            {atGate && (
              <MergeStatusLine
                id={statusId}
                offered={Boolean(mergeChoice)}
                refusal={mergeRefusal?.text}
                readiness={detail?.merge_readiness}
                loaded={detail !== undefined}
              />
            )}
          </>
        )
      }
      leading={
        leadsWithMerge && (
          <MergeAtGateAction
            taskId={taskId}
            kind={action.kind}
            action={action}
            choice={mergeChoice}
            reason={capabilityReason?.text ?? (mergeChoice ? '' : (mergeRefusal?.text ?? ''))}
            describedBy={capabilityReason ? capabilityReason.id : mergeChoice ? undefined : statusId}
            readiness={detail?.merge_readiness}
            readinessError={detail === undefined && detailError != null ? detailError : null}
            readinessRetrying={detailFetching}
            onRetryReadiness={() => void refetchDetail()}
            primary={primary === 'merge'}
            // Merging past a failed check is still this decision's main act,
            // but it never wears the safe primary look under the warning about
            // those checks.
            caution={action.kind === 'check_hold'}
            layout={footer ? 'footer' : atGate ? 'grid' : 'row'}
            onMerged={() => onActed?.()}
          />
        )
      }
    />
  )
}

// The kinds whose merge choice lands a head, so its button opens the confirm
// rather than posting (component-web-task-surfaces invariant 45).
const mergeKinds = new Set(['merge_approval', 'merge_execution', 'check_hold'])

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
      className={cn('flex items-start gap-2 rounded-lg px-2.5 py-2 text-xs leading-5', mergeToneClass[status.tone])}
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

// The merge button's width in each choice layout: the merge_approval grid
// gives it its own line on a phone, the Inbox footer fills the line below md,
// and an ordinary row keeps its natural width.
const mergeWidthClass: Record<'grid' | 'footer' | 'row', string> = {
  grid: 'w-full sm:w-auto',
  footer: 'max-md:w-full',
  row: '',
}

// Whether the act may be taken, the reason it may not, and its endpoint are the
// server's choice; the browser adds only the confirm, which names the head and
// the pull request the act merges and, at check_hold, the checks the merge
// overrides. The confirm posts through the same mutation as before, so it
// keeps the duplicate-send guard, and a 409 refetches, keeps the refusal in the
// dialog, and never retries (component-web-task-surfaces invariant 46).
function MergeAtGateAction({
  taskId,
  kind,
  action,
  choice,
  reason,
  describedBy,
  readiness,
  readinessError,
  readinessRetrying,
  onRetryReadiness,
  primary,
  caution,
  layout,
  onMerged,
}: {
  taskId: string
  kind: NextAction['kind']
  action: NextAction
  choice: NextActionChoice | undefined
  reason: string
  describedBy: string | undefined
  readiness: MergeReadiness
  readinessError: unknown
  readinessRetrying: boolean
  onRetryReadiness: () => void
  primary: boolean
  caution: boolean
  layout: 'grid' | 'footer' | 'row'
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
    // The refetch below lands before the mutation leaves pending, so a refused
    // merge stays disabled until the current state renders beside the refusal.
    // As for the gate's other choices, a conflict means the task moved under
    // the operator, so it refetches; any other failure leaves the state as it
    // was and refetches nothing.
    onSettled: async (_result, error, _variables, context) => {
      inFlight.current = false
      if (error != null && !(error instanceof GateMutationError && error.status === 409)) return
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

  const widthClass = mergeWidthClass[layout]
  // merge_approval approves and merges in one act; merge_execution and
  // check_hold merge a head a review round already approved.
  const gateAct = kind === 'merge_approval'
  const headName = gateAct ? 'reviewed head' : 'approved head'
  // While more than one approval remains the server labels the merge choice
  // Approve: the act records the person's approval and merges nothing
  // (req-risk-based-review AC-8.7; feature-risk-based-review RR-7).
  const approveOnly = choice?.label === 'Approve'

  return (
    <span className={cn('flex min-w-0 flex-col gap-0.5', widthClass)}>
      <span className={cn('flex min-w-0 items-stretch gap-1.5', widthClass)}>
        <Button
          size="sm"
          className={cn(
            `${choiceButtonClass} flex-1 justify-center`,
            caution && 'border-attention/50 text-attention hover:border-attention hover:bg-attention-soft',
          )}
          variant={caution ? 'outline' : primary && reason === '' ? 'default' : 'secondary'}
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
        <Dialog
          label={approveOnly ? 'Confirm approval' : 'Confirm merge'}
          onClose={() => !mutation.isPending && setConfirming(false)}
        >
          <div className="space-y-3 p-4">
            <h2 className="text-sm font-semibold">
              {approveOnly ? 'Approve' : 'Merge'} {pullRequest}?
            </h2>
            {head ? (
              <p className="text-sm leading-6 text-muted" data-confirm-head={head}>
                {approveOnly ? 'This records your approval of' : gateAct ? 'This approves and merges' : 'This merges'}{' '}
                {pullRequest} at the {headName}{' '}
                <code className="font-mono text-foreground" title={head}>
                  {head.slice(0, 7)}
                </code>{' '}
                {approveOnly
                  ? 'under your name. It does not merge, because more approvals are needed.'
                  : gateAct
                    ? 'in one act, under your name.'
                    : 'under your name.'}
                {readiness?.url && (
                  <>
                    {' '}
                    <a className="underline" href={readiness.url} target="_blank" rel="noreferrer">
                      Open {pullRequest}
                    </a>
                  </>
                )}
              </p>
            ) : readinessError != null ? (
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <span className="text-failure">{errorMessage(readinessError, `Could not load the ${headName}.`)}</span>
                <Button size="sm" variant="secondary" disabled={readinessRetrying} onClick={onRetryReadiness}>
                  Retry
                </Button>
              </div>
            ) : (
              <p className="text-sm leading-6 text-muted">Loading the {headName}…</p>
            )}
            {kind === 'check_hold' && (
              <div data-confirm-checks className="space-y-1.5 rounded-lg bg-attention-soft px-3 py-2">
                <p className="text-sm font-medium text-attention">These checks hold the merge:</p>
                <CheckStatusList action={action} />
                <p className="text-xs leading-5 text-muted">
                  The merge reads no check status. Conveyor records you and these checks with it.
                </p>
              </div>
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
                {mutation.isPending
                  ? approveOnly
                    ? 'Approving…'
                    : 'Merging…'
                  : `${approveOnly ? 'Approve' : 'Merge'} ${pullRequest}`}
              </Button>
            </div>
          </div>
        </Dialog>
      )}
    </span>
  )
}
