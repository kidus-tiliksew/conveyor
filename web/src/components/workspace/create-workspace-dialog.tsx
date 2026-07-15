import { useNavigate } from '@tanstack/react-router'
import { Dialog } from '../ui/dialog'
import { CreateWorkspaceForm } from './create-workspace-form'

// URL-driven modal at /workspaces/new: the board stays mounted underneath,
// and every existing "create workspace" affordance keeps linking here.
export function CreateWorkspaceDialog() {
  const navigate = useNavigate()
  const close = () => void navigate({ to: '/' })
  return (
    <Dialog onClose={close} label="Create workspace">
      <div className="border-b border-border px-5 py-4">
        <h2 className="text-base font-semibold tracking-tight">Create workspace</h2>
        <p className="mt-0.5 text-sm text-muted">A workspace scopes repositories, stage routing, and tasks.</p>
      </div>
      <div className="px-5 py-4">
        <CreateWorkspaceForm onCreated={close} onCancel={close} />
      </div>
    </Dialog>
  )
}
