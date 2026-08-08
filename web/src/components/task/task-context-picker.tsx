import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { Check, X } from 'lucide-react'
import { Input } from '../ui/input'

export interface ContextOption {
  id: string
  title: string
}

// One group per document tier. Each keeps its own selection
// callback so intake still submits two separate ID arrays even though the
// operator sees a single control.
export interface ContextGroup {
  key: string
  label: string
  options: ContextOption[]
  selected: string[]
  onChange: (ids: string[]) => void
}

// The searchable Context control: one dropdown over every attachable document
// instead of a checkbox list per tier. Selected entries stay visible above the
// search box whether the list is open or closed, so intake reads as "what this
// task is anchored to" rather than two scrolling inventories.
export function TaskContextPicker({
  label,
  hint,
  loading,
  groups,
}: {
  label: string
  hint: string
  loading: boolean
  groups: ContextGroup[]
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  // -1 is "nothing highlighted yet" so the first ArrowDown lands on the first
  // option rather than skipping it.
  const [active, setActive] = useState(-1)
  const root = useRef<HTMLDivElement>(null)
  const listId = useId()
  const hintId = useId()

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false)
    }
    document.addEventListener('pointerdown', onPointerDown)
    return () => document.removeEventListener('pointerdown', onPointerDown)
  }, [open])

  const chips = groups.flatMap((group) =>
    group.selected.map((id) => ({ group, option: group.options.find((item) => item.id === id) ?? { id, title: id } })),
  )
  const totalOptions = groups.reduce((count, group) => count + group.options.length, 0)

  // Flatten while matching so keyboard traversal has one index space across
  // groups and each option carries the group that owns its selection.
  const matches = useMemo(() => {
    const needle = query.trim().toLowerCase()
    const sections: Array<{ group: ContextGroup; start: number; options: ContextOption[] }> = []
    let start = 0
    for (const group of groups) {
      const options = group.options.filter(
        (option) => !needle || option.title.toLowerCase().includes(needle) || option.id.toLowerCase().includes(needle),
      )
      if (options.length === 0) continue
      sections.push({ group, start, options })
      start += options.length
    }
    return { sections, count: start }
  }, [groups, query])
  // Clamp rather than reset on every keystroke: filtering can shrink the list
  // out from under the cursor while the operator is still typing.
  const activeIndex = matches.count === 0 || active < 0 ? -1 : Math.min(active, matches.count - 1)
  const optionId = (index: number) => `${listId}-option-${index}`

  const toggle = (group: ContextGroup, option: ContextOption) => {
    group.onChange(
      group.selected.includes(option.id)
        ? group.selected.filter((id) => id !== option.id)
        : [...group.selected, option.id],
    )
  }
  const optionAt = (index: number) => {
    for (const section of matches.sections) {
      if (index < section.start + section.options.length) {
        return { group: section.group, option: section.options[index - section.start] }
      }
    }
    return null
  }

  // Only the states an operator has to act on get a line — a running match
  // count would be noise above a list they can already see.
  const status = loading
    ? 'Loading context…'
    : totalOptions === 0
      ? 'No confirmed documents yet.'
      : matches.count === 0
        ? 'No context matches your search.'
        : ''

  return (
    <div ref={root}>
      <span className="mb-1 block text-[11px] font-medium uppercase tracking-wider text-muted">{label}</span>
      {chips.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {chips.map(({ group, option }) => (
            <button
              key={`${group.key}:${option.id}`}
              type="button"
              aria-label={`Remove context ${option.title}`}
              title={`${option.title} · ${option.id}`}
              onClick={() => group.onChange(group.selected.filter((id) => id !== option.id))}
              className="inline-flex items-center gap-1.5 rounded-full border border-border bg-surface px-2 py-1 text-xs"
            >
              <span className="max-w-64 truncate">{option.title}</span>
              <span className="shrink-0 text-faint">{group.label}</span>
              <X className="size-3" />
            </button>
          ))}
        </div>
      )}
      <div className="relative">
        <Input
          type="text"
          role="combobox"
          aria-label="Search context"
          aria-expanded={open}
          aria-controls={listId}
          aria-describedby={hintId}
          aria-autocomplete="list"
          aria-activedescendant={open && activeIndex >= 0 ? optionId(activeIndex) : undefined}
          value={query}
          placeholder="Search requirements and system design"
          onFocus={() => setOpen(true)}
          onClick={() => setOpen(true)}
          onChange={(event) => {
            setQuery(event.target.value)
            setActive(-1)
            setOpen(true)
          }}
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              // The intake sheet closes on Escape from a document listener, so
              // an open dropdown has to swallow the key or dismissing the list
              // would discard the whole draft task.
              if (!open) return
              event.stopPropagation()
              setOpen(false)
              return
            }
            if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
              event.preventDefault()
              setOpen(true)
              if (matches.count === 0) return
              const step = event.key === 'ArrowDown' ? 1 : -1
              setActive((current) =>
                current < 0
                  ? step === 1
                    ? 0
                    : matches.count - 1
                  : (Math.min(current, matches.count - 1) + step + matches.count) % matches.count,
              )
              return
            }
            if (event.key === 'Enter' && open && activeIndex >= 0) {
              event.preventDefault()
              const entry = optionAt(activeIndex)
              if (entry) toggle(entry.group, entry.option)
            }
          }}
        />
        {open && (
          <div className="absolute z-20 mt-1 w-full overflow-hidden rounded-md border border-border bg-card shadow-lg">
            <p aria-live="polite" className={status ? 'px-2.5 py-2 text-xs text-faint' : 'sr-only'}>
              {status}
            </p>
            <div
              id={listId}
              role="listbox"
              aria-multiselectable
              aria-label={label}
              className={matches.count > 0 ? 'max-h-56 overflow-y-auto p-1' : ''}
            >
              {matches.sections.map((section) => (
                // biome-ignore lint/a11y/useSemanticElements: a listbox may only contain option and group children, so a fieldset would break the ARIA tree.
                <div key={section.group.key} role="group" aria-label={section.group.label}>
                  <p className="px-1.5 py-1 text-[11px] font-medium uppercase tracking-wider text-muted">
                    {section.group.label}
                  </p>
                  {section.options.map((option, index) => {
                    const position = section.start + index
                    const isSelected = section.group.selected.includes(option.id)
                    return (
                      // biome-ignore lint/a11y/useKeyWithClickEvents: the combobox input drives keyboard selection through aria-activedescendant.
                      <div
                        key={option.id}
                        id={optionId(position)}
                        role="option"
                        tabIndex={-1}
                        aria-selected={isSelected}
                        onMouseDown={(event) => event.preventDefault()}
                        onMouseEnter={() => setActive(position)}
                        onClick={() => toggle(section.group, option)}
                        className={`flex cursor-pointer items-start gap-2 rounded px-1.5 py-1.5 text-xs ${
                          position === activeIndex ? 'bg-raised' : ''
                        }`}
                      >
                        <Check className={`mt-0.5 size-3.5 shrink-0 ${isSelected ? 'text-primary' : 'invisible'}`} />
                        <span className="min-w-0">
                          <span className="block truncate">{option.title}</span>
                          <span className="font-mono text-faint">{option.id}</span>
                        </span>
                      </div>
                    )
                  })}
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
      <span id={hintId} className="mt-1 block text-xs text-faint">
        {hint}
      </span>
    </div>
  )
}
