import type { ButtonHTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

const buttonVariants = cva(
  'inline-flex items-center justify-center rounded-md px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 disabled:pointer-events-none disabled:opacity-45',
  {
    variants: {
      variant: {
        default: 'bg-stone-100 text-stone-950 hover:bg-white focus-visible:outline-stone-200',
        outline: 'border border-stone-700 bg-transparent text-stone-200 hover:border-stone-500 hover:bg-stone-800',
        danger: 'bg-red-950 text-red-100 hover:bg-red-900 focus-visible:outline-red-700',
        ghost: 'text-stone-300 hover:bg-stone-800 hover:text-white',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)

type Props = ButtonHTMLAttributes<HTMLButtonElement> & VariantProps<typeof buttonVariants>

export function Button({ className, variant, ...props }: Props) {
  return <button className={cn(buttonVariants({ variant }), className)} {...props} />
}
