import { cva, type VariantProps } from 'class-variance-authority'
import type { ButtonHTMLAttributes } from 'react'
import { cn } from '../../lib/utils'

const buttonVariants = cva(
  'inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md text-sm font-medium transition-[background-color,border-color,color,box-shadow] duration-100 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-3.5 [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        default: 'bg-primary-fill text-primary-foreground shadow-xs hover:bg-primary-fill/90',
        secondary: 'border border-border bg-card text-foreground shadow-xs hover:border-edge hover:bg-raised/60',
        outline: 'border border-border bg-transparent text-foreground hover:border-edge hover:bg-raised/60',
        ghost: 'text-muted hover:bg-raised/70 hover:text-foreground',
        // The confirm of an action that closes, rejects or merges past a warning.
        danger: 'bg-failure-fill text-white shadow-xs hover:bg-failure-fill/90',
        destructive: 'border border-border bg-transparent text-failure hover:border-failure/40 hover:bg-failure-soft',
      },
      size: {
        default: 'h-8 px-3',
        sm: 'h-7 px-2.5 text-xs',
        icon: 'size-8',
      },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
)

type Props = ButtonHTMLAttributes<HTMLButtonElement> & VariantProps<typeof buttonVariants>

export function Button({ className, variant, size, ...props }: Props) {
  return <button className={cn(buttonVariants({ variant, size }), className)} {...props} />
}
