import { useSyncExternalStore } from 'react'

// Per-browser Board preferences (component-web-dashboard, Board preferences;
// req-operator-next-action AC-3.3). They are not scoped to a workspace and
// never reach the server, so one choice holds across sessions and workspaces
// in this browser.
export const STAGE_PROGRESS_STORAGE_KEY = 'conveyor-show-stage-progress'

export function readShowStageProgress(storage?: Pick<Storage, 'getItem'>): boolean {
  try {
    return (storage ?? window.localStorage).getItem(STAGE_PROGRESS_STORAGE_KEY) === 'true'
  } catch {
    return false
  }
}

export function persistShowStageProgress(show: boolean, storage?: Pick<Storage, 'setItem'>) {
  try {
    ;(storage ?? window.localStorage).setItem(STAGE_PROGRESS_STORAGE_KEY, show ? 'true' : 'false')
  } catch {
    // Local preferences are best-effort; an unavailable store must not break the dashboard.
  }
}

// One shared value for every subscriber in the page: the Settings switch and
// each Board card observe the same preference, and a change in another tab
// arrives through the storage event.
const listeners = new Set<() => void>()

function notify() {
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => {
    if (event.key === null || event.key === STAGE_PROGRESS_STORAGE_KEY) listener()
  }
  window.addEventListener('storage', onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('storage', onStorage)
  }
}

export function setShowStageProgress(show: boolean) {
  persistShowStageProgress(show)
  notify()
}

export function useShowStageProgress(): boolean {
  return useSyncExternalStore(subscribe, () => readShowStageProgress())
}
