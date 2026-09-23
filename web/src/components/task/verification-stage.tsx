import { useState } from 'react'
import type {
  ActivityItem,
  VerificationAssessment,
  VerificationCollection,
  VerificationMetadata,
  VerificationPage,
} from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { useTaskAudit, useTaskVerification, useVerificationPages } from './use-task-detail'
import { VerificationEvidenceDisclosure, VerificationObservationForm } from './verification-evidence'

const collections: Array<[VerificationCollection, string]> = [
  ['selections', 'Kit selection'],
  ['obligations', 'Ordinary obligations'],
  ['assertions', 'Assertion outcomes'],
  ['operations', 'External operations'],
  ['publications', 'Publication status'],
  ['attempts', 'Attempts'],
  ['evidence', 'Evidence'],
]

export function VerificationStage({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  if (
    !item.task.policy_contract?.verify_stage &&
    !(item.work_orders ?? []).some((order) => order.stage === 'verify') &&
    item.task.next_stage !== 'verify'
  )
    return null
  return <VerificationStageBody key={`${workspace}:${item.task.id}`} item={item} />
}

function elapsed(start: string, end?: string) {
  const ms = (end ? Date.parse(end) : Date.now()) - Date.parse(start)
  if (!Number.isFinite(ms)) return 'Unknown'
  const seconds = Math.max(0, Math.floor(ms / 1000))
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

function VerificationStageBody({ item }: { item: ActivityItem }) {
  const query = useTaskVerification(item.task.id)
  const [historyOpen, setHistoryOpen] = useState(false)
  const first = query.data?.pages[0]
  const contexts = query.data?.pages.flatMap((page) => page.contexts?.items ?? []) ?? []
  const current = contexts.find((context) => context.id === first?.current_context_id)
  const historical = contexts.filter((context) => context.id !== first?.current_context_id)
  const order = [...(item.work_orders ?? [])]
    .filter((order) => order.stage === 'verify')
    .sort((a, b) => (Date.parse(b.created_at ?? '') || 0) - (Date.parse(a.created_at ?? '') || 0))[0]
  const state =
    current?.metadata.outcome === 'operator_action_required'
      ? 'blocked'
      : current?.metadata.sealed_at
        ? 'completed'
        : order?.state === 'claimed'
          ? 'running'
          : order?.state === 'queued'
            ? 'queued'
            : order?.state === 'completed'
              ? 'completed'
              : order
                ? 'blocked'
                : 'queued'
  return (
    <section aria-label="Verification" className="min-w-0 space-y-3 rounded-lg border border-border bg-surface p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Verify · {state}</h2>
        <Button variant="ghost" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
          Refresh verification
        </Button>
      </div>
      <p className="break-words text-xs text-muted">
        Claimant: {order?.claimed_by || current?.metadata.claimant || 'Unclaimed'} · Attempts:{' '}
        {current?.metadata.attempt_count ?? '0'}
        {current && ` · Elapsed: ${elapsed(current.at, current.metadata.sealed_at)}`}
      </p>
      {query.isPending && <p role="status">Loading verification summary…</p>}
      {query.error && <p role="alert">Verification could not be loaded: {query.error.message}</p>}
      {!query.isPending && !query.error && contexts.length === 0 && (
        <p className="text-xs text-muted">No verification context recorded yet.</p>
      )}
      {current && <ContextCard taskId={item.task.id} context={current} overview={first?.overview} />}
      {(historical.length > 0 || query.hasNextPage) && (
        <div>
          <Button variant="ghost" size="sm" aria-expanded={historyOpen} onClick={() => setHistoryOpen(!historyOpen)}>
            Historical verification ({historical.length}
            {query.hasNextPage ? '+' : ''})
          </Button>
          {historyOpen && (
            <div className="space-y-3 pt-2">
              {historical.map((context) => (
                <ContextCard key={context.id} taskId={item.task.id} context={context} historical />
              ))}
              {query.hasNextPage && (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void query.fetchNextPage()}
                  disabled={query.isFetchingNextPage}
                >
                  Load earlier contexts
                </Button>
              )}
            </div>
          )}
        </div>
      )}
    </section>
  )
}

function ContextCard({
  taskId,
  context,
  overview,
  historical = false,
}: {
  taskId: string
  context: VerificationMetadata
  overview?: Partial<Record<VerificationCollection, VerificationPage>>
  historical?: boolean
}) {
  return (
    <article className="min-w-0 space-y-2 rounded border border-border p-3">
      <h3 className="break-all text-xs font-semibold">
        {historical ? 'Historical revision' : 'Current revision'} · {context.metadata.source_sha || 'Unknown SHA'}
      </h3>
      <p className="break-all text-xs text-muted">
        Context {context.id} · {context.at}
      </p>
      {context.metadata.sealed_at && (
        <p className="break-words text-xs">
          Sealed result: <strong>{context.metadata.outcome}</strong> · {context.metadata.disposition} · Deciding actor:{' '}
          {context.metadata.deciding_actor}
        </p>
      )}
      {context.metadata.required_action && (
        <p className="break-words text-xs">Required action: {context.metadata.required_action}</p>
      )}
      {collections.map(([kind, label]) => (
        <CollectionDisclosure
          key={kind}
          taskId={taskId}
          contextId={context.id}
          kind={kind}
          label={label}
          preview={overview?.[kind]}
        />
      ))}
    </article>
  )
}

function CollectionDisclosure({
  taskId,
  contextId,
  kind,
  label,
  preview,
}: {
  taskId: string
  contextId: string
  kind: VerificationCollection
  label: string
  preview?: VerificationPage
}) {
  const [open, setOpen] = useState(false)
  return (
    <div className="min-w-0 border-t border-border pt-2">
      <Button variant="ghost" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        {open ? 'Hide' : 'Show'} {label.toLowerCase()}
      </Button>
      {!open && preview && preview.items.length > 0 && (
        <MetadataList taskId={taskId} contextId={contextId} kind={kind} items={preview.items} />
      )}
      {!open && preview?.next_cursor && <p className="text-xs text-muted">More {label.toLowerCase()} available.</p>}
      {open && <CollectionPages taskId={taskId} contextId={contextId} kind={kind} label={label} />}
    </div>
  )
}

function CollectionPages({
  taskId,
  contextId,
  kind,
  label,
}: {
  taskId: string
  contextId: string
  kind: VerificationCollection
  label: string
}) {
  const query = useVerificationPages(taskId, contextId, kind)
  const items = query.data?.pages.flatMap((page) => page.items) ?? []
  return (
    <div className="min-w-0 space-y-2">
      {query.isPending && <p role="status">Loading {label.toLowerCase()}…</p>}
      {query.error && <p role="alert">{query.error.message}</p>}
      {!query.isPending && !query.error && items.length === 0 && (
        <p className="text-xs text-muted">No {label.toLowerCase()} recorded.</p>
      )}
      <MetadataList taskId={taskId} contextId={contextId} kind={kind} items={items} />
      {query.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => void query.fetchNextPage()}
          disabled={query.isFetchingNextPage}
        >
          Load more {label.toLowerCase()}
        </Button>
      )}
    </div>
  )
}

