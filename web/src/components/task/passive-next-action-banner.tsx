import { Copy, ListTree, OctagonX } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import type { ActivityItem, NextAction, WorkOrder } from '../../lib/types'
import { cn, fullTime } from '../../lib/utils'
import { Disclosure } from '../ui/disclosure'
import { DropdownMenu, DropdownMenuItem, DropdownMenuSeparator } from '../ui/dropdown-menu'
import { NextActionIDList } from './next-action-parts'
import { WorkOrderPreemptControl } from './work-order-preempt-card'

// The share of the execution span after which the banner takes the attention
// tone: the last tenth of the agent's time.
const attentionShare = 0.9

// The Timeline's one-row banner for a `working` or `queued` statement that
// offers no choice (component-web-task-surfaces invariant 32;
// req-operator-next-action AC-7.1). It prints the server's label, actor,
// summary, if-ignored sentence, deadline, and identifiers, and derives none of
// them (AC-1.7, AC-1.8; component-next-action invariant 33). The summary is
// the row's one heading; the actor and the if-ignored sentence sit in the
// chip's tooltip and accessible description; the identifiers and the stop act
// sit behind one "More actions" menu. The model and the elapsed time come
// from the stated current order alone, and a field the order lacks prints
// nothing.
export function PassiveNextActionBanner({
  item,
  action,
  preemptOrder,
}: {
  item: ActivityItem
  action: NextAction
  preemptOrder: WorkOrder | undefined
}) {
  const [panel, setPanel] = useState<'ids' | 'stop' | null>(null)
  const [copied, setCopied] = useState(false)
  const section = useRef<HTMLElement>(null)
  const now = useNow()
  const orderID = action.current_order?.id
  const order = orderID ? item.work_orders?.find((candidate) => candidate.id === orderID) : undefined
  const working = action.kind === 'working'
  const started = working ? validTime(order?.execution_started_at) : undefined
  const queued = working ? undefined : validTime(order?.queue_entered_at)
  const deadline = validTime(action.deadline ?? undefined)
  const span = started !== undefined && deadline !== undefined && deadline > started ? deadline - started : undefined
  const share = span === undefined || started === undefined ? undefined : clamp((now - started) / span)
  const attention = share !== undefined && share >= attentionShare

  useEffect(() => {
    if (!copied) return
    const timer = window.setTimeout(() => setCopied(false), 1500)
    return () => window.clearTimeout(timer)
  }, [copied])

  const closeStop = () => {
    setPanel(null)
    section.current?.querySelector<HTMLButtonElement>('button[aria-label="More actions"]')?.focus()
  }
  const model = working ? order?.model?.trim() : undefined
  const since = started ?? queued
  // On a tight row the meta gives way before the sentence. It is one clipped
  // line that wraps in reverse: the model shows in full or drops off the line
  // first, and the elapsed time truncates only when it is alone and still
  // too wide.
  const meta = (model || since !== undefined) && (
    <p
      data-banner-meta
      className="flex h-4 min-w-0 flex-row-reverse flex-wrap items-baseline justify-end overflow-hidden text-xs leading-4 whitespace-nowrap text-muted max-sm:col-start-2 max-sm:col-end-5 max-sm:row-start-2"
    >
      {since !== undefined && (
        <time
          className="min-w-0 max-w-full truncate"
          dateTime={new Date(since).toISOString()}
          title={fullTime(new Date(since).toISOString())}
        >
          {started !== undefined ? 'started' : 'queued'} {compactSpan(now - since)} ago
        </time>
      )}
      {model && (
        <span data-banner-model title={model}>
          {model}
          {since !== undefined && (
            <span aria-hidden="true" className="mx-1.5">
              ·
            </span>
          )}
        </span>
      )}
    </p>
  )

  return (
    <section
      ref={section}
      aria-label="Next action"
      data-passive-banner
      data-tone={attention ? 'attention' : 'neutral'}
      className={cn('min-w-0 rounded-lg border px-3 py-2', attention ? 'border-attention/60' : 'border-border/70')}
    >
      {/* The sentence's column takes its full width before the meta's flexible
          one, so on a narrow row the meta truncates before the sentence.
          Below 640 px the meta moves to a second row under the sentence. */}
      <div className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)_auto_auto] items-start gap-x-2 gap-y-0.5 sm:grid-cols-[auto_minmax(0,max-content)_minmax(0,1fr)_auto_auto] sm:items-center">
        <StatusChip action={action} />
        <h2 className="min-w-0 py-0.5 text-sm font-medium leading-5 sm:truncate" title={action.summary}>
          {action.summary}
        </h2>
        {meta || <span aria-hidden="true" className="max-sm:hidden" />}
        <div className="flex min-h-6 items-center">
          {deadline !== undefined && action.deadline && (
            <TimeLeft deadline={action.deadline} remaining={deadline - now} share={share} attention={attention} />
          )}
        </div>
        <DropdownMenu label="More actions" trigger="overflow" className="-my-1 -mr-1.5">
          {orderID && (
            <DropdownMenuItem
              onSelect={() => {
                void navigator.clipboard.writeText(orderID).then(() => setCopied(true))
              }}
            >
              <Copy aria-hidden="true" />
              Copy order ID
            </DropdownMenuItem>
          )}
          <DropdownMenuItem onSelect={() => setPanel((open) => (open === 'ids' ? null : 'ids'))}>
            <ListTree aria-hidden="true" />
            {panel === 'ids' ? 'Hide IDs' : 'Show IDs'}
          </DropdownMenuItem>
          {preemptOrder && (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem destructive onSelect={() => setPanel('stop')}>
                <OctagonX aria-hidden="true" />
                Stop attempt…
              </DropdownMenuItem>
            </>
          )}
        </DropdownMenu>
      </div>
      <span className="sr-only" aria-live="polite">
        {copied ? 'Order ID copied' : ''}
      </span>
      {panel === 'ids' && (
        <div data-next-action-ids className="mt-2">
          <NextActionIDList value={{ details: action.details, current_order: action.current_order }} />
        </div>
      )}
      {preemptOrder && (
        <div className={cn(panel === 'stop' && 'mt-2')}>
          <WorkOrderPreemptControl
            order={preemptOrder}
            open={panel === 'stop'}
            onOpenChange={(open) => (open ? setPanel('stop') : closeStop())}
          />
        </div>
      )}
    </section>
  )
}

