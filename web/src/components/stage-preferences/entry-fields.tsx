import { type ReactNode, useId } from 'react'
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
  const field = (
    name: keyof StagePreferenceEntry,
    title: ReactNode,
    limit: number,
    placeholder?: string,
    inputClassName?: string,
  ) => (
    <div className="min-w-0 space-y-1 text-xs text-muted">
      <label htmlFor={`${id}-${name}`} className="block truncate">
        {title}
      </label>
      <Input
        id={`${id}-${name}`}
        aria-label={`${label} ${name}`}
        value={entry[name]}
        maxLength={limit}
        disabled={disabled}
        placeholder={placeholder}
        className={inputClassName}
        spellCheck={false}
        autoComplete="off"
        onChange={(event) => onChange({ ...entry, [name]: event.target.value })}
      />
    </div>
  )
  // The three fields share one line at every width from 375 px, with Effort
  // the narrowest. On a narrow screen the effort label is short, and the
  // placeholder still says that the field is optional.
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,0.7fr)] gap-2">
      {field('harness', 'Harness', maxNameBytes)}
      {field('model', 'Model', maxNameBytes)}
      {field(
        'effort',
        <>
          Effort<span className="max-sm:hidden"> (optional)</span>
        </>,
        maxEffortBytes,
        'Optional',
        'px-2 placeholder:text-xs',
      )}
    </div>
  )
}

/** One entry as text, or "Not set". */
export function describeEntry(entry: StagePreferenceEntry | null) {
  if (!entry) return 'Not set'
  const effort = entry.effort?.trim()
  return `${entry.harness}, ${entry.model}${effort ? `, ${effort} effort` : ''}`
}

/** One entry as harness · model · effort for a closed Settings row, or "Not set". */
export function summarizeEntry(entry: StagePreferenceEntry | null) {
  if (!entry) return 'Not set'
  const effort = entry.effort?.trim()
  return [entry.harness, entry.model, effort].filter(Boolean).join(' · ')
}
