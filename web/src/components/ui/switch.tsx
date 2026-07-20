import { cn } from '../../lib/utils'

export function Switch({ checked, onChange, 'aria-label': ariaLabel }: { checked: boolean; onChange: (value: boolean) => void; 'aria-label'?: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative h-[18px] w-8 shrink-0 rounded-full transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
        checked ? 'bg-primary' : 'bg-edge',
      )}
    >
      <span className={cn('absolute left-0.5 top-0.5 size-3.5 rounded-full bg-white shadow-sm transition-transform', checked && 'translate-x-3.5')} />
    </button>
  )
}
