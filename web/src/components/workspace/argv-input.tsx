import { useRef, useState } from 'react'
import { X } from 'lucide-react'
import { cn } from '../../lib/utils'

// Splits pasted or typed text on whitespace outside quotes. Quote characters
// are kept verbatim: harness argv is executed without a shell (spec §21.14),
// so every character in a token is literal — quotes only group spaces.
export function splitArgv(text: string): string[] {
  const tokens: string[] = []
  let current = ''
  let quote: '"' | "'" | null = null
  for (const ch of text) {
    if (/\s/.test(ch) && !quote) {
      if (current) { tokens.push(current); current = '' }
      continue
    }
    if (ch === '"' || ch === "'") {
      if (quote === ch) quote = null
      else if (!quote) quote = ch
    }
    current += ch
  }
  if (current) tokens.push(current)
  return tokens
}

function hasOpenQuote(text: string): boolean {
  let quote: '"' | "'" | null = null
  for (const ch of text) {
    if (ch === '"' || ch === "'") {
      if (quote === ch) quote = null
      else if (!quote) quote = ch
    }
  }
  return quote !== null
}

const PLACEHOLDER = /^\{[a-z_]+\}$/

// Token-chip editor for argv arrays. Type or paste to append arguments,
// click a chip to edit it in place, Backspace on an empty input removes the
// last argument. Placeholders like {prompt} render as accent chips.
export function ArgvInput({ label, value, onChange, placeholder }: { label: string; value: string[]; onChange: (value: string[]) => void; placeholder?: string }) {
  const [text, setText] = useState('')
  const [editing, setEditing] = useState<number | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const commit = () => {
    const tokens = splitArgv(text)
    if (editing !== null) {
      const next = [...value]
      next.splice(editing, 1, ...tokens)
      onChange(next)
      setEditing(null)
    } else if (tokens.length) {
      onChange([...value, ...tokens])
    }
    setText('')
  }

  const startEdit = (index: number) => {
    setEditing(index)
    setText(value[index])
    requestAnimationFrame(() => inputRef.current?.focus())
  }

  const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') { event.preventDefault(); commit() }
    else if (event.key === ' ' && !hasOpenQuote(text)) { event.preventDefault(); commit() }
    else if (event.key === 'Escape') { setEditing(null); setText('') }
    else if (event.key === 'Backspace' && text === '' && editing === null && value.length) onChange(value.slice(0, -1))
  }

  const input = (
    <input
      ref={inputRef}
      aria-label={label}
      value={text}
      onChange={(event) => setText(event.target.value)}
      onKeyDown={onKeyDown}
      onBlur={commit}
      placeholder={value.length || editing !== null ? undefined : placeholder}
      className="h-6 min-w-16 flex-1 bg-transparent font-mono text-xs text-foreground outline-none placeholder:font-sans placeholder:text-faint"
    />
  )

  return (
    <div
      className="flex min-h-9 w-full flex-wrap items-center gap-1 rounded-md border border-edge bg-background px-2 py-1.5 transition-colors focus-within:border-primary"
    >
      {value.map((token, index) =>
        editing === index ? (
          <span key={index} className="contents">{input}</span>
        ) : (
          <span
            key={index}
            className={cn(
              'inline-flex max-w-full items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-xs',
              PLACEHOLDER.test(token) ? 'border-transparent bg-primary-soft font-semibold text-primary' : 'border-border bg-raised text-foreground',
            )}
          >
            <button type="button" className="truncate" title={`Edit ${token}`} onClick={(event) => { event.stopPropagation(); startEdit(index) }}>
              {token}
            </button>
            <button
              type="button"
              aria-label={`Remove ${token} from ${label}`}
              className="text-faint hover:text-failure"
              onClick={(event) => { event.stopPropagation(); onChange(value.filter((_, i) => i !== index)) }}
            >
              <X className="size-3" />
            </button>
          </span>
        ),
      )}
      {editing === null && input}
    </div>
  )
}
