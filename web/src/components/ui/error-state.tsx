import { CircleAlert } from 'lucide-react'
import { cn } from '../../lib/utils'
import { Button } from './button'

// A server's refusal can be a JSON body or a stack-like string. Only a plain
// sentence reaches the page; anything else falls back to the page's own words.
export function readableError(error: unknown, fallback: string) {
  const raw = error instanceof Error ? error.message : typeof error === 'string' ? error : ''
  const text = raw.replace(/^Error:\s*/, '').trim()
  if (!text) return fallback
  try {
    const parsed = JSON.parse(text) as { error?: unknown; message?: unknown }
    const inner = parsed.error ?? parsed.message
    if (typeof inner === 'string' && inner.trim()) return sentence(inner)
    return fallback
  } catch {
    return text.length > 240 || /[{}<>]/.test(text) ? fallback : sentence(text)
  }
}

function sentence(text: string) {
  const trimmed = text.trim()
  const capital = trimmed.charAt(0).toUpperCase() + trimmed.slice(1)
  return /[.!?]$/.test(capital) ? capital : `${capital}.`
}

// The one way a page says it could not load something.
export function ErrorState({
  title,
  error,
  fallback = 'Check your connection and try again.',
  onRetry,
  retrying,
  className,
}: {
  title: string
  error?: unknown
  fallback?: string
  onRetry?: () => void
  retrying?: boolean
  className?: string
}) {
  return (
    <div role="alert" className={cn('mx-auto flex max-w-sm flex-col items-center px-6 py-16 text-center', className)}>
      <CircleAlert aria-hidden="true" className="size-5 text-faint" />
      <h2 className="mt-3 text-base font-semibold tracking-tight">{title}</h2>
      <p className="mt-1 text-sm text-muted">{readableError(error, fallback)}</p>
      {onRetry && (
        <Button variant="secondary" className="mt-4" disabled={retrying} onClick={onRetry}>
          {retrying ? 'Retrying…' : 'Try again'}
        </Button>
      )}
    </div>
  )
}
