import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useCallback, useEffect, useRef, useState } from 'react'
import { postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { NextAction, NextActionChoice, NextActionContextProposal } from '../../lib/types'
import { absoluteTime, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { choiceBody } from './choice-bodies'
import { Kbd, useShortcutChoice } from './inbox-keyboard'
import { nextActionQueryKey, nextActionsQueryKey } from './use-next-actions'

// The words the row prints for a proposal's stored source (NA-6).
const sourceWords: Record<string, string> = { triage: 'triage', planning: 'planning', operator: 'operator' }

type Act = 'confirm' | 'dismiss'

// The server choice that decides one proposal, matched by its documented ID
// (feature-operator-next-action NA-3).
function proposalChoice(action: NextAction, proposal: NextActionContextProposal, act: Act) {
  const id = `${act}:task_context:${proposal.kind}:${proposal.target_id}`
  return action.choices.find((choice) => choice.id === id)
}

// Why this page cannot send a choice, or '' when it can: the same reasons the
// shared choice row states.
function unavailable(choice: NextActionChoice | undefined) {
  if (!choice) return 'This page cannot send this choice.'
  if (!choice.allowed) return `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
  if (!choiceBody(choice.id)) return 'This page cannot send this choice.'
  return ''
}

// The sends of one context_proposal card. A run posts each snapshot choice
// once through the shared choice transport, awaiting each before the next; the
// first refusal stops it and nothing is retried or rolled back. One
// synchronous guard covers row and batch sends, and leaving the card abandons
// the unsent remainder. Every post is the same audited act as the row button
// (AC-2.2; DEC-45); there is no bulk route (NA-6).
// `onSent` runs after each recorded act, so the Inbox can tell its own acts
// from other operators' when the item leaves (AC-2.10).
export function useContextProposalSends(taskId: string, onSent?: () => void) {
  const { workspace } = useWorkspaceSelection()
  const sent = useRef(onSent)
  sent.current = onSent
  const client = useQueryClient()
  const inFlight = useRef(false)
  const mounted = useRef(true)
  // The workspace on screen now; a run stops when it differs from the one the
  // click acted in.
  const current = useRef(workspace)
  current.current = workspace
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const run = useCallback(
    async (choices: NextActionChoice[]) => {
      if (inFlight.current || choices.length === 0) return
      inFlight.current = true
      // The workspace the click acted in, kept for the refresh.
      const acted = workspace
      setBusy(true)
      setError(null)
      try {
        for (const choice of choices) {
          if (!mounted.current || current.current !== acted) return
          await postNextActionChoice(acted, choice, undefined)
          sent.current?.()
        }
      } catch (failure) {
        if (mounted.current) setError(failure)
      } finally {
        inFlight.current = false
        if (mounted.current) setBusy(false)
        await Promise.all([
          client.invalidateQueries({ queryKey: nextActionsQueryKey(acted) }),
          client.invalidateQueries({ queryKey: nextActionQueryKey(acted, taskId) }),
          client.invalidateQueries({ queryKey: ['activity'] }),
          client.invalidateQueries({ queryKey: ['task-operations', acted] }),
          client.invalidateQueries({ queryKey: ['pending-proposals', acted] }),
          client.invalidateQueries({ queryKey: taskDetailQueryKey(acted, taskId) }),
        ])
      }
    },
    [client, taskId, workspace],
  )
  return { run, busy, error }
}

export type ContextProposalSends = ReturnType<typeof useContextProposalSends>

// The task-level origin line of a monitor-filed task (NA-6).
export function MonitorOriginLine({ action }: { action: NextAction }) {
  const origin = action.details?.monitor_origin
  if (!origin) return null
  return (
    <p data-monitor-origin className="break-words text-xs text-muted">
      {origin.summary}
      {origin.source_url && (
        <>
          {' · '}
          <a
            href={origin.source_url}
            target="_blank"
            rel="noreferrer"
            className="font-medium text-primary hover:underline"
          >
            View the run
          </a>
        </>
      )}
    </p>
  )
}

// One row per pending proposal, in the server's order: tier, linked title,
// justification, source and proposer, age, and that proposal's own Confirm and
// Dismiss. Target IDs stay in the card's IDs disclosure (NA-6; AC-1.8, AC-2.1).
export function ContextProposalRows({ action, sends }: { action: NextAction; sends: ContextProposalSends }) {
  const proposals = action.details?.context_proposals ?? []
  return (
    <ul data-context-proposals className="divide-y divide-border rounded-md border border-border">
      {proposals.map((proposal) => (
        <li
          key={`${proposal.kind}:${proposal.target_id}`}
          data-context-proposal={proposal.kind}
          className="min-w-0 space-y-1.5 px-3 py-2.5"
        >
          <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
            <span className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted">
              {proposal.tier_label}
            </span>
            <ProposalTitle proposal={proposal} />
          </div>
          {proposal.justification && <p className="break-words text-sm leading-6">{proposal.justification}</p>}
          <p className="flex min-w-0 flex-wrap gap-x-1.5 text-xs text-muted">
            <span>Suggested by {sourceWords[proposal.source] ?? proposal.source}</span>
            {proposal.proposed_by && <span className="break-all">· {proposal.proposed_by}</span>}
            {proposal.created_at && (
              <time dateTime={proposal.created_at} title={absoluteTime(proposal.created_at)}>
                · {relativeTime(proposal.created_at)}
              </time>
            )}
          </p>
          <div className="flex min-w-0 flex-wrap items-start gap-2 text-[13px]">
            {(['confirm', 'dismiss'] as const).map((act) => (
              <ProposalButton key={act} act={act} choice={proposalChoice(action, proposal, act)} sends={sends} />
            ))}
          </div>
        </li>
      ))}
    </ul>
  )
}

function ProposalTitle({ proposal }: { proposal: NextActionContextProposal }) {
  const title = proposal.target_title.trim()
  if (!title) return null
  const className = 'min-w-0 break-words font-medium text-primary hover:underline'
  return proposal.kind === 'requirement' ? (
    <Link to="/requirements" search={{ requirement: proposal.target_id }} className={className}>
      {title}
    </Link>
  ) : (
    <Link to="/system-design" search={{ document: proposal.target_id }} className={className}>
      {title}
    </Link>
  )
}

// A proposal's own Confirm or Dismiss: the server label, with the effect as its
// tooltip and never the endpoint, so no target ID shows outside the
// disclosure.
function ProposalButton({
  act,
  choice,
  sends,
}: {
  act: Act
  choice: NextActionChoice | undefined
  sends: ContextProposalSends
}) {
  const shortcut = useShortcutChoice()
  if (!choice) return null
  const reason = unavailable(choice)
  const keyed = shortcut === choice.id && reason === ''
  return (
    <span className="inline-flex max-w-full min-w-0 flex-col gap-0.5">
      <span className="inline-flex max-w-full min-w-0 items-center gap-1.5">
        <Button
          size="sm"
          variant={act === 'confirm' && reason === '' ? 'default' : 'secondary'}
          className="h-auto min-h-8 max-w-full py-1.5 text-left whitespace-normal [overflow-wrap:anywhere]"
          title={choice.effect}
          data-choice={choice.id}
          data-shortcut={keyed ? 'c' : undefined}
          disabled={sends.busy || reason !== ''}
          onClick={() => void sends.run([choice])}
        >
          {choice.label}
        </Button>
        {keyed && <Kbd>c</Kbd>}
      </span>
      {reason && <span className="text-[11px] text-muted">{reason}</span>}
    </span>
  )
}

// "Confirm all" and "Dismiss all" for two or more proposals: client chrome over
// the server's choices. A click snapshots the matching choices in the
// server's order and sends them one after another (NA-6).
export function ContextProposalBatch({ action, sends }: { action: NextAction; sends: ContextProposalSends }) {
  const proposals = action.details?.context_proposals ?? []
  if (proposals.length < 2) return null
  return (
    <div data-context-batch className="flex min-w-0 flex-wrap items-start gap-2 text-[13px]">
      {(['confirm', 'dismiss'] as const).map((act) => {
        const choices = proposals.map((proposal) => proposalChoice(action, proposal, act))
        const reason = choices.map(unavailable).find((text) => text !== '') ?? ''
        return (
          <span key={act} className="inline-flex max-w-full min-w-0 flex-col gap-0.5">
            <Button
              size="sm"
              variant={act === 'confirm' && reason === '' ? 'default' : 'secondary'}
              data-batch={act}
              disabled={sends.busy || reason !== ''}
              onClick={() => void sends.run(choices.filter((choice): choice is NextActionChoice => Boolean(choice)))}
            >
              {act === 'confirm' ? 'Confirm all' : 'Dismiss all'}
            </Button>
            {reason && <span className="text-[11px] text-muted">{reason}</span>}
          </span>
        )
      })}
    </div>
  )
}

export function ContextProposalError({ sends }: { sends: ContextProposalSends }) {
  if (sends.error == null) return null
  return (
    <p data-context-refusal className="break-words text-xs text-failure">
      {errorMessage(sends.error, 'Could not record this choice.')}
    </p>
  )
}
