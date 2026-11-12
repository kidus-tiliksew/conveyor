import { Copy, Cpu, ListTree, OctagonX } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { selectModelLogo } from '../../lib/model-logo'
import type { ActivityItem, NextAction, WorkOrder } from '../../lib/types'
import { cn, fullTime } from '../../lib/utils'
import { Disclosure } from '../ui/disclosure'
import { DropdownMenu, DropdownMenuItem, DropdownMenuSeparator } from '../ui/dropdown-menu'
import { logoAssets } from './model-logo-assets'
import { NextActionIDList } from './next-action-parts'
import { PersonAvatar, personLabel, runClaimantUserId, usePersonResolver } from './run-by'
import { WorkOrderPreemptControl } from './work-order-preempt-card'

// The Timeline's banner for a `working` or `queued` statement that offers no
// choice (component-web-task-surfaces invariant 32; req-operator-next-action
// AC-7.1). It prints the server's label, actor, summary, if-ignored sentence,
// and identifiers, and derives none of them (AC-1.7, AC-1.8;
// component-next-action invariant 33). It prints no deadline, time left, or
// deadline progress, and takes no tone from the deadline (DEC-129(1)). The
// summary is the banner's one heading; the actor and the if-ignored sentence
// sit in the chip's tooltip and accessible description; the identifiers and
// the stop act sit behind one "More actions" menu. The model logo and the
// elapsed time come from the stated current order alone, and a field the
// order lacks prints nothing.
//
// The layout is one grid (DEC-129(3)). Below 640 px it has two lines: the
// claimant avatar, the summary, and the menu; then the status chip, the model
// logo, and the elapsed time at the line's right edge. From 640 px it is one
// row: chip, avatar and summary, model logo and elapsed time, then the menu.
// A long summary wraps and is never truncated.
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
  const [copy, setCopy] = useState<'idle' | 'copied' | 'failed'>('idle')
  const copyAttempt = useRef(0)
  const section = useRef<HTMLElement>(null)
  const now = useNow()
  const resolve = usePersonResolver()
  const orderID = action.current_order?.id
  const order = orderID ? item.work_orders?.find((candidate) => candidate.id === orderID) : undefined
  const working = action.kind === 'working'
  const started = working ? validTime(order?.execution_started_at) : undefined
  const queued = working ? undefined : validTime(order?.queue_entered_at)

  useEffect(() => {
    if (copy !== 'copied') return
    const timer = window.setTimeout(() => setCopy('idle'), 1500)
    return () => window.clearTimeout(timer)
  }, [copy])

  // `navigator.clipboard` is undefined outside a secure context, `writeText`
  // can throw before it returns a promise, and the browser can refuse the
  // write. Each is a failed copy: the banner says so and points to Show IDs,
  // and no error escapes the menu's handler. Only the newest attempt sets the
  // result, so a slow earlier attempt cannot overwrite a later one. The result
  // is a local copy outcome, not a server statement (AC-1.7, AC-1.8).
  const copyOrderID = async (id: string) => {
    const attempt = ++copyAttempt.current
    setCopy('idle')
    let result: 'copied' | 'failed'
    try {
      await navigator.clipboard.writeText(id)
      result = 'copied'
    } catch {
      result = 'failed'
    }
    if (attempt === copyAttempt.current) setCopy(result)
  }

  const closeStop = () => {
    setPanel(null)
    section.current?.querySelector<HTMLButtonElement>('button[aria-label="More actions"]')?.focus()
  }
  // The person who holds the stated current order: a run:<user id> claimant
  // of a claimed order. The banner still prints the server's summary as is
  // (component-next-action invariant 17).
  const claimedBy =
    order?.state === 'claimed'
      ? (order.claimed_by ?? action.attempt?.claimed_by)
      : !order && action.current_order?.state === 'claimed'
        ? action.attempt?.claimed_by
        : undefined
  const claimant = resolve(runClaimantUserId(claimedBy))
  const model = working ? order?.model?.trim() : undefined
  const since = started ?? queued
  // The meta line holds the model logo, then the elapsed time pushed to the
  // line's right edge. Below 640 px it takes the second line beside the chip;
  // from 640 px it sits between the summary and the menu.
  const meta = (model || since !== undefined) && (
    <p
      data-banner-meta
      className="flex h-6 min-w-0 items-center gap-2 text-xs leading-4 whitespace-nowrap text-muted max-sm:col-span-2 max-sm:col-start-2 max-sm:row-start-2"
    >
      {model && <BannerModelLogo model={model} agent={order?.agent} />}
      {since !== undefined && (
        <span className="ml-auto flex min-w-0">
          <time
            className="min-w-0 truncate"
            dateTime={new Date(since).toISOString()}
            title={fullTime(new Date(since).toISOString())}
          >
            {started !== undefined ? 'started' : 'queued'} {compactSpan(now - since)} ago
          </time>
        </span>
      )}
    </p>
  )

  return (
    <section
      ref={section}
      aria-label="Next action"
      data-passive-banner
      data-tone="neutral"
      className="min-w-0 rounded-lg border border-border/70 px-3 py-2"
    >
      {/* Below 640 px: three columns, summary and menu on the first line, chip
          and meta on the second. From 640 px: four columns in one row. The
          summary's column is the only flexible one, so a long summary wraps
          and the meta and menu stay inside the banner. */}
      <div className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1.5 sm:grid-cols-[auto_minmax(0,1fr)_auto_auto] sm:items-start sm:gap-y-0.5">
        <StatusChip action={action} className="max-sm:col-start-1 max-sm:row-start-2" />
        <div className="flex min-w-0 items-start gap-1.5 max-sm:col-span-2 max-sm:col-start-1 max-sm:row-start-1 max-sm:self-start">
          {claimant && (
            // The solid ring marks the live claimant; the halo is off so it
            // cannot run into the chip (DEC-129).
            <PersonAvatar
              person={claimant}
              label={personLabel(claimant, 'working on this order')}
              size="inline"
              live
              halo={false}
              className="mt-1 max-sm:ml-1"
            />
          )}
          <h2 className="min-w-0 py-0.5 text-sm font-medium leading-5">{action.summary}</h2>
        </div>
        {meta || <span aria-hidden="true" className="max-sm:hidden" />}
        <DropdownMenu
          label="More actions"
          trigger="overflow"
          className="-my-1 -mr-1.5 max-sm:col-start-3 max-sm:row-start-1 max-sm:self-start"
        >
          {orderID && (
            <DropdownMenuItem
              onSelect={() => {
                void copyOrderID(orderID)
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
        {copy === 'copied' ? 'Order ID copied' : ''}
      </span>
      {copy === 'failed' && (
        <p data-copy-failed role="alert" className="mt-2 text-xs text-failure">
          Could not copy the order ID. Use Show IDs to view it.
        </p>
      )}
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
// on hover alone (component-next-action invariant 33). On a coarse pointer the
// chip keeps its 24 px height: it opts out of the shared 40 px minimum and a
// pseudo-element outside the layout box gives it 40 px of tap height, without
// reaching the model logo's tap area (DEC-129(4)).
function StatusChip({ action, className }: { action: NextAction; className?: string }) {
  const descriptionID = useId()
  const working = action.kind === 'working'
  const ignored = action.if_ignored ? `If ignored: ${action.if_ignored}` : ''
  return (
    <>
      <Disclosure
        className={className}
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
              'relative h-6 shrink-0 gap-1.5 rounded-[5px] bg-raised px-1.5 text-xs font-medium whitespace-nowrap text-muted outline-none focus-visible:outline-2 focus-visible:outline-primary',
              'pointer-coarse:min-h-0 pointer-coarse:min-w-0 pointer-coarse:after:absolute pointer-coarse:after:inset-x-0 pointer-coarse:after:-inset-y-2',
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

// The current order's model as its provider logo, or its harness logo when the
// model names no known provider, and a generic chip icon when neither matches.
// The model name, or "<model> via <harness>" for a harness logo, is the
// logo's accessible name and its tooltip, which opens on hover, keyboard
// focus, and tap (DEC-129(2)). On a coarse pointer the 14 px logo keeps its
// size; a pseudo-element outside the layout box gives it 40 px of tap height
// and reaches no farther left than the gap after the chip (DEC-129(4)).
function BannerModelLogo({ model, agent }: { model: string; agent?: string }) {
  const selected = selectModelLogo(model, agent)
  const logo = selected ? logoAssets[selected.logo] : undefined
  const name = selected?.source === 'harness' ? `${model} via ${selected.harness}` : model
  return (
    <Disclosure
      className="shrink-0"
      content={name}
      renderTrigger={(trigger) => (
        <button
          {...trigger}
          aria-label={name}
          data-banner-model={model}
          className={cn(
            trigger.className,
            'relative size-3.5 shrink-0 justify-center rounded-sm outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
            'pointer-coarse:min-h-0 pointer-coarse:min-w-0 pointer-coarse:after:absolute pointer-coarse:after:-inset-y-[13px] pointer-coarse:after:-right-[13px] pointer-coarse:after:-left-1',
          )}
        >
          {logo ? (
            <span
              aria-hidden="true"
              className={cn('inline-flex size-3.5 [&_svg]:size-3.5', logo.className)}
              dangerouslySetInnerHTML={{ __html: logo.svg }}
            />
          ) : (
            <Cpu aria-hidden="true" className="size-3.5 text-faint" />
          )}
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
