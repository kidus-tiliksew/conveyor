// Pieces the crates list and the crate page share (component-crates
// invariant 17): the target date, the progress bar, the counts, and the form
// that creates or edits a crate.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CalendarDays, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { createCrate, fetchRequirements, updateCrate } from '../../lib/api'
import type { Crate, CrateRequirement } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { TaskContextPicker } from '../task/task-context-picker'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { Input } from '../ui/input'

// A target date is a calendar date, so it is read and written in UTC and
// never shifts with the viewer's zone (component-crates invariant 4).
const dateFormat = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' })

export function formatTargetDate(value: string) {
  return dateFormat.format(new Date(`${value}T00:00:00Z`))
}

// Whole days from the viewer's today to the target date.
export function daysUntil(value: string, now = new Date()) {
  const today = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate())
  return Math.round((new Date(`${value}T00:00:00Z`).getTime() - today) / 86_400_000)
}

// How far off the date is, in words. Only an open crate that has not shipped
// can be late.
export function targetDateHint(crate: Pick<Crate, 'target_date' | 'state' | 'shipped'>) {
  if (!crate.target_date || crate.state === 'closed') return null
  const days = daysUntil(crate.target_date)
  if (days === 0) return { text: 'Today', late: false }
  if (days === 1) return { text: 'Tomorrow', late: false }
  if (days > 1) return { text: `In ${days} days`, late: false }
  const past = -days
  return { text: past === 1 ? '1 day ago' : `${past} days ago`, late: !crate.shipped }
}

export function TargetDate({ crate, className }: { crate: Crate; className?: string }) {
  if (!crate.target_date) return <span className={cn('text-faint', className)}>No target date</span>
  const hint = targetDateHint(crate)
  return (
    <span className={cn('inline-flex min-w-0 flex-wrap items-baseline gap-x-1.5', className)}>
      <span className="text-foreground" data-target-date>
        {formatTargetDate(crate.target_date)}
      </span>
      {hint && <span className={cn('text-xs', hint.late ? 'text-attention' : 'text-faint')}>{hint.text}</span>}
    </span>
  )
}

// Merged, then closed without merging, then open: the bar fills as the work
// lands. The widths come from the server's counts (invariant 9).
export function CrateProgress({ crate, className }: { crate: Crate; className?: string }) {
  const total = crate.total_task_count
  const closed = Math.max(0, total - crate.open_task_count - crate.merged_task_count)
  const width = (count: number) => (total > 0 ? `${(count / total) * 100}%` : '0%')
  return (
    <div
      role="img"
      aria-label={
        total > 0
          ? `${crate.merged_task_count} of ${total} tasks merged, ${crate.open_task_count} open`
          : 'No tasks yet'
      }
      className={cn('flex h-1.5 w-full overflow-hidden rounded-full bg-raised', className)}
    >
      <span className="h-full bg-positive transition-[width]" style={{ width: width(crate.merged_task_count) }} />
      <span className="h-full bg-edge transition-[width]" style={{ width: width(closed) }} />
    </div>
  )
}

// The three counts in the order the requirement names them (AC-5.1).
export function CrateCounts({ crate, className }: { crate: Crate; className?: string }) {
  return (
    <span className={cn('flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 text-xs text-muted', className)}>
      <Count value={crate.open_task_count} label="open" />
      <Count
        value={crate.merged_task_count}
        label="merged"
        tone={crate.merged_task_count > 0 ? 'positive' : undefined}
      />
      <Count value={crate.total_task_count} label="total" />
    </span>
  )
}

function Count({ value, label, tone }: { value: number; label: string; tone?: 'positive' }) {
  return (
    <span data-count={label}>
      <span className={cn('font-medium tabular-nums', tone === 'positive' ? 'text-positive' : 'text-foreground')}>
        {value}
      </span>{' '}
      {label}
    </span>
  )
}

export function CrateStateBadge({ crate }: { crate: Crate }) {
  if (crate.state === 'closed')
    return (
      <span className="inline-flex items-center rounded-[5px] bg-raised px-1.5 text-xs font-medium leading-[18px] text-muted">
        Closed
      </span>
    )
  if (crate.shipped)
    return (
      <span className="inline-flex items-center rounded-[5px] bg-positive-soft px-1.5 text-xs font-medium leading-[18px] text-positive">
        Shipped
      </span>
    )
  return null
}

