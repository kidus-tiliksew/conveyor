import { Link } from '@tanstack/react-router'
import { useWorkspace, useWorkspaceSelection } from '../app-shell'

// req-repository-onboarding REQ-2: both intake surfaces observe the shell cache.
export function EmptyRepositoryNotice() {
  const { workspace } = useWorkspaceSelection()
  const { data } = useWorkspace()
  if (!data || (data.repos?.length ?? 0) !== 0) return null
  return (
    <p role="status" className="my-3 rounded-md border border-attention/40 bg-attention-soft p-3 text-sm text-muted">
      This workspace has no registered repository.{' '}
      <Link to="/w/$workspaceId/workspace" params={{ workspaceId: workspace }} className="text-primary hover:underline">
        Register a repository on the Workspace page
      </Link>{' '}
      before creating a task.
    </p>
  )
}
