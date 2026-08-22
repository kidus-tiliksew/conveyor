import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, MailPlus, RotateCw, Trash2 } from 'lucide-react'
import { useState } from 'react'
import {
  fetchWorkspaceInvitations,
  inviteWorkspaceMember,
  LastWorkspaceOperatorError,
  resendWorkspaceInvitation,
  revokeWorkspaceInvitation,
  revokeWorkspaceMember,
  WorkspaceNotVisibleError,
} from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { MembershipGrant, WorkspaceRole } from '../../lib/types'
import { useOperatorToken, useWorkspaceMembers, useWorkspaceSelection } from '../app-shell'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { Input, Select } from '../ui/input'

function formatTimestamp(value: string) {
  return new Date(value).toLocaleString()
}

function roleLabel(role: WorkspaceRole) {
  return role.charAt(0).toUpperCase() + role.slice(1)
}

type DeliveryNotice = {
  grant: MembershipGrant
  kind: 'invitation' | 'role_change' | 'sign_in'
  previousRole?: WorkspaceRole
}

/**
 * Members and pending invitations for the selected workspace.
 *
 * Whether the reader may manage membership is answered by the server rather
 * than guessed: the invitation list is the operator-gated read, and a workspace
 * that refuses it hides the management controls. Every member still sees who
 * else is here, which the members API already restricts to co-members.
 */
