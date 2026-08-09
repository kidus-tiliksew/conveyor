import type { HTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/utils'

const badgeVariants = cva(
  'inline-flex items-center gap-1.5 whitespace-nowrap rounded-md px-2 py-0.5 text-[11px] font-medium leading-4 [&_svg]:size-3',
  {
    variants: {
      variant: {
        default: 'border border-border bg-surface text-muted',
        outline: 'border border-edge bg-background text-foreground',
        // The single alarm color on the page.
        attention:
          'bg-attention-soft font-semibold text-attention before:size-1.5 before:rounded-full before:bg-attention-dot',
        accent: 'bg-primary-soft text-primary',
        positive: 'bg-positive-soft text-positive',
        failure: 'bg-failure-soft text-failure',
        mono: 'border border-border bg-surface font-mono text-muted rounded-md',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)

type Props = HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>

export function Badge({ className, variant, ...props }: Props) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />
}
