import { useCallback, useState } from 'react'

// What the crate page emphasizes. Pointing at a task in any part of the page
// sets the task, and every part reads it: the line and the branch map
// emphasize the task, and the task list marks its row (req-crate-insights
// REQ-3; component-crates invariant 33). Pointing at one of the branch map's
// counts sets a group instead, and the branch map emphasizes the lines of the
// group's served task IDs (AC-2.4). A part added to the page takes the same
// object, so the page keeps one emphasized task or group.
//
// The state holds two targets apart: what the pointer rests on and what holds
// keyboard focus, and which of them was set last. The page shows the target
// set last. When that target is cleared it shows the other one, if set. So
// focus that moves to a line shows that line while the pointer rests on
// another task, the pointer that then enters a task shows that task, and
// leaving a row, a line, or a count gives the emphasis back to a line or count
// that still has focus (AC-2.5).
export type EmphasisTarget = { task: string } | { group: string }

export interface CrateEmphasis {
  // The shown task and group. At most one of them is set.
  emphasizedId: string | null
  emphasizedGroup: string | null
  // Set or clear the pointer's target: a task, or a group by its key.
  // Clearing leaves the focused target in place.
  emphasize: (id: string | null) => void
  emphasizeGroup: (key: string | null) => void
  // Set the focused target, or clear it when `target` still holds focus.
  focus: (target: EmphasisTarget) => void
  blur: (target: EmphasisTarget) => void
}

// `latest` names the target set last. It is an order of acts, not a time.
type Targets = { pointer: EmphasisTarget | null; focus: EmphasisTarget | null; latest: 'pointer' | 'focus' }

function same(a: EmphasisTarget | null, b: EmphasisTarget) {
  if (a === null) return false
  return 'task' in a ? 'task' in b && a.task === b.task : 'group' in b && a.group === b.group
}

// The page holds this state once. It starts empty for each crate, because
// the page is keyed by the crate's ID.
export function useCrateEmphasis(): CrateEmphasis {
  const [targets, setTargets] = useState<Targets>({ pointer: null, focus: null, latest: 'pointer' })
  // Setting a target makes it the latest. Clearing one leaves `latest` as it
  // is, and the shown target falls back to the other one below.
  const setPointer = useCallback(
    (target: EmphasisTarget | null) =>
      setTargets((current) =>
        target === null ? { ...current, pointer: null } : { ...current, pointer: target, latest: 'pointer' },
      ),
    [],
  )
  const emphasize = useCallback((id: string | null) => setPointer(id === null ? null : { task: id }), [setPointer])
  const emphasizeGroup = useCallback(
    (key: string | null) => setPointer(key === null ? null : { group: key }),
    [setPointer],
  )
  const focus = useCallback(
    (target: EmphasisTarget) => setTargets((current) => ({ ...current, focus: target, latest: 'focus' })),
    [],
  )
  const blur = useCallback(
    (target: EmphasisTarget) =>
      setTargets((current) => (same(current.focus, target) ? { ...current, focus: null } : current)),
    [],
  )
  const shown = targets.latest === 'focus' ? (targets.focus ?? targets.pointer) : (targets.pointer ?? targets.focus)
  return {
    emphasizedId: shown && 'task' in shown ? shown.task : null,
    emphasizedGroup: shown && 'group' in shown ? shown.group : null,
    emphasize,
    emphasizeGroup,
    focus,
    blur,
  }
}
