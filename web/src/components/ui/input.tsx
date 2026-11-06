import { ChevronDown } from 'lucide-react'
import type { InputHTMLAttributes, SelectHTMLAttributes, TextareaHTMLAttributes } from 'react'
import { cn } from '../../lib/utils'

const fieldClasses =
  'w-full rounded-md border border-edge bg-card px-3 py-2 text-sm text-foreground shadow-xs placeholder:text-faint outline-none transition-[border-color,box-shadow] duration-150 hover:border-faint/70 focus:border-primary focus:ring-3 focus:ring-primary/15 disabled:opacity-50 disabled:shadow-none'

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(fieldClasses, 'h-8.5', className)} {...props} />
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn(fieldClasses, 'min-h-20 resize-y leading-6', className)} {...props} />
}

export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <span className={cn('relative inline-flex w-full', className)}>
      <select className={cn(fieldClasses, 'h-8.5 appearance-none py-0 pr-8')} {...props}>
        {children}
      </select>
      <ChevronDown className="pointer-events-none absolute right-2.5 top-1/2 size-4 -translate-y-1/2 text-faint" />
    </span>
  )
}
