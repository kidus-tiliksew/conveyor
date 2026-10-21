import { useQueryClient } from '@tanstack/react-query'
import { ChevronRight, Eye } from 'lucide-react'
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { postNextActionChoice } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { taskDetailQueryKey } from '../../lib/query-keys'
import type { NextAction, NextActionChoice, NextActionContextProposal } from '../../lib/types'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { choiceBody } from './choice-bodies'
import { ContextDocumentPreview } from './context-document-preview'
import { CONTEXT_APPLY_SHORTCUT, Kbd, useShortcutChoice } from './inbox-keyboard'
import { nextActionQueryKey, nextActionsQueryKey } from './use-next-actions'

// The words the checklist prints for a proposal's stored source (NA-6).
const sourceWords: Record<string, string> = { triage: 'triage', planning: 'planning', operator: 'operator' }

type Act = 'confirm' | 'dismiss'

const proposalKey = (proposal: NextActionContextProposal) => `${proposal.kind}:${proposal.target_id}`

const choiceId = (proposal: NextActionContextProposal, act: Act) =>
  `${act}:task_context:${proposal.kind}:${proposal.target_id}`

// The server choice that decides one proposal, matched by its documented ID
// (feature-operator-next-action NA-3).
function proposalChoice(action: NextAction, proposal: NextActionContextProposal, act: Act) {
  const id = choiceId(proposal, act)
  return action.choices.find((choice) => choice.id === id)
}

// Why this page cannot send a choice, or '' when it can: the same reasons the
// shared choice row states (DEC-61).
function unavailable(choice: NextActionChoice | undefined) {
  if (!choice) return 'This page cannot send this choice.'
  if (!choice.allowed) return `Needs the ${choice.capability.replaceAll('_', ' ')} capability.`
  if (!choiceBody(choice.id)) return 'This page cannot send this choice.'
  return ''
}

export function contextProposals(action: NextAction) {
  return action.details?.context_proposals ?? []
}

// The checked state of one context_proposal decision, keyed by proposal kind
// and target ID. A proposal starts checked, a proposal that survives a refetch
// keeps its state, and the state resets when the task or workspace changes
// (NA-6). Only unchecked keys are stored, so a newly listed proposal reads as
// checked. The hook is the one source of a row's state, so another component
// can mirror a row through `checked` and `toggle`.
export function useContextProposalSelection(taskId: string) {
  const { workspace } = useWorkspaceSelection()
  const scope = `${workspace}\u0000${taskId}`
  const [state, setState] = useState<{ scope: string; unchecked: ReadonlySet<string> }>({
    scope,
    unchecked: new Set(),
  })
  // A new task or workspace commits a fresh, all-checked state during render,
  // so a later return to an earlier scope never resurrects its selection.
  if (state.scope !== scope) setState({ scope, unchecked: new Set() })
  const unchecked = state.scope === scope ? state.unchecked : new Set<string>()
  const checked = useCallback(
    (proposal: NextActionContextProposal) => !unchecked.has(proposalKey(proposal)),
    [unchecked],
  )
  const toggle = useCallback(
    (proposal: NextActionContextProposal, value: boolean) => {
      setState((previous) => {
        const next = new Set(previous.scope === scope ? previous.unchecked : [])
        if (value) next.delete(proposalKey(proposal))
        else next.add(proposalKey(proposal))
        return { scope, unchecked: next }
      })
    },
    [scope],
  )
  return { checked, toggle }
}

export type ContextProposalSelection = ReturnType<typeof useContextProposalSelection>

