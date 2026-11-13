import { defaultReasonCode, planRevisionReasonCodes } from '../../lib/contracts'
import type { DispositionAnswer, NextActionDetails } from '../../lib/types'

// The request body each server choice needs, keyed by the choice id the
// server's catalogue states (internal/nextaction/choices.go). The server owns
// the label, the effect, the method, the path and whether the caller may act;
// this registry only builds the body and says whether the act needs typed input.
// A choice whose id resolves to no body renders disabled, so a server-added act
// is never posted with a guessed body (DEC-61; component-next-action invariant 32).
export interface ChoiceBody {
  // Typed input the act carries. `required` input blocks sending while empty.
  input?: { label: string; required: boolean }
  // Acts the server deduplicates by request id carry one per send.
  idempotent?: boolean
  // `details` is the next action's own; a body built from server state reads it there.
  // `answers` holds the operator's answer for each attempt of a disposition set, by run ID.
  body: (
    input: string,
    requestId: string,
    details?: NextActionDetails,
    answers?: Record<string, DispositionAnswer>,
  ) => Record<string, unknown> | undefined
  // Why the act cannot be sent from the state the server stated, or undefined when it can.
  unavailable?: (details?: NextActionDetails) => string | undefined
  // An act whose request a dedicated dialog builds: the choice opens that
  // dialog instead of posting `body`.
  dialog?: 'restart'
  // An act whose composer asks for one answer for each attempt of the
  // disposition set, above its typed input.
  composer?: 'dispositions'
}

// Recording dispositions answers every attempt of the disposition set the
// server states at row 11, with one reason, in one request
// (component-next-action invariant 15; DEC-133(1)). The client copies each
// entry's binding and reads no verification record. Without a set the act
// stays disabled with the server's refusal.
const recordDispositions: ChoiceBody = {
  input: { label: 'Reason', required: true },
  idempotent: true,
  composer: 'dispositions',
  unavailable: (details) =>
    details?.dispositions?.length
      ? undefined
      : (details?.refusals?.find((refusal) => refusal.choice === 'record_dispositions')?.text ??
        'Take this from the task page.'),
  body: (input, requestId, details, answers) => {
    const entries = details?.dispositions ?? []
    if (entries.length === 0 || entries.some((entry) => !answers?.[entry.run_id]))
      throw new Error('Record an answer for each attempt.')
    return {
      request_id: requestId,
      verification: {
        reason: input.trim(),
        dispositions: entries.map((entry) => ({
          context_id: entry.context_id,
          run_id: entry.run_id,
          operation_ids: entry.operation_ids ?? [],
          input_digest: entry.input_digest,
          disposition: answers?.[entry.run_id],
        })),
      },
    }
  },
}

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
  // Start over opens the task's restart dialog, which builds the request with
  // its reason, note and request ID (component-web-task-surfaces, Task header).
  start_over: { dialog: 'restart', body: () => undefined },
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
  record_dispositions: recordDispositions,
}

// A task-context proposal choice names its target in the id
// (component-next-action, Public surfaces): confirm:task_context:<kind>:<target-id>.
// Its route takes no body, so only the documented kinds resolve; a document
// proposal id or any other unregistered form stays without a body.
const taskContextChoice = /^(confirm|dismiss):task_context:(requirement|system_design):.+$/
const taskContextBody: ChoiceBody = { body: () => undefined }

// The body contract for one server choice id, or undefined when this client
// cannot build it (component-web-task-surfaces, Next-action card).
export function choiceBody(id: string): ChoiceBody | undefined {
  if (Object.hasOwn(choiceBodies, id)) return choiceBodies[id]
  return taskContextChoice.test(id) ? taskContextBody : undefined
}
