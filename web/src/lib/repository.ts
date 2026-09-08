// req-repository-onboarding AC-1.3; mirrors gitx.NormalizeRepositoryIdentity.
export function githubSlug(raw: string): string {
  const value = raw.trim()
  let host: string
  let path: string
  if (!value.includes('://')) {
    const at = value.lastIndexOf('@')
    const colon = value.indexOf(':', at + 1)
    if (at < 0 || colon < 0) return ''
    host = value.slice(at + 1, colon)
    path = value.slice(colon + 1)
  } else {
    // Parse without WHATWG path cleanup, which would erase dot segments that
    // Go's net/url preserves. Path unescaping follows Go's URL.Path field.
    const match = /^(https?|ssh|git):\/\/([^/?#]+)([^?#]*)/i.exec(value)
    if (!match) return ''
    const authority = match[2].slice(match[2].lastIndexOf('@') + 1)
    const [hostname, port, extra] = authority.split(':')
    if (extra !== undefined) return ''
    const defaultPorts: Record<string, string> = { http: '80', https: '443', ssh: '22', git: '9418' }
    if (port !== undefined && port !== '' && port !== defaultPorts[match[1].toLowerCase()]) return ''
    host = hostname
    try {
      path = decodeURIComponent(match[3])
    } catch {
      return ''
    }
  }
  if (host.trim().toLowerCase() !== 'github.com') return ''
  path = path
    .trim()
    .replace(/^\/+|\/+$/g, '')
    .replace(/\.git$/, '')
  return path && !path.includes('\\') ? path.toLowerCase() : ''
}
