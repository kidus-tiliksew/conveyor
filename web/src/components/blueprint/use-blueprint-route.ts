import { useEffect } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { isBlueprintAnchor } from '../../lib/blueprint'
import type { Task } from '../../lib/types'

// One canonical home for an anchor. `/blueprints/$taskId` is it;
// the task routes are a second door back into the task costume, so an anchor
// reached through one is sent to the canonical route rather than rendered
// there.
//
// Anchor-ness is only knowable once the detail payload arrives — children come
// with it — so this is an effect after load rather than a `beforeLoad` guard.
// The navigation replaces the history entry so Back leaves the blueprint
// instead of bouncing off the redirect.
export function useCanonicalBlueprintRedirect(task: Pick<Task, 'id' | 'children'> | undefined): boolean {
  const navigate = useNavigate()
  const taskId = task?.id
  const redirecting = !!task && isBlueprintAnchor(task)

  useEffect(() => {
    if (!redirecting || !taskId) return
    void navigate({ to: '/blueprints/$taskId', params: { taskId }, replace: true })
  }, [redirecting, taskId, navigate])

  return redirecting
}
