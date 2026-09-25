import { AlertTriangle, Check, ChevronRight, X } from 'lucide-react'
import { type ReactNode, useEffect, useState } from 'react'
import type { ActivityItem, Job, VerificationCollection, VerificationMetadata, WorkOrder } from '../../lib/types'
import { absoluteTime, cn, duration } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { useTaskVerification, useVerificationPages } from './use-task-detail'
import { VerificationEvidenceDisclosure, VerificationObservationForm } from './verification-evidence'
import { CollectionPages } from './verification-stage'

// The verify stage is a structured result, not a narration, so like the
// review panel it gets its own timeline entry instead of the generic job
// card. The entry answers three questions at a glance — did it pass, which
// exercises and assertions ran, and did the pull request body publish — and
// keeps the collection pages behind a closed details fold.

type Outcome = 'passed' | 'failed' | 'needs_operator' | 'running' | 'queued' | 'pending'

function outcomeOf(context: VerificationMetadata | undefined, order: WorkOrder | undefined, job: Job): Outcome {
  const outcome = context?.metadata.outcome
  if (context?.metadata.sealed_at) {
    if (outcome === 'operator_action_required') return 'needs_operator'
    if (outcome === 'succeeded' || outcome === 'pass' || outcome === 'passed') return 'passed'
    return 'failed'
  }
  if (job.state === 'running' || order?.state === 'claimed') return 'running'
  if (job.state === 'failed') return 'failed'
  if (order?.state === 'queued') return 'queued'
  return 'pending'
}

const outcomeBadge: Record<
  Outcome,
  { label: string; variant: 'positive' | 'failure' | 'attention' | 'accent' | 'default' }
> = {
  passed: { label: 'Passed', variant: 'positive' },
  failed: { label: 'Failed', variant: 'failure' },
  needs_operator: { label: 'Needs you', variant: 'attention' },
  running: { label: 'Running', variant: 'accent' },
  queued: { label: 'Queued', variant: 'default' },
  pending: { label: 'Pending', variant: 'default' },
}

export function verificationDot(outcome: Outcome) {
  return cn(
    'bg-edge',
    outcome === 'passed' && 'bg-positive',
    outcome === 'failed' && 'bg-failure',
    outcome === 'needs_operator' && 'bg-attention-dot',
    outcome === 'running' && 'animate-pulse bg-primary',
  )
}

function short(sha?: string) {
  return sha ? sha.slice(0, 7) : ''
}

function items(query: ReturnType<typeof useVerificationPages>) {
  return query.data?.pages.flatMap((page) => page.items) ?? []
}

// Exercises are the attempts a kit ran (one per fixture); an assertion's
// run_id names the attempt it belongs to, which is how the row gets a name
// a person recognises instead of a hash.
function exerciseName(attempt?: VerificationMetadata) {
  if (!attempt) return undefined
  return attempt.metadata.exercise_id || attempt.metadata.obligation_id || undefined
}