// The label as a compact status chip. The actor and the if-ignored sentence
// are its tooltip, which opens on hover, keyboard focus, and tap, and the
// if-ignored sentence is also its accessible description, so it never depends
// on hover alone (component-next-action invariant 33).
function StatusChip({ action }: { action: NextAction }) {
  const descriptionID = useId()
  const working = action.kind === 'working'
  const ignored = action.if_ignored ? `If ignored: ${action.if_ignored}` : ''
  return (
    <>
      <Disclosure
        contentClassName="space-y-1"
        content={
          <>
            <span className="block capitalize">{action.actor}</span>
            {ignored && <span className="block text-muted">{ignored}</span>}
          </>
        }
        renderTrigger={(trigger) => (
          <button
            {...trigger}
            aria-label={`${action.label}, ${action.actor}`}
            aria-describedby={ignored ? descriptionID : undefined}
            data-actor={action.actor}
            data-status-chip={action.kind}
            className={cn(
              trigger.className,
              'h-6 shrink-0 gap-1.5 rounded-[5px] bg-raised px-1.5 text-xs font-medium whitespace-nowrap text-muted outline-none focus-visible:outline-2 focus-visible:outline-primary',
            )}
          >
            <span aria-hidden="true" className="relative flex size-2">
              {/* A few pulses when the banner mounts, then a steady dot: an
                  endless animation would keep the Board sheet from settling. */}
              {working && (
                <span
                  data-pulse
                  className="absolute inline-flex size-full animate-ping rounded-full bg-primary/60 [animation-iteration-count:3] motion-reduce:animate-none"
                />
              )}
              <span className={cn('relative inline-flex size-2 rounded-full', working ? 'bg-primary' : 'bg-faint')} />
            </span>
            {action.label}
          </button>
        )}
      />
      {ignored && (
        <span id={descriptionID} className="sr-only">
          {ignored}
        </span>
      )}
    </>
  )
}

// The time left before the stated deadline, with the full time in a tooltip
// and a `<time>` element. When the execution start is known, a bar shows the
// elapsed share of the span; its last tenth takes the attention tone.
function TimeLeft({
  deadline,
  remaining,
  share,
  attention,
}: {
  deadline: string
  remaining: number
  share: number | undefined
  attention: boolean
}) {
  const absolute = `Deadline ${fullTime(deadline)}`
  return (
    <Disclosure
      className="justify-end"
      contentClassName="right-0 left-auto"
      content={absolute}
      renderTrigger={(trigger) => (
        <button
          {...trigger}
          data-next-action-deadline
          data-tone={attention ? 'attention' : 'neutral'}
          className={cn(
            trigger.className,
            'gap-2 rounded text-xs whitespace-nowrap outline-none focus-visible:outline-2 focus-visible:outline-primary',
            attention ? 'font-medium text-attention' : 'text-muted',
          )}
        >
          {share !== undefined && (
            <span
              aria-hidden="true"
              data-deadline-progress={Math.round(share * 100)}
              className="relative h-1 w-6 overflow-hidden rounded-full bg-border max-sm:hidden"
            >
              <span
                className={cn('absolute inset-y-0 left-0 rounded-full', attention ? 'bg-attention-dot' : 'bg-faint')}
                style={{ width: `${share * 100}%` }}
              />
            </span>
          )}
          <time dateTime={deadline}>{remaining > 0 ? `${compactSpan(remaining)} left` : 'Deadline passed'}</time>
          <span className="sr-only">({absolute})</span>
        </button>
      )}
    />
  )
}

// A display clock for the banner's durations. It reads no API, drives no
// lifecycle decision, and stops with the banner.
function useNow() {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000)
    return () => window.clearInterval(timer)
  }, [])
  return now
}

function validTime(value: string | undefined) {
  if (!value) return undefined
  const at = Date.parse(value)
  return Number.isFinite(at) && at > 0 ? at : undefined
}

function clamp(value: number) {
  return Math.min(1, Math.max(0, value))
}

// "4h", "2h 51m", "12m", "<1m", "1d 4h": a span at the precision a glance needs.
function compactSpan(ms: number) {
  const minutes = Math.floor(Math.max(0, ms) / 60_000)
  if (minutes < 1) return '<1m'
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return minutes % 60 ? `${hours}h ${minutes % 60}m` : `${hours}h`
  const days = Math.floor(hours / 24)
  return hours % 24 ? `${days}d ${hours % 24}h` : `${days}d`
}
