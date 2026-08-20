import { useEffect, useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, Clock3, FileText, Link2, RotateCcw, TriangleAlert } from 'lucide-react'
import { confirmRequirementVersion, confirmSystemDesignVersion, recoverWorkOrder } from '../../lib/api'
import { deriveCurrentExecutionState, pendingPlanRevisionRequest, type CurrentExecutionState } from '../../lib/activity'
import { errorMessage } from '../../lib/errors'
import type { ActivityItem, WorkOrderCheckpointCitation, WorkOrderCheckpointPendingProposal } from '../../lib/types'
import { useOperatorToken, useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { type Proposal, proposalIdentity } from './system-design-proposal-card'
import { TaskContextAttachmentDialog } from './task-context-attachment-dialog'

export function hasWorkerRecovery(item: ActivityItem) {
  const state = deriveCurrentExecutionState(item)
  return (
    pendingPlanRevisionRequest(item.events) == null &&
    state != null &&
    state.kind !== 'running' &&
    state.kind !== 'dependency_waiting' &&
    state.kind !== 'dependency_attention'
  )
}

export function isCheckpointReleasedRecovery(state: CurrentExecutionState | undefined) {
  return (
    state != null &&
    state.order.stage === 'implement' &&
    state.order.last_failure_message === 'operator checkpoint reached' &&
    state.kind !== 'running' &&
    state.kind !== 'dependency_waiting' &&
    state.kind !== 'dependency_attention'
  )
}

export function CheckpointProposalRecoveryCard({
  item,
  state,
  proposals,
}: {
  item: ActivityItem
  state: CurrentExecutionState
  proposals: Proposal[]
}) {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const orderedProposals = [...proposals].sort(
    (left, right) => left.document.id.localeCompare(right.document.id) || left.version.version - right.version.version,
  )
  const mutation = useMutation({
    mutationFn: async () => {
      // Freeze the click's inputs. Query invalidation can replace proposal
      // records while this sequence is running, but it must not alter which
      // decisions this operator action confirms or the audit direction it
      // records. Multiple pending versions for one document advance If-Match
      // from the version confirmed immediately before them.
      const snapshot = orderedProposals.map((proposal) => ({ ...proposal }))
      const expectedByDocument = new Map<string, number>()
      for (const proposal of snapshot) {
        const expected = expectedByDocument.get(proposal.document.id) ?? proposal.expected
        await confirmSystemDesignVersion(token, proposal.document.id, proposal.version.version, expected)
        expectedByDocument.set(proposal.document.id, proposal.version.version)
      }
      const direction = snapshot
        .map((proposal) => `Operator confirmed ${proposal.document.id} v${proposal.version.version}.`)
        .join(' ')
      return recoverWorkOrder(state.order.id, token, requestId.current, direction)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
    // This also refreshes after a partial multi-proposal sequence: confirmed
    // rows disappear even when a later confirmation failed and recovery was
    // correctly withheld.
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ['system-designs', workspace] })
    },
  })

  if (!isCheckpointReleasedRecovery(state) || orderedProposals.length === 0) return null

  const actionLabel =
    orderedProposals.length === 1
      ? `Confirm v${orderedProposals[0].version.version} and recover`
      : `Confirm ${orderedProposals.length} proposals and recover`

  return (
    <section
      aria-label="Confirm proposals and recover work order"
      className="space-y-3 rounded-lg border border-attention/50 bg-attention-soft px-3 py-3"
    >
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        <div className="min-w-0 space-y-1 text-xs leading-5 text-muted">
          <p className="font-medium text-attention">{state.title}</p>
          <p>Confirm the task&apos;s pending System Design proposal before recovering this checkpoint.</p>
        </div>
      </div>
      {state.order.progress?.trim() && (
        <div className="space-y-1.5 text-xs leading-5 text-muted">
          <p className="font-medium text-foreground">Agent checkpoint message</p>
          <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-sans text-sm text-foreground">
            {state.order.progress}
          </pre>
        </div>
      )}
      <div className="space-y-3">
        {orderedProposals.map((proposal) => (
          <div key={proposalIdentity(proposal)} className="flex items-start gap-2">
            <FileText className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
            <div className="min-w-0 flex-1 text-xs leading-5 text-muted">
              <p className="font-medium text-attention">System Design update proposed</p>
              <p>
                Version {proposal.version.version} proposed by this task for{' '}
                <Link
                  to="/system-design"
                  search={{ document: proposal.document.id }}
                  className="text-primary hover:underline"
                >
                  {proposal.document.title}
                </Link>
                .
              </p>
            </div>
          </div>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <Button size="sm" disabled={!token || mutation.isPending} onClick={() => mutation.mutate()}>
          <Check aria-hidden />
          {mutation.isPending ? 'Confirming and recovering…' : actionLabel}
        </Button>
        <Link
          to="/pending-proposals"
          search={{ task: item.task.id }}
          className="text-xs font-medium text-primary hover:underline"
        >
          Review or dismiss proposals
        </Link>
      </div>
      {!token && <p className="text-xs text-muted">Operator authorization is required to continue.</p>}
      {mutation.error != null && (
        <p className="text-xs text-failure">{errorMessage(mutation.error, 'Could not confirm and recover.')}</p>
      )}
    </section>
  )
}

export function WorkOrderRecoveryCard({
  item,
  state = deriveCurrentExecutionState(item),
}: {
  item: ActivityItem
  state?: CurrentExecutionState
}) {
  if (
    pendingPlanRevisionRequest(item.events) != null ||
    !state ||
    state.kind === 'running' ||
    state.kind === 'dependency_waiting' ||
    state.kind === 'dependency_attention'
  )
    return null
  return <RecoveryState item={item} state={state} />
}

function retryCountdown(at: string, now: number) {
  const seconds = Math.max(0, Math.ceil((new Date(at).getTime() - now) / 1000))
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.ceil(seconds / 60)
  if (minutes < 60) return `${minutes}m`
  return `${Math.ceil(minutes / 60)}h`
}

function RecoveryState({ item, state }: { item: ActivityItem; state: CurrentExecutionState }) {
  const { order } = state
  const token = useOperatorToken()
  const canConfirmDocuments = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const [checkoutResolved, setCheckoutResolved] = useState(false)
  const requestedDecision = order.checkpoint?.decision_request?.trim() ?? ''
  const citations = order.checkpoint?.citations ?? []
  const resolvedDirection = checkpointResolvedDirection(order.checkpoint?.class, citations)
  const [direction, setDirection] = useState(resolvedDirection)
  const [attachingContext, setAttachingContext] = useState(false)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (state.kind !== 'retry_pending') return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [state.kind])
  useEffect(() => {
    if (!resolvedDirection) return
    setDirection((current) => (current.trim() ? current : resolvedDirection))
  }, [resolvedDirection])
  const checkpointReleased = order.last_failure_message === 'operator checkpoint reached'
  const mutation = useMutation({
    mutationFn: () => recoverWorkOrder(order.id, token, requestId.current, direction.trim() || undefined),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['task', item.task.id] })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  const confirmProposal = useMutation({
    mutationFn: ({
      citation,
      proposal,
    }: {
      citation: WorkOrderCheckpointCitation
      proposal: WorkOrderCheckpointPendingProposal
    }) => {
      if (proposal.version == null) throw new Error('This proposal did not include a version.')
      return citation.document_kind === 'requirement'
        ? confirmRequirementVersion(token, citation.document_id, proposal.version, citation.current_confirmed_version)
        : confirmSystemDesignVersion(token, citation.document_id, proposal.version, citation.current_confirmed_version)
    },
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['task', workspace, item.task.id] }),
        queryClient.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
        queryClient.invalidateQueries({ queryKey: ['requirements', workspace] }),
        queryClient.invalidateQueries({ queryKey: ['system-designs', workspace] }),
      ])
    },
  })

  if (state.kind === 'retry_pending') {
    return (
      <div className="space-y-1 rounded-lg border border-primary/25 bg-primary-soft/40 px-3 py-2.5 text-xs text-muted">
        <p className="flex items-center gap-2 font-medium text-foreground">
          <Clock3 className="size-4 text-primary" aria-hidden />
          {order.next_retry_at ? `Retrying in ${retryCountdown(order.next_retry_at, now)}` : 'Retrying automatically'}
        </p>
        <p>
          Conveyor will start the next attempt automatically. No recovery action is available while this retry is
          pending.
        </p>
      </div>
    )
  }

  const checkoutBlocked = state.kind === 'checkout_blocked'
  const actionLabel =
    state.action === 'retry_implementation' || checkoutBlocked ? 'Retry implementation' : 'Recover work order'
  const canRecover =
    Boolean(token) &&
    !mutation.isPending &&
    (!checkoutBlocked || checkoutResolved) &&
    (!checkpointReleased || direction.trim().length > 0)
  return (
    <section
      aria-label={requestedDecision ? 'Checkpoint decision and recovery' : 'Work order recovery'}
      className="space-y-3 rounded-lg border border-attention/50 bg-attention-soft px-3 py-3"
    >
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        {requestedDecision ? (
          <div className="min-w-0 space-y-1 leading-5">
            <p className="text-[11px] font-semibold uppercase tracking-[0.08em] text-attention">Decision needed</p>
            <p className="text-sm font-semibold text-foreground">{requestedDecision}</p>
            <p className="text-xs text-muted">{state.nextAction}</p>
          </div>
        ) : (
          <div className="min-w-0 space-y-1 text-xs leading-5 text-muted">
            <p className="font-medium text-attention">{state.title}</p>
            <p>{state.nextAction}</p>
          </div>
        )}
      </div>
      <div className="space-y-2 text-xs leading-5 text-muted">
        {checkpointReleased ? (
          <p>
            A decision is required before this work can continue. Recovery without direction will repeat the checkpoint.
          </p>
        ) : (
          <p>You can add an optional instruction for the next attempt.</p>
        )}
        {checkpointReleased && !requestedDecision && order.progress?.trim() && (
          <div className="space-y-1.5">
            <p className="font-medium text-foreground">Agent checkpoint message</p>
            <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-sans text-sm text-foreground">
              {order.progress}
            </pre>
          </div>
        )}
        {checkpointReleased && requestedDecision && order.progress?.trim() && (
          <details>
            <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
              Show full progress report
            </summary>
            <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-sans text-sm text-foreground">
              {order.progress}
            </pre>
          </details>
        )}
        {checkpointReleased && order.checkpoint?.class === 'authority_conflict' && (
          <section className="space-y-2" aria-label="Authority conflict references">
            <p className="font-medium text-foreground">Authority in question</p>
            {citations.map((citation) => (
              <CheckpointCitation
                key={`${citation.document_kind}:${citation.document_id}:${citation.cited_version}:${citation.statement_or_section_id ?? ''}`}
                citation={citation}
                token={token}
                canConfirm={canConfirmDocuments}
                activeKey={
                  confirmProposal.variables
                    ? `${confirmProposal.variables.citation.document_id}:${confirmProposal.variables.proposal.version ?? 0}`
                    : ''
                }
                isPending={confirmProposal.isPending}
                error={confirmProposal.error}
                onConfirm={(proposal) => confirmProposal.mutate({ citation, proposal })}
              />
            ))}
          </section>
        )}
        <div className="flex flex-wrap items-center justify-between gap-2 rounded border border-attention/30 bg-surface/60 p-2">
          <span>
            <span className="font-medium text-foreground">Attached context: </span>
            {(item.task.context?.requirements?.length ?? 0) + (item.task.context?.designs?.length ?? 0) === 0
              ? 'None'
              : `${item.task.context?.requirements?.length ?? 0} requirement(s), ${item.task.context?.designs?.length ?? 0} design document(s)`}
          </span>
          <Button variant="secondary" size="sm" disabled={!token} onClick={() => setAttachingContext(true)}>
            <Link2 aria-hidden /> Attach context
          </Button>
        </div>
        <label className="block space-y-1.5">
          <span className="font-medium text-foreground">Operator direction</span>
          <textarea
            aria-label="Operator direction"
            maxLength={4096}
            rows={4}
            value={direction}
            onChange={(event) => setDirection(event.target.value)}
            placeholder="State the decision or instruction the agent should follow."
            className="w-full resize-y rounded-md border border-attention/30 bg-surface px-3 py-2 text-sm text-foreground outline-none placeholder:text-faint focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
      </div>
      {order.last_failure_detail && (
        <details className="text-xs text-muted">
          <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
            Show technical details
          </summary>
          <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-attention/30 bg-surface p-2 font-mono">
            {order.last_failure_detail}
          </pre>
        </details>
      )}
      {checkoutBlocked && (
        <div className="space-y-2 text-xs leading-5 text-muted">
          <p>
            Review the affected files, then commit, stash, or otherwise resolve those changes in the primary checkout.
            Conveyor will not clean, commit, stash, or discard them.
          </p>
          <label className="flex cursor-pointer items-start gap-2 rounded border border-attention/30 bg-surface/60 p-2 focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-primary">
            <input
              type="checkbox"
              className="mt-0.5 size-4 accent-primary"
              checked={checkoutResolved}
              onChange={(event) => setCheckoutResolved(event.target.checked)}
            />
            <span>I resolved the primary checkout changes.</span>
          </label>
        </div>
      )}
      <Button variant="secondary" size="sm" disabled={!canRecover} onClick={() => mutation.mutate()}>
        <RotateCcw aria-hidden />
        {mutation.isPending ? 'Retrying…' : actionLabel}
      </Button>
      {!token && <p className="text-xs text-muted">Operator authorization is required to retry.</p>}
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
      {attachingContext && (
        <TaskContextAttachmentDialog task={item.task} token={token} onClose={() => setAttachingContext(false)} />
      )}
    </section>
  )
}

