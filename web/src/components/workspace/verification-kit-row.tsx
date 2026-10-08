import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { type ReactNode, useId, useState } from 'react'
import type {
  VerificationKit,
  VerificationKitDiagnostic,
  VerificationKitExercise,
  VerificationKitPin,
  VerificationKitStatus,
} from '../../lib/types'
import { cn } from '../../lib/utils'
import { Badge } from '../ui/badge'

// One kit in the workspace verification list (component-web-dashboard). Manifest
// text renders as plain text nodes, never markup (req-verification-kits AC-10.4).

const STATUS: Record<VerificationKitStatus, { label: string; variant: 'positive' | 'failure' | 'default' }> = {
  current: { label: 'Current', variant: 'positive' },
  behind: { label: 'Behind', variant: 'default' },
  pending: { label: 'Awaiting confirmation', variant: 'default' },
  unresolved: { label: 'Unresolved pin', variant: 'default' },
  unpinned: { label: 'No pins', variant: 'default' },
  invalid: { label: 'Invalid', variant: 'failure' },
}

export function VerificationKitStatusChip({ status }: { status: VerificationKitStatus }) {
  const entry = STATUS[status] ?? STATUS.unresolved
  return <Badge variant={entry.variant}>{entry.label}</Badge>
}

/** A disclosure whose trigger is a real button carrying aria-expanded. */
function Expandable({
  header,
  children,
  className,
  buttonClassName,
  label,
}: {
  header: ReactNode | ((open: boolean) => ReactNode)
  children: ReactNode
  className?: string
  buttonClassName?: string
  label?: string
}) {
  const [open, setOpen] = useState(false)
  const id = useId()
  return (
    <div className={className}>
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        aria-label={label}
        onClick={() => setOpen((value) => !value)}
        className={cn(
          'flex w-full min-w-0 items-start gap-2 text-left focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
          buttonClassName,
        )}
      >
        <ChevronRight
          aria-hidden
          className={cn('mt-0.5 size-4 shrink-0 text-muted transition-transform', open && 'rotate-90')}
        />
        <span className="min-w-0 flex-1">{typeof header === 'function' ? header(open) : header}</span>
      </button>
      {open && <div id={id}>{children}</div>}
    </div>
  )
}

function Text({ children, className }: { children: string; className?: string }) {
  return <p className={cn('whitespace-pre-line break-words', className)}>{children}</p>
}

function Heading({ children }: { children: ReactNode }) {
  return <p className="text-[11px] font-semibold uppercase tracking-[0.12em] text-faint">{children}</p>
}

export function VerificationDiagnostics({ diagnostics }: { diagnostics: VerificationKitDiagnostic[] }) {
  return (
    <ul className="space-y-1 rounded-md border border-failure/30 bg-failure-soft p-3 text-xs">
      {diagnostics.map((diagnostic, index) => (
        <li key={`${diagnostic.path}-${index}`} className="min-w-0 break-words text-failure">
          <span className="font-mono">{diagnostic.path}</span>
          <span className="text-muted">: </span>
          <span>{diagnostic.message}</span>
        </li>
      ))}
    </ul>
  )
}

function pinSummary(pin: VerificationKitPin) {
  switch (pin.status) {
    case 'current':
      return `v${pin.version} · current`
    case 'behind':
      return `v${pin.version} · current is v${pin.current_version}`
    case 'pending':
      return `v${pin.version} · awaiting confirmation`
    default:
      return `v${pin.version} · unresolved`
  }
}

function PinLink({ pin }: { pin: VerificationKitPin }) {
  const className = 'font-mono text-xs text-primary hover:underline break-all'
  return pin.kind === 'requirement' ? (
    <Link to="/requirements" search={{ requirement: pin.document_id }} className={className}>
      {pin.document_id}
    </Link>
  ) : (
    <Link to="/system-design" search={{ document: pin.document_id }} className={className}>
      {pin.document_id}
    </Link>
  )
}

function RunDetails({ exercise }: { exercise: VerificationKitExercise }) {
  const rows: Array<[string, string[]]> = [
    ['Needs', exercise.prerequisites.map((p) => `${p.id} (${p.kind})`)],
    [
      'Permissions',
      exercise.permissions.map((p) => [p.kind, p.target_binding ?? p.path].filter(Boolean).join(' · ') as string),
    ],
    ['Retry policy', [exercise.retry_policy.replaceAll('_', ' ')]],
    ['Operations', exercise.operations.map((o) => `${o.id} → ${o.target_binding}`)],
    ['Evidence', exercise.evidence_outputs.map((o) => `${o.type.replaceAll('_', ' ')} ×${o.minimum_items}`)],
  ]
  return (
    <dl className="mt-2 grid gap-x-4 gap-y-1.5 text-xs sm:grid-cols-[8rem_1fr]">
      {rows.map(([label, values]) => (
        <div key={label} className="contents">
          <dt className="text-faint">{label}</dt>
          <dd className="min-w-0 break-words text-muted">{values.length ? values.join(', ') : 'None'}</dd>
        </div>
      ))}
    </dl>
  )
}

