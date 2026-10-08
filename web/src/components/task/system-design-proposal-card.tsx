import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { AlertTriangle, Check, FileText } from 'lucide-react'
import { confirmSystemDesignVersion, fetchSystemDesigns, SystemDesignConflictError } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type {
  ActivityItem,
  PendingProposal,
  SystemDesignSummary,
  SystemDesignVersionSummary,
  Task,
  WaitingProposal,
} from '../../lib/types'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

export interface Proposal {
  document: SystemDesignSummary['document']
  version: SystemDesignVersionSummary
  /** The confirmed version the document is on, for the confirm route's If-Match. */
  expected: number
}

// req-260810-70ce2f REQ-1 and req-260810-23b69f AC-2.1: only the task's own
// implementation-origin requirement and System Design proposals withhold its
// verify and review claims. The server names them in `waiting_proposals`; this
// copy never infers a wait from the broader attention signal.
export function waitingGateCopy(blockers: readonly Pick<WaitingProposal, 'tier'>[]) {
  const tiers = blockers.map((blocker) => blocker.tier as string)
  const noun =
    tiers.length > 0 && tiers.every((tier) => tier === 'requirement')
      ? 'requirement'
      : tiers.length > 0 && tiers.every((tier) => tier === 'system_design')
        ? 'System Design'
        : 'document'
  const proposal = tiers.length > 1 ? 'proposals' : 'proposal'
  return {
    headline: `Verification and review are waiting on a ${noun} decision`,
    explanation: `Verification and review cannot be claimed until you confirm or dismiss the task's pending ${proposal}.`,
    link: `Confirm or dismiss the ${proposal}`,
  }
}

/** The accessible name of a proposal notice that reports a real claim wait. */
export const waitingRegionLabel = 'Verification and review are waiting on a document decision'
/** The accessible name of a notice whose proposals hold nothing. */
export const signalOnlyRegionLabel = 'Pending proposals from this task'

const tierLabels: Record<string, string> = {
  requirement: 'Requirement',
  system_design: 'System Design',
  decision: 'Decision',
}

function proposalLabel(proposal: { tier: string; id: string; version?: number }) {
  const version = proposal.tier === 'decision' || proposal.version == null ? '' : ` v${proposal.version}`
  return `${tierLabels[proposal.tier] ?? 'Document'} ${proposal.id}${version}`
}

// A task-authored proposal that is pending but withholds no claim: decisions,
// and any requirement or System Design version the server did not name as a
// blocker. Review does not include it and nothing waits on it
// (req-260810-23b69f AC-2.1, AC-2.2; REQ-3).
export function signalOnlyCopy(proposals: readonly Pick<PendingProposal, 'tier'>[]) {
  const plural = proposals.length > 1
  const decisions = proposals.length > 0 && proposals.every((proposal) => proposal.tier === 'decision')
  const noun = `${decisions ? 'decision ' : ''}${plural ? 'proposals' : 'proposal'}`
  return {
    headline: `Review will not include ${plural ? 'these' : 'this'} pending ${noun}`,
    explanation: `Verification and review are not waiting on ${plural ? 'them' : 'it'}.`,
  }
}

export interface ProposalReviewEffect {
  /** The server's claim-wait projection for this task. */
  waiting: boolean
  /** The proposals that withhold verify and review claims. */
  blockers: WaitingProposal[]
  /** Task-authored pending proposals that withhold nothing. */
  signalOnly: PendingProposal[]
}

/**
 * Splits the task's own pending proposals by their actual effect on review.
 * Only a task carrying the pending-authority attention signal has an effect to
 * state; the wait itself is read from `proposal_claim_waiting` and
 * `waiting_proposals`, never from the proposal tiers.
 */
export function proposalReviewEffect(
  item: Pick<ActivityItem, 'task' | 'pending_authority' | 'proposal_claim_waiting' | 'waiting_proposals'>,
  pending: readonly PendingProposal[],
): ProposalReviewEffect | undefined {
  if (item.pending_authority !== true) return undefined
  const waiting = item.proposal_claim_waiting === true
  const blockers = waiting ? (item.waiting_proposals ?? []) : []
  const blocking = (proposal: PendingProposal) =>
    blockers.some(
      (blocker) => blocker.tier === proposal.tier && blocker.id === proposal.id && blocker.version === proposal.version,
    )
  const signalOnly = pending.filter(
    (proposal) =>
      proposal.origin_type === 'task' &&
      proposal.origin_id === item.task.id &&
      (proposal.tier === 'decision' || proposal.tier === 'requirement' || proposal.tier === 'system_design') &&
      !blocking(proposal),
  )
  return { waiting, blockers, signalOnly }
}

