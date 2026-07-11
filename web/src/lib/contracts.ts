export const pipelineGroups = [
  ['triage', 'Triage'],
  ['spec', 'Spec'],
  ['implement', 'Implementing'],
  ['review', 'Reviewing'],
  ['verify', 'Verifying'],
  ['human', 'Awaiting human'],
] as const

export const interventionActions = [
  ['approve', 'Approve'],
  ['reject', 'Reject'],
  ['redirect', 'Redirect'],
  ['pull_to_local', 'Pull to local'],
] as const

export type InterventionAction = typeof interventionActions[number][0]

export const reasonCodes = [
  'approved',
  'spec-wrong',
  'hallucinated-api',
  'style',
  'flaky-env',
  'scope-creep',
  'broken-pair',
  'needs-human',
] as const
