import { useQuery } from '@tanstack/react-query'
import { Link, Outlet, useNavigate, useParams, useRouterState } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { useEffect, useState } from 'react'
import { fetchWorkspaces, probeTaskWorkspace } from '../../lib/api'
import type { WorkspaceRecord } from '../../lib/types'
import { readStoredWorkspace, useCanCreateWorkspaces, useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'

// The `/w/$workspaceId` layout (req-workspace-addresses AC-1.1, AC-1.4). The
// shell resolves the address's workspace against the caller's memberships, so
// an ID outside them reaches here as no workspace and renders the one
// not-found page: no page below mounts, so no scoped request names that ID.
// The page subtree is keyed by workspace, so no page state outlives a change
// of the address's workspace (AC-1.6).
export function WorkspaceLayout() {
  const { workspaceId } = useParams({ from: '/w/$workspaceId' })
  const { workspace } = useWorkspaceSelection()
  if (workspace !== workspaceId) return <PageNotFound />
  return <Outlet key={workspace} />
}

// One not-found page for a nonexistent workspace, a workspace without a
// binding, an unknown page, and a legacy task address no membership answers,
// so the page reveals nothing about workspaces outside the caller's
// memberships (req-accounts-and-membership AC-4.2; DEC-19).
export function PageNotFound() {
  return (
    <div className="grid h-full place-items-center px-6">
      <div className="max-w-md text-center">
        <h1 className="text-lg font-semibold tracking-tight">Page not found</h1>
        <p className="mt-2 text-sm leading-6 text-muted">
          This address names nothing you can open. Choose a workspace from the left rail.
        </p>
      </div>
    </div>
  )
}

// An address that names no workspace (req-workspace-addresses AC-1.8, AC-1.9).
// A task address redirects to the membership that holds the task. Any other
// address redirects under the remembered workspace, or under the only
// membership when none is remembered, and otherwise asks for a choice. Every
// redirect keeps the path, search, and hash.
export function LegacyAddress() {
  const location = useRouterState({ select: (state) => state.location })
  const { data: workspaces } = useQuery({ queryKey: ['workspaces'], queryFn: fetchWorkspaces })
  const { pathname, searchStr, hash } = location
  // `/w/…` addresses that matched no workspace route are unknown pages.
  if (pathname === '/w' || pathname.startsWith('/w/') || !workspaces) return <PageNotFound />
  const suffix = `${pathname}${searchStr}${hash ? `#${hash}` : ''}`
  const taskId = legacyTaskId(pathname)
  if (taskId) return <TaskAddressRedirect key={suffix} taskId={taskId} suffix={suffix} workspaces={workspaces} />
  const stored = readStoredWorkspace()
  const target = workspaces.some((item) => item.id === stored)
    ? stored
    : workspaces.length === 1
      ? workspaces[0].id
      : ''
  if (target) return <HrefRedirect href={scopedHref(target, suffix)} />
  return <WorkspaceChooser workspaces={workspaces} suffix={suffix} />
}

// `/tasks/<id>`, `/tasks/<id>/full`, and `/blueprints/<id>` name a task, whose
// ID is unique in the deployment.
function legacyTaskId(pathname: string) {
  const match = /^\/(?:tasks\/([^/]+)(?:\/full)?|blueprints\/([^/]+))\/?$/.exec(pathname)
  const raw = match?.[1] ?? match?.[2]
  if (!raw) return ''
  try {
    return decodeURIComponent(raw)
  } catch {
    return ''
  }
}

// Replaces the unscoped address with its scoped equivalent, so Back skips it.
function HrefRedirect({ href }: { href: string }) {
  const navigate = useNavigate()
  useEffect(() => {
    void navigate({ href, replace: true })
  }, [href, navigate])
  return null
}

function scopedHref(workspace: string, suffix: string) {
  return `/w/${encodeURIComponent(workspace)}${suffix}`
}

type Probe = { state: 'probing' } | { state: 'found'; workspace: string } | { state: 'missing' } | { state: 'failed' }

// Probes the remembered workspace first, then the caller's other memberships
// in list order, and redirects to the first that answers the task. Only
// memberships are probed, so nothing outside them is revealed; no workspace
// answering shows the not-found page.
function TaskAddressRedirect({
  taskId,
  suffix,
  workspaces,
}: {
  taskId: string
  suffix: string
  workspaces: WorkspaceRecord[]
}) {
  const [probe, setProbe] = useState<Probe>({ state: 'probing' })
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    void attempt
    const stored = readStoredWorkspace()
    const order = [
      ...workspaces.filter((item) => item.id === stored),
      ...workspaces.filter((item) => item.id !== stored),
    ].map((item) => item.id)
    const controller = new AbortController()
    setProbe({ state: 'probing' })
    void (async () => {
      let failed = false
      for (const workspace of order) {
        try {
          if (await probeTaskWorkspace(workspace, taskId, controller.signal)) {
            setProbe({ state: 'found', workspace })
            return
          }
        } catch {
          if (controller.signal.aborted) return
          failed = true
        }
      }
      if (!controller.signal.aborted) setProbe({ state: failed ? 'failed' : 'missing' })
    })()
    return () => controller.abort()
  }, [taskId, workspaces, attempt])
  if (probe.state === 'found') return <HrefRedirect href={scopedHref(probe.workspace, suffix)} />
  if (probe.state === 'missing') return <PageNotFound />
  if (probe.state === 'failed') {
    return (
      <div className="grid h-full place-items-center px-6">
        <div role="alert" className="max-w-sm text-center">
          <h1 className="text-base font-semibold">Could not open this task</h1>
          <p className="mt-2 text-sm text-muted">Conveyor did not answer for every workspace. Check your connection.</p>
          <Button className="mt-4" onClick={() => setAttempt((value) => value + 1)}>
            Retry
          </Button>
        </div>
      </div>
    )
  }
  return <div className="grid h-full place-items-center text-sm text-muted">Opening task…</div>
}

