import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronRight, ChevronUp, FlaskConical, Globe, MousePointerClick, Square } from 'lucide-react'
import { useId, useMemo, useState } from 'react'
import { planOriginLabels, taskStateLabels } from '../../lib/contracts'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { AcceptanceCriterion, PlanState, SpecVersion } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { MarkdownProse } from '../ui/markdown-prose'
import { SectionCount, SectionHeading } from './section-heading'
import { useCollapsibleContent } from './use-collapsible-content'

// Machine-owned fenced blocks of the legacy spec format are stripped from the prose and
// rendered structurally by the acceptance checklist below.
const machineBlock = /```conveyor:(?:acceptance|decomposition)\n[\s\S]*?```\n?/g

const verifyIcons: Record<AcceptanceCriterion['verify'], typeof FlaskConical> = {
  test: FlaskConical,
  playwright: Globe,
  'computer-use': MousePointerClick,
  human: Square,
}

// The spec review card: rendered markdown plus the
// acceptance-criteria checklist. Human-verify criteria surface as explicit
// checkboxes rather than being pretend-verified.
export function SpecCard({
  spec,
  collapsible = true,
  overflowExpandable = false,
  routeVariant = 'sheet',
  minimumSeatCount,
  planState,
}: {
  spec: SpecVersion
  collapsible?: boolean
  overflowExpandable?: boolean
  routeVariant?: TaskRouteVariant
  // The task's review seat minimum, stated beside this version's planned
  // count (feature-review-seat-minimum RS-3, RS-7).
  minimumSeatCount?: number
  // The task's served plan state. Task detail passes it, and the card then
  // states the version's origin, base commit, and approval from it
  // (component-web-task-surfaces invariant 60; req-task-plan-state AC-1.4).
  planState?: PlanState
}) {
  const { workspace } = useWorkspaceSelection()
  const [collapsed, setCollapsed] = useState(false)
  const [contentExpanded, setContentExpanded] = useState(false)
  const viewportID = useId()
  const expanded = !collapsible || !collapsed
  const overflowExpanded = !overflowExpandable || contentExpanded
  // The preview may run up to half its height further to end at a block
  // boundary instead of fading through the space around a heading.
  const collapse = useCollapsibleContent(176, overflowExpanded, overflowExpandable && expanded, 88)
  // Agent-authored prose sometimes runs a sentence straight into a heading with
  // a single newline; markdown needs a blank line for the `#` to render as one.
  const prose = useMemo(
    () => spec.content.replace(machineBlock, '').replace(/([^\n])\n(#{1,6} )/g, '$1\n\n$2'),
    [spec.content],
  )
  const criteria = spec.acceptance ?? []
  // Only this displayed version's own count is shown; no other plan's count
  // substitutes for it.
  const plannedSeats = typeof spec.review_seats === 'number' ? spec.review_seats : undefined
  const relatedRoute = relatedTaskRoute(routeVariant)
  // On task detail approval reads from the version and the served plan state,
  // never from orders or events: "Awaiting approval" only while the task is
  // at awaiting_approval (invariant 60).
  const approval = spec.approved
    ? 'Approved'
    : planState === undefined || planState === 'awaiting_approval'
      ? 'Awaiting approval'
      : 'Not approved'

  const toggleCollapsed = () => {
    if (!collapsed) setContentExpanded(false)
    setCollapsed(!collapsed)
  }

  return (
    <Card className="rounded-lg">
      {/* The title group stays whole on one line. The status line follows it
          when both fit, and otherwise moves to its own line, where it wraps
          between its parts (component-web-task-surfaces). */}
      <CardHeader className="flex-wrap items-center gap-y-2">
        {collapsible ? (
          <button
            type="button"
            onClick={toggleCollapsed}
            aria-expanded={expanded}
            className="flex shrink-0 items-center gap-2 whitespace-nowrap text-left"
          >
            <ChevronRight className={cn('size-3.5 text-faint transition-transform', expanded && 'rotate-90')} />
            <SectionHeading>Execution plan</SectionHeading>
            <SectionCount>v{spec.version}</SectionCount>
          </button>
        ) : (
          <div className="flex shrink-0 items-center gap-2 whitespace-nowrap">
            <SectionHeading>Execution plan</SectionHeading>
            <SectionCount>v{spec.version}</SectionCount>
          </div>
        )}
        {/* One run of text in one style: the seat count, a separator, the
            time, then the badge. It breaks only after the separator or before
            the badge, so a wrapped line starts with the time or the badge and
            nothing extends past the card's border. */}
        <div className="min-w-0 text-xs leading-6 text-muted" data-plan-status>
          {plannedSeats !== undefined && (
            <>
              <span className="whitespace-nowrap">
                {`Review: ${plannedSeats} ${plannedSeats === 1 ? 'seat' : 'seats'}`}
                {minimumSeatCount !== undefined ? ` (minimum ${minimumSeatCount})` : ''}
              </span>
              <span aria-hidden="true">{'\u00a0· '}</span>
            </>
          )}
          <span className="whitespace-nowrap">
            {spec.approved && spec.approved_at
              ? `approved ${absoluteTime(spec.approved_at)}`
              : `drafted ${absoluteTime(spec.created_at)}`}
          </span>{' '}
          {/* The plan version's own state, quiet: the task's state is named once,
              in the header chip and the next action. */}
          <Badge variant={spec.approved ? 'positive' : 'outline'} className="ml-1 align-middle">
            {approval}
          </Badge>
        </div>
        {planState !== undefined && <PlanProvenance spec={spec} />}
      </CardHeader>
      {expanded && (
        <CardContent className="py-4">
          <div className="relative">
            <div id={viewportID} ref={collapse.viewportRef} style={collapse.viewportStyle}>
              <div>
                <MarkdownProse>{prose}</MarkdownProse>
                {criteria.length > 0 && (
                  <div className="mt-5 border-t border-border pt-4">
                    <SectionHeading as="h4" count={criteria.length} className="mb-3">
                      Acceptance criteria
                    </SectionHeading>
                    <ul className="space-y-2">
                      {criteria.map((criterion) => (
                        <CriterionRow key={criterion.id} criterion={criterion} />
                      ))}
                    </ul>
                  </div>
                )}
                {(spec.decomposition?.length ?? 0) > 0 && (
                  <div className="mt-5 border-t border-border pt-4">
                    <SectionHeading as="h4" count={spec.decomposition!.length} className="mb-3">
                      Decomposition
                    </SectionHeading>
                    <ul className="space-y-2">
                      {spec.decomposition!.map((item) => (
                        <li key={item.id} className="flex items-baseline gap-2 text-sm">
                          <Badge variant="mono">{item.id}</Badge>
                          {spec.materialized_children?.find((child) => child.origin_sub_id === item.id) ? (
                            <Link
                              to={relatedRoute}
                              params={{
                                workspaceId: workspace,
                                taskId: spec.materialized_children!.find((child) => child.origin_sub_id === item.id)!
                                  .id,
                              }}
                              className="text-primary hover:underline"
                            >
                              {item.summary}
                            </Link>
                          ) : (
                            <span className="text-foreground/85">{item.summary}</span>
                          )}
                          <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">
                            {item.repo}
                            {item.depends_on?.length ? ` ← ${item.depends_on.join(', ')}` : ''}
                            {spec.materialized_children?.find((child) => child.origin_sub_id === item.id) &&
                              ` · ${taskStateLabels[spec.materialized_children.find((child) => child.origin_sub_id === item.id)!.state] ?? spec.materialized_children.find((child) => child.origin_sub_id === item.id)!.state}`}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </div>
            </div>
            {collapse.clamped && (
              <div
                aria-hidden="true"
                className={cn(
                  'spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0',
                  collapse.fadeClass,
                )}
                data-spec-overflow-shadow
              />
            )}
          </div>
          {collapse.collapsible && (
            <div className="-mx-4 -mb-4 mt-3 border-t border-border">
              <button
                type="button"
                aria-controls={viewportID}
                aria-expanded={overflowExpanded}
                className="flex w-full items-center justify-center gap-1.5 rounded-b-md py-2 text-xs font-medium text-muted hover:bg-surface hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
                onClick={() => setContentExpanded((value) => !value)}
              >
                {overflowExpanded ? (
                  <ChevronUp aria-hidden="true" className="size-3.5" />
                ) : (
                  <ChevronDown aria-hidden="true" className="size-3.5" />
                )}
                {overflowExpanded ? 'Show less' : 'Show full plan'}
              </button>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  )
}

// The version's origin and the commit its plan was read against: 12
// characters with the full ID copyable, or "None recorded"
// (component-web-task-surfaces invariant 60; req-task-plan-state AC-2.5).
function PlanProvenance({ spec }: { spec: SpecVersion }) {
  const baseCommit = typeof spec.base_commit === 'string' ? spec.base_commit.trim() : ''
  return (
    <dl data-plan-provenance className="flex basis-full flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
      {spec.origin && (
        <div className="flex items-center gap-1.5">
          <dt className="text-faint">Origin</dt>
          <dd data-plan-origin={spec.origin}>{planOriginLabels[spec.origin] ?? spec.origin}</dd>
        </div>
      )}
      <div className="flex items-center gap-1.5">
        <dt className="text-faint">Base commit</dt>
        {baseCommit ? (
          <dd className="flex items-center gap-0.5" data-plan-base-commit={baseCommit}>
            <code className="font-mono" title={baseCommit}>
              {baseCommit.slice(0, 12)}
            </code>
            <CopyButton value={baseCommit} label="Copy base commit" className="size-6" />
          </dd>
        ) : (
          <dd data-plan-base-commit="">None recorded</dd>
        )}
      </div>
    </dl>
  )
}

function CriterionRow({ criterion }: { criterion: AcceptanceCriterion }) {
  const Icon = verifyIcons[criterion.verify] ?? FlaskConical
  const human = criterion.verify === 'human'
  return (
    <li
      className={cn(
        'flex items-start gap-2.5 rounded-md border px-3 py-2',
        human ? 'border-edge bg-raised/40' : 'border-border',
      )}
    >
      <Icon className={cn('mt-0.5 size-4 shrink-0', human ? 'text-foreground' : 'text-faint')} />
      <div className="min-w-0">
        <p className="text-sm leading-6 text-foreground/90">{criterion.criterion}</p>
        <p className="mt-0.5 font-mono text-[11px] text-faint">
          {criterion.id} · {criterion.verify}
          {criterion.ref ? ` · ${criterion.ref}` : ''}
          {human ? ' — checked by the reviewer at the gate' : ''}
        </p>
      </div>
    </li>
  )
}