// Creates a crate, or edits the name, target date, and declared requirements
// of one. Declared requirements are confirmed requirements only; the server
// checks each one again (AC-1.3).
export function CrateFormDialog({
  crate,
  declared,
  onClose,
  onSaved,
}: {
  crate?: Crate
  declared?: CrateRequirement[]
  onClose: () => void
  onSaved: (crate: Crate) => void
}) {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const [name, setName] = useState(crate?.name ?? '')
  // Focus the name after the dialog records what had focus, so closing it
  // returns focus to the control that opened it.
  const nameRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const frame = requestAnimationFrame(() => nameRef.current?.querySelector('input')?.focus())
    return () => cancelAnimationFrame(frame)
  }, [])
  const [targetDate, setTargetDate] = useState(crate?.target_date ?? '')
  const [requirementIds, setRequirementIds] = useState<string[]>(declared?.map((item) => item.id) ?? [])
  const requirements = useQuery({
    queryKey: ['requirements', workspace],
    queryFn: () => fetchRequirements(workspace),
    enabled: Boolean(workspace),
    staleTime: 60_000,
  })
  const save = useMutation({
    mutationFn: () => {
      const input = { name: name.trim(), target_date: targetDate || null, requirement_ids: requirementIds }
      return crate ? updateCrate(workspace, crate.id, input) : createCrate(workspace, input)
    },
    onSuccess: (saved) => {
      void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
      onSaved(saved)
    },
  })
  const options = (requirements.data ?? [])
    .filter((item) => item.current_version != null)
    .map((item) => ({ id: item.requirement.id, title: item.requirement.title }))
  // A declared requirement that is no longer listed still shows by its title.
  for (const item of declared ?? []) {
    if (!options.some((option) => option.id === item.id)) options.push({ id: item.id, title: item.title })
  }
  const trimmed = name.trim()
  return (
    <Dialog onClose={onClose} label={crate ? 'Edit crate' : 'New crate'} className="max-w-xl overflow-visible">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          if (trimmed && !save.isPending) save.mutate()
        }}
      >
        <header className="flex items-start justify-between gap-3 border-b border-border px-5 py-3.5">
          <div className="min-w-0">
            <h2 className="text-base font-semibold tracking-tight">{crate ? 'Edit crate' : 'New crate'}</h2>
            <p className="mt-0.5 text-xs text-muted">
              A crate groups the tasks of one delivery. It shows their progress and changes nothing about how they run.
            </p>
          </div>
          <Button type="button" variant="ghost" size="icon" aria-label="Close" onClick={onClose}>
            <X />
          </Button>
        </header>
        <div className="space-y-4 px-5 py-4">
          <div ref={nameRef}>
            <span className="mb-1 block text-xs font-medium text-muted">Name</span>
            <Input
              aria-label="Crate name"
              value={name}
              maxLength={120}
              placeholder="Billing v2"
              onChange={(event) => setName(event.target.value)}
            />
          </div>
          <div>
            <span className="mb-1 block text-xs font-medium text-muted">Target date</span>
            <div className="flex items-center gap-2">
              <div className="relative min-w-0 flex-1 sm:max-w-56">
                <CalendarDays
                  aria-hidden
                  className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-faint"
                />
                <Input
                  type="date"
                  aria-label="Target date"
                  value={targetDate}
                  onChange={(event) => setTargetDate(event.target.value)}
                  className="pl-8"
                />
              </div>
              {targetDate && (
                <Button type="button" variant="ghost" size="sm" onClick={() => setTargetDate('')}>
                  Clear
                </Button>
              )}
            </div>
            <span className="mt-1 block text-xs text-faint">Optional. The day you expect this work to ship.</span>
          </div>
          <TaskContextPicker
            label="Requirements"
            hint="Optional. The requirements this work serves. The crate page counts the tasks that carry each one. A new task takes only the requirements you pick for it."
            loading={requirements.isPending}
            placeholder="Search confirmed requirements"
            groups={[
              {
                key: 'requirements',
                label: 'Requirements',
                options,
                selected: requirementIds,
                onChange: setRequirementIds,
              },
            ]}
          />
          {save.error != null && (
            <p role="alert" className="rounded-md bg-failure-soft px-3 py-2 text-xs text-failure">
              {save.error instanceof Error ? save.error.message : String(save.error)}
            </p>
          )}
        </div>
        <footer className="flex items-center justify-end gap-2 border-t border-border px-5 py-3">
          <Button type="button" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={!trimmed || save.isPending}>
            {save.isPending ? 'Saving…' : crate ? 'Save changes' : 'Create crate'}
          </Button>
        </footer>
      </form>
    </Dialog>
  )
}
