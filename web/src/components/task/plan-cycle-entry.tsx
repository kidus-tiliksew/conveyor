import { ChevronDown, ChevronUp } from 'lucide-react'
import { type ReactNode, useId, useLayoutEffect, useRef, useState } from 'react'
import type { TaskPlanCycle, TaskPlanCycleOutcome, TaskPlanCycleStep, TaskPlanCycleStepKind } from '../../lib/types'
import { absoluteTime, cn, fullTime } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { PersonAvatar, personLabel, usePersonResolver } from './run-by'
import { RuntimeDisclosure } from './runtime-disclosure'

// One plan cycle as one timeline entry: the request, every operator decision
// with its comment, every plan version, and the approval, read only from
// `summaries.plan_cycles` (component-web-task-surfaces invariants 67 to 71;
// DEC-130; req-260809-a40c75 AC-4.1, AC-4.2).

const outcomeBadges: Record<
  TaskPlanCycleOutcome,
  { label: string; variant: 'default' | 'attention' | 'accent' | 'positive' }
> = {
  approved: { label: 'Approved', variant: 'positive' },
  awaiting_decision: { label: 'Needs your decision', variant: 'attention' },
  planning: { label: 'Planning', variant: 'accent' },
  declined: { label: 'Revision declined', variant: 'default' },
  not_needed: { label: 'Plan no longer needed', variant: 'default' },
  closed: { label: 'Task closed', variant: 'default' },
  merged: { label: 'Task merged', variant: 'default' },
}

const requestKinds = new Set<TaskPlanCycleStepKind>(['revision_requested', 'planning_requested'])

export function planCycleStepLabel(step: TaskPlanCycleStep): string {
  const version = step.plan_version === undefined ? '' : `v${step.plan_version}`
  switch (step.kind) {
    case 'revision_requested':
      return `Agent asked to revise plan ${version}`
    case 'planning_requested':
      return 'Agent asked for a plan before implementing'
    case 'revision_approved':
      return 'Allowed a revision — back to planning'
    case 'revision_declined':
      return 'Declined the revision — implementation retried with direction'
    case 'revision_rejected':
      return 'Rejected the revision — task closed'
    case 'plan_drafted':
      return `Plan ${version} ready`
    case 'plan_filed':
      return `Plan ${version} filed with the task`
    case 'plan_attached':
      return `Plan ${version} attached`
    case 'changes_requested':
      return `Asked for changes to plan ${version}`
    case 'plan_approved':
      return `Approved plan ${version}`
    case 'plan_sent_back':
      return 'Sent back for a new plan'
    case 'plan_not_needed':
      return 'Plan no longer needed'
    case 'task_started_over':
      return 'Started over as a new task'
    case 'task_closed':
      return 'Task closed'
    case 'task_merged':
      return 'Task merged'
  }
}

// The header: "Plan vN" for a cycle a plan opened, "Plan revision" for a
// revision, with "vA → vB" once the revision produced a version.
export function planCycleHeading(cycle: TaskPlanCycle): { title: string; range?: string } {
  const { first_version: first, final_version: final, contested_version: contested } = cycle
  if (cycle.opened_by === 'plan') {
    const title = first === null ? 'Plan' : `Plan v${first}`
    return { title, range: first !== null && final !== null && final !== first ? `v${first} → v${final}` : undefined }
  }
  if (cycle.opened_by === 'revision_request')
    return {
      title: 'Plan revision',
      range: contested !== null && final !== null ? `v${contested} → v${final}` : undefined,
    }
  return {
    title: 'Planning request',
    range: first === null ? undefined : first === final || final === null ? `v${first}` : `v${first} → v${final}`,
  }
}

// A cycle that waits on a decision or a new plan version never folds, nor does
// the task's newest cycle, which explains the plan the task runs now
// (invariant 70; DEC-130(3)).
export function planCycleFoldable(cycle: TaskPlanCycle, newest: boolean) {
  return !newest && !planCycleOpen(cycle)
}

// The cycle's last deciding person, whom the rail shows.
export function planCycleDecider(cycle: TaskPlanCycle): TaskPlanCycleStep | undefined {
  return [...cycle.steps].reverse().find((step) => step.user_id)
}

function sameDay(a: string, b: string) {
  return new Date(a).toDateString() === new Date(b).toDateString()
}

function clockTime(value: string) {
  return new Date(value).toLocaleTimeString('en', { hour: 'numeric', minute: '2-digit' })
}

