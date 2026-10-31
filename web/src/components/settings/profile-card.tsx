import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { UserRound } from 'lucide-react'
import { useState } from 'react'
import { fetchCallerIdentity, updateOwnDisplayName } from '../../lib/api'
import type { CallerIdentity } from '../../lib/types'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'

export function ProfileCard() {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const identity = useQuery({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(workspace),
    enabled: Boolean(workspace),
  })
  // The field shows the identity read's name until the user types. Typed
  // text is a draft that a later identity read never replaces, and the field
  // is never editable while it shows anything else (component-web-dashboard,
  // Settings).
  const [draft, setDraft] = useState<string | null>(null)
  const displayName = draft ?? identity.data?.display_name ?? ''
  const [message, setMessage] = useState('')
  const [failed, setFailed] = useState(false)

  const update = useMutation({
    mutationFn: (name: string) => updateOwnDisplayName(name),
    onSuccess: async (profile, name) => {
      setFailed(false)
      // The identity entry carries the saved name, so the field follows it
      // again; without a loaded identity the saved name stays as the draft.
      const saved = queryClient.setQueryData<CallerIdentity>(
        ['caller-identity', workspace],
        (current) => current && { ...current, display_name: profile.display_name },
      )
      // Text typed while the save was in flight stays.
      setDraft((current) => (current === name ? (saved ? null : profile.display_name) : current))
      setMessage('Display name updated.')
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['caller-identity'] }),
        queryClient.invalidateQueries({ queryKey: ['workspace-members'] }),
      ])
    },
    onError: (cause) => {
      setFailed(true)
      setMessage(cause instanceof Error ? cause.message : 'Could not update display name.')
    },
  })

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Profile</CardTitle>
        <UserRound className="size-4 text-primary" />
      </CardHeader>
      <CardContent>
        <p className="mb-4 text-sm leading-6 text-muted">This is the name teammates see in workspace member lists.</p>
        <form
          className="max-w-sm space-y-3"
          aria-label="Update profile"
          onSubmit={(event) => {
            event.preventDefault()
            setMessage('')
            update.mutate(displayName)
          }}
        >
          <Input
            aria-label="Display name"
            autoComplete="name"
            maxLength={128}
            value={displayName}
            onChange={(event) => setDraft(event.target.value)}
            disabled={identity.isPending}
          />
          <Button type="submit" disabled={update.isPending || !displayName.trim()}>
            {update.isPending ? 'Saving…' : 'Save profile'}
          </Button>
          {message && (
            <p className={`text-sm ${failed ? 'text-failure' : 'text-positive'}`} role="status">
              {message}
            </p>
          )}
        </form>
      </CardContent>
    </Card>
  )
}
