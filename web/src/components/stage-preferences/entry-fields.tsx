import { useId } from 'react'
import { maxEffortBytes, maxNameBytes, type StagePreferenceEntry } from '../../lib/stage-preferences'
import { Input } from '../ui/input'

// The three free-text fields of one entry. Nothing checks a value against a
// list of harnesses or models (component-stage-preferences invariant 2).
export function EntryFields({
  label,
  entry,
  disabled,
  onChange,
}: {
  label: string
  entry: StagePreferenceEntry
  disabled?: boolean
  onChange: (entry: StagePreferenceEntry) => void
}) {
  const id = useId()
  const field = (name: keyof StagePreferenceEntry, title: string, limit: number) => (
    <div className="min-w-0 space-y-1 text-xs text-muted">
      <label htmlFor={`${id}-${name}`}>{title}</label>
      <Input
        id={`${id}-${name}`}
        aria-label={`${label} ${name}`}
        value={entry[name]}
        maxLength={limit}
        disabled={disabled}
        spellCheck={false}
        autoComplete="off"
        onChange={(event) => onChange({ ...entry, [name]: event.target.value })}
      />
    </div>
  )
  return (
    <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,0.6fr)]">
      {field('harness', 'Harness', maxNameBytes)}
      {field('model', 'Model', maxNameBytes)}
      {field('effort', 'Effort (optional)', maxEffortBytes)}
    </div>
  )
}

/** One entry as text, or "Not set". */
export function describeEntry(entry: StagePreferenceEntry | null) {
  if (!entry) return 'Not set'
  const effort = entry.effort?.trim()
  return `${entry.harness}, ${entry.model}${effort ? `, ${effort} effort` : ''}`
}
