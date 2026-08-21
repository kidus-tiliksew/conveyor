import { KeyRound } from 'lucide-react'
import { useState } from 'react'
import { updateOwnPassword } from '../../lib/api'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { Input } from '../ui/input'

export function PasswordCard() {
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [message, setMessage] = useState('')
  const [failed, setFailed] = useState(false)

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setFailed(false)
    if (newPassword !== confirmPassword) {
      setFailed(true)
      setMessage('New passwords do not match.')
      return
    }
    setPending(true)
    setMessage('')
    try {
      await updateOwnPassword(currentPassword, newPassword)
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
      setMessage('Password updated.')
    } catch (error) {
      setFailed(true)
      setMessage(error instanceof Error ? error.message : 'Could not update password.')
    } finally {
      setPending(false)
    }
  }

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Password</CardTitle>
        <KeyRound className="size-4 text-primary" />
      </CardHeader>
      <CardContent>
        <p className="mb-4 text-sm leading-6 text-muted">
          Set a password for returning sign-in. Enter your current password when changing it. If you arrived through a
          fresh operator-issued link, you can leave the current password blank to set or replace it.
        </p>
        <form className="max-w-sm space-y-3" aria-label="Update password" onSubmit={submit}>
          <Input
            type="password"
            aria-label="Current password"
            autoComplete="current-password"
            placeholder="Current password (when required)"
            value={currentPassword}
            onChange={(event) => setCurrentPassword(event.target.value)}
          />
          <Input
            type="password"
            aria-label="New password"
            autoComplete="new-password"
            placeholder="New password (12 characters minimum)"
            value={newPassword}
            onChange={(event) => setNewPassword(event.target.value)}
          />
          <Input
            type="password"
            aria-label="Confirm new password"
            autoComplete="new-password"
            placeholder="Confirm new password"
            value={confirmPassword}
            onChange={(event) => setConfirmPassword(event.target.value)}
          />
          <Button type="submit" disabled={pending || newPassword.length < 12 || !confirmPassword}>
            {pending ? 'Updating…' : 'Update password'}
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
