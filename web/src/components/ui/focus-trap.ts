const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])'

// Keeps Tab and Shift+Tab inside the topmost open sheet or dialog, so keyboard
// focus can't land on controls hidden behind its overlay. A sheet can open over
// another one, and only the last one open answers.
export function containTab(event: KeyboardEvent, panel: HTMLElement | null) {
  if (event.key !== 'Tab' || !panel) return
  const modals = document.querySelectorAll('[aria-modal="true"]')
  if (modals[modals.length - 1] !== panel) return
  const focusable = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (element) => element.getClientRects().length > 0,
  )
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  const active = document.activeElement
  if (!first || !last) {
    event.preventDefault()
    panel.focus()
  } else if (!panel.contains(active)) {
    event.preventDefault()
    ;(event.shiftKey ? last : first).focus()
  } else if (event.shiftKey && (active === first || active === panel)) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && active === last) {
    event.preventDefault()
    first.focus()
  }
}
