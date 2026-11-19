import { useCallback, useState } from 'react'

// What the crate page emphasizes. Pointing at a task in any part of the page
// sets the task, and every part reads it: the line and the branch map
// emphasize the task, and the task list marks its row (req-crate-insights
// REQ-3; component-crates invariant 33). Pointing at one of the branch map's
// counts sets a group instead, and the branch map emphasizes the lines of the
// group's served task IDs (AC-2.4). A part added to the page takes the same
// object, so the page keeps one emphasized task or group.
export interface CrateEmphasis {
  emphasizedId: string | null
  emphasize: (id: string | null) => void
  // The key of the pointed-at whereabouts group, or null. The branch map
  // names each group by its kind and its branch.
  emphasizedGroup: string | null
  emphasizeGroup: (key: string | null) => void
}

type Target = { id: string | null; group: string | null }

// The page holds this state once. It starts empty for each crate, because
// the page is keyed by the crate's ID. A task and a group never hold
// together: setting one clears the other.
export function useCrateEmphasis(): CrateEmphasis {
  const [target, setTarget] = useState<Target>({ id: null, group: null })
  const emphasize = useCallback((id: string | null) => setTarget({ id, group: null }), [])
  const emphasizeGroup = useCallback((group: string | null) => setTarget({ id: null, group }), [])
  return { emphasizedId: target.id, emphasize, emphasizedGroup: target.group, emphasizeGroup }
}
