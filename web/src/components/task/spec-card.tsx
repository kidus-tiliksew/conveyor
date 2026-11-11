import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronRight, ChevronUp, FlaskConical, Globe, MousePointerClick, Square } from 'lucide-react'
import { useId, useMemo, useState } from 'react'
import { taskStateLabels } from '../../lib/contracts'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { AcceptanceCriterion, PlanState, RiskTier, SpecVersion } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { Disclosure } from '../ui/disclosure'
import { MarkdownProse } from '../ui/markdown-prose'
import { RiskTierBadge, RiskTierDisclosure, riskTierLabel } from './risk-tier'
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
  // states the version's base commit and approval from it, and no origin
  // (component-web-task-surfaces invariant 60; req-task-plan-state AC-1.4;
  // DEC-127(3)).
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
  // The approval or draft time sits behind the approval badge (DEC-127(1)).
  const approvalTime =
    spec.approved && spec.approved_at
      ? `Approved ${absoluteTime(spec.approved_at)}`
      : `Drafted ${absoluteTime(spec.created_at)}`
  const provenance = planState !== undefined

  const toggleCollapsed = () => {
    if (!collapsed) setContentExpanded(false)
    setCollapsed(!collapsed)
  }

  return (
    <Card className="rounded-lg">
      {/* The title group stays whole on one line. The status line follows it
          when both fit, and otherwise moves to its own line. The header is a
          size container, so the plan facts below lay out by its width
          (component-web-task-surfaces). */}
      <CardHeader className="@container flex-wrap items-center gap-y-2">
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
        {/* The seat count, then the approval badge. The approval or draft time
            sits behind the badge, which hover, tap, keyboard focus, and its
            accessible name reach (component-web-task-surfaces invariant 60;
            DEC-127(1)). */}
        <div
          className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs leading-6 text-muted"
          data-plan-status
        >
          {plannedSeats !== undefined && (
            <span className="whitespace-nowrap">
              {`Review: ${plannedSeats} ${plannedSeats === 1 ? 'seat' : 'seats'}`}
              {minimumSeatCount !== undefined ? ` (minimum ${minimumSeatCount})` : ''}
            </span>
          )}
          {/* The plan version's own state, quiet: the task's state is named once,
              in the header chip and the next action. */}
          <Disclosure
            label={`${approval}. ${approvalTime}.`}
            content={<span data-plan-approval-time>{approvalTime}</span>}
            triggerClassName="rounded-md pointer-coarse:-my-2"
          >
            <Badge variant={spec.approved ? 'positive' : 'outline'}>{approval}</Badge>
          </Disclosure>
        </div>
        {(provenance || spec.risk_tier) && (
          <PlanFacts spec={spec} provenance={provenance} tier={spec.risk_tier} reason={spec.risk_reason} />
        )}
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

// The commit the plan was read against and the plan's risk tier. On a header
// narrower than 40rem they form a two-column list with a fixed 6rem label
// column. On a wider header they share one line. Each row is a `contents`
// group on the narrow list, so its term and value sit in the list's columns.
// No origin shows (component-web-task-surfaces invariant 60; DEC-127(3)).
function PlanFacts({
  spec,
  provenance,
  tier,
  reason,
}: {
  spec: SpecVersion
  provenance: boolean
  tier?: RiskTier
  reason?: string
}) {
  return (
    <dl
      data-plan-facts
      className="grid basis-full grid-cols-[6rem_minmax(0,1fr)] items-center gap-x-2 gap-y-1 text-xs text-muted @min-[40rem]:flex @min-[40rem]:flex-wrap @min-[40rem]:gap-x-4"
    >
      {provenance && <PlanProvenance spec={spec} />}
      {tier && <PlanRiskTier tier={tier} reason={reason} />}
    </dl>
  )
}

const planFactRow = 'contents @min-[40rem]:flex @min-[40rem]:min-w-0 @min-[40rem]:items-center @min-[40rem]:gap-1.5'

// The commit the plan was read against: 12 characters with the full ID
// copyable, or "None recorded" (component-web-task-surfaces invariant 60;
// req-task-plan-state AC-2.5).
function PlanProvenance({ spec }: { spec: SpecVersion }) {
  const baseCommit = typeof spec.base_commit === 'string' ? spec.base_commit.trim() : ''
  return (
    <div data-plan-provenance className={planFactRow}>
      <dt className="text-faint">Base commit</dt>
      {baseCommit ? (
        <dd className="flex min-w-0 items-center gap-0.5" data-plan-base-commit={baseCommit}>
          <code className="font-mono" title={baseCommit}>
            {baseCommit.slice(0, 12)}
          </code>
          <CopyButton value={baseCommit} label="Copy base commit" className="size-6" />
        </dd>
      ) : (
        <dd data-plan-base-commit="">None recorded</dd>
      )}
    </div>
  )
}

// The risk tier this plan version states, beside the plan
// (req-risk-based-review AC-4.4). It is the plan's own statement, labeled as
// such, and not the task's tier, which the task facts show. The chip shows
// alone and its reason sits behind it (component-web-task-surfaces invariants
// 60 and 66; DEC-127(1), (2)). A version that states no tier shows nothing
// rather than an invented one.
function PlanRiskTier({ tier, reason }: { tier: RiskTier; reason?: string }) {
  return (
    <div data-plan-risk={tier} className={planFactRow}>
      <dt className="text-faint">Plan risk tier</dt>
      <dd className="flex min-w-0 items-center">
        {reason ? (
          <RiskTierDisclosure
            tier={tier}
            label={`Plan risk tier ${riskTierLabel(tier)}. ${reason}`}
            content={
              <span className="whitespace-normal break-words text-foreground/85" data-plan-risk-reason>
                {reason}
              </span>
            }
          />
        ) : (
          <RiskTierBadge tier={tier} />
        )}
      </dd>
    </div>
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
