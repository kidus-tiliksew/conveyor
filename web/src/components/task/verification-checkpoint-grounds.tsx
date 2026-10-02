import { verificationGroundText, verificationPermissionText } from '../../lib/activity'
import type { VerificationMetadata, WorkOrderVerificationCheckpointGround } from '../../lib/types'
import { cn } from '../../lib/utils'
import { VerificationEvidenceDisclosure } from './verification-evidence'

// feature-verification-kit-execution VK-13.5; component-web-dashboard VK-WEB-6:
// each checkpoint ground names its subject, the permissions an operator grants
// for an unstarted subject, and links the attempt and retained evidence. A
// ground without an attempt states that its evidence is missing.

export function verificationAttemptAnchor(contextId: string, attemptId: string) {
  return `verification-${contextId}-attempt-${attemptId}`
}

// Historical contexts keep their grounds in the sealed record, not on the
// order. The record names its attempt grounds — the latest attempt of each
// subject that stopped verification — and only those attempts are grounds;
// a superseded attempt stays in the attempt history. Unstarted subjects and
// unresolved operations stay in the sealed summary.
export function sealedAttemptGroundIds(context: VerificationMetadata): string[] {
  return (context.metadata.checkpoint_attempt_grounds ?? '')
    .split(',')
    .map((id) => id.trim())
    .filter(Boolean)
}

export function sealedAttemptGrounds(
  attemptIds: string[],
  attempts: VerificationMetadata[],
  evidence: VerificationMetadata[],
  evidenceComplete: boolean,
): { grounds: WorkOrderVerificationCheckpointGround[]; unloaded: string[] } {
  const grounds: WorkOrderVerificationCheckpointGround[] = []
  const unloaded: string[] = []
  for (const id of attemptIds) {
    const attempt = attempts.find((candidate) => candidate.id === id)
    if (!attempt) {
      unloaded.push(id)
      continue
    }
    grounds.push({
      kind: `attempt_${attempt.metadata.outcome || attempt.state}` as WorkOrderVerificationCheckpointGround['kind'],
      subject: {
        kind: attempt.metadata.kind === 'kit' ? 'kit' : 'ordinary',
        kit_id: attempt.metadata.kit_id,
        exercise_id: attempt.metadata.exercise_id,
        obligation_id: attempt.metadata.obligation_id,
      },
      attempt_id: attempt.id,
      explanation: attempt.metadata.required_action,
      evidence_ids: evidence.filter((item) => item.run_id === attempt.id).map((item) => item.id),
      truncated: !evidenceComplete,
      server_verified: true,
    })
  }
  return { grounds, unloaded }
}

export function VerificationCheckpointGrounds({
  taskId,
  contextId,
  grounds,
  className,
}: {
  taskId: string
  contextId: string
  grounds: WorkOrderVerificationCheckpointGround[]
  className?: string
}) {
  return (
    <ul aria-label="Checkpoint grounds" className={cn('space-y-1.5 text-xs font-normal text-foreground/85', className)}>
      {grounds.map((ground, index) => (
        <li key={`${ground.kind}:${ground.attempt_id ?? ''}:${index}`} className="space-y-0.5">
          <p>{verificationGroundText(ground)}</p>
          {ground.explanation && <p className="whitespace-pre-wrap break-words text-muted">{ground.explanation}</p>}
          {(ground.permissions?.length ?? 0) > 0 && (
            <p>
              Requires{' '}
              <span className="font-mono">
                {ground.permissions?.map((permission) => verificationPermissionText(permission)).join(', ')}
              </span>
            </p>
          )}
          {ground.attempt_id && (
            <a
              href={`#${verificationAttemptAnchor(contextId, ground.attempt_id)}`}
              className="font-mono text-primary hover:underline"
            >
              Attempt {ground.attempt_id}
            </a>
          )}
          {(ground.evidence_ids ?? []).map((evidenceId) => (
            <VerificationEvidenceDisclosure
              key={evidenceId}
              taskId={taskId}
              contextId={contextId}
              evidenceId={evidenceId}
            />
          ))}
          {ground.truncated && <p className="text-muted">Further identifiers are omitted from this bounded view.</p>}
        </li>
      ))}
    </ul>
  )
}
