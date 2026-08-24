import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { UserRound } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'
import { fetchCallerIdentity, updateOwnDisplayName } from '../../lib/api'

export function ProfileCard() {
  const { workspace } = useWorkspaceSelection()
  const queryClient = useQueryClient()
  const identity = useQuery({
    queryKey: ['caller-identity', workspace],
    queryFn: () => fetchCallerIdentity(),
    enabled: Boolean(workspace),
  })
  const [displayName, setDisplayName] = useState('')
  const [message, setMessage] = useState('')
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    if (identity.data) setDisplayName(identity.data.display_name ?? '')
  }, [identity.data])

  const update = useMutation({
    mutationFn: () => updateOwnDisplayName(displayName),
    onSuccess: async (profile) => {
      setFailed(false)
      setDisplayName(profile.display_name)
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
            update.mutate()
          }}
        >
          <Input
            aria-label="Display name"
            autoComplete="name"
            maxLength={128}
            value={displayName}
            onChange={(event) => setDisplayName(event.target.value)}
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
