import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { ArrowRight, ChevronRight, Clock, Scale, Search } from 'lucide-react'
import { type ReactNode, useMemo } from 'react'
import { useWorkspaceSelection } from '../components/app-shell'
import {
  alternativeRows,
  citationsByDecision,
  type DecisionFilter,
  decisionDate,
  decisionFilters,
  findDecision,
  formatDecisionDate,
  inFilter,
  matchesSearch,
  openSweepEntries,
  recordedTime,
  sortDecisions,
  statementClauses,
  supersessionChain,
} from '../components/decisions/decision-model'
import { DecisionSweep } from '../components/decisions/decision-sweep'
import { LineageExplorer } from '../components/lineage/lineage-explorer'
import { Badge } from '../components/ui/badge'
import { CopyButton } from '../components/ui/copy-button'
import { Input } from '../components/ui/input'
import { Skeleton } from '../components/ui/skeleton'
import { fetchDecisionCitations, fetchDecisions } from '../lib/api'
import { errorMessage } from '../lib/errors'
import { decisionCitationsQueryKey, decisionsQueryKey } from '../lib/query-keys'
import type { Decision, DecisionCitation } from '../lib/types'
import { cn } from '../lib/utils'

// The two workspace reads behind the Decisions surface and every decision
// page: the decision list and the decision citations, one query each, keyed by
// the workspace the address names and passing it explicitly
// (component-web-decision-surfaces; req-workspace-addresses AC-1.6, AC-1.7).
function useDecisionReads() {
  const { workspace } = useWorkspaceSelection()
  const decisions = useQuery({
    queryKey: decisionsQueryKey(workspace),
    queryFn: ({ signal }) => fetchDecisions(workspace, signal),
    enabled: Boolean(workspace),
  })
  const citations = useQuery({
    queryKey: decisionCitationsQueryKey(workspace),
    queryFn: ({ signal }) => fetchDecisionCitations(workspace, signal),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  return { workspace, decisions, citations }
}

const statusLabels: Record<Decision['status'], string> = {
  confirmed: 'In force',
  superseded: 'Superseded',
  dismissed: 'Dismissed',
  proposed: 'Proposed',
}

const statusVariants: Record<Decision['status'], 'positive' | 'default' | 'outline' | 'accent'> = {
  confirmed: 'positive',
  superseded: 'default',
  dismissed: 'outline',
  proposed: 'accent',
}

const originLabels: Record<Decision['origin'], string> = {
  planning_session: 'Recorded in a planning conversation',
  implementation_deliberation: 'Proposed while implementing a task',
  operator: 'Recorded by an operator',
}

/**
 * The Decisions surface (req-document-operating-surfaces REQ-7). It lists the
 * workspace's decisions highest DEC-n first, narrows them by state and text,
 * and voices the open supersession flags and the proposed-decision count
 * without offering any confirm or dismiss control.
 */
export function DecisionsPage() {
  const { workspace, decisions, citations } = useDecisionReads()
  const search = useSearch({ from: '/w/$workspaceId/decisions' })
  const navigate = useNavigate()
  const filter: DecisionFilter = search.filter ?? 'in_force'
  const all = decisions.data ?? []
  const cited = useMemo(() => citationsByDecision(citations.data?.items ?? []), [citations.data])
  const openFlags = all.reduce((total, decision) => total + openSweepEntries(decision).length, 0)
  const proposed = all.filter((decision) => decision.status === 'proposed').length
  const rows = useMemo(
    () =>
      sortDecisions(
        (decisions.data ?? []).filter(
          (decision) =>
            inFilter(decision, filter) &&
            matchesSearch(decision, search.q ?? '') &&
            (!search.flagged || openSweepEntries(decision).length > 0),
        ),
      ),
    [decisions.data, filter, search.flagged, search.q],
  )
  const setSearch = (next: typeof search) =>
    void navigate({ to: '/w/$workspaceId/decisions', params: { workspaceId: workspace }, search: next, replace: true })

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-3 lg:px-6 lg:py-4">
        <span className="flex size-8 items-center justify-center rounded-lg bg-primary-soft text-primary">
          <Scale className="size-4" />
        </span>
        <div className="min-w-0">
          <h1 className="text-lg font-semibold tracking-tight">Decisions</h1>
          <p className="mt-0.5 text-xs text-muted">
            The choices that govern how this system works, and the ones that replaced them.
          </p>
        </div>
      </header>
      <main className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-4xl px-4 py-6 sm:px-6 lg:px-8">
          {openFlags > 0 && (
            <section
              aria-label="Decisions needing attention"
              className="mb-4 flex flex-wrap items-center gap-3 rounded-lg border border-attention/40 bg-attention-soft/50 px-4 py-3 text-sm"
            >
              <p className="min-w-0 flex-1">
                <span className="font-semibold text-attention">
                  {openFlags} open {openFlags === 1 ? 'flag' : 'flags'}
                </span>{' '}
                <span className="text-muted">for documents that still cite a replaced decision.</span>
              </p>
              {search.flagged ? (
                <button
                  type="button"
                  className="text-xs font-medium text-primary hover:underline"
                  onClick={() => setSearch({ ...search, flagged: undefined })}
                >
                  Show all decisions
                </button>
              ) : (
                <button
                  type="button"
                  className="rounded-md border border-attention/40 bg-background px-2.5 py-1 text-xs font-medium text-foreground hover:bg-surface"
                  onClick={() => setSearch({ ...search, filter: 'all', flagged: true })}
                >
                  Review
                </button>
              )}
            </section>
          )}
          {proposed > 0 && (
            <p className="mb-4 rounded-lg border border-border bg-surface/50 px-4 py-3 text-sm text-muted">
              {proposed} proposed {proposed === 1 ? 'decision waits' : 'decisions wait'} for an operator.{' '}
              <Link
                to="/w/$workspaceId/inbox"
                params={{ workspaceId: workspace }}
                search={{ band: 'decisions' }}
                className="font-medium text-primary hover:underline"
              >
                Open the Inbox
              </Link>
            </p>
          )}
          <div className="flex flex-wrap items-center gap-3">
            <fieldset
              aria-label="Filter decisions"
              className="m-0 inline-flex rounded-lg border border-border bg-surface p-0.5"
            >
              {decisionFilters.map((option) => {
                const count = all.filter((decision) => inFilter(decision, option.value)).length
                return (
                  <button
                    key={option.value}
                    type="button"
                    aria-pressed={filter === option.value}
                    onClick={() =>
                      setSearch({
                        ...search,
                        filter: option.value === 'in_force' ? undefined : option.value,
                        flagged: undefined,
                      })
                    }
                    className={cn(
                      'inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
                      filter === option.value
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted hover:text-foreground',
                    )}
                  >
                    {option.label}
                    <span className="font-mono text-[11px] text-faint">{count}</span>
                  </button>
                )
              })}
            </fieldset>
            <div className="relative min-w-48 flex-1">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
              <Input
                aria-label="Search decisions"
                placeholder="Search DEC-n, statement, or context"
                className="pl-8"
                value={search.q ?? ''}
                onChange={(event) => setSearch({ ...search, q: event.target.value || undefined })}
              />
            </div>
          </div>
          {search.flagged && (
            <p className="mt-3 text-xs text-muted">Showing the decisions whose replaced decision is still cited.</p>
          )}
          <div className="mt-4">
            {decisions.isLoading ? (
              <div className="space-y-2" role="status" aria-label="Loading decisions">
                <Skeleton className="h-16" />
                <Skeleton className="h-16" />
              </div>
            ) : decisions.error ? (
              <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
                {errorMessage(decisions.error, 'Could not load decisions.')}
              </p>
            ) : rows.length === 0 ? (
              <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted">
                {all.length === 0 ? 'No decisions are recorded in this workspace.' : 'No decisions match.'}
              </p>
            ) : (
              <ul aria-label="Decisions" className="divide-y divide-border rounded-lg border border-border">
                {rows.map((decision) => (
                  <DecisionRow
                    key={decision.id}
                    decision={decision}
                    citingCount={citations.data ? (cited.get(decision.id)?.length ?? 0) : undefined}
                  />
                ))}
              </ul>
            )}
            {citations.error && (
              <p className="mt-2 text-xs text-failure">
                {errorMessage(citations.error, 'Could not load which documents cite each decision.')}
              </p>
            )}
          </div>
        </div>
      </main>
    </div>
  )
}

function DecisionRow({ decision, citingCount }: { decision: Decision; citingCount?: number }) {
  const { workspace } = useWorkspaceSelection()
  const clauses = statementClauses(decision.statement)
  const open = openSweepEntries(decision).length
  return (
    <li>
      <Link
        to="/w/$workspaceId/decisions/$decisionId"
        params={{ workspaceId: workspace, decisionId: decision.id }}
        className="block px-4 py-3 transition-colors hover:bg-surface/60"
      >
        <div className="flex items-start gap-3">
          <Badge variant="mono" className="mt-0.5 shrink-0">
            {decision.id}
          </Badge>
          <div className="min-w-0 flex-1">
            <p className="line-clamp-2 text-sm text-foreground">{clauses ? `(1) ${clauses[0]}` : decision.statement}</p>
            <p className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
              {decision.status !== 'confirmed' && (
                <Badge variant={statusVariants[decision.status]}>{statusLabels[decision.status]}</Badge>
              )}
              {clauses && <span>{clauses.length} clauses</span>}
              <span className="inline-flex items-center gap-1">
                <Clock className="size-3" />
                <time dateTime={decisionDate(decision)}>{formatDecisionDate(decisionDate(decision))}</time>
              </span>
              {citingCount !== undefined && <span>Cited by {citingCount}</span>}
              {decision.supersedes && <span>Replaces {decision.supersedes}</span>}
              {decision.superseded_by && <span>Replaced by {decision.superseded_by}</span>}
              {open > 0 && decision.supersedes && (
                <Badge variant="attention">
                  {open} still cite {decision.supersedes}
                </Badge>
              )}
            </p>
          </div>
          <ChevronRight className="mt-1 size-4 shrink-0 text-faint" aria-hidden="true" />
        </div>
      </Link>
    </li>
  )
}

/**
 * One decision's page at `/w/<id>/decisions/<DEC-n>`
 * (req-document-operating-surfaces REQ-8). It reads the same two workspace
 * queries as the list, so opening a decision from the list issues no new read.
 */
export function DecisionPage() {
  const { decisionId } = useParams({ from: '/w/$workspaceId/decisions/$decisionId' })
  const { workspace, decisions, citations } = useDecisionReads()
  const byId = useMemo(
    () => new Map((decisions.data ?? []).map((decision) => [decision.id, decision])),
    [decisions.data],
  )
  const decision = decisions.data ? findDecision(decisions.data, decisionId) : undefined
  const breadcrumb = (
    <nav aria-label="Breadcrumb" className="flex items-center gap-1.5 text-xs text-muted">
      <Link to="/w/$workspaceId/decisions" params={{ workspaceId: workspace }} className="hover:text-foreground">
        Decisions
      </Link>
      <ChevronRight className="size-3" aria-hidden="true" />
      <span className="font-mono text-foreground">{decision?.id ?? decisionId}</span>
    </nav>
  )
  if (decisions.isLoading)
    return (
      <DecisionFrame>
        {breadcrumb}
        <div className="mt-6 space-y-3" role="status" aria-label="Loading decision">
          <Skeleton className="h-10" />
          <Skeleton className="h-32" />
        </div>
      </DecisionFrame>
    )
  if (decisions.error)
    return (
      <DecisionFrame>
        {breadcrumb}
        <p role="alert" className="mt-6 rounded-md bg-failure-soft px-3 py-2 text-sm text-failure">
          {errorMessage(decisions.error, 'Could not load this decision.')}
        </p>
      </DecisionFrame>
    )
  if (!decision)
    return (
      <DecisionFrame>
        {breadcrumb}
        <section aria-label="Decision not found" className="mt-10 text-center">
          <h1 className="text-lg font-semibold tracking-tight">No such decision</h1>
          <p className="mt-2 text-sm text-muted">
            This workspace has no decision <span className="font-mono">{decisionId}</span>.
          </p>
          <Link
            to="/w/$workspaceId/decisions"
            params={{ workspaceId: workspace }}
            className="mt-4 inline-block text-sm font-medium text-primary hover:underline"
          >
            Open Decisions
          </Link>
        </section>
      </DecisionFrame>
    )
  return (
    <DecisionFrame>
      {breadcrumb}
      <DecisionRecord
        // Keyed by the decision, so no copy or mutation state outlives it.
        key={decision.id}
        decision={decision}
        byId={byId}
        citations={citations.data?.items.filter((citation) => citation.decision_id === decision.id)}
        citationsError={citations.error}
      />
    </DecisionFrame>
  )
}

function DecisionFrame({ children }: { children: ReactNode }) {
  return (
    <main className="h-full min-h-0 overflow-y-auto">
      <article className="mx-auto max-w-3xl px-4 py-6 sm:px-6 lg:px-8 lg:py-8">{children}</article>
    </main>
  )
}

function DecisionRecord({
  decision,
  byId,
  citations,
  citationsError,
}: {
  decision: Decision
  byId: ReadonlyMap<string, Decision>
  citations?: DecisionCitation[]
  citationsError: Error | null
}) {
  const { workspace } = useWorkspaceSelection()
  const clauses = statementClauses(decision.statement)
  const chain = supersessionChain(decision, byId)
  const newest = chain[chain.length - 1]
  const alternatives = alternativeRows(decision.alternatives_rejected ?? '')
  const confirmedAt = recordedTime(decision.confirmed_at)
  const dismissedAt = recordedTime(decision.dismissed_at)
  return (
    <>
      <header className="mt-4 border-b border-border pb-5">
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="font-mono text-2xl font-semibold tracking-tight">{decision.id}</h1>
          <Badge variant={statusVariants[decision.status]}>{statusLabels[decision.status]}</Badge>
          <span className="ml-auto flex items-center gap-1">
            <CopyButton
              value={`${window.location.origin}/w/${encodeURIComponent(workspace)}/decisions/${encodeURIComponent(decision.id)}`}
              label="Copy link"
              showLabel
            />
            <LineageExplorer type="decision" id={decision.id} />
          </span>
        </div>
        <p className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
          {decision.status === 'dismissed' ? (
            <span>
              Dismissed{decision.dismissed_by ? ` by ${decision.dismissed_by}` : ''}
              {dismissedAt ? ` on ${formatDecisionDate(dismissedAt)}` : ''}
            </span>
          ) : decision.status === 'proposed' ? (
            <span>Proposed on {formatDecisionDate(decision.created_at)}</span>
          ) : (
            <span>
              Confirmed{decision.confirmed_by ? ` by ${decision.confirmed_by}` : ''}
              {confirmedAt ? ` on ${formatDecisionDate(confirmedAt)}` : ''}
            </span>
          )}
          <span>
            {originLabels[decision.origin] ?? decision.origin}
            {decision.origin_task_id && (
              <>
                {' · '}
                <Link
                  to="/w/$workspaceId/tasks/$taskId/full"
                  params={{ workspaceId: workspace, taskId: decision.origin_task_id }}
                  className="font-mono text-primary hover:underline"
                >
                  {decision.origin_task_id}
                </Link>
              </>
            )}
          </span>
        </p>
      </header>
      {decision.status === 'superseded' && decision.superseded_by && (
        <section
          aria-label="No longer governs"
          className="mt-5 rounded-lg border border-border bg-surface/60 px-4 py-3 text-sm"
        >
          <p>
            {decision.id} no longer governs. <DecisionLink id={decision.superseded_by} /> replaced it.
          </p>
          <p className="mt-1 text-muted">
            The newest decision in its chain is <DecisionLink id={newest.id} />.
          </p>
        </section>
      )}
      {decision.status === 'proposed' && (
        <section
          aria-label="Waiting for an operator"
          className="mt-5 rounded-lg border border-attention/40 bg-attention-soft/50 px-4 py-3 text-sm"
        >
          <p>{decision.id} waits for an operator decision.</p>
          <Link
            to="/w/$workspaceId/inbox"
            params={{ workspaceId: workspace }}
            search={{ band: 'decisions', selected: `decision:${decision.id}` }}
            className="mt-1 inline-block font-medium text-primary hover:underline"
          >
            Open it in the Inbox
          </Link>
        </section>
      )}
      <section aria-label="Statement" className="mt-6">
        {clauses ? (
          <ol className="divide-y divide-border rounded-lg border border-border">
            {clauses.map((clause, index) => (
              <li key={`${index}:${clause}`} className="flex gap-3 px-4 py-3 text-[15px] leading-7">
                <span className="shrink-0 font-mono text-sm font-semibold text-muted">({index + 1})</span>
                <span>{clause}</span>
              </li>
            ))}
          </ol>
        ) : (
          <p className="text-[15px] leading-7">{decision.statement}</p>
        )}
      </section>
      {chain.length > 1 && (
        <section aria-label="Version chain" className="mt-8">
          <h2 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Version chain</h2>
          <ol className="mt-2 flex flex-wrap items-center gap-1.5">
            {chain.map((item, index) => (
              <li key={item.id} className="flex items-center gap-1.5">
                {index > 0 && <ArrowRight className="size-3 text-faint" aria-hidden="true" />}
                {item.id === decision.id ? (
                  <span
                    aria-current="page"
                    className="rounded-full bg-primary px-2.5 py-0.5 font-mono text-xs font-semibold text-primary-foreground"
                  >
                    {item.id}
                  </span>
                ) : (
                  <Link
                    to="/w/$workspaceId/decisions/$decisionId"
                    params={{ workspaceId: workspace, decisionId: item.id }}
                    className="rounded-full border border-border px-2.5 py-0.5 font-mono text-xs text-muted hover:border-edge hover:text-foreground"
                  >
                    {item.id}
                  </Link>
                )}
              </li>
            ))}
          </ol>
        </section>
      )}
      <section aria-label="Why" className="mt-8">
        <h2 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Why</h2>
        <p className="mt-2 whitespace-pre-wrap text-sm leading-6">{decision.context || 'No context was recorded.'}</p>
      </section>
      <section aria-label="Alternatives rejected" className="mt-8">
        <h2 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Alternatives rejected</h2>
        {alternatives.length > 0 ? (
          <ul className="mt-2 divide-y divide-border rounded-lg border border-border">
            {alternatives.map((alternative, index) => (
              <li key={`${index}:${alternative}`} className="px-4 py-2.5 text-sm leading-6">
                {alternative}
              </li>
            ))}
          </ul>
        ) : (
          <p className="mt-2 text-sm text-muted">No alternatives were recorded.</p>
        )}
      </section>
      <CitedBy citations={citations} error={citationsError} />
      {decision.supersedes && <DecisionSweep decision={{ ...decision, supersedes: decision.supersedes }} />}
    </>
  )
}

// The live documents whose current confirmed version names the decision as a
// whole token, from the workspace citations read (AC-8.5).
function CitedBy({ citations, error }: { citations?: DecisionCitation[]; error: Error | null }) {
  const { workspace } = useWorkspaceSelection()
  const requirements = (citations ?? []).filter((citation) => citation.tier === 'requirement')
  const designs = (citations ?? []).filter((citation) => citation.tier === 'system_design')
  const column = (label: string, marker: string, items: DecisionCitation[]) => (
    <div className="min-w-0">
      <h3 className="text-[11px] font-medium text-faint">{label}</h3>
      {items.length === 0 ? (
        <p className="mt-1 text-sm text-muted">None</p>
      ) : (
        <ul aria-label={`Citing ${label}`} className="mt-1 space-y-1">
          {items.map((citation) => (
            <li key={citation.document_id} className="flex min-w-0 items-center gap-2 text-sm">
              <Badge variant="mono" className="shrink-0">
                {marker}
              </Badge>
              {citation.tier === 'requirement' ? (
                <Link
                  to="/w/$workspaceId/requirements"
                  params={{ workspaceId: workspace }}
                  search={{ requirement: citation.document_id }}
                  className="truncate text-primary hover:underline"
                  title={citation.document_id}
                >
                  {citation.title || citation.document_id}
                </Link>
              ) : (
                <Link
                  to="/w/$workspaceId/system-design"
                  params={{ workspaceId: workspace }}
                  search={{ document: citation.document_id }}
                  className="truncate text-primary hover:underline"
                  title={citation.document_id}
                >
                  {citation.title || citation.document_id}
                </Link>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
  return (
    <section aria-label="Cited by" className="mt-8">
      <h2 className="text-xs font-semibold uppercase tracking-[0.12em] text-muted">Cited by</h2>
      {error ? (
        <p className="mt-2 text-sm text-failure">{errorMessage(error, 'Could not load the citing documents.')}</p>
      ) : !citations ? (
        <p className="mt-2 text-sm text-muted">Loading citing documents…</p>
      ) : (
        <div className="mt-2 grid gap-4 sm:grid-cols-2">
          {column('Requirements', 'REQ', requirements)}
          {column('System Design', 'SD', designs)}
        </div>
      )}
    </section>
  )
}

function DecisionLink({ id }: { id: string }) {
  const { workspace } = useWorkspaceSelection()
  return (
    <Link
      to="/w/$workspaceId/decisions/$decisionId"
      params={{ workspaceId: workspace, decisionId: id }}
      className="font-mono font-medium text-primary hover:underline"
    >
      {id}
    </Link>
  )
}