export function VerificationEntry({
  item,
  job,
  order,
  footer,
  dotSlot,
}: {
  item: ActivityItem
  job: Job
  order?: WorkOrder
  footer?: ReactNode
  dotSlot?: (className: string) => ReactNode
}) {
  const summary = useTaskVerification(item.task.id)
  const first = summary.data?.pages[0]
  const contexts = summary.data?.pages.flatMap((page) => page.contexts?.items ?? []) ?? []
  const context =
    contexts.find((candidate) => order && candidate.metadata.work_order_id === order.id) ??
    (order ? undefined : contexts.find((candidate) => candidate.id === first?.current_context_id))
  const superseded = Boolean(context && first?.current_context_id && context.id !== first.current_context_id)
  // An older run's context may sit on a later summary page: keep paging
  // until this entry finds its own or the pages run out.
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = summary
  useEffect(() => {
    if (!context && summary.data && hasNextPage && !isFetchingNextPage) void fetchNextPage()
  }, [context, summary.data, hasNextPage, isFetchingNextPage, fetchNextPage])
  const outcome = outcomeOf(context, order, job)
  const badge = outcomeBadge[outcome]
  const anchor = context ? `verification-${context.id}` : undefined
  return (
    <li className="relative pl-7" id={anchor}>
      {dotSlot?.(verificationDot(outcome))}
      <article
        aria-label="Verification"
        className={cn('rounded-lg border border-border bg-card', superseded && 'opacity-80')}
      >
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">Verify</span>
          <Badge variant={badge.variant}>{badge.label}</Badge>
          {context?.metadata.source_sha && (
            <span className="font-mono text-[11px] text-muted" title={context.metadata.source_sha}>
              at {short(context.metadata.source_sha)}
            </span>
          )}
          {superseded && <span className="text-[11px] text-faint">superseded by a later revision</span>}
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(job.started_at ?? context?.at ?? '')}</time>
        </div>
        {summary.isPending && (
          <p role="status" className="px-4 py-3 text-sm text-muted">
            Loading verification…
          </p>
        )}
        {summary.error && (
          <p role="alert" className="px-4 py-3 text-sm text-attention">
            Verification could not be loaded: {summary.error.message}
          </p>
        )}
        {!summary.isPending && !summary.error && !context && (
          <p className="px-4 py-3 text-sm text-muted">
            {outcome === 'running' || outcome === 'queued'
              ? 'The verifier has not recorded a context yet.'
              : 'No verification context was recorded for this run.'}
          </p>
        )}
        {context && <ContextBody taskId={item.task.id} context={context} outcome={outcome} />}
        {footer}
      </article>
    </li>
  )
}

function ContextBody({
  taskId,
  context,
  outcome,
}: {
  taskId: string
  context: VerificationMetadata
  outcome: Outcome
}) {
  const attempts = useVerificationPages(taskId, context.id, 'attempts')
  const assertions = useVerificationPages(taskId, context.id, 'assertions')
  const selections = useVerificationPages(taskId, context.id, 'selections')
  const publications = useVerificationPages(taskId, context.id, 'publications')
  const attemptList = items(attempts)
  const attemptByRun = new Map<string, VerificationMetadata>()
  for (const attempt of attemptList) {
    attemptByRun.set(attempt.id, attempt)
    if (attempt.run_id) attemptByRun.set(attempt.run_id, attempt)
  }
  const assertionList = items(assertions)
  const passed = assertionList.filter((assertion) => assertion.metadata.outcome === 'pass').length
  const kits = items(selections)
  const eligible = kits.filter((kit) => kit.metadata.eligibility === 'eligible')
  const publicationList = items(publications)
  const publication =
    publicationList.find((entry) => entry.metadata.current === 'true') ??
    [...publicationList].sort((a, b) => Number(b.metadata.generation ?? 0) - Number(a.metadata.generation ?? 0))[0]
  const earlierPublications = publicationList.filter((entry) => entry.id !== publication?.id).length
  const running = attemptList.filter((attempt) => ['running', 'pending'].includes(attempt.state))
  const note = context.metadata.required_action?.trim()

  return (
    <>
      <ResultBanner context={context} outcome={outcome} passed={passed} total={assertionList.length} />
      <div className="divide-y divide-border/60">
        {eligible.length > 0 && (
          <Row label="Kits">
            <span className="flex flex-wrap gap-1.5">
              {eligible.map((kit) => (
                <Badge key={kit.id} variant="mono" title={kit.metadata.digest}>
                  {kit.metadata.kit_id}
                </Badge>
              ))}
              {kits.length > eligible.length && (
                <span className="text-xs text-muted">
                  {kits.length - eligible.length} not eligible for this revision
                </span>
              )}
            </span>
          </Row>
        )}
        {(assertionList.length > 0 || attemptList.length > 0) && (
          <Row label="Assertions">
            {assertions.isPending || attempts.isPending ? (
              <span className="text-xs text-muted">Loading…</span>
            ) : (
              <AssertionTable
                taskId={taskId}
                contextId={context.id}
                assertions={assertionList}
                attemptByRun={attemptByRun}
                attempts={attemptList}
              />
            )}
          </Row>
        )}
        {publication && (
          <Row label="Pull request">
            <PublicationLine publication={publication} earlier={earlierPublications} />
          </Row>
        )}
        {running.map((attempt) => (
          <Row key={attempt.id} label="In progress">
            <div className="space-y-2">
              <span className="text-sm">
                {exerciseName(attempt) ?? attempt.metadata.kit_id ?? 'Attempt'} running ·{' '}
                {duration(attempt.metadata.started_at ?? attempt.at)}
              </span>
              <VerificationObservationForm taskId={taskId} contextId={context.id} runId={attempt.id} />
            </div>
          </Row>
        ))}
      </div>
      {note && outcome !== 'needs_operator' && (
        <Fold summary="Verifier's report">
          <p className="whitespace-pre-wrap break-words text-sm leading-6 text-foreground/85">{note}</p>
        </Fold>
      )}
      <Fold summary="Details">
        <dl className="mb-3 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          <dt className="text-muted">Context</dt>
          <dd className="break-all font-mono">{context.id}</dd>
          <dt className="text-muted">Head</dt>
          <dd className="break-all font-mono">{context.metadata.source_sha}</dd>
          {context.metadata.baseline_sha && (
            <>
              <dt className="text-muted">Baseline</dt>
              <dd className="break-all font-mono">{context.metadata.baseline_sha}</dd>
            </>
          )}
          {context.metadata.deciding_actor && (
            <>
              <dt className="text-muted">Sealed by</dt>
              <dd className="break-all">{context.metadata.deciding_actor}</dd>
            </>
          )}
          {context.metadata.disposition && (
            <>
              <dt className="text-muted">Disposition</dt>
              <dd>{context.metadata.disposition.replaceAll('_', ' ')}</dd>
            </>
          )}
          <dt className="text-muted">Attempts</dt>
          <dd>{context.metadata.attempt_count ?? attemptList.length}</dd>
        </dl>
        <div className="divide-y divide-border/60 rounded border border-border">
          {(
            [
              ['attempts', 'Attempts'],
              ['evidence', 'Evidence'],
              ['obligations', 'Ordinary obligations'],
              ['operations', 'External operations'],
              ['publications', `Publication history${earlierPublications ? ` (${earlierPublications + 1})` : ''}`],
              ['selections', 'Kit selection'],
            ] as Array<[VerificationCollection, string]>
          ).map(([kind, label]) => (
            <CollectionFold key={kind} taskId={taskId} contextId={context.id} kind={kind} label={label} />
          ))}
        </div>
      </Fold>
    </>
  )
}

