export function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
	<>
	  {/* biome-ignore lint/a11y/noLabelWithoutControl: Field wraps a custom component that renders its native control child. */}
	  <label className="block">
		<span className="mb-1 flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wider text-muted">
		  {label}
		  {hint && <span title={hint} className="inline-flex size-3.5 cursor-help items-center justify-center rounded-full border border-edge text-[9px] font-semibold normal-case tracking-normal text-faint">?</span>}
		</span>
		{children}
	  </label>
	</>
  )
}
