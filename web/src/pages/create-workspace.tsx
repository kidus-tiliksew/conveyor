import { useNavigate } from '@tanstack/react-router'
import { CreateWorkspaceForm } from '../components/workspace/create-workspace-form'

export function CreateWorkspacePage() {
  const navigate = useNavigate()
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-2xl px-6 py-8">
        <h1 className="text-xl font-semibold">Create workspace</h1>
        <p className="mt-1 text-sm text-muted">A workspace scopes repositories, stage routing, and tasks.</p>
        <div className="mt-6">
          <CreateWorkspaceForm onCreated={() => void navigate({ to: '/' })} />
        </div>
      </div>
    </div>
  )
}