export function MembersSection() {
  const token = useOperatorToken()
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const enabled = Boolean(token && workspace)

  const members = useWorkspaceMembers()
  const invitations = useQuery({
    queryKey: ['workspace-invitations', token, workspace],
    queryFn: () => fetchWorkspaceInvitations(token, workspace),
    enabled,
    retry: false,
  })
  const canManage = invitations.isSuccess
  const invitationsFailed = invitations.error != null && !(invitations.error instanceof WorkspaceNotVisibleError)

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['workspace-members', token, workspace] }),
      queryClient.invalidateQueries({ queryKey: ['workspace-invitations', token, workspace] }),
    ])
  }

  const [email, setEmail] = useState('')
  const [role, setRole] = useState<WorkspaceRole>('contributor')
  const [delivery, setDelivery] = useState<DeliveryNotice | null>(null)
  const normalizedEmail = email.trim().toLowerCase()
  const existingMember = members.data?.find((member) => member.email?.trim().toLowerCase() === normalizedEmail)
  const roleChange = existingMember && existingMember.role !== role ? existingMember : undefined
  const invite = useMutation({
    mutationFn: (request: { email: string; role: WorkspaceRole; existingRole?: WorkspaceRole }) =>
      inviteWorkspaceMember(token, workspace, { email: request.email, role: request.role }),
    onSuccess: async (result, request) => {
      setDelivery({
        grant: result,
        kind: request.existingRole ? 'role_change' : 'invitation',
        previousRole: request.existingRole,
      })
      setEmail('')
      setRole('contributor')
      await refresh()
    },
  })
  const removeMember = useMutation({
    mutationFn: (userID: string) => revokeWorkspaceMember(token, workspace, userID),
    onSuccess: refresh,
  })
  const removeInvitation = useMutation({
    mutationFn: (invitedEmail: string) => revokeWorkspaceInvitation(token, workspace, invitedEmail),
    onSuccess: refresh,
  })
  const resendInvitation = useMutation({
    mutationFn: (invitedEmail: string) => resendWorkspaceInvitation(token, workspace, invitedEmail),
    onSuccess: (result) => setDelivery({ grant: result, kind: 'invitation' }),
  })
  const sendMemberSignInLink = useMutation({
    mutationFn: (memberEmail: string) => resendWorkspaceInvitation(token, workspace, memberEmail),
    onSuccess: (result) => setDelivery({ grant: result, kind: 'sign_in' }),
  })

  const attemptedRoleChange = invite.variables?.existingRole

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Members</CardTitle>
          {!canManage && <span className="text-xs text-faint">View only</span>}
        </CardHeader>
        <CardContent className="space-y-3">
          {canManage && (
            <form
              className="flex flex-wrap items-end gap-2"
              aria-label="Invite a member"
              onSubmit={(event) => {
                event.preventDefault()
                invite.mutate({ email: email.trim(), role, existingRole: roleChange?.role })
              }}
            >
              <Input
                type="email"
                required
                aria-label="Email address"
                placeholder="person@example.com"
                className="max-w-xs"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
              />
              <Select
                aria-label="Role"
                className="max-w-36"
                value={role}
                onChange={(event) => setRole(event.target.value as WorkspaceRole)}
              >
                <option value="viewer">Viewer</option>
                <option value="executor">Executor</option>
                <option value="contributor">Contributor</option>
                <option value="maintainer">Maintainer</option>
                <option value="operator">Operator</option>
              </Select>
              <Button type="submit" disabled={!email.trim() || invite.isPending}>
                <MailPlus />
                {invite.isPending
                  ? roleChange
                    ? 'Changing role…'
                    : 'Inviting…'
                  : roleChange
                    ? 'Change role'
                    : 'Invite'}
              </Button>
              <p className="basis-full text-xs leading-5 text-muted">
                {roleChange
                  ? `Role change: ${roleChange.email} already has the ${roleLabel(roleChange.role)} role. Submitting will change their role to ${roleLabel(role)}.`
                  : 'The person receives a one-time sign-in link. Opening it creates their account and workspace membership. Operators can invite, remove, and change who else is an operator.'}
              </p>
              {invite.error && (
                <p className="basis-full text-xs text-failure">
                  {invite.error instanceof LastWorkspaceOperatorError && attemptedRoleChange
                    ? `This account already exists as ${invite.variables.email} with the ${roleLabel(attemptedRoleChange)} role. The attempted role change to ${roleLabel(invite.variables.role)} was refused because this is the sole workspace operator. Grant another member the Operator role first, then try the demotion again.`
                    : errorMessage(invite.error, 'Could not send that invitation.')}
                </p>
              )}
            </form>
          )}
          {delivery && (
            <div className="space-y-2 rounded-lg border border-primary/25 bg-primary-soft/40 p-3" role="status">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <p className="text-sm font-medium text-foreground">
                    {delivery.kind === 'sign_in'
                      ? delivery.grant.delivery === 'sent'
                        ? 'Sign-in link sent'
                        : 'Sign-in link ready to share'
                      : delivery.kind === 'role_change'
                        ? 'Role updated'
                        : delivery.grant.delivery === 'sent'
                          ? 'Invitation sent'
                          : 'Invitation ready to share'}
                  </p>
                  <p className="mt-1 text-xs leading-5 text-muted">
                    {delivery.kind === 'sign_in'
                      ? delivery.grant.delivery === 'sent'
                        ? 'We emailed a fresh sign-in link. The member can use it to get back in if they forgot their password.'
                        : delivery.grant.sign_in_url
                          ? 'Email delivery is unavailable. Copy this fresh sign-in link so the member can get back in if they forgot their password.'
                          : 'A fresh sign-in link could not be delivered. Try sending it again.'
                      : delivery.kind === 'role_change'
                        ? `${delivery.grant.email} changed from ${roleLabel(delivery.previousRole ?? delivery.grant.role)} to ${roleLabel(delivery.grant.role)}.${delivery.grant.sign_in_url ? ' Email delivery is unavailable; copy the fresh sign-in link below.' : ''}`
                        : delivery.grant.delivery === 'sent'
                          ? 'We sent a sign-in link by email.'
                          : delivery.grant.sign_in_url
                            ? 'Email delivery is unavailable. Copy this link and send it yourself.'
                            : 'The invitation was created. Use resend to create a new sign-in link.'}
                  </p>
                </div>
                <Button size="sm" variant="ghost" onClick={() => setDelivery(null)}>
                  Dismiss
                </Button>
              </div>
              {delivery.grant.sign_in_url && (
                <div className="flex items-center gap-2 rounded-md border border-border bg-card p-2">
                  <p className="min-w-0 flex-1 truncate font-mono text-xs" title={delivery.grant.sign_in_url}>
                    {delivery.grant.sign_in_url}
                  </p>
                  <CopyButton
                    value={delivery.grant.sign_in_url}
                    label={delivery.kind === 'invitation' ? 'Copy invitation link' : 'Copy sign-in link'}
                  />
                </div>
              )}
            </div>
          )}
          {members.error && (
            <p className="text-sm text-failure">{errorMessage(members.error, 'Could not load the member list.')}</p>
          )}
          {members.isSuccess && members.data.length === 0 && <p className="text-sm text-faint">No members yet.</p>}
          {members.data?.map((member) => (
            <div key={member.user_id} className="flex items-center justify-between rounded-md border border-border p-3">
              <div className="min-w-0">
                <p className="truncate text-sm font-medium">{member.display_name || member.email || member.user_id}</p>
                <p className="truncate text-xs text-muted" title={`Joined ${formatTimestamp(member.created_at)}`}>
                  {member.email}
                </p>
              </div>
              <div className="flex shrink-0 items-center gap-2">
                <Badge variant={member.role === 'operator' ? 'accent' : 'default'}>{roleLabel(member.role)}</Badge>
                {canManage && (
                  <Button
                    size="sm"
                    variant="secondary"
                    aria-label={`Send sign-in link to ${member.display_name || member.email || member.user_id}`}
                    disabled={sendMemberSignInLink.isPending || !member.email}
                    onClick={() => member.email && sendMemberSignInLink.mutate(member.email)}
                  >
                    <KeyRound />
                    {sendMemberSignInLink.isPending && sendMemberSignInLink.variables === member.email
                      ? 'Sending…'
                      : 'Send sign-in link'}
                  </Button>
                )}
                {canManage && (
                  <Button
                    size="icon"
                    variant="ghost"
                    className="hover:text-failure"
                    aria-label={`Remove ${member.display_name || member.email || member.user_id}`}
                    disabled={removeMember.isPending}
                    onClick={() => removeMember.mutate(member.user_id)}
                  >
                    <Trash2 />
                  </Button>
                )}
              </div>
            </div>
          ))}
          {removeMember.error && (
            <p className="text-sm text-failure">
              {removeMember.error instanceof LastWorkspaceOperatorError
                ? 'This workspace would be left without an operator. Make someone else an operator first, then remove this one.'
                : errorMessage(removeMember.error, 'Could not remove that member.')}
            </p>
          )}
          {sendMemberSignInLink.error && (
            <p className="text-sm text-failure">
              {errorMessage(sendMemberSignInLink.error, 'Could not send that sign-in link.')}
            </p>
          )}
        </CardContent>
      </Card>

      {invitationsFailed && (
        <Card>
          <CardContent className="text-sm text-failure">
            {errorMessage(invitations.error, 'Could not load pending invitations.')}
          </CardContent>
        </Card>
      )}

      {canManage && (
        <Card>
          <CardHeader>
            <CardTitle>Pending invitations</CardTitle>
            <span className="text-xs text-faint">{invitations.data.length}</span>
          </CardHeader>
          <CardContent className="space-y-3">
            {invitations.data.length === 0 && <p className="text-sm text-faint">No pending invitations.</p>}
            {invitations.data.map((invitation) => (
              <div
                key={invitation.email}
                className="flex items-center justify-between rounded-md border border-border p-3"
              >
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{invitation.email}</p>
                  <p
                    className="truncate text-xs text-muted"
                    title={`Invited ${formatTimestamp(invitation.created_at)}`}
                  >
                    Invited by {invitation.invited_by_display_name || invitation.invited_by}
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Badge variant={invitation.role === 'operator' ? 'accent' : 'default'}>
                    {roleLabel(invitation.role)}
                  </Badge>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={resendInvitation.isPending}
                    onClick={() => resendInvitation.mutate(invitation.email)}
                  >
                    <RotateCw />
                    {resendInvitation.isPending && resendInvitation.variables === invitation.email
                      ? 'Resending…'
                      : 'Resend'}
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    disabled={removeInvitation.isPending}
                    onClick={() => removeInvitation.mutate(invitation.email)}
                  >
                    Revoke
                  </Button>
                </div>
              </div>
            ))}
            {removeInvitation.error && (
              <p className="text-sm text-failure">
                {errorMessage(removeInvitation.error, 'Could not revoke that invitation.')}
              </p>
            )}
            {resendInvitation.error && (
              <p className="text-sm text-failure">
                {errorMessage(resendInvitation.error, 'Could not resend that invitation.')}
              </p>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
