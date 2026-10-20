import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, Clock3, FileText, Link2, RotateCcw, TriangleAlert } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import type { CurrentExecutionState } from '../../lib/activity'
import { confirmRequirementVersion, confirmSystemDesignVersion, recoverWorkOrder } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { type MutationWorkspace, taskDetailQueryKey } from '../../lib/query-keys'
import type { ActivityItem, WorkOrderCheckpointCitation, WorkOrderCheckpointPendingProposal } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { CheckpointProposalDiff } from './checkpoint-proposal-diff'
import { type Proposal, proposalIdentity } from './system-design-proposal-card'
import { TaskContextAttachmentDialog } from './task-context-attachment-dialog'
import { VerificationCheckpointGrounds } from './verification-checkpoint-grounds'

// Whether the current order needs a worker-recovery act. The current order and
// its state come from the server's next action (deriveCurrentExecutionState);
// a plan revision is decided at the gate, never recovered here.
export function hasWorkerRecovery(item: ActivityItem, state: CurrentExecutionState | undefined) {
  return (
    item.next_action?.kind !== 'plan_revision' &&
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
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const canRecover = useWorkspaceCapability('recover_work')
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
        await confirmSystemDesignVersion(proposal.document.id, proposal.version.version, expected)
        expectedByDocument.set(proposal.document.id, proposal.version.version)
      }
      const direction = snapshot
        .map((proposal) => `Operator confirmed ${proposal.document.id} v${proposal.version.version}.`)
        .join(' ')
      return recoverWorkOrder(state.order.id, requestId.current, direction)
    },
    onMutate: (): MutationWorkspace => ({ workspace }),
    onSuccess: (_data, _variables, started) => {
      if (started) void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(started.workspace, item.task.id) })
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
      className="space-y-3 rounded-lg border border-border bg-card px-3 py-3"
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
          <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded border border-edge bg-surface p-2 font-sans text-sm text-foreground">
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
                  search={{ document: proposal.document.id, tab: 'changes', target: proposal.version.version }}
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
        <Button size="sm" disabled={!canConfirm || !canRecover || mutation.isPending} onClick={() => mutation.mutate()}>
          <Check aria-hidden />
          {mutation.isPending ? 'Confirming and recovering…' : actionLabel}
        </Button>
        <Link to="/inbox" search={{ task: item.task.id }} className="text-xs font-medium text-primary hover:underline">
          Review or dismiss proposals
        </Link>
      </div>
      {!canConfirm && <p className="text-xs text-muted">Document confirmation capability is required.</p>}
      {!canRecover && <p className="text-xs text-muted">Work recovery capability is required.</p>}
      {mutation.error != null && (
        <p className="text-xs text-failure">{errorMessage(mutation.error, 'Could not confirm and recover.')}</p>
      )}
    </section>
  )
}

export function WorkOrderRecoveryCard({
  item,
  state,
}: {
  item: ActivityItem
  state: CurrentExecutionState | undefined
}) {
  if (!state || !hasWorkerRecovery(item, state)) return null
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
  if (state.kind === 'verification_checkpoint') return <VerificationCheckpointRecovery item={item} state={state} />
  return <WorkOrderRecoveryState item={item} state={state} />
}

