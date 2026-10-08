import type { WorkspaceConfigDocument } from '../../lib/types'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Switch } from '../ui/switch'

// The workspace's verification default (component-web-dashboard VK-WEB-4).
// The switch edits the shared configuration draft and saves through the page's
// save bar; tasks freeze it at intake (DEC-43). No workspace switch refuses a
// submission for missing verification evidence (req-review-gates-evidence
// AC-8.3; DEC-53).
export function VerificationSettings({
  draft,
  setDraft,
}: {
  draft: WorkspaceConfigDocument
  setDraft: (value: WorkspaceConfigDocument) => void
}) {
  const setExecution = (change: Partial<WorkspaceConfigDocument['execution']>) =>
    setDraft({ ...draft, execution: { ...draft.execution, ...change } })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Verification</CardTitle>
        <span className="text-xs text-faint">Defaults are frozen onto each task at intake.</span>
      </CardHeader>
      <CardContent className="grid gap-x-6 gap-y-4 md:grid-cols-2">
        <div className="flex items-center gap-3">
          <Switch
            aria-label="Verify before review"
            checked={draft.execution.verify_stage ?? false}
            onChange={(checked) => setExecution({ verify_stage: checked })}
          />
          <div>
            <p className="text-sm font-medium">Verify before review</p>
            <p className="text-xs text-faint">
              New tasks run their repository's kits and checks before review. Filed tasks keep the policy they started
              with.
            </p>
          </div>
        </div>
      </CardContent>
    </Card>
  )
}