// A step on the same day as the step before shows its time alone; the full
// date stays in the title and the machine-readable dateTime.
function StepTime({ at, previous }: { at: string; previous?: string }) {
  return (
    <time
      dateTime={at}
      title={fullTime(at)}
      className="shrink-0 text-[11px] text-faint tabular-nums sm:ml-auto"
      data-plan-cycle-time
    >
      {previous && sameDay(previous, at) ? clockTime(at) : absoluteTime(at)}
    </time>
  )
}

// A rationale previews three lines and a comment four. The control appears
// only when the clamp hides text; a folded cycle mounts no steps, so this
// always measures visible text.
function StepText({ text, request, audit }: { text: string; request: boolean; audit?: ReactNode }) {
  const [expanded, setExpanded] = useState(false)
  const [overflowing, setOverflowing] = useState(false)
  const textRef = useRef<HTMLParagraphElement>(null)
  const textId = useId()
  useLayoutEffect(() => {
    const element = textRef.current
    if (!element || expanded) return
    const measure = () => setOverflowing(element.scrollHeight > element.clientHeight + 1)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    return () => observer.disconnect()
  }, [expanded, text])
  return (
    <div className="mt-1">
      <p
        id={textId}
        ref={textRef}
        className={cn(
          'whitespace-pre-wrap break-words text-sm leading-6 text-foreground/85',
          !expanded && (request ? 'line-clamp-3' : 'line-clamp-4'),
        )}
        data-plan-cycle-text={request ? 'request' : 'comment'}
      >
        {text}
      </p>
      {(overflowing || expanded) && (
        <button
          type="button"
          aria-expanded={expanded}
          aria-controls={textId}
          onClick={() => setExpanded((value) => !value)}
          className="inline-flex min-h-8 cursor-pointer items-center gap-1 rounded text-xs font-medium text-primary hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
        >
          {expanded ? <ChevronUp aria-hidden className="size-3.5" /> : <ChevronDown aria-hidden className="size-3.5" />}
          {expanded ? 'Show less' : request ? 'Show full request' : 'Show full comment'}
        </button>
      )}
      {audit}
    </div>
  )
}

function StepRow({
  step,
  previous,
  audit,
}: {
  step: TaskPlanCycleStep
  previous?: string
  audit?: (eventId: string) => ReactNode
}) {
  const resolve = usePersonResolver()
  const person = resolve(step.user_id)
  const label = planCycleStepLabel(step)
  return (
    <li className="py-1.5" data-plan-cycle-step={step.kind}>
      <div className="flex flex-col gap-0.5 sm:flex-row sm:items-baseline sm:gap-2">
        <span className="flex min-w-0 items-center gap-1.5 text-sm text-foreground/90">
          {person && <PersonAvatar person={person} label={personLabel(person, label)} size="inline" />}
          <span className="min-w-0 break-words">{label}</span>
        </span>
        <StepTime at={step.at} previous={previous} />
      </div>
      {step.text && (
        <StepText
          text={step.text}
          request={requestKinds.has(step.kind)}
          audit={step.truncated ? audit?.(step.event_id) : undefined}
        />
      )}
    </li>
  )
}

// The folded summary line: the opening rationale, or else the last comment.
function previewText(cycle: TaskPlanCycle) {
  const opening = cycle.steps[0]
  if (opening && requestKinds.has(opening.kind) && opening.text) return opening.text
  return [...cycle.steps].reverse().find((step) => step.text && !requestKinds.has(step.kind))?.text
}

// A cycle that waits on a decision or a new plan version is still open, so
// its runtime advances once a second (DEC-130(3)).
export function planCycleOpen(cycle: TaskPlanCycle) {
  return cycle.outcome === 'awaiting_decision' || cycle.outcome === 'planning'
}

// The cycle's span as a runtime with the step count. The first and last step
// times sit behind the runtime (invariant 70; DEC-127(1)). Each step row keeps
// its own time, because each step is one instant.
function CycleSpan({ cycle }: { cycle: TaskPlanCycle }) {
  const first = cycle.steps[0]?.at
  const last = cycle.steps[cycle.steps.length - 1]?.at
  if (!first || !last) return null
  const open = planCycleOpen(cycle)
  const count = cycle.steps.length
  const steps = `${count} ${count === 1 ? 'step' : 'steps'}`
  const firstText = `First step ${absoluteTime(first)}`
  const lastText = `Last step ${absoluteTime(last)}`
  return (
    <span
      className="ml-auto inline-flex shrink-0 items-center gap-1 text-xs text-muted tabular-nums"
      data-plan-cycle-span
    >
      <RuntimeDisclosure
        start={first}
        // An open cycle measures to now. A closed cycle ends at its last step.
        end={open ? undefined : last}
        open={open}
        contentClassName="left-auto right-0"
        triggerClassName="justify-end whitespace-nowrap pointer-coarse:-my-3"
        trigger={{ 'data-plan-cycle-runtime': '' }}
        render={(runtime) => ({
          face: (
            <span>
              {open && 'Open for '}
              <span data-plan-cycle-duration>{runtime ?? '—'}</span>
            </span>
          ),
          label: `${[open ? `Open for ${runtime}` : `Took ${runtime}`, steps, firstText, open ? `${lastText}. Still open` : lastText].join('. ')}.`,
          details: (
            <span className="flex flex-col gap-0.5 tabular-nums" data-plan-cycle-span-details>
              <time dateTime={first} data-plan-cycle-first>
                {firstText}
              </time>
              <time dateTime={last} data-plan-cycle-last>
                {lastText}
              </time>
              {open && <span data-plan-cycle-open>Still open</span>}
            </span>
          ),
        })}
      />
      <span aria-hidden>·</span>
      <span>{steps}</span>
    </span>
  )
}