function ResultBanner({
  context,
  outcome,
  passed,
  total,
}: {
  context: VerificationMetadata
  outcome: Outcome
  passed: number
  total: number
}) {
  if (outcome === 'running' || outcome === 'queued' || outcome === 'pending') return null
  const head = short(context.metadata.source_sha)
  const tally = total > 0 ? `${passed} of ${total} assertions passed` : undefined
  if (outcome === 'needs_operator')
    return (
      <div className="flex items-start gap-2 border-b border-border bg-attention-soft px-4 py-2.5 text-sm text-attention">
        <AlertTriangle className="mt-0.5 size-4 shrink-0" />
        <div className="min-w-0">
          <p className="font-medium">Verification is waiting on you</p>
          {context.metadata.required_action && (
            <p className="whitespace-pre-wrap break-words font-normal text-foreground/85">
              {context.metadata.required_action}
            </p>
          )}
        </div>
      </div>
    )
  const ok = outcome === 'passed'
  return (
    <div
      className={cn(
        'flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-border px-4 py-2.5 text-sm font-medium',
        ok ? 'bg-positive-soft text-positive' : 'bg-failure-soft text-failure',
      )}
    >
      {ok ? <Check className="size-4 shrink-0" /> : <X className="size-4 shrink-0" />}
      {ok ? 'Verification passed' : 'Verification failed'}
      {head && <span className="font-mono text-xs font-normal opacity-80">at {head}</span>}
      {tally && <span className="text-xs font-normal opacity-80">· {tally}</span>}
    </div>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[6.5rem_1fr] gap-x-3 px-4 py-2.5">
      <span className="pt-0.5 font-mono text-[10px] uppercase tracking-[0.08em] text-faint">{label}</span>
      <div className="min-w-0">{children}</div>
    </div>
  )
}

