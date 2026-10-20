import { createContext, type ReactNode, useContext, useEffect, useRef } from 'react'
import type { NextAction } from '../../lib/types'
import { cn } from '../../lib/utils'
import { choiceBody } from './choice-bodies'

// The Inbox keyboard map (req-operator-next-action AC-2.11):
// j / ArrowDown and k / ArrowUp move the selection; c activates the selected
// item's first allowed choice; r and d activate Revise and Dismiss where the
// detail offers them. A key clicks the rendered button that carries its
// `data-shortcut`, so it runs that button's own handler, dialog, composer and
// duplicate-send guard and never a second posting path.

// The choice ID the selected decision binds to `c`, provided by its detail
// view. Outside the Inbox it is null, so shared choice rows render no hint.
const ShortcutChoice = createContext<string | null>(null)

export function ShortcutChoiceProvider({ choiceId, children }: { choiceId: string | null; children: ReactNode }) {
  return <ShortcutChoice.Provider value={choiceId}>{children}</ShortcutChoice.Provider>
}

export function useShortcutChoice() {
  return useContext(ShortcutChoice)
}

// The first choice whose button renders enabled, matching the rules each
// choice row applies: the gate's one-act merge needs only the capability (its
// button opens the merge confirmation), and every other choice also needs a
// request body this client can build and no server refusal.
export function shortcutChoiceId(action: NextAction) {
  for (const choice of action.choices) {
    if (!choice.allowed) continue
    if (action.kind === 'merge_approval' && choice.id === 'merge') return choice.id
    const body = choiceBody(choice.id)
    if (!body || body.unavailable?.(action.details)) continue
    return choice.id
  }
  return null
}

// A key hint beside the control it activates. It is hidden from assistive
// technology, so the control keeps its own name, and hidden below md, where
// the Inbox shows one pane at a time (AC-2.11).
export function Kbd({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span aria-hidden="true" data-kbd-hint className={cn('hidden md:inline-flex', className)}>
      <kbd className="inline-flex h-5 min-w-5 items-center justify-center rounded border border-b-2 border-border bg-surface px-1 font-mono text-[10.5px] leading-none text-muted">
        {children}
      </kbd>
    </span>
  )
}

function editable(target: EventTarget | null) {
  if (!(target instanceof HTMLElement)) return false
  if (target.isContentEditable) return true
  return target.closest('input, textarea, select, [contenteditable="true"]') != null
}

function shown(element: Element) {
  return element.getClientRects().length > 0
}

// Binds the map on the window while the Inbox page is mounted. A key does
// nothing while a modifier is held, during IME composition, while focus is in
// a text field or outside the Inbox page, while any dialog or sheet is open,
// or while the selected detail has a composer open. An action key ignores
// auto-repeat, so holding c never sends twice.
export function useInboxKeyboard({
  root,
  move,
}: {
  root: React.RefObject<HTMLElement | null>
  move: (step: 1 | -1) => void
}) {
  const moveRef = useRef(move)
  moveRef.current = move
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey || event.isComposing) return
      const step =
        event.key === 'j' || event.key === 'ArrowDown' ? 1 : event.key === 'k' || event.key === 'ArrowUp' ? -1 : 0
      const act = event.key === 'c' || event.key === 'r' || event.key === 'd' ? event.key : null
      if (!step && !act) return
      const page = root.current
      if (!page || editable(event.target)) return
      const active = document.activeElement
      if (active && active !== document.body && !page.contains(active)) return
      if (document.querySelector('[role="dialog"]')) return
      if (step) {
        event.preventDefault()
        moveRef.current(step)
        return
      }
      if (event.repeat) return
      const detail = page.querySelector('[data-inbox-detail]')
      if (!detail || !shown(detail) || detail.querySelector('textarea')) return
      const target =
        detail.querySelector(`[data-shortcut="${act}"]`) ??
        (act === 'c' ? detail.querySelector('[data-shortcut-merge] [data-merge-at-gate]') : null)
      if (!(target instanceof HTMLButtonElement) || target.disabled) return
      event.preventDefault()
      target.click()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [root])
}