function MetadataList({
  taskId,
  contextId,
  kind,
  items,
}: {
  taskId: string
  contextId: string
  kind: VerificationCollection
  items: VerificationMetadata[]
}) {
  return (
    <ul className="min-w-0 space-y-2">
      {items.map((item) => (
        <li key={item.id} className="min-w-0 rounded bg-background p-2 text-xs">
          <p className="break-all font-medium">
            {item.metadata.kit_id ||
              item.metadata.obligation_id ||
              item.metadata.assertion_id ||
              item.metadata.type ||
              item.metadata.step_id ||
              item.id}{' '}
            · {item.metadata.outcome || item.metadata.eligibility || item.state}
          </p>
          {kind === 'assertions' && (
            <p>{item.metadata.required === 'true' ? 'Required assertion' : 'Optional assertion'}</p>
          )}
          <dl className="min-w-0 space-y-1">
            {Object.entries(item.metadata)
              .filter(
                ([key, value]) =>
                  value && !['truncated', 'type', 'outcome', 'required', 'assertion_id', 'evidence_id'].includes(key),
              )
              .map(([key, value]) => (
                <div key={key} className="min-w-0">
                  <dt className="inline text-muted">{key.replaceAll('_', ' ')}: </dt>
                  <dd className="inline whitespace-pre-wrap break-all">{value}</dd>
                </div>
              ))}
          </dl>
          {item.metadata.truncated === 'true' && (
            <p className="text-muted">Long metadata is shortened in this summary.</p>
          )}
          {(kind === 'evidence' || item.metadata.evidence_id) && (
            <VerificationEvidenceDisclosure
              taskId={taskId}
              contextId={contextId}
              evidenceId={item.metadata.evidence_id || item.id}
            />
          )}
          {kind === 'attempts' && <p>Elapsed: {elapsed(item.metadata.started_at, item.metadata.ended_at)}</p>}
          {kind === 'attempts' && ['running', 'pending'].includes(item.state) && (
            <VerificationObservationForm taskId={taskId} contextId={contextId} runId={item.id} />
          )}
        </li>
      ))}
    </ul>
  )
}

