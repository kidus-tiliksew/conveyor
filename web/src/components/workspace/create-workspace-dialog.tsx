import { useNavigate, useRouter } from '@tanstack/react-router'
import { workspaceCreationRefusal } from '../../lib/api'
import { useWorkspaceCreationStatus } from '../app-shell'
import { Button } from '../ui/button'
import { Dialog } from '../ui/dialog'
import { CreateWorkspaceForm } from './create-workspace-form'

// URL-driven modal at the unscoped /workspaces/new: creation holds no
// workspace data, so it opens over the shell alone, and every "create
// workspace" affordance links here. Closing returns to the previous address,
// or resolves the unscoped home for a direct visit; creating opens the new
// workspace's Board. A direct visit follows the same creation right as those
// affordances: the form renders only once the unscoped read allows it, so a
// caller without the right sees why instead of a form that can only fail
// (component-web-dashboard).
export function CreateWorkspaceDialog() {
  const navigate = useNavigate()
  const router = useRouter()
  const close = () => {
    if (router.history.canGoBack()) router.history.back()
    else void navigate({ href: '/' })
  }
  const creation = useWorkspaceCreationStatus()
  return (
    <Dialog onClose={close} label="Create workspace">
      <div className="border-b border-border px-5 py-4">
        <h2 className="text-base font-semibold tracking-tight">Create workspace</h2>
        <p className="mt-0.5 text-sm text-muted">A workspace scopes repositories, stage routing, and tasks.</p>
      </div>
      <div className="px-5 py-4">
        {creation === 'allowed' ? (
          <CreateWorkspaceForm onCancel={close} />
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
