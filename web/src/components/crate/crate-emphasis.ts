import { useState } from 'react'

// The one task the crate page emphasizes. Pointing at a task in any part of
// the page sets it, and every part reads it: the line emphasizes the task's
// block and the task list marks its row (req-crate-insights REQ-3;
// component-crates invariant 33). A part added to the page takes the same
// pair, so the page keeps one emphasized task.
export interface CrateEmphasis {
  emphasizedId: string | null
  emphasize: (id: string | null) => void
}

// The page holds this state once. It starts empty for each crate, because
// the page is keyed by the crate's ID.
export function useCrateEmphasis(): CrateEmphasis {
  const [emphasizedId, emphasize] = useState<string | null>(null)
  return { emphasizedId, emphasize }
}