// component-web-task-surfaces: the sealed verification checkpoint names its reason, required act,
// submitted head and grounds. Recovery returns the order to verify at that head;
// unresolved operations need the typed disposition, never free-text direction.
function VerificationCheckpointRecovery({ item, state }: { item: ActivityItem; state: CurrentExecutionState }) {
  const { order } = state
  const checkpoint = order.checkpoint?.verification
  const canOperateGates = useWorkspaceCapability('operate_gates')
  const canRecoverWork = useWorkspaceCapability('recover_work')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const mutation = useMutation({
    mutationFn: () => recoverWorkOrder(order.id, requestId.current),
    onMutate: (): MutationWorkspace => ({ workspace }),
    onSuccess: (_data, _variables, started) => {
      if (started) void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(started.workspace, item.task.id) })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  if (!checkpoint) return null
  const operations = checkpoint.operation_ids ?? []
  return (
    <section
      aria-label="Verification checkpoint"
      className="space-y-3 rounded-lg border border-border bg-card px-3 py-3"
    >
      <div className="flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
        <div className="min-w-0 space-y-1 leading-5">
          <p className="text-[11px] font-semibold uppercase tracking-[0.08em] text-attention">{state.title}</p>
          <p className="whitespace-pre-wrap break-words text-sm text-foreground">{checkpoint.reason}</p>
        </div>
      </div>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs leading-5">
        <dt className="text-muted">Required action</dt>
        <dd className="whitespace-pre-wrap break-words font-medium text-foreground">{checkpoint.required_action}</dd>
        <dt className="text-muted">Submitted head</dt>
        <dd className="break-all font-mono" title={checkpoint.head_sha}>
          {checkpoint.head_sha.slice(0, 12)}
        </dd>
        <dt className="text-muted">Context</dt>
        <dd className="break-all">
          <a href={`#verification-${checkpoint.context_id}`} className="font-mono text-primary hover:underline">
            {checkpoint.context_id}
          </a>
        </dd>
        <dt className="text-muted">Replay</dt>
        <dd>{state.retry}</dd>
      </dl>
      {checkpoint.grounds.length > 0 && (
        <VerificationCheckpointGrounds
          taskId={item.task.id}
          contextId={checkpoint.context_id}
          grounds={checkpoint.grounds}
          className="leading-5 text-foreground"
        />
      )}
      {operations.length > 0 ? (
        <div className="space-y-1 rounded border border-border bg-surface/60 p-2 text-xs leading-5 text-muted">
          <p className="font-medium text-foreground">Typed disposition required</p>
          <p>
            Recovery must record an applied, not applied or unknown disposition for{' '}
            <span className="font-mono">{operations.join(', ')}</span> through the authenticated recovery route. A
            free-text direction cannot authorize their replay.
          </p>
        </div>
      ) : (
        canOperateGates && (
          <Button
            variant="secondary"
            size="sm"
            disabled={!canRecoverWork || mutation.isPending}
            onClick={() => mutation.mutate()}
          >
            <RotateCcw aria-hidden />
            {mutation.isPending ? 'Recovering…' : 'Recover verification'}
          </Button>
        )
      )}
      {canOperateGates && !canRecoverWork && operations.length === 0 && (
        <p className="text-xs text-muted">Work recovery capability is required.</p>
      )}
      {mutation.error != null && (
        <p role="alert" className="text-xs text-failure">
          {errorMessage(mutation.error, 'Could not recover the verification checkpoint.')}
        </p>
      )}
    </section>
  )
}

function WorkOrderRecoveryState({ item, state }: { item: ActivityItem; state: CurrentExecutionState }) {
  const { order } = state
  const canOperateGates = useWorkspaceCapability('operate_gates')
  const canRecoverWork = useWorkspaceCapability('recover_work')
  const canConfirmDocuments = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const requestId = useRef(crypto.randomUUID())
  const drawerId = useId()
  const directionRef = useRef<HTMLTextAreaElement>(null)
  const [checkoutResolved, setCheckoutResolved] = useState(false)
  const requestedDecision = order.checkpoint?.decision_request?.trim() ?? ''
  const authorityConflict = order.checkpoint?.class === 'authority_conflict'
  const citedDocuments = citedDocumentsOf(order.checkpoint?.citations ?? [])
  const resolvedDirection = checkpointResolvedDirection(order.checkpoint?.class, citedDocuments)
  const [direction, setDirection] = useState(resolvedDirection)
  const [attachingContext, setAttachingContext] = useState(false)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [focusDirection, setFocusDirection] = useState(false)
  // 'resuming' follows a successful confirm-and-resume until refreshed task
  // detail arrives; the server's work-order clock recovers an answered
  // conflict on its next tick (req-260820-394cac AC-2.6).
  const [resume, setResume] = useState<'idle' | 'resuming' | 'refreshed'>('idle')
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
  useEffect(() => {
    if (!drawerOpen || !focusDirection) return
    directionRef.current?.focus()
    setFocusDirection(false)
  }, [drawerOpen, focusDirection])
  const answered = authorityConflict && citedDocuments.length > 0 && citedDocuments.every(isAnswered)
  // A refreshed order that is still released and not answered is an AC-2.7
  // case: the operator recovers it, so the drawer opens on the recovery form.
  useEffect(() => {
    if (resume !== 'refreshed' || answered) return
    setResume('idle')
    setDrawerOpen(true)
  }, [resume, answered])
  const checkpointReleased = order.last_failure_message === 'operator checkpoint reached'
  const mutation = useMutation({
    mutationFn: () => recoverWorkOrder(order.id, requestId.current, direction.trim() || undefined),
    onMutate: (): MutationWorkspace => ({ workspace }),
    onSuccess: (_data, _variables, started) => {
      if (started) void queryClient.invalidateQueries({ queryKey: taskDetailQueryKey(started.workspace, item.task.id) })
      void queryClient.invalidateQueries({ queryKey: ['activity'] })
    },
  })
  // A document confirmation changes the versions every confirm control reads,
  // so those controls stay disabled until task detail is read again. A failed
  // read keeps the old projection mounted; it is offered a read-only retry and
  // never re-enables confirmation (component-web-task-surfaces).
  const [projection, setProjection] = useState<'current' | 'refreshing' | 'stale'>('current')
  const refreshProjection = async () => {
    setProjection('refreshing')
    try {
      await queryClient.invalidateQueries(
        { queryKey: taskDetailQueryKey(workspace, item.task.id) },
        { throwOnError: true },
      )
      setProjection('current')
      return true
    } catch {
      setProjection('stale')
      return false
    }
  }
  const refreshDocumentFamilies = async () => {
    const [current] = await Promise.all([
      refreshProjection(),
      queryClient.invalidateQueries({ queryKey: ['pending-proposals', workspace] }),
      queryClient.invalidateQueries({ queryKey: ['requirements', workspace] }),
      queryClient.invalidateQueries({ queryKey: ['system-designs', workspace] }),
    ])
    return current
  }
  const retryProjection = async () => {
    if (await refreshProjection()) setResume((current) => (current === 'resuming' ? 'refreshed' : current))
  }
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
        ? confirmRequirementVersion(citation.document_id, proposal.version, citation.current_confirmed_version)
        : confirmSystemDesignVersion(citation.document_id, proposal.version, citation.current_confirmed_version)
    },
    onMutate: () => setProjection('refreshing'),
    onSettled: refreshDocumentFamilies,
  })
  const bulk = authorityConflict ? bulkConfirmation(citedDocuments) : undefined
  // Confirm and resume: the click's plan is frozen, confirmations run in
  // ascending version order per document with If-Match advancing from the
  // version confirmed before, and nothing calls the recovery route.
  const confirmAll = useMutation({
    mutationFn: async (plan: BulkConfirmation) => {
      for (const step of plan.documents) {
        let expected = step.expected
        for (const version of step.versions) {
          await (step.kind === 'requirement'
            ? confirmRequirementVersion(step.id, version, expected)
            : confirmSystemDesignVersion(step.id, version, expected))
          expected = version
        }
      }
    },
    onMutate: () => setProjection('refreshing'),
    onSuccess: () => setResume('resuming'),
    onSettled: async (_data, error) => {
      const current = await refreshDocumentFamilies()
      if (error == null && current) setResume('refreshed')
    },
  })
  const documentDecisionPending = confirmProposal.isPending || confirmAll.isPending || projection !== 'current'
  const projectionAlert = projection === 'stale' && (
    <div role="alert" className="flex flex-wrap items-center gap-2 text-xs text-failure">
      <span>Could not refresh this task after the confirmation. Refresh it before confirming again.</span>
      <Button variant="secondary" size="sm" onClick={() => void retryProjection()}>
        <RotateCcw aria-hidden /> Refresh
      </Button>
    </div>
  )

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
  const recoveryFormValid =
    !mutation.isPending &&
    (!checkoutBlocked || checkoutResolved) &&
    (!checkpointReleased || direction.trim().length > 0)
  const recoveryForm = (
    <>
      <div className="space-y-2 text-xs leading-5 text-muted">
        {checkpointReleased && authorityConflict && citedDocuments.length > 0 ? (
          <AuthorityConflictGuidance documents={citedDocuments} />
        ) : checkpointReleased ? (
          <p>
            A decision is required before this work can continue. Recovery without direction will repeat the checkpoint.
          </p>
        ) : (
          <p>You can add an optional instruction for the next attempt.</p>
        )}
        {checkpointReleased && !requestedDecision && order.progress?.trim() && (
          <div className="space-y-1.5">
            <p className="font-medium text-foreground">Agent checkpoint message</p>
            <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded border border-edge bg-surface p-2 font-sans text-sm text-foreground">
              {order.progress}
            </pre>
          </div>
        )}
        {checkpointReleased && authorityConflict && (
          <section className="space-y-2" aria-label="Authority conflict references">
            <p className="font-medium text-foreground">Authority in question</p>
            {citedDocuments.map((document) => (
              <CitedDocumentBlock
                key={document.key}
                document={document}
                canConfirm={canConfirmDocuments}
                activeKey={
                  confirmProposal.variables
                    ? `${confirmProposal.variables.citation.document_id}:${confirmProposal.variables.proposal.version ?? 0}`
                    : ''
                }
                isPending={documentDecisionPending}
                error={confirmProposal.error}
                onConfirm={(proposal) =>
                  confirmProposal.mutate({
                    citation: { ...document.citations[0], current_confirmed_version: document.currentConfirmedVersion },
                    proposal,
                  })
                }
              />
            ))}
          </section>
        )}
        <div className="flex flex-wrap items-center justify-between gap-2 rounded border border-border bg-surface/60 p-2">
          <span>
            <span className="font-medium text-foreground">Attached context: </span>
            {(item.task.context?.requirements?.length ?? 0) + (item.task.context?.designs?.length ?? 0) === 0
              ? 'None'
              : `${item.task.context?.requirements?.length ?? 0} requirement(s), ${item.task.context?.designs?.length ?? 0} design document(s)`}
          </span>
          {canOperateGates && (
            <Button variant="secondary" size="sm" onClick={() => setAttachingContext(true)}>
              <Link2 aria-hidden /> Attach context
            </Button>
          )}
        </div>
        <label className="block space-y-1.5">
          <span className="font-medium text-foreground">Operator direction</span>
          <textarea
            ref={directionRef}
            aria-label="Operator direction"
            maxLength={4096}
            rows={4}
            value={direction}
            onChange={(event) => setDirection(event.target.value)}
            placeholder="State the decision or instruction the agent should follow."
            className="w-full resize-y rounded-md border border-edge bg-surface px-3 py-2 text-sm text-foreground outline-none placeholder:text-faint focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
      </div>
      {order.last_failure_detail && (
        <details className="text-xs text-muted">
          <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
            Show technical details
          </summary>
          <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-edge bg-surface p-2 font-mono">
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
          <label className="flex cursor-pointer items-start gap-2 rounded border border-border bg-surface/60 p-2 focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-primary">
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
      <Button
        variant="secondary"
        size="sm"
        disabled={!canRecoverWork || !recoveryFormValid}
        onClick={() => mutation.mutate()}
      >
        <RotateCcw aria-hidden />
        {mutation.isPending ? 'Retrying…' : actionLabel}
      </Button>
      {!canRecoverWork && <p className="text-xs text-muted">Work recovery capability is required.</p>}
      {mutation.error != null && <p className="text-xs text-failure">{String(mutation.error)}</p>}
      {attachingContext && <TaskContextAttachmentDialog task={item.task} onClose={() => setAttachingContext(false)} />}
    </>
  )

  if (!requestedDecision) {
    return (
      <section aria-label="Work order recovery" className="space-y-3 rounded-lg border border-border bg-card px-3 py-3">
        <div className="flex items-start gap-2">
          <TriangleAlert className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
          <div className="min-w-0 space-y-1 text-xs leading-5 text-muted">
            <p className="font-medium text-attention">{state.title}</p>
            <p>{state.nextAction}</p>
          </div>
        </div>
        {projectionAlert}
        {recoveryForm}
      </section>
    )
  }

  // component-web-task-surfaces, Implementation checkpoints: a decision request
  // renders as one row (the server label, the request clamped to two lines,
  // Review, and at most one primary act) over a drawer holding the rest
  // (req-260820-394cac AC-1.2).
  const resuming = resume === 'resuming' || (resume === 'refreshed' && answered)
  const confirmLabel =
    bulk == null
      ? ''
      : bulk.count === 1
        ? `Confirm v${bulk.documents.find((step) => step.versions.length > 0)?.versions[0]} & resume`
        : `Confirm ${bulk.count} revisions & resume`
  return (
    <section
      aria-label="Checkpoint decision and recovery"
      className="space-y-3 rounded-lg border border-border bg-card px-3 py-3"
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <span aria-hidden className="mt-1.5 size-2 shrink-0 rounded-full bg-attention-dot" />
        <div className="min-w-0 flex-1 basis-56 leading-5">
          <p className="text-sm font-semibold text-foreground">{state.title}</p>
          <p
            data-testid="checkpoint-decision-request"
            className={cn('whitespace-pre-wrap break-words text-xs text-muted', !drawerOpen && 'line-clamp-2')}
          >
            {requestedDecision}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            aria-expanded={drawerOpen}
            aria-controls={drawerId}
            onClick={() => setDrawerOpen((open) => !open)}
          >
            Review
          </Button>
          {resuming ? (
            <p role="status" className="flex items-center gap-1.5 text-xs font-medium text-positive">
              <Check className="size-4" aria-hidden /> Confirmed — resuming
            </p>
          ) : bulk != null ? (
            <Button
              size="sm"
              disabled={!canConfirmDocuments || documentDecisionPending}
              onClick={() => confirmAll.mutate(bulk)}
            >
              <Check aria-hidden />
              {confirmAll.isPending ? 'Confirming…' : confirmLabel}
            </Button>
          ) : (
            <Button
              size="sm"
              onClick={() => {
                setDrawerOpen(true)
                setFocusDirection(true)
              }}
            >
              Respond
            </Button>
          )}
        </div>
      </div>
      {bulk != null && !resuming && !canConfirmDocuments && (
        <p className="text-xs text-muted">Document confirmation capability is required.</p>
      )}
      {projectionAlert}
      {confirmAll.error != null && (
        <p role="alert" className="text-xs text-failure">
          {errorMessage(confirmAll.error, 'Could not confirm the cited revisions.')}
        </p>
      )}
      <div id={drawerId} hidden={!drawerOpen} className="space-y-3 border-t border-border pt-3">
        <div className="space-y-2 text-xs leading-5 text-muted">
          {checkpointReleased && order.progress?.trim() && (
            <details>
              <summary className="cursor-pointer rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
                Show full progress report
              </summary>
              <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap rounded border border-edge bg-surface p-2 font-sans text-sm text-foreground">
                {order.progress}
              </pre>
            </details>
          )}
        </div>
        {recoveryForm}
      </div>
    </section>
  )
}

function isAnswered(document: CitedDocument) {
  return document.newerConfirmed && document.proposals.length === 0
}

interface BulkConfirmation {
  count: number
  documents: Array<{
    id: string
    kind: WorkOrderCheckpointCitation['document_kind']
    expected: number
    versions: number[]
  }>
}

// The cited documents' pending proposals that one confirm-and-resume act
// decides (component-web-task-surfaces, Implementation checkpoints). It is
// offered only when every cited document carries pending proposals with known
// versions and a known current confirmed version; otherwise the decision
// belongs in the drawer.
function bulkConfirmation(documents: CitedDocument[]): BulkConfirmation | undefined {
  if (documents.length === 0) return undefined
  const steps: BulkConfirmation['documents'] = []
  for (const document of documents) {
    const versions = document.proposals.flatMap((proposal) => (proposal.version == null ? [] : [proposal.version]))
    if (
      versions.length === 0 ||
      versions.length !== document.proposals.length ||
      !Number.isFinite(document.currentConfirmedVersion)
    )
      return undefined
    const [first] = document.citations
    steps.push({
      id: first.document_id,
      kind: first.document_kind,
      expected: document.currentConfirmedVersion,
      versions: [...versions].sort((left, right) => left - right),
    })
  }
  const count = steps.reduce((total, step) => total + step.versions.length, 0)
  return { count, documents: steps }
}

// One cited document of an authority-conflict checkpoint. A checkpoint cites
// clauses, so one document can appear under several sections or versions; the
// card decides per document (component-web-task-surfaces, Implementation
// checkpoints): its newer-confirmed state holds only when every citation of it
// is newer-confirmed, and each pending proposal appears once.
interface CitedDocument {
  key: string
  citations: WorkOrderCheckpointCitation[]
  currentConfirmedVersion: number
  newerConfirmed: boolean
  proposals: WorkOrderCheckpointPendingProposal[]
}

function citedDocumentsOf(citations: WorkOrderCheckpointCitation[]): CitedDocument[] {
  const documents = new Map<string, CitedDocument>()
  for (const citation of citations) {
    const key = `${citation.document_kind}:${citation.document_id}`
    const document = documents.get(key)
    if (!document) {
      documents.set(key, {
        key,
        citations: [citation],
        currentConfirmedVersion: citation.current_confirmed_version,
        newerConfirmed: citation.newer_confirmed,
        proposals: [...citation.pending_proposals],
      })
      continue
    }
    document.citations.push(citation)
    document.currentConfirmedVersion = Math.max(document.currentConfirmedVersion, citation.current_confirmed_version)
    document.newerConfirmed &&= citation.newer_confirmed
    for (const proposal of citation.pending_proposals) {
      if (!document.proposals.some((seen) => seen.version === proposal.version)) document.proposals.push(proposal)
    }
  }
  for (const document of documents.values()) {
    document.proposals.sort((left, right) => (left.version ?? 0) - (right.version ?? 0))
  }
  return [...documents.values()]
}

function documentName(document: CitedDocument) {
  return document.citations[0].document_title || document.citations[0].document_id
}

function nameList(documents: CitedDocument[]) {
  const names = documents.map(documentName)
  return names.length <= 2 ? names.join(' and ') : `${names.slice(0, -1).join(', ')}, and ${names[names.length - 1]}`
}

// The cited documents with pending proposals ask for a confirmation; the rest
// ask for authorship (req-260820-394cac AC-3.2). Either decision is made on the
// documents themselves, and recovery with direction follows it.
function AuthorityConflictGuidance({ documents }: { documents: CitedDocument[] }) {
  const pending = documents.filter((document) => document.proposals.length > 0)
  const unproposed = documents.filter((document) => document.proposals.length === 0)
  if (pending.length === 0) {
    return (
      <p>
        No cited document has a pending revision. Author a revision of {nameList(unproposed)}, then recover the order
        with direction.
      </p>
    )
  }
  return (
    <div className="space-y-1">
      <p>
        Confirm or dismiss the pending {pending.length === 1 ? 'revision' : 'revisions'} of {nameList(pending)}. That
        decision settles the conflict; recover the order with direction after it.
      </p>
      {unproposed.length > 0 && (
        <p>No revision is waiting for {nameList(unproposed)}. Author one if the conflict remains.</p>
      )}
    </div>
  )
}

function checkpointResolvedDirection(checkpointClass: string | undefined, documents: CitedDocument[]) {
  if (
    checkpointClass !== 'authority_conflict' ||
    documents.length === 0 ||
    !documents.every((item) => item.newerConfirmed)
  )
    return ''
  const versions = documents
    .map((document) => `${document.citations[0].document_id} v${document.currentConfirmedVersion}`)
    .join(', ')
  return `Continue under the newly confirmed authority: ${versions}.`
}

function CitedDocumentBlock({
  document,
  canConfirm,
  activeKey,
  isPending,
  error,
  onConfirm,
}: {
  document: CitedDocument
  canConfirm: boolean
  activeKey: string
  isPending: boolean
  error: Error | null
  onConfirm: (proposal: WorkOrderCheckpointPendingProposal) => void
}) {
  const [first] = document.citations
  return (
    <div className="space-y-2 rounded border border-border bg-surface/60 p-2">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          {first.document_kind === 'requirement' ? (
            <Link
              to="/requirements"
              search={{ requirement: first.document_id }}
              hash={first.statement_or_section_id?.toLowerCase()}
              className="font-medium text-primary hover:underline"
            >
              {documentName(document)}
            </Link>
          ) : (
            <Link
              to="/system-design"
              search={{ document: first.document_id }}
              className="font-medium text-primary hover:underline"
            >
              {documentName(document)}
            </Link>
          )}
          <ul aria-label={`Citations of ${documentName(document)}`} className="text-muted">
            {document.citations.map((citation) => (
              <li key={`${citation.cited_version}:${citation.statement_or_section_id ?? ''}`}>
                Cited v{citation.cited_version}
                {citation.statement_or_section_id
                  ? ` · ${citation.statement_or_section_id}`
                  : ' · document-level reference'}
              </li>
            ))}
          </ul>
        </div>
        <Badge variant={document.newerConfirmed ? 'positive' : 'attention'}>
          {document.newerConfirmed
            ? `Now confirmed at v${document.currentConfirmedVersion}`
            : `Still confirmed at v${document.currentConfirmedVersion}`}
        </Badge>
      </div>
      {document.proposals.length === 0 ? (
        <p className="text-xs text-muted">No revision is waiting for a decision. Author one if the conflict remains.</p>
      ) : (
        <div className="space-y-2">
          <p className="text-xs font-medium text-attention">
            {document.proposals.length === 1
              ? 'A revision is waiting for your decision.'
              : `${document.proposals.length} revisions are waiting for your decision.`}
          </p>
          <div className="space-y-2">
            {document.proposals.map((proposal) => {
              const key = `${first.document_id}:${proposal.version ?? 0}`
              return (
                <div key={key} className="flex flex-wrap items-center gap-2">
                  <Button size="sm" disabled={!canConfirm || isPending} onClick={() => onConfirm(proposal)}>
                    <Check aria-hidden />
                    {isPending && activeKey === key ? 'Confirming…' : `Confirm v${proposal.version}`}
                  </Button>
                  {canConfirm && (
                    <Link
                      to="/inbox"
                      search={{ document: first.document_id, tier: first.document_kind }}
                      className="text-xs font-medium text-primary hover:underline"
                    >
                      Review or dismiss v{proposal.version}
                    </Link>
                  )}
                  {proposal.version != null && (
                    <CheckpointProposalDiff
                      kind={first.document_kind}
                      documentId={first.document_id}
                      baseVersion={document.currentConfirmedVersion}
                      proposedVersion={proposal.version}
                    />
                  )}
                </div>
              )
            })}
          </div>
          {!canConfirm && <p className="text-xs text-muted">Document confirmation capability is required.</p>}
          {error != null && activeKey.startsWith(`${first.document_id}:`) && (
            <p className="text-xs text-failure">{errorMessage(error, 'Could not confirm this revision.')}</p>
          )}
        </div>
      )}
    </div>
  )
}