export function VerificationReviewResult({ item }: { item: ActivityItem }) {
  const { workspace } = useWorkspaceSelection()
  return <VerificationReviewResultBody key={`${workspace}:${item.task.id}`} item={item} />
}

function VerificationReviewResultBody({ item }: { item: ActivityItem }) {
  const [open, setOpen] = useState(false)
  const event = [...(item.events ?? [])].reverse().find((event) => event.kind === 'review.completed')
  const audit = useTaskAudit(item.task.id, 'event', event ? String(event.id) : '', open)
  const payload = audit.data?.kind === 'event' ? audit.data.event.payload : undefined
  const assessment = payload?.verification_assessment as VerificationAssessment | undefined
  if (
    !event ||
    (!item.task.policy_contract?.verify_stage && !(item.work_orders ?? []).some((order) => order.stage === 'verify'))
  )
    return null
  return (
    <div className="space-y-2 py-2">
      <Button variant="ghost" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        Verification relied on by review
      </Button>
      {open && (
        <div className="space-y-2">
          {audit.isPending && <p role="status">Loading review verification…</p>}
          {audit.error && <p role="alert">{audit.error.message}</p>}
          {assessment?.context_ids?.map((contextId) => (
            <ReviewedContext key={contextId} taskId={item.task.id} contextId={contextId} assessment={assessment} />
          ))}
          {audit.data && !assessment && (
            <p className="text-xs text-muted">This verdict did not cite a sealed verification result.</p>
          )}
        </div>
      )}
    </div>
  )
}

function ReviewedContext({
  taskId,
  contextId,
  assessment,
}: {
  taskId: string
  contextId: string
  assessment: VerificationAssessment
}) {
  const query = useVerificationPages(taskId, contextId, 'contexts')
  const evidence = useVerificationPages(taskId, contextId, 'evidence')
  const context = query.data?.pages[0]?.items[0]
  const cited = new Set(assessment.evidence_ids ?? [])
  const references = evidence.data?.pages.flatMap((page) => page.items).filter((item) => cited.has(item.id)) ?? []
  return (
    <div className="min-w-0 space-y-2 rounded border border-border p-3 text-xs">
      {query.error && <p role="alert">{query.error.message}</p>}
      {context && (
        <>
          <p className="break-all">
            Sealed result: <strong>{context.metadata.outcome}</strong> · {context.metadata.source_sha}
          </p>
          <p className="break-words">
            Sealed by {context.metadata.deciding_actor} · Review deciding actor: {assessment.actor || 'Unknown'}
          </p>
        </>
      )}
      {evidence.error && <p role="alert">{evidence.error.message}</p>}
      {references.map((item) => (
        <VerificationEvidenceDisclosure key={item.id} taskId={taskId} contextId={contextId} evidenceId={item.id} />
      ))}
      {evidence.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => void evidence.fetchNextPage()}
          disabled={evidence.isFetchingNextPage}
        >
          Find more cited evidence
        </Button>
      )}
    </div>
  )
}