// Neither a remembered workspace nor a single membership: the caller chooses
// one, and the choice opens the same page there (req-workspace-addresses
// AC-1.9). With no membership, a caller with the creation right is offered a
// new workspace instead (component-web-dashboard).
function WorkspaceChooser({ workspaces, suffix }: { workspaces: WorkspaceRecord[]; suffix: string }) {
  const canCreateWorkspaces = useCanCreateWorkspaces()
  const navigate = useNavigate()
  return (
    <div className="grid h-full place-items-center px-6">
      <div className="w-full max-w-md text-center">
        <h1 className="text-lg font-semibold tracking-tight">Welcome to Conveyor</h1>
        {workspaces.length > 0 ? (
          <>
            <p className="mt-2 text-sm leading-6 text-muted">Choose a workspace to open.</p>
            <ul aria-label="Choose a workspace" className="mt-4 space-y-2 text-left">
              {workspaces.map((item) => (
                <li key={item.id}>
                  <a
                    href={scopedHref(item.id, suffix)}
                    onClick={(event) => {
                      // The anchor carries the chosen address verbatim; the
                      // router handles the click without a reload.
                      if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return
                      event.preventDefault()
                      void navigate({ href: scopedHref(item.id, suffix) })
                    }}
                    className="block rounded-lg border border-border bg-card px-3 py-2 text-sm font-medium hover:border-edge"
                  >
                    {item.name || item.id}
                  </a>
                </li>
              ))}
            </ul>
          </>
        ) : canCreateWorkspaces ? (
          <>
            <p className="mt-2 text-sm leading-6 text-muted">No workspaces yet. Create one to start the line.</p>
            <Link to="/workspaces/new" className="mt-4 inline-block">
              <Button tabIndex={-1}>
                <Plus />
                Create workspace
              </Button>
            </Link>
          </>
        ) : (
          <p className="mt-2 text-sm leading-6 text-muted">You do not belong to a workspace yet.</p>
        )}
      </div>
    </div>
  )
}
