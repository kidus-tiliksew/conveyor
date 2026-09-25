import { useState } from 'react'
import type {
  ActivityItem,
  VerificationAssessment,
  VerificationCollection,
  VerificationMetadata,
} from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { useTaskAudit, useVerificationPages } from './use-task-detail'
import { VerificationEvidenceDisclosure, VerificationObservationForm } from './verification-evidence'

// Paged collection reads for one verification context. The timeline entry
// (verification-entry.tsx) owns the summary; these pages sit behind its
// details fold and are only fetched once a person opens one.

function elapsed(start: string, end?: string) {
  const ms = (end ? Date.parse(end) : Date.now()) - Date.parse(start)
  if (!Number.isFinite(ms)) return 'Unknown'
  const seconds = Math.max(0, Math.floor(ms / 1000))
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

export function CollectionPages({
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

// Fields the summary row already states, or that only the store cares about.
const hiddenFields = new Set([
  'truncated',
  'type',
  'outcome',
  'required',
  'assertion_id',
  'evidence_id',
  'kit_id',
  'obligation_id',
  'step_id',
  'eligibility',
  'current',
  'delivery_state',
])

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
            {item.metadata.exercise_id ||
              item.metadata.kit_id ||
              item.metadata.obligation_id ||
              item.metadata.assertion_id ||
              item.metadata.type ||
              item.metadata.step_id ||
              item.id}{' '}
            · {item.metadata.outcome || item.metadata.eligibility || item.metadata.delivery_state || item.state}
            {kind === 'assertions' && item.metadata.required === 'true' && (
              <span className="font-normal text-muted"> · required</span>
            )}
          </p>
          <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
            {Object.entries(item.metadata)
              .filter(([key, value]) => value && value !== '[]' && !hiddenFields.has(key))
              .map(([key, value]) => (
                <div key={key} className="contents">
                  <dt className="text-muted">{key.replaceAll('_', ' ')}</dt>
                  <dd className="min-w-0 whitespace-pre-wrap break-all font-mono">{value}</dd>
                </div>
              ))}
          </dl>
          {item.metadata.truncated === 'true' && (
            <p className="mt-1 text-muted">Long metadata is shortened in this summary.</p>
          )}
          {(kind === 'evidence' || item.metadata.evidence_id) && (
            <VerificationEvidenceDisclosure
              taskId={taskId}
              contextId={contextId}
              evidenceId={item.metadata.evidence_id || item.id}
            />
          )}
          {kind === 'attempts' && (
            <p className="mt-1">Elapsed: {elapsed(item.metadata.started_at, item.metadata.ended_at)}</p>
          )}
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
