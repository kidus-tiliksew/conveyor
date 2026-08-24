import capabilityBundles from './workspace-capabilities.json'
import type { WorkspaceRole } from './types'

export type WorkspaceCapability =
  | 'view_workspace'
  | 'claim_work'
  | 'request_changes'
  | 'propose_documents'
  | 'confirm_documents'
  | 'manage_membership'
  | 'set_assignee'
  | 'operate_gates'
  | 'recover_work'
  | 'manage_workspace'

export const roleCapabilities = capabilityBundles as Record<WorkspaceRole, readonly WorkspaceCapability[]>
