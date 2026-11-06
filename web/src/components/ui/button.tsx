import { cva, type VariantProps } from 'class-variance-authority'
import type { ButtonHTMLAttributes } from 'react'
import { cn } from '../../lib/utils'

const buttonVariants = cva(
  'inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md text-[13px] leading-5 font-medium transition-[background-color,border-color,color,box-shadow] duration-150 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        default:
          'bg-primary text-primary-foreground shadow-[inset_0_1px_0_rgb(255_255_255/0.14),0_1px_2px_rgb(22_26_38/0.16)] hover:bg-primary/90 disabled:bg-raised disabled:text-muted disabled:opacity-100 disabled:shadow-none',
        secondary: 'border border-border bg-card text-foreground shadow-xs hover:border-edge hover:bg-surface',
        outline: 'border border-edge bg-card text-foreground shadow-xs hover:bg-surface',
        ghost: 'text-muted hover:bg-raised/70 hover:text-foreground',
        destructive: 'bg-failure-soft text-failure hover:bg-failure/15',
      },
      size: {
        default: 'h-8 px-3.5',
        sm: 'h-8 px-3',
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