// A viewer who cannot see the decision panel below the timeline gets wording
// that names the wait without directing them to a control they do not have.
const waitingWithoutPanel = { label: 'Waiting for a decision', step: 'Waiting for an operator decision' }

export function PlanCycleEntry({
  cycle,
  newest,
  rail,
  audit,
  decisionVisible = true,
}: {
  cycle: TaskPlanCycle
  newest: boolean
  rail: ReactNode
  audit?: (eventId: string) => ReactNode
  // Whether this viewer sees the decision panel below the timeline.
  decisionVisible?: boolean
}) {
  const foldable = planCycleFoldable(cycle, newest)
  // The fold state lives in the browser only (invariant 70).
  const [open, setOpen] = useState(!foldable)
  const stepsId = useId()
  const { title, range } = planCycleHeading(cycle)
  const badge =
    cycle.outcome === 'awaiting_decision' && !decisionVisible
      ? { ...outcomeBadges.awaiting_decision, label: waitingWithoutPanel.label }
      : outcomeBadges[cycle.outcome]
  const heading = (
    <>
      <strong className="font-semibold">{title}</strong>
      {range && <span className="font-mono text-xs text-muted">{range}</span>}
      <Badge variant={badge.variant}>{badge.label}</Badge>
    </>
  )
  const preview = previewText(cycle)
  const waiting = cycle.outcome === 'awaiting_decision'
  return (
    <li className="relative pl-7" data-plan-cycle={cycle.outcome} data-plan-cycle-folded={open ? undefined : ''}>
      {rail}
      <article aria-label={range ? `${title} ${range}` : title} className="rounded-lg border border-edge bg-raised/40">
        {/* The runtime sits beside the fold button, never inside it, so each
            control keeps its own activation. The heading does not shrink for
            it: on a narrow card the runtime takes its own line. */}
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 px-4 pt-3" data-plan-cycle-header>
          {foldable ? (
            <button
              type="button"
              aria-expanded={open}
              aria-controls={stepsId}
              onClick={() => setOpen((value) => !value)}
              className="flex min-h-8 min-w-0 max-w-full flex-[1_0_auto] cursor-pointer flex-wrap items-center gap-x-2 gap-y-1 rounded-lg text-left text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
            >
              {open ? (
                <ChevronUp aria-hidden className="size-3.5 text-muted" />
              ) : (
                <ChevronDown aria-hidden className="size-3.5 text-muted" />
              )}
              {heading}
            </button>
          ) : (
            <div className="flex min-h-8 min-w-0 max-w-full flex-[1_0_auto] flex-wrap items-center gap-x-2 gap-y-1 text-sm">
              {heading}
            </div>
          )}
          <CycleSpan cycle={cycle} />
        </div>
        <div id={stepsId} className="px-4 pb-3">
          {open ? (
            <ol className="mt-1 divide-y divide-border/60" aria-label="Plan cycle steps">
              {cycle.steps.map((step, index) => (
                <StepRow
                  key={`${step.event_id}-${step.kind}`}
                  step={step}
                  previous={cycle.steps[index - 1]?.at}
                  audit={audit}
                />
              ))}
              {waiting && (
                <li className="py-1.5 text-sm font-medium text-attention" data-plan-cycle-waiting>
                  {decisionVisible ? 'Waiting for your decision below' : waitingWithoutPanel.step}
                </li>
              )}
            </ol>
          ) : (
            preview && (
              <div className="mt-1" data-plan-cycle-summary>
                <p className="truncate text-sm text-foreground/85">{preview}</p>
              </div>
            )
          )}
        </div>
      </article>
    </li>
  )
}
