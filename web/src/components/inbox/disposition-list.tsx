import { useId } from 'react'
import type { DispositionAnswer, NextActionDisposition } from '../../lib/types'
import { cn } from '../../lib/utils'

const answerLabels: { value: DispositionAnswer; label: string }[] = [
  { value: 'applied', label: 'Applied' },
  { value: 'not_applied', label: 'Not applied' },
]

// The disposition set of a verify order, one entry for each attempt in the
// server's order, each with an Applied and a Not applied control and no
// answer selected at first (component-next-action invariant 15; DEC-133(1)).
// The entry shows the attempt's subject, state, run, and unresolved
// operations as the server states them.
export function DispositionList({
  entries,
  answers,
  disabled,
  onAnswer,
}: {
  entries: NextActionDisposition[]
  answers: Record<string, DispositionAnswer>
  disabled?: boolean
  onAnswer: (runId: string, answer: DispositionAnswer) => void
}) {
  const prefix = useId()
  const answered = entries.filter((entry) => answers[entry.run_id]).length
  return (
    <div data-disposition-list className="space-y-2">
      <p className="text-xs text-muted">
        Record for each attempt if its change applied. {answered} of {entries.length} answered.
      </p>
      <ul className="space-y-2">
        {entries.map((entry, index) => {
          const name = `${prefix}-answer-${index}`
          const operations = entry.operation_ids?.length ?? 0
          return (
            <li key={entry.run_id} data-disposition-entry={entry.run_id}>
              <fieldset className="min-w-0 space-y-2 rounded-md border border-border px-3 py-2">
                <legend className="sr-only">{entry.subject}</legend>
                <div className="min-w-0 space-y-0.5">
                  <p className="text-sm font-medium [overflow-wrap:anywhere]" aria-hidden="true">
                    {entry.subject}
                  </p>
                  <p className="text-xs text-muted [overflow-wrap:anywhere]">
                    <span data-disposition-state>{entry.state.replaceAll('_', ' ')}</span>
                    {' · '}
                    {operations === 0
                      ? 'no unresolved operations'
                      : `${operations} unresolved ${operations === 1 ? 'operation' : 'operations'}`}
                    {' · run '}
                    <span className="font-mono">{entry.run_id}</span>
                  </p>
                </div>
                <div className="flex flex-wrap gap-2">
                  {answerLabels.map((option) => {
                    const checked = answers[entry.run_id] === option.value
                    return (
                      <label
                        key={option.value}
                        className={cn(
                          'inline-flex min-h-8 cursor-pointer items-center gap-2 rounded-md border px-3 py-1.5 text-sm',
                          checked ? 'border-primary/60 bg-primary-soft' : 'border-border',
                          disabled && 'cursor-not-allowed opacity-60',
                        )}
                      >
                        <input
                          className="size-4 accent-primary"
                          type="radio"
                          name={name}
                          value={option.value}
                          checked={checked}
                          disabled={disabled}
                          onChange={() => onAnswer(entry.run_id, option.value)}
                        />
                        {option.label}
                      </label>
                    )
                  })}
                </div>
              </fieldset>
            </li>
          )
        })}
      </ul>
    </div>
  )
}
