import { useNavigate } from '@tanstack/react-router'
import { workspaceCreationRefusal } from '../../lib/api'
import { useWorkspaceCreationStatus } from '../app-shell'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { CreateWorkspaceForm } from './create-workspace-form'

// URL-driven modal at /workspaces/new: the board stays mounted underneath,
// and every existing "create workspace" affordance keeps linking here. A
// direct visit follows the same creation right as those affordances: the form
// renders only once the unscoped read allows it, so a caller without the
// right sees why instead of a form that can only fail (component-web-dashboard).
export function CreateWorkspaceDialog() {
  const navigate = useNavigate()
  const close = () => void navigate({ to: '/' })
  const creation = useWorkspaceCreationStatus()
  return (
    <Dialog onClose={close} label="Create workspace">
      <div className="border-b border-border px-5 py-4">
        <h2 className="text-base font-semibold tracking-tight">Create workspace</h2>
        <p className="mt-0.5 text-sm text-muted">A workspace scopes repositories, stage routing, and tasks.</p>
      </div>
      <div className="px-5 py-4">
        {creation === 'allowed' ? (
          <CreateWorkspaceForm onCreated={close} onCancel={close} />
        ) : creation === 'pending' ? (
          <p className="text-sm text-muted">Checking workspace access…</p>
        ) : (
          <div className="space-y-4">
            <p className="text-sm">{workspaceCreationRefusal}</p>
            <div className="flex justify-end border-t border-border pt-4">
              <Button variant="secondary" onClick={close}>
                Close
              </Button>
            </div>
          </div>
        )}
      </div>
    </Dialog>
  )
}
