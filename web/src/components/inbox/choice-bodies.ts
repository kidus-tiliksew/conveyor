import { defaultReasonCode, planRevisionReasonCodes } from '../../lib/contracts'
import type { NextActionDetails } from '../../lib/types'

// The request body each server choice needs, keyed by the choice id the
// server's catalogue states (internal/nextaction/choices.go). The server owns
// the label, the effect, the method, the path and whether the caller may act;
// this registry only builds the body and says whether the act needs typed input.
// A choice whose id is absent here is not rendered, so a server-added act is
// never posted with a guessed body (DEC-61; feature-operator-next-action NA-4).
export interface ChoiceBody {
  // Typed input the act carries. `required` input blocks sending while empty.
  input?: { label: string; required: boolean }
  // Acts the server deduplicates by request id carry one per send.
  idempotent?: boolean
  // `details` is the next action's own; a body built from server state reads it there.
  body: (input: string, requestId: string, details?: NextActionDetails) => Record<string, unknown> | undefined
  // Why the act cannot be sent from the state the server stated, or undefined when it can.
  unavailable?: (details?: NextActionDetails) => string | undefined
}

// A verification disposition posts the server's row-10 binding with the
// operator's reason (feature-operator-next-action NA-3, NA-6). Without the
// binding the act stays disabled with the server's refusal.
const disposition = (value: 'applied' | 'not_applied'): ChoiceBody => ({
  input: { label: 'Reason', required: true },
  idempotent: true,
  unavailable: (details) =>
    details?.disposition
      ? undefined
      : (details?.refusals?.find((refusal) => refusal.choice === value)?.text ?? 'Take this from the task page.'),
  body: (input, requestId, details) => ({
    request_id: requestId,
    verification: details?.disposition && {
      context_id: details.disposition.context_id,
      run_id: details.disposition.run_id,
      operation_ids: details.disposition.operation_ids,
      input_digest: details.disposition.input_digest,
      disposition: value,
      reason: input.trim(),
    },
  }),
})

const review = (action: string, reasonCode: string) => (input: string) => ({
  action,
  reason_code: reasonCode,
  comment: input.trim(),
})

export const choiceBodies: Record<string, ChoiceBody> = {
  approve: { body: review('approve', defaultReasonCode.approve) },
  redirect: { input: { label: 'Guidance', required: true }, body: review('redirect', defaultReasonCode.redirect) },
  reject: { input: { label: 'Rejection note', required: false }, body: review('reject', defaultReasonCode.reject) },
  approve_revision: {
    input: { label: 'Revision direction', required: false },
    body: review('redirect', planRevisionReasonCodes.approve),
  },
  decline: {
    input: { label: 'Implementation direction', required: true },
    body: review('redirect', planRevisionReasonCodes.decline),
  },
  request_changes: {
    input: { label: 'Changes you want', required: true },
    body: (input) => ({ feedback: input.trim() }),
  },
  merge: { body: () => undefined },
  fix_merge_conflict: { body: () => undefined },
  redispatch: { body: () => undefined },
  close: { input: { label: 'Reason', required: true }, body: (input) => ({ reason: input.trim() }) },
  answer: {
    input: { label: 'Direction', required: true },
    idempotent: true,
    body: (input, requestId) => ({ request_id: requestId, direction: input.trim() }),
  },
  // `recover` is stated against a work order (recover) or the task (redispatch);
  // both accept a request id, and the redispatch route ignores it.
  recover: { idempotent: true, body: (_input, requestId) => ({ request_id: requestId }) },
  recover_round: { idempotent: true, body: (_input, requestId) => ({ request_id: requestId }) },
  retry_round: {
    input: { label: 'Reason', required: true },
    idempotent: true,
    body: (input, requestId) => ({ request_id: requestId, reason: input.trim() }),
  },
  applied: disposition('applied'),
  not_applied: disposition('not_applied'),
}