// The sends of one context_proposal decision. A run posts each snapshot
// choice once through the shared choice transport, awaiting each before the
// next; the first refusal stops it, and nothing is retried or rolled back. A
// synchronous guard covers every run, and leaving the card abandons the
// unsent remainder. Every post is the same audited act as a single choice
// (AC-2.2; DEC-45); there is no bulk route (NA-6). `progress` counts the
// posts recorded against the snapshot's size, so a stopped run can say how
// far it got. `onSent` runs after each recorded act, so the Inbox can tell its
// own acts from other operators' when the item leaves (AC-2.10).
export function useContextProposalSends(taskId: string, onSent?: () => void) {
  const { workspace } = useWorkspaceSelection()
  const sent = useRef(onSent)
  sent.current = onSent
  const client = useQueryClient()
  const inFlight = useRef(false)
  const mounted = useRef(true)
  // The workspace and task on screen now; a run stops when either differs
  // from the one the click acted in.
  const scope = `${workspace}\u0000${taskId}`
  const current = useRef(scope)
  current.current = scope
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [progress, setProgress] = useState({ recorded: 0, total: 0 })
  // A run's busy, refusal, and progress belong to the task it acted on.
  const [shown, setShown] = useState(scope)
  if (shown !== scope) {
    setShown(scope)
    setBusy(false)
    setError(null)
    setProgress({ recorded: 0, total: 0 })
  }
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
      // The workspace the click acted in, kept for the refresh, and the scope
      // the run belongs to.
      const acted = workspace
      const runScope = current.current
      const live = () => mounted.current && current.current === runScope
      let recorded = 0
      setBusy(true)
      setError(null)
      setProgress({ recorded, total: choices.length })
      try {
        for (const choice of choices) {
          if (!live()) return
          await postNextActionChoice(acted, choice, undefined)
          recorded += 1
          if (live()) setProgress({ recorded, total: choices.length })
          sent.current?.()
        }
      } catch (failure) {
        if (live()) setError(failure)
      } finally {
        inFlight.current = false
        if (live()) setBusy(false)
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
  return { run, busy, error, progress }
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

function sourceWord(proposal: NextActionContextProposal) {
  return sourceWords[proposal.source] ?? proposal.source
}

function Age({ value }: { value: string }) {
  return (
    <time dateTime={value} title={absoluteTime(value)}>
      {relativeTime(value)}
    </time>
  )
}

// The context_proposal checklist (NA-6; AC-1.8, AC-2.1): one row per pending
// proposal in the server's order, each with a checkbox (checked by default),
// the tier word, the title, a Preview control, and the justification. The
// title and Preview open the document preview panel over the page. The source and age
// print once under the question when every proposal shares them, and on each
// row otherwise. No row prints a Confirm or Dismiss button: each checkbox
// carries the server label of the choice its state selects as its name and
// the other label and both effects as its description, never the endpoint.
// Target IDs and the proposer stay in the IDs disclosure. `compact` is task
// detail's form: one line per row, with a chevron that expands the
// justification.
export function ContextProposalChecklist({
  action,
  selection,
  sends,
  compact = false,
}: {
  action: NextAction
  selection: ContextProposalSelection
  sends: ContextProposalSends
  compact?: boolean
}) {
  const proposals = contextProposals(action)
  // The proposal the preview panel shows, by key, or null while it is closed.
  // A proposal that leaves the list closes the panel (NA-6).
  const [previewKey, setPreviewKey] = useState<string | null>(null)
  const previewIndex = previewKey == null ? -1 : proposals.findIndex((proposal) => proposalKey(proposal) === previewKey)
  if (proposals.length === 0) return null
  const sharedSource = proposals.every((proposal) => proposal.source === proposals[0].source)
  // The age is shared only when the creation times agree, never because two
  // times round to the same label, so each row keeps its own timestamp.
  const sharedAge = proposals.every((proposal) => sameInstant(proposal.created_at, proposals[0].created_at))
  const unmatched = unmatchedChoices(action)
  return (
    <div data-context-checklist={compact ? 'compact' : 'full'} className="min-w-0 space-y-2">
      {sharedSource && (
        <p data-context-source className="text-xs text-muted">
          Suggested by {sourceWord(proposals[0])}
          {sharedAge && proposals[0].created_at && (
            <>
              {' · '}
              <Age value={proposals[0].created_at} />
            </>
          )}
        </p>
      )}
      <ul data-context-proposals className="divide-y divide-border rounded-md border border-border">
        {proposals.map((proposal) => (
          <ChecklistRow
            key={proposalKey(proposal)}
            action={action}
            proposal={proposal}
            selection={selection}
            sends={sends}
            compact={compact}
            source={sharedSource ? undefined : sourceWord(proposal)}
            age={sharedSource && sharedAge ? undefined : proposal.created_at}
            current={previewIndex >= 0 && proposalKey(proposal) === previewKey}
            onPreview={() => setPreviewKey(proposalKey(proposal))}
          />
        ))}
      </ul>
      {previewIndex >= 0 && (
        <ContextDocumentPreview
          proposals={proposals}
          index={previewIndex}
          onIndex={(index) => setPreviewKey(proposalKey(proposals[index]))}
          onClose={() => setPreviewKey(null)}
          selection={selection}
          disabled={sends.busy}
        />
      )}
      {unmatched.length > 0 && (
        <ul data-context-unmatched className="space-y-1 text-xs text-muted">
          {unmatched.map((choice) => (
            <li key={choice.id} className="break-words">
              {choice.label}: This page cannot send this choice.
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function sameInstant(a: string, b: string) {
  const left = Date.parse(a)
  const right = Date.parse(b)
  return Number.isNaN(left) || Number.isNaN(right) ? a === b : left === right
}

// A server choice that names no listed proposal stays visible with its
// reason instead of disappearing (DEC-61).
function unmatchedChoices(action: NextAction) {
  const known = new Set(
    contextProposals(action).flatMap((proposal) => [choiceId(proposal, 'confirm'), choiceId(proposal, 'dismiss')]),
  )
  return action.choices.filter((choice) => !known.has(choice.id))
}

function ChecklistRow({
  action,
  proposal,
  selection,
  sends,
  compact,
  source,
  age,
  current,
  onPreview,
}: {
  action: NextAction
  proposal: NextActionContextProposal
  selection: ContextProposalSelection
  sends: ContextProposalSends
  compact: boolean
  source?: string
  age?: string
  current: boolean
  onPreview: () => void
}) {
  const [expanded, setExpanded] = useState(false)
  const clamped = useClamped(compact ? null : proposal.justification)
  const descriptionId = useId()
  const justificationId = useId()
  const checked = selection.checked(proposal)
  const confirm = proposalChoice(action, proposal, 'confirm')
  const dismiss = proposalChoice(action, proposal, 'dismiss')
  const selected = checked ? confirm : dismiss
  const other = checked ? dismiss : confirm
  const fallback = `${checked ? 'Attach' : 'Dismiss'} ${proposal.target_title.trim() || proposal.tier_label}`
  const reasons = reasonLines([
    ['Attach', unavailable(confirm)],
    ['Dismiss', unavailable(dismiss)],
  ])
  const description = [
    other ? `Other choice: ${other.label}.` : '',
    confirm?.effect ? `${confirm.label}: ${confirm.effect}` : '',
    dismiss?.effect ? `${dismiss.label}: ${dismiss.effect}` : '',
  ]
    .filter(Boolean)
    .join(' ')
  const faded = checked ? '' : 'opacity-50 line-through decoration-faint'
  return (
    <li
      data-context-proposal={proposal.kind}
      data-checked={checked ? 'true' : 'false'}
      data-current={current ? 'true' : undefined}
      aria-current={current ? 'true' : undefined}
      className={cn('min-w-0 px-3', compact ? 'py-2' : 'py-2.5', current && 'bg-primary-soft/40')}
    >
      <div className={cn('flex min-w-0 gap-2.5', compact ? 'items-center' : 'items-start')}>
        <input
          type="checkbox"
          className={cn('size-4 shrink-0 cursor-pointer accent-primary disabled:cursor-default', !compact && 'mt-0.5')}
          checked={checked}
          disabled={sends.busy}
          aria-label={selected?.label ?? fallback}
          aria-describedby={description ? descriptionId : undefined}
          title={selected?.effect}
          data-context-check={proposalKey(proposal)}
          onChange={(event) => selection.toggle(proposal, event.target.checked)}
        />
        <span id={descriptionId} hidden>
          {description}
        </span>
        <div className="min-w-0 flex-1">
          <div className={cn('flex min-w-0 items-baseline gap-x-2', compact ? 'flex-nowrap' : 'flex-wrap gap-y-0.5')}>
            <span className="shrink-0 text-[11px] font-medium uppercase tracking-[0.06em] text-muted">
              {proposal.tier_label}
            </span>
            <ProposalTitle proposal={proposal} className={cn(faded, compact && 'truncate')} onPreview={onPreview} />
            {compact ? (
              <button
                type="button"
                className="ml-auto inline-flex shrink-0 items-center self-center rounded p-0.5 text-faint hover:text-foreground"
                aria-label={`Preview ${proposal.target_title.trim() || proposal.tier_label}`}
                data-context-preview-open
                onClick={onPreview}
              >
                <Eye className="size-3.5" />
              </button>
            ) : (
              <button
                type="button"
                className="ml-auto shrink-0 text-xs font-medium text-primary hover:underline"
                aria-label={`Preview ${proposal.target_title.trim() || proposal.tier_label}`}
                data-context-preview-open
                onClick={onPreview}
              >
                Preview
              </button>
            )}
            {compact && proposal.justification && (
              <button
                type="button"
                className="inline-flex shrink-0 items-center self-center rounded p-0.5 text-faint hover:text-foreground"
                aria-expanded={expanded}
                aria-controls={justificationId}
                aria-label={expanded ? 'Hide why' : 'Show why'}
                data-context-why
                onClick={() => setExpanded((value) => !value)}
              >
                <ChevronRight className={cn('size-3.5 transition-transform', expanded && 'rotate-90')} />
              </button>
            )}
          </div>
          {proposal.justification && (!compact || expanded) && (
            <p
              ref={clamped.ref}
              id={justificationId}
              data-context-justification
              className={cn(
                'mt-1 break-words text-sm leading-6 text-muted',
                !compact && !expanded && 'line-clamp-2',
                faded,
              )}
            >
              {proposal.justification}
            </p>
          )}
          {!compact && proposal.justification && (expanded || clamped.overflows) && (
            <button
              type="button"
              className="mt-0.5 text-xs font-medium text-primary hover:underline"
              aria-expanded={expanded}
              aria-controls={justificationId}
              data-context-expand
              onClick={() => setExpanded((value) => !value)}
            >
              {expanded ? 'Show less' : 'Show more'}
            </button>
          )}
          {(source || age) && (
            <p data-context-row-meta className="mt-1 flex min-w-0 flex-wrap gap-x-1.5 text-xs text-muted">
              {source && <span>Suggested by {source}</span>}
              {source && age && <span aria-hidden="true">·</span>}
              {age && <Age value={age} />}
            </p>
          )}
          {reasons.map((reason) => (
            <p key={reason} data-context-row-reason className="mt-1 text-[11px] text-muted">
              {reason}
            </p>
          ))}
        </div>
      </div>
    </li>
  )
}

// Whether a clamped paragraph hides text, so the row offers "Show more" only
// when there is more to show. It re-measures when the paragraph resizes.
function useClamped(text: string | null) {
  const ref = useRef<HTMLParagraphElement>(null)
  const [overflows, setOverflows] = useState(false)
  useLayoutEffect(() => {
    const node = ref.current
    if (!node || text == null) return
    const measure = () => {
      if (node.classList.contains('line-clamp-2')) setOverflows(node.scrollHeight > node.clientHeight + 1)
    }
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(measure)
    observer.observe(node)
    return () => observer.disconnect()
  }, [text])
  return { ref, overflows }
}

// The proposal's title opens the document preview panel in place; the
// panel's "Open full page" link keeps the document page route (NA-6).
function ProposalTitle({
  proposal,
  className,
  onPreview,
}: {
  proposal: NextActionContextProposal
  className?: string
  onPreview: () => void
}) {
  const title = proposal.target_title.trim()
  if (!title) return null
  return (
    <button
      type="button"
      data-context-title
      className={cn('min-w-0 break-words text-left font-medium text-foreground hover:underline', className)}
      onClick={onPreview}
    >
      {title}
    </button>
  )
}

// The choices one control would send, in the order of the server's
// `choices[]`, and the reason it cannot send them, or '' when it can. A
// control is disabled when any choice in its own snapshot is absent,
// disallowed, or unsendable, so a disallowed dismiss never disables an
// all-checked Apply (NA-6; DEC-61).
function snapshot(action: NextAction, wanted: string[]) {
  const ids = new Set(wanted)
  const choices = action.choices.filter((choice) => ids.has(choice.id))
  const found = new Set(choices.map((choice) => choice.id))
  const missing = wanted.some((id) => !found.has(id))
  const reason = missing
    ? 'This page cannot send this choice.'
    : (choices.map(unavailable).find((text) => text !== '') ?? '')
  return { choices, reason }
}

// The reasons two controls cannot send, each prefixed by its control's name,
// or one unprefixed line when both share the reason.
function reasonLines(pairs: [string, string][]) {
  const reasons = pairs.filter(([, reason]) => reason !== '')
  if (reasons.length === 2 && reasons[0][1] === reasons[1][1]) return [reasons[0][1]]
  return reasons.map(([name, reason]) => `${name}: ${reason}`)
}

function applyLabel(total: number, attach: number) {
  const dismiss = total - attach
  if (total === 1) return attach === 1 ? 'Attach' : 'Dismiss'
  if (dismiss === 0) return `Attach all ${attach}`
  if (attach === 0) return `Dismiss all ${dismiss}`
  return `Attach ${attach} · dismiss ${dismiss}`
}

// Apply and Dismiss all: client chrome over the server's choices, not a
// server act (NA-6). Apply sends each checked row's confirm choice and each
// unchecked row's dismiss choice; Dismiss all sends every dismiss choice.
// Inside the Inbox, c clicks Apply while it renders enabled (AC-2.11).
export function ContextProposalFooter({
  action,
  selection,
  sends,
}: {
  action: NextAction
  selection: ContextProposalSelection
  sends: ContextProposalSends
}) {
  const shortcut = useShortcutChoice()
  const proposals = contextProposals(action)
  if (proposals.length === 0) return null
  const attach = proposals.filter((proposal) => selection.checked(proposal)).length
  const apply = snapshot(
    action,
    proposals.map((proposal) => choiceId(proposal, selection.checked(proposal) ? 'confirm' : 'dismiss')),
  )
  const dismissAll = snapshot(
    action,
    proposals.map((proposal) => choiceId(proposal, 'dismiss')),
  )
  const keyed = shortcut === CONTEXT_APPLY_SHORTCUT && apply.reason === '' && !sends.busy
  return (
    <div data-context-footer className="min-w-0 space-y-1.5 text-[13px]">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <span className="inline-flex items-center gap-1.5">
          <Button
            size="sm"
            data-context-apply
            data-shortcut={keyed ? 'c' : undefined}
            disabled={sends.busy || apply.reason !== ''}
            onClick={() => void sends.run(apply.choices)}
          >
            {applyLabel(proposals.length, attach)}
          </Button>
          {keyed && <Kbd>c</Kbd>}
        </span>
        <Button
          size="sm"
          variant="secondary"
          data-context-dismiss-all
          disabled={sends.busy || dismissAll.reason !== ''}
          onClick={() => void sends.run(dismissAll.choices)}
        >
          Dismiss all
        </Button>
      </div>
      {reasonLines([
        ['Apply', apply.reason],
        ['Dismiss all', dismissAll.reason],
      ]).map((reason) => (
        <p key={reason} data-context-control-reason className="text-[11px] text-muted">
          {reason}
        </p>
      ))}
      <ContextProposalError sends={sends} />
    </div>
  )
}

export function ContextProposalError({ sends }: { sends: ContextProposalSends }) {
  if (sends.error == null) return null
  const { recorded, total } = sends.progress
  return (
    <p data-context-refusal className="break-words text-xs text-failure">
      {total > 0 && `${recorded} of ${total} recorded. `}
      {errorMessage(sends.error, 'Could not record this choice.')}
    </p>
  )
}
