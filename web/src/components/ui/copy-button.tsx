import { useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { Button } from './button'

export function CopyButton({
  value,
  label = 'Copy',
  showLabel = false,
}: {
  value: string
  label?: string
  showLabel?: boolean
}) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = window.setTimeout(() => setCopied(false), 1500)
    return () => window.clearTimeout(timer)
  }, [copied])
  return (
    <Button
      variant="ghost"
      size={showLabel ? 'sm' : 'icon'}
      aria-label={copied ? 'Copied' : label}
      title={copied ? 'Copied' : label}
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => setCopied(true))
      }}
    >
      {copied ? <Check className="text-primary" /> : <Copy />}
      {showLabel && (copied ? 'Copied' : 'Copy')}
      <span className="sr-only" aria-live="polite">
        {copied ? 'Copied' : ''}
      </span>
    </Button>
  )
}
