import { useId, useState } from 'react'
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
  const ids = useId()

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
      </CardHeader>
      <CardContent>
        <p className="mb-4 text-sm leading-6 text-muted">Use a password to sign in next time.</p>
        <form className="w-full space-y-3" aria-label="Update password" onSubmit={submit}>
          <div className="space-y-1">
            <label htmlFor={`${ids}-current`} className="block text-xs font-medium text-muted">
              Current password
            </label>
            <Input
              id={`${ids}-current`}
              type="password"
              autoComplete="current-password"
              aria-describedby={`${ids}-current-hint`}
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
            />
            <p id={`${ids}-current-hint`} className="text-[11px] text-faint">
              Leave blank if you just opened a sign-in link.
            </p>
          </div>
          <div className="space-y-1">
            <label htmlFor={`${ids}-new`} className="block text-xs font-medium text-muted">
              New password
            </label>
            <Input
              id={`${ids}-new`}
              type="password"
              autoComplete="new-password"
              placeholder="At least 12 characters"
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
            />
          </div>
          <div className="space-y-1">
            <label htmlFor={`${ids}-confirm`} className="block text-xs font-medium text-muted">
              Confirm new password
            </label>
            <Input
              id={`${ids}-confirm`}
              type="password"
              autoComplete="new-password"
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
            />
          </div>
          <Button
            type="submit"
            className="w-full sm:w-auto"
            disabled={pending || newPassword.length < 12 || !confirmPassword}
          >
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
