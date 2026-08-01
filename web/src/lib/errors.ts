export function errorMessage(error: unknown, fallback = 'Something went wrong.') {
  if (error instanceof Error) return error.message.replace(/^Error:\s*/i, '').trim() || fallback
  if (typeof error === 'string') return error.replace(/^Error:\s*/i, '').trim() || fallback
  return fallback
}