function Exercise({ exercise }: { exercise: VerificationKitExercise }) {
  return (
    <Expandable
      className="rounded-md border border-border bg-background"
      buttonClassName="px-3 py-2"
      header={
        <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
          <span className="font-mono text-xs font-medium">{exercise.id}</span>
          <span className="text-xs text-faint">
            {exercise.kind} · {exercise.stages.join(', ')}
          </span>
        </span>
      }
    >
      <div className="space-y-3 border-t border-border px-3 py-3 pl-9">
        {exercise.description && <Text className="text-sm text-muted">{exercise.description}</Text>}
        <div className="space-y-1.5">
          <Heading>Assertions</Heading>
          {exercise.required_assertions.length === 0 ? (
            <p className="text-xs text-faint">No required assertions; the exercise is checked by its outputs.</p>
          ) : (
            <ul className="space-y-1.5">
              {exercise.required_assertions.map((assertion) => (
                <li key={assertion.id} className="min-w-0 text-sm">
                  <span className="font-mono text-xs">{assertion.id}</span>
                  {assertion.description && <Text className="text-muted">{assertion.description}</Text>}
                </li>
              ))}
            </ul>
          )}
        </div>
        {exercise.supports.length > 0 && (
          <div className="space-y-1.5">
            <Heading>Supports</Heading>
            <div className="flex flex-wrap gap-1.5">
              {exercise.supports.map((support) => (
                <Link
                  key={`${support.document_id}-${support.acceptance_criterion_id}`}
                  to="/requirements"
                  search={{ requirement: support.document_id }}
                  className="font-mono text-xs text-primary hover:underline"
                  title={`${support.document_id} v${support.version}`}
                >
                  {support.acceptance_criterion_id}
                </Link>
              ))}
            </div>
          </div>
        )}
        <Expandable header={<span className="text-xs text-muted">Run details</span>}>
          <div className="pl-6">
            <RunDetails exercise={exercise} />
          </div>
        </Expandable>
      </div>
    </Expandable>
  )
}

export function VerificationKitRow({ kit }: { kit: VerificationKit }) {
  const firstLine = kit.description.split('\n')[0]
  return (
    <Expandable
      label={`${kit.name} ${kit.version}`}
      className="border-t border-border first:border-t-0"
      buttonClassName="px-4 py-3 hover:bg-surface"
      header={(open) => (
        <span className="block min-w-0">
          <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
            <span className="text-sm font-medium">{kit.name}</span>
            <span className="font-mono text-xs text-faint" title={kit.digest || undefined}>
              v{kit.version}
            </span>
            <VerificationKitStatusChip status={kit.status} />
          </span>
          {!open && firstLine && <span className="mt-0.5 block truncate text-xs text-muted">{firstLine}</span>}
        </span>
      )}
    >
      <div className="space-y-4 px-4 pb-4 pl-10">
        {kit.description && <Text className="text-sm text-muted">{kit.description}</Text>}
        <div className="space-y-1.5">
          <Heading>Pinned documents</Heading>
          {kit.pins.length === 0 ? (
            <p className="text-xs text-faint">No pins; the kit is never selected automatically.</p>
          ) : (
            <ul className="space-y-1">
              {kit.pins.map((pin) => (
                <li
                  key={`${pin.kind}-${pin.document_id}-${pin.version}`}
                  className="flex min-w-0 flex-wrap items-baseline gap-x-2"
                >
                  <PinLink pin={pin} />
                  <span className={cn('text-xs', pin.status === 'current' ? 'text-positive' : 'text-muted')}>
                    {pinSummary(pin)}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
        {kit.status === 'invalid' && kit.diagnostics.length > 0 ? (
          <VerificationDiagnostics diagnostics={kit.diagnostics} />
        ) : (
          <div className="space-y-1.5">
            <Heading>Exercises</Heading>
            <div className="space-y-2">
              {kit.exercises.map((exercise) => (
                <Exercise key={exercise.id} exercise={exercise} />
              ))}
            </div>
          </div>
        )}
      </div>
    </Expandable>
  )
}
