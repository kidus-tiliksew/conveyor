import {
  AlertTriangle,
  CircleDot,
  CircleHelp,
  ClipboardCheck,
  FileText,
  GitMerge,
  GitPullRequestClosed,
  type LucideIcon,
  PauseCircle,
  RefreshCw,
  RotateCcw,
} from 'lucide-react'
import type { InboxItem } from '../../lib/types'
import { cn } from '../../lib/utils'

// The inbox's kind presentation (feature-operator-next-action NA-6). Each
// next-action kind, and a document proposal, maps to one icon and one tone.
// The tone says what sort of call it is, so the attention tone is left for
// urgency alone: a decision that unblocks other tasks or one due soon. The
// attention variant stays the only alarm colour (component-web-dashboard).
export type InboxTone = 'gate' | 'ship' | 'question' | 'broken' | 'document' | 'neutral'

const kinds: Record<string, { tone: InboxTone; icon: LucideIcon }> = {
  plan_approval: { tone: 'gate', icon: ClipboardCheck },
  plan_revision: { tone: 'gate', icon: ClipboardCheck },
  merge_approval: { tone: 'ship', icon: GitMerge },
  merge_execution: { tone: 'ship', icon: GitMerge },
  agent_question: { tone: 'question', icon: CircleHelp },
  context_proposal: { tone: 'question', icon: FileText },
  review_bounce_limit: { tone: 'broken', icon: RotateCcw },
  job_failure: { tone: 'broken', icon: AlertTriangle },
  merge_conflict: { tone: 'broken', icon: GitPullRequestClosed },
  dispatch_exhausted: { tone: 'broken', icon: AlertTriangle },
  verification_disposition: { tone: 'broken', icon: AlertTriangle },
  document_proposal: { tone: 'document', icon: FileText },
  parked: { tone: 'neutral', icon: PauseCircle },
  stalled: { tone: 'neutral', icon: PauseCircle },
  blocked: { tone: 'neutral', icon: PauseCircle },
  retrying: { tone: 'neutral', icon: RefreshCw },
}

export function inboxKind(kind: string | undefined) {
  return kinds[kind ?? ''] ?? { tone: 'neutral' as const, icon: CircleDot }
}

// The kind an inbox item presents: `document_proposal` for a proposal, else
// its next action's kind.
export function itemKind(item: InboxItem) {
  return item.kind === 'document_proposal' ? 'document_proposal' : (item.next_action?.kind ?? '')
}

export const toneText: Record<InboxTone, string> = {
  gate: 'text-primary',
  ship: 'text-positive',
  question: 'text-info',
  broken: 'text-failure',
  document: 'text-document',
  neutral: 'text-muted',
}

export const tonePill: Record<InboxTone, string> = {
  gate: 'bg-primary-soft text-primary',
  ship: 'bg-positive-soft text-positive',
  question: 'bg-info-soft text-info',
  broken: 'bg-failure-soft text-failure',
  document: 'bg-document-soft text-document',
  neutral: 'bg-surface text-muted',
}

export const toneBar: Record<InboxTone, string> = {
  gate: 'bg-primary',
  ship: 'bg-positive',
  question: 'bg-info',
  broken: 'bg-failure',
  document: 'bg-document',
  neutral: 'bg-edge',
}

export function KindIcon({ kind, className }: { kind: string; className?: string }) {
  const { tone, icon: Icon } = inboxKind(kind)
  return <Icon aria-hidden data-kind-tone={tone} className={cn('size-3.5 shrink-0', toneText[tone], className)} />
}

// The server's label for a kind, as a pill in the kind's tone with its icon.
export function KindPill({ kind, label, className }: { kind: string; label: string; className?: string }) {
  const { tone } = inboxKind(kind)
  return (
    <span
      data-decision-label
      data-kind-tone={tone}
      className={cn(
        'inline-flex min-w-0 items-center gap-1.5 rounded-md px-2 py-0.5 text-xs font-semibold',
        tonePill[tone],
        className,
      )}
    >
      <KindIcon kind={kind} className="text-current" />
      <span className="min-w-0 truncate">{label}</span>
    </span>
  )
}

// A deadline within this many minutes is urgent.
export const URGENT_MINUTES = 180

// Minutes until the deadline (negative once passed) and its countdown text:
// "due in 1h 32m", "due in 2d", or "<x> overdue". Read from the client clock,
// the same way in the row, the detail and the summary's urgent count.
export function countdown(deadline: string, now = Date.now()) {
  const minutes = Math.round((new Date(deadline).getTime() - now) / 60_000)
  const abs = Math.abs(minutes)
  const span =
    abs < 60
      ? `${abs}m`
      : abs < 24 * 60
        ? `${Math.floor(abs / 60)}h${abs % 60 ? ` ${abs % 60}m` : ''}`
        : `${Math.round(abs / 1440)}d`
  return { minutes, text: minutes < 0 ? `${span} overdue` : `due in ${span}` }
}

function validDeadline(deadline: string | null | undefined): deadline is string {
  return Boolean(deadline) && Number.isFinite(new Date(deadline as string).getTime())
}

// The deadline countdown chip: attention under three hours, failure once
// overdue, neutral otherwise. No deadline renders nothing.
export function DueChip({ deadline, className }: { deadline: string | null | undefined; className?: string }) {
  if (!validDeadline(deadline)) return null
  const due = countdown(deadline)
  const tone = due.minutes < 0 ? 'overdue' : due.minutes < URGENT_MINUTES ? 'soon' : 'later'
  return (
    <time
      data-due={tone}
      dateTime={deadline}
      title={new Date(deadline).toLocaleString()}
      className={cn(
        'inline-flex shrink-0 items-center rounded px-1.5 font-medium tabular-nums',
        tone === 'overdue'
          ? 'bg-failure-soft text-failure'
          : tone === 'soon'
            ? 'bg-attention-soft text-attention'
            : 'bg-surface text-muted',
        className,
      )}
    >
      {due.text}
    </time>
  )
}

// A decision is urgent when it unblocks other tasks or is due within three
// hours (an overdue one included).
export function isUrgent(item: InboxItem, now = Date.now()) {
  if ((item.next_action?.blocks?.length ?? 0) > 0) return true
  const deadline = item.deadline ?? item.next_action?.deadline
  return validDeadline(deadline) && countdown(deadline, now).minutes < URGENT_MINUTES
}

// "14m", "3h", "2d" since the given time, or '' when it is unusable.
export function compactAge(value: string | undefined, now = Date.now()) {
  if (!value) return ''
  const at = new Date(value).getTime()
  if (!Number.isFinite(at) || at <= 0) return ''
  const minutes = Math.max(0, Math.round((now - at) / 60_000))
  if (minutes < 60) return `${minutes}m`
  if (minutes < 24 * 60) return `${Math.round(minutes / 60)}h`
  return `${Math.round(minutes / 1440)}d`
}

// The local time of day a deadline falls at: "11:26 AM".
export function timeOfDay(value: string) {
  return new Date(value).toLocaleTimeString('en', { hour: 'numeric', minute: '2-digit' })
}

// "Unblocks N" in the attention tone: deciding this frees N waiting tasks.
export function UnblocksMark({ count, className }: { count: number; className?: string }) {
  if (count <= 0) return null
  return (
    <span
      data-row-blocks
      className={cn(
        'inline-flex shrink-0 items-center rounded bg-attention-soft px-1.5 font-medium text-attention',
        className,
      )}
    >
      Unblocks {count}
    </span>
  )
}