function checkpointResolvedDirection(checkpointClass: string | undefined, citations: WorkOrderCheckpointCitation[]) {
  if (
    checkpointClass !== 'authority_conflict' ||
    citations.length === 0 ||
    !citations.every((item) => item.newer_confirmed)
  )
    return ''
  const versions = citations
    .map((citation) => `${citation.document_id} v${citation.current_confirmed_version}`)
    .join(', ')
  return `Continue under the newly confirmed authority: ${versions}.`
}

function CheckpointCitation({
  citation,
  token,
  canConfirm,
  activeKey,
  isPending,
  error,
  onConfirm,
}: {
  citation: WorkOrderCheckpointCitation
  token: string
  canConfirm: boolean
  activeKey: string
  isPending: boolean
  error: Error | null
  onConfirm: (proposal: WorkOrderCheckpointPendingProposal) => void
}) {
  return (
    <div className="space-y-2 rounded border border-attention/30 bg-surface/60 p-2">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          {citation.document_kind === 'requirement' ? (
            <Link
              to="/requirements"
              search={{ requirement: citation.document_id }}
              hash={citation.statement_or_section_id?.toLowerCase()}
              className="font-medium text-primary hover:underline"
            >
              {citation.document_title || citation.document_id}
            </Link>
          ) : (
            <Link
              to="/system-design"
              search={{ document: citation.document_id }}
              className="font-medium text-primary hover:underline"
            >
              {citation.document_title || citation.document_id}
            </Link>
          )}
          <p className="text-muted">
            Cited v{citation.cited_version}
            {citation.statement_or_section_id
              ? ` · ${citation.statement_or_section_id}`
              : ' · document-level reference'}
          </p>
        </div>
        <Badge variant={citation.newer_confirmed ? 'positive' : 'attention'}>
          {citation.newer_confirmed
            ? `Now confirmed at v${citation.current_confirmed_version}`
            : `Still confirmed at v${citation.current_confirmed_version}`}
        </Badge>
      </div>
      {citation.pending_proposals.length === 0 ? (
        <p className="text-xs text-muted">No revision is waiting for a decision. Author one if the conflict remains.</p>
      ) : (
        <div className="space-y-2">
          <p className="text-xs font-medium text-attention">
            {citation.pending_proposals.length === 1
              ? 'A revision is waiting for your decision.'
              : `${citation.pending_proposals.length} revisions are waiting for your decision.`}
          </p>
          <div className="flex flex-wrap gap-2">
            {citation.pending_proposals.map((proposal) => {
              const key = `${citation.document_id}:${proposal.version ?? 0}`
              return (
                <div key={key} className="flex flex-wrap items-center gap-2">
                  <Button size="sm" disabled={!token || !canConfirm || isPending} onClick={() => onConfirm(proposal)}>
                    <Check aria-hidden />
                    {isPending && activeKey === key ? 'Confirming…' : `Confirm v${proposal.version}`}
                  </Button>
                  {canConfirm && (
                    <Link
                      to="/pending-proposals"
                      search={{ document: citation.document_id, tier: citation.document_kind }}
                      className="text-xs font-medium text-primary hover:underline"
                    >
                      Review or dismiss v{proposal.version}
                    </Link>
                  )}
                </div>
              )
            })}
          </div>
          {!canConfirm && <p className="text-xs text-muted">Document confirmation capability is required.</p>}
          {error != null && activeKey.startsWith(`${citation.document_id}:`) && (
            <p className="text-xs text-failure">{errorMessage(error, 'Could not confirm this revision.')}</p>
          )}
        </div>
      )}
    </div>
  )
}
