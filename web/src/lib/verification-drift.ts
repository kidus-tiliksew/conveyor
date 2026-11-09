// A kit pin that names an older version of a document the work order holds
// keeps the kit eligible and records a pin_version_drift warning in the kit's
// selection reasons (component-verification-kit-contract invariant 12;
// DEC-111(1)). The warning is never an exclusion, and the review shows it
// (component-review invariant 23).

export type PinDrift = {
  kind: string
  documentId: string
  pinnedVersion: number
  // The selection message names the document, the kit's pinned version, and
  // the authoritative version, for example
  // "requirement req-integrations: kit pins v14, authoritative v15".
  text: string
}

type Reason = {
  code?: unknown
  message?: unknown
  pin?: { kind?: unknown; document_id?: unknown; version?: unknown }
}

// pinDriftWarnings reads the drift warnings from a kit's `reasons` metadata,
// the receipt's JSON reason list. A list cut by the read bound does not parse
// and yields none; the raw reasons stay visible in the kit selection details.
export function pinDriftWarnings(reasons: string | undefined): PinDrift[] {
  if (!reasons) return []
  let parsed: unknown
  try {
    parsed = JSON.parse(reasons)
  } catch {
    return []
  }
  if (!Array.isArray(parsed)) return []
  return (parsed as Reason[]).flatMap((reason) => {
    if (reason?.code !== 'pin_version_drift') return []
    const kind = typeof reason.pin?.kind === 'string' ? reason.pin.kind : ''
    const documentId = typeof reason.pin?.document_id === 'string' ? reason.pin.document_id : ''
    const pinnedVersion = typeof reason.pin?.version === 'number' ? reason.pin.version : 0
    const message = typeof reason.message === 'string' ? reason.message.trim() : ''
    const text = message || `${kind} ${documentId}: kit pins v${pinnedVersion}`.trim()
    return [{ kind, documentId, pinnedVersion, text }]
  })
}
