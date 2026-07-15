import type { InputHTMLAttributes, TextareaHTMLAttributes, SelectHTMLAttributes } from 'react'
import { ChevronDown } from 'lucide-react'
import { cn } from '../../lib/utils'

const fieldClasses =
  'w-full rounded-md border border-edge bg-background px-3 py-2 text-sm text-foreground placeholder:text-faint outline-none transition-colors focus:border-primary focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-primary/40 disabled:opacity-40'

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(fieldClasses, 'h-9', className)} {...props} />
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn(fieldClasses, 'min-h-20 resize-y leading-6', className)} {...props} />
}

export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <span className={cn('relative inline-flex w-full', className)}>
      <select className={cn(fieldClasses, 'h-9 appearance-none pr-8')} {...props}>
        {children}
      </select>
      <ChevronDown className="pointer-events-none absolute right-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
    </span>
  )
}