function AssertionTable({
  taskId,
  contextId,
  assertions,
  attemptByRun,
  attempts,
}: {
  taskId: string
  contextId: string
  assertions: VerificationMetadata[]
  attemptByRun: Map<string, VerificationMetadata>
  attempts: VerificationMetadata[]
}) {
  // Exercises that produced no assertion (an attempt that failed before
  // asserting, or is still running) still get a row so the table reads as
  // "what the kit ran", not just "what passed".
  const covered = new Set(assertions.map((assertion) => assertion.run_id))
  const silent = attempts.filter(
    (attempt) => !covered.has(attempt.id) && !covered.has(attempt.run_id) && attempt.state !== 'running',
  )
  const rows = [...assertions].sort((a, b) =>
    (exerciseName(attemptByRun.get(a.run_id)) ?? '').localeCompare(exerciseName(attemptByRun.get(b.run_id)) ?? ''),
  )
  return (
    <ul className="divide-y divide-border/60 text-sm">
      {rows.map((assertion) => {
        const attempt = attemptByRun.get(assertion.run_id)
        const name = exerciseName(attempt)
        const pass = assertion.metadata.outcome === 'pass'
        const evidenceId = assertion.metadata.evidence_id || assertion.id
        return (
          <li key={assertion.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-1.5 first:pt-0 last:pb-0">
            <span
              className={cn(
                'inline-flex size-4 shrink-0 items-center justify-center rounded-full',
                pass ? 'bg-positive-soft text-positive' : 'bg-failure-soft text-failure',
              )}
            >
              {pass ? <Check className="size-3" /> : <X className="size-3" />}
              <span className="sr-only">{pass ? 'Passed' : 'Failed'}</span>
            </span>
            <span className="min-w-0 flex-1 break-words">
              {name ? <span className="font-medium">{name}</span> : <span className="text-muted">Unnamed run</span>}
              <span className="text-muted"> · {assertion.metadata.assertion_id}</span>
            </span>
            {assertion.metadata.required !== 'true' && <span className="text-[11px] text-faint">optional</span>}
            <span className="ml-auto text-xs">
              <VerificationEvidenceDisclosure taskId={taskId} contextId={contextId} evidenceId={evidenceId} compact />
            </span>
          </li>
        )
      })}
      {silent.map((attempt) => (
        <li key={attempt.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-1.5 text-muted">
          <span
            className={cn(
              'inline-flex size-4 shrink-0 items-center justify-center rounded-full',
              attempt.state === 'failed' ? 'bg-failure-soft text-failure' : 'border border-edge',
            )}
          >
            {attempt.state === 'failed' ? <X className="size-3" /> : null}
          </span>
          <span className="min-w-0 flex-1 break-words">
            {exerciseName(attempt) ?? attempt.id}
            <span> · no assertions recorded{attempt.state === 'failed' ? ', attempt failed' : ''}</span>
          </span>
        </li>
      ))}
    </ul>
  )
}

const deliveryBadge: Record<string, 'positive' | 'attention' | 'failure' | 'default'> = {
  published: 'positive',
  pending: 'default',
  failed: 'failure',
  superseded: 'default',
}

function PublicationLine({ publication, earlier }: { publication: VerificationMetadata; earlier: number }) {
  const state = publication.metadata.delivery_state || publication.state
  const variant = deliveryBadge[state] ?? 'attention'
  const drift =
    publication.metadata.observed_head &&
    publication.metadata.target_head &&
    publication.metadata.observed_head !== publication.metadata.target_head
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
      <Badge variant={variant}>{state === 'published' ? 'Body published' : state.replaceAll('_', ' ')}</Badge>
      {publication.metadata.generation && (
        <span className="text-xs text-muted">generation {publication.metadata.generation}</span>
      )}
      {publication.metadata.head_sha && (
        <span className="font-mono text-xs text-muted" title={publication.metadata.head_sha}>
          head {short(publication.metadata.head_sha)}
        </span>
      )}
      {drift && (
        <span className="inline-flex items-center gap-1 text-xs text-attention">
          <AlertTriangle className="size-3" /> observed head differs from target
        </span>
      )}
      {publication.metadata.last_attempt_at && (
        <span className="text-[11px] text-faint">{absoluteTime(publication.metadata.last_attempt_at)}</span>
      )}
      {earlier > 0 && (
        <span className="text-[11px] text-faint">
          · {earlier} earlier generation{earlier === 1 ? '' : 's'}
        </span>
      )}
    </div>
  )
}

function Fold({ summary, children }: { summary: string; children: ReactNode }) {
  const [open, setOpen] = useState(false)
  return (
    <details
      className="group border-t border-border"
      open={open}
      onToggle={(event) => setOpen((event.target as HTMLDetailsElement).open)}
    >
      <summary className="flex cursor-pointer select-none items-center gap-1.5 px-4 py-2 text-xs text-muted hover:text-foreground [&::-webkit-details-marker]:hidden">
        <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />
        {summary}
      </summary>
      <div className="px-4 pb-3">{open && children}</div>
    </details>
  )
}

function CollectionFold({
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
  const [open, setOpen] = useState(false)
  return (
    <details open={open} onToggle={(event) => setOpen((event.target as HTMLDetailsElement).open)} className="group">
      <summary className="flex cursor-pointer select-none items-center gap-1.5 px-3 py-1.5 text-xs hover:bg-surface [&::-webkit-details-marker]:hidden">
        <ChevronRight className="size-3.5 text-muted transition-transform group-open:rotate-90" />
        {label}
      </summary>
      {open && (
        <div className="px-3 pb-3">
          <CollectionPages taskId={taskId} contextId={contextId} kind={kind} label={label} />
        </div>
      )}
    </details>
  )
}

// The merge gate needs one line, not a second rendering of the table: the
// verdict at the head under decision, with a jump to the entry that has the
// rest.
export function VerificationVerdictLine({ item }: { item: ActivityItem }) {
  // Tasks without a verify stage never query the verification summary.
  if (
    !item.task.policy_contract?.verify_stage &&
    !(item.work_orders ?? []).some((order) => order.stage === 'verify') &&
    item.task.next_stage !== 'verify'
  )
    return null
  return <VerdictLine item={item} />
}

function VerdictLine({ item }: { item: ActivityItem }) {
  const summary = useTaskVerification(item.task.id)
  const first = summary.data?.pages[0]
  const contexts = summary.data?.pages.flatMap((page) => page.contexts?.items ?? []) ?? []
  const current = contexts.find((context) => context.id === first?.current_context_id)
  const assertions = useVerificationPages(item.task.id, current?.id ?? '', 'assertions')
  if (summary.isPending) return null
  if (!current)
    return (
      <p className="flex items-center gap-2 py-2 text-sm text-muted">
        <AlertTriangle className="size-4 text-attention" /> No sealed verification at the head under review.
      </p>
    )
  const list = items(assertions)
  const passed = list.filter((assertion) => assertion.metadata.outcome === 'pass').length
  const outcome = current.metadata.outcome
  const ok = outcome === 'succeeded'
  return (
    <p
      className={cn(
        'flex flex-wrap items-center gap-x-2 gap-y-1 py-2 text-sm',
        ok ? 'text-positive' : 'text-attention',
      )}
    >
      {ok ? <Check className="size-4" /> : <AlertTriangle className="size-4" />}
      <span className="font-medium">
        {ok ? 'Verification passed' : `Verification ${outcome?.replaceAll('_', ' ')}`}
      </span>
      <span className="font-mono text-xs text-muted">at {short(current.metadata.source_sha)}</span>
      {list.length > 0 && (
        <span className="text-xs text-muted">
          · {passed} of {list.length} assertions
        </span>
      )}
      <a href={`#verification-${current.id}`} className="text-xs text-primary hover:underline">
        See the run
      </a>
    </p>
  )
}
