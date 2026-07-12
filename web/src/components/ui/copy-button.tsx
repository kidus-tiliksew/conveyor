import { useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { Button } from './button'

export function CopyButton({ value, label = 'Copy' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = window.setTimeout(() => setCopied(false), 1500)
    return () => window.clearTimeout(timer)
  }, [copied])
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={label}
      title={label}
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => setCopied(true))
      }}
    >
      {copied ? <Check className="text-primary" /> : <Copy />}
    </Button>
  )
}