export function hasProposalReviewNotice(effect: ProposalReviewEffect | undefined): effect is ProposalReviewEffect {
  return effect != null && (effect.waiting || effect.signalOnly.length > 0)
}

export function proposalNoticeLabel(effect: ProposalReviewEffect) {
  return effect.waiting ? waitingRegionLabel : signalOnlyRegionLabel
}

function proposalLinkText(effect: ProposalReviewEffect) {
  return effect.blockers.length + effect.signalOnly.length > 1
    ? 'Confirm or dismiss the proposals'
    : 'Confirm or dismiss the proposal'
}

/**
 * The statement of each task-authored proposal's effect on review. Blockers are
 * named apart from signal-only proposals, so a decision is never presented as
 * the reason verification or review is waiting.
 */
export function ProposalReviewNotice({
  effect,
  taskId,
  pending,
  showLink = true,
}: {
  effect: ProposalReviewEffect
  taskId: string
  pending: readonly PendingProposal[]
  showLink?: boolean
}) {
  if (!hasProposalReviewNotice(effect)) return null
  const gate = waitingGateCopy(effect.blockers)
  const signal = signalOnlyCopy(effect.signalOnly)
  const title = (blocker: WaitingProposal) =>
    pending.find(
      (proposal) =>
        proposal.tier === blocker.tier && proposal.id === blocker.id && proposal.version === blocker.version,
    )?.title
  return (
    <div className="flex items-start gap-2">
      <AlertTriangle className="mt-0.5 size-4 shrink-0 text-attention" aria-hidden />
      <div className="min-w-0 space-y-2 text-xs leading-5 text-muted">
        {effect.waiting && (
          <div>
            <p className="font-medium text-attention">{gate.headline}</p>
            <p>{gate.explanation}</p>
            {effect.blockers.length > 0 && (
              <ul aria-label="Proposals verification and review are waiting on" className="mt-1 list-disc pl-4">
                {effect.blockers.map((blocker) => (
                  <li key={`${blocker.tier}:${blocker.id}:${blocker.version}`}>
                    <span className="font-medium">{proposalLabel(blocker)}</span>
                    {title(blocker) ? ` — ${title(blocker)}` : ''}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
        {effect.signalOnly.length > 0 && (
          <div>
            <p className={effect.waiting ? 'font-medium' : 'font-medium text-attention'}>{signal.headline}</p>
            <p>{signal.explanation}</p>
            <ul aria-label="Proposals review will not include" className="mt-1 list-disc pl-4">
              {effect.signalOnly.map((proposal) => (
                <li key={`${proposal.tier}:${proposal.id}:${proposal.version ?? ''}`}>
                  <span className="font-medium">{proposalLabel(proposal)}</span>
                  {proposal.title ? ` — ${proposal.title}` : ''}
                </li>
              ))}
            </ul>
          </div>
        )}
        {showLink && (
          <Link
            to="/pending-proposals"
            search={{ task: taskId }}
            className="inline-block font-medium text-primary hover:underline"
          >
            {proposalLinkText(effect)}
          </Link>
        )}
      </div>
    </div>
  )
}

// Document plus version, not the object reference: a refetch rebuilds these
// records, and the in-flight and failed states have to keep pointing at the
// same proposal across it.
export function proposalIdentity(proposal: Proposal) {
  return `${proposal.document.id}:${proposal.version.version}`
}

// A task's own unresolved proposals on its own attached documents, and nothing
// else. Both halves of that scope are load-bearing. Origin is what keeps this
// own-proposal rendering a carve-out rather than a second attention surface — this task renders what it
// raised, never another task's pending versions. Attachment is what keeps the
// carve-out inside the task's declared context: the read is the workspace-wide
// collection, so a proposal this task raised against a document it does not
// carry stays on the document's own attention surface, where it belongs.
//
// The collection is shared with the task and board filters, which read only
// each document's identity, so neither the pending list nor its resolution
// flags are assumed present — a partial payload leaves a task with no card,
// never a detail surface that fails to render.
function selfOriginated(task: Task, designs: SystemDesignSummary[]): Proposal[] {
  const attached = new Set((task.context?.designs ?? []).map((design) => design.id))
  return designs
    .filter((item) => attached.has(item.document.id))
    .flatMap((item) =>
      (item.pending_versions ?? [])
        .filter((version) => version.origin_task_id === task.id && !version.confirmed && !version.dismissed)
        .map((version) => ({ document: item.document, version, expected: item.document.current_version ?? 0 })),
    )
}

/**
 * The proposals this task raised on the documents it carries, over the existing
 * System Design read API. The timeline asks before it renders a tail slot, so a
 * task with nothing pending grows no empty row; the query key is the one the
 * document surfaces and the task filters already use, so this is normally a
 * cache read.
 */
export function useSystemDesignProposals(task: Task): Proposal[] {
  const { workspace } = useWorkspaceSelection()
  const designs = useQuery({
    queryKey: ['system-designs', workspace],
    queryFn: fetchSystemDesigns,
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  return selfOriginated(task, designs.data ?? [])
}

/**
 * The origin-task proposal card. When an implement session
 * proposes a System Design revision, the confirm affordance used to live only
 * on the per-document attention surface — a different page from the task the
 * pipeline was waiting on. This renders the same decision where it was raised.
 *
 * Presentation only: the read is the existing `/v1/system-designs` collection
 * narrowed client-side to this task's attached documents and its own pending
 * versions, and Confirm posts to the same operator-credentialed
 * route the attention surface uses. The action follows the same
 * `confirm_documents` capability as that route; agents still never confirm. A resolved
 * version stops matching the filter, so the card clears itself — no new state,
 * and the timeline already records what happened.
 *
 * The document surfaces are otherwise untouched (component-web-document-surfaces):
 * the document canvas keeps its
 * attention surface as the only document-side rendering, and drift and
 * staleness are not rendered here at all.
 */
export function SystemDesignProposalCard({
  task,
  proposals,
  effect,
  pending = [],
}: {
  task: Task
  proposals: Proposal[]
  /** The task's proposal effect on review, when it is in review. */
  effect?: ProposalReviewEffect
  pending?: readonly PendingProposal[]
}) {
  const canConfirm = useWorkspaceCapability('confirm_documents')
  const { workspace } = useWorkspaceSelection()
  const client = useQueryClient()
  const confirm = useMutation({
    mutationFn: (proposal: Proposal) =>
      confirmSystemDesignVersion(proposal.document.id, proposal.version.version, proposal.expected),
    // Refetching the collection is what removes the card: the confirmed
    // version is no longer pending, so it stops matching. A conflict means
    // the document moved under us, and the refreshed list is the answer to
    // that too.
    onSettled: (_data, error) => {
      if (error == null || error instanceof SystemDesignConflictError)
        void client.invalidateQueries({ queryKey: ['system-designs', workspace] })
    },
  })

  if (proposals.length === 0) return null
  const notice = hasProposalReviewNotice(effect) ? effect : undefined

  return (
    <section
      aria-label={notice?.waiting ? waitingRegionLabel : 'System Design proposals from this task'}
      className="space-y-3 rounded-lg border border-attention/40 bg-attention-soft px-3 py-3"
    >
      {notice && <ProposalReviewNotice effect={notice} taskId={task.id} pending={pending} showLink={false} />}
      {proposals.map((proposal) => {
        const active = confirm.variables != null && proposalIdentity(confirm.variables) === proposalIdentity(proposal)
        return (
          <div key={proposalIdentity(proposal)} className="flex items-start gap-2">
            <FileText className="mt-0.5 size-4 shrink-0 text-attention" />
            <div className="min-w-0 flex-1 space-y-2 text-xs leading-5 text-muted">
              <div>
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
              <Button size="sm" disabled={!canConfirm || confirm.isPending} onClick={() => confirm.mutate(proposal)}>
                <Check />
                {active && confirm.isPending ? 'Confirming…' : `Confirm version ${proposal.version.version}`}
              </Button>
              {!canConfirm && <p>Document confirmation capability is required.</p>}
              {confirm.error != null && active && (
                <p className="text-failure">{errorMessage(confirm.error, 'Could not confirm this version.')}</p>
              )}
            </div>
          </div>
        )
      })}
      {notice && (
        <Link
          to="/pending-proposals"
          search={{ task: task.id }}
          className="inline-block text-xs font-medium text-primary hover:underline"
        >
          {proposalLinkText(notice)}
        </Link>
      )}
    </section>
  )
}
