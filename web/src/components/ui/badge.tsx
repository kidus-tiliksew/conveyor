import { cva, type VariantProps } from 'class-variance-authority'
import type { HTMLAttributes } from 'react'
import { cn } from '../../lib/utils'

const badgeVariants = cva(
  'inline-flex items-center gap-1.5 whitespace-nowrap rounded-[5px] px-1.5 py-px text-xs font-medium leading-[18px] [&_svg]:size-3',
  {
    variants: {
      variant: {
        default: 'bg-raised text-muted',
        outline: 'border border-border text-muted',
        // The single alarm color on the page.
        attention: 'bg-attention-soft text-attention before:size-1.5 before:rounded-full before:bg-attention-dot',
        accent: 'bg-primary-soft text-primary',
        positive: 'bg-positive-soft text-positive',
        failure: 'bg-failure-soft text-failure',
        mono: 'bg-raised text-muted',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)

type Props = HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>

export function Badge({ className, variant, ...props }: Props) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />
}
