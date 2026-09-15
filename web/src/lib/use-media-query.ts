import { useSyncExternalStore } from 'react'

// Tailwind's `lg` breakpoint, the width at which the shell keeps its two
// navigation columns and the document pages keep their tree beside the canvas.
export const wideLayoutQuery = '(min-width: 64rem)'

function subscribe(query: string, onChange: () => void) {
  const list = window.matchMedia(query)
  list.addEventListener('change', onChange)
  return () => list.removeEventListener('change', onChange)
}

// A layout decision that has to change which component mounts, not only how
// it is styled, reads the same breakpoint the stylesheet uses.
export function useMediaQuery(query: string) {
  return useSyncExternalStore(
    (onChange) => subscribe(query, onChange),
    () => window.matchMedia(query).matches,
    () => true,
  )
}
