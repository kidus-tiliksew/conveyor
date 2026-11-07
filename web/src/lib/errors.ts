// An API body such as {"error":"not found"} reads as its message.
function readableMessage(text: string, fallback: string) {
  const trimmed = text.replace(/^Error:\s*/i, '').trim()
  if (trimmed.startsWith('{')) {
    try {
      const body = JSON.parse(trimmed) as { error?: unknown; message?: unknown }
      const message = body.error ?? body.message
      if (typeof message === 'string' && message.trim()) return message.trim()
    } catch {
      // Not JSON after all: the text stands as it is.
    }
  }
  return trimmed || fallback
}

export function errorMessage(error: unknown, fallback = 'Something went wrong.') {
  if (error instanceof Error) return readableMessage(error.message, fallback)
  if (typeof error === 'string') return readableMessage(error, fallback)
  return fallback
}
