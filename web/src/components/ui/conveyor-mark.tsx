// The product mark, drawn from the theme so it follows the accent in both modes.
export function ConveyorMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 64 64" aria-hidden="true" className={className}>
      <rect width="64" height="64" rx="16" fill="var(--color-primary)" />
      <path d="M17 20h30v7H24v10h18v7H17z" fill="var(--color-primary-foreground)" />
    </svg>
  )
}
