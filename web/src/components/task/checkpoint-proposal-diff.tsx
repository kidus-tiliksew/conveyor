import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchRequirement, fetchSystemDesign } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import { useWorkspaceSelection } from '../app-shell'
import { VersionDiff } from '../documents/version-diff'

// Each side scrolls on its own and wraps long lines, so a whole-document
// comparison stays inside the drawer at 390 px.
const comparisonPane = {
  paneClassName: 'max-h-96 min-w-0 overflow-auto bg-card p-3',
  preClassName: 'whitespace-pre-wrap break-words font-sans text-xs leading-5',
}

interface VersionBody {
  version: number
  content: string
}

// component-web-task-surfaces, Implementation checkpoints: "Show changes" for
// one pending proposal of a cited document. The document read mounts only
// while the disclosure is open, under the selected workspace, and a missing
// baseline reads as an unavailable comparison rather than an empty version.
export function CheckpointProposalDiff({
  kind,
  documentId,
  baseVersion,
  proposedVersion,
}: {
  kind: 'requirement' | 'system_design'
  documentId: string
  baseVersion: number
  proposedVersion: number
}) {
  const [open, setOpen] = useState(false)
  return (
    <details className="w-full text-xs text-muted" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary className="cursor-pointer rounded-sm font-medium text-primary focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
        Show changes in v{proposedVersion}
      </summary>
      {open && (
        <ProposalComparison
          kind={kind}
          documentId={documentId}
          baseVersion={baseVersion}
          proposedVersion={proposedVersion}
        />
      )}
    </details>
  )
}

function ProposalComparison({
  kind,
  documentId,
  baseVersion,
  proposedVersion,
}: {
  kind: 'requirement' | 'system_design'
  documentId: string
  baseVersion: number
  proposedVersion: number
}) {
  const { workspace } = useWorkspaceSelection()
  const read = useQuery({
    queryKey: ['checkpoint-proposal-versions', workspace, kind, documentId],
    queryFn: async ({ signal }): Promise<VersionBody[]> => {
      if (kind === 'requirement') {
        const view = await fetchRequirement(documentId, workspace, signal)
        return [...(view.current_version ? [view.current_version] : []), ...view.pending_versions]
      }
      const view = await fetchSystemDesign(documentId, workspace, signal)
      return [...view.versions, ...(view.current_version ? [view.current_version] : []), ...view.pending_versions]
    },
  })
  if (read.isPending) return <p className="mt-2">Loading the comparison…</p>
  if (read.error != null)
    return (
      <p role="alert" className="mt-2 text-failure">
        {errorMessage(read.error, 'Could not read the document versions.')}
      </p>
    )
  const base = read.data.find((version) => version.version === baseVersion)
  const proposed = read.data.find((version) => version.version === proposedVersion)
  if (!base || !proposed)
    return (
      <p className="mt-2">
        Comparison unavailable: v{base ? proposedVersion : baseVersion} is not readable. Open the document to review its
        history.
      </p>
    )
  return (
    <div className="mt-2 min-w-0 overflow-hidden rounded border border-border">
      <VersionDiff
        bounded
        left={{ content: base.content, label: `Confirmed v${baseVersion}`, ...comparisonPane }}
        right={{ content: proposed.content, label: `Proposed v${proposedVersion}`, ...comparisonPane }}
        className="grid min-w-0 gap-px bg-border md:grid-cols-2"
      />
    </div>
  )
}
