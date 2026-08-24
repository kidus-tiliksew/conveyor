import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'

const frontend = JSON.parse(readFileSync(new URL('../src/lib/workspace-capabilities.json', import.meta.url), 'utf8'))
const backend = JSON.parse(
  execFileSync('go', ['run', './scripts/capability-probe.go'], {
    cwd: new URL('..', import.meta.url),
    encoding: 'utf8',
  }),
)

const canonical = (bundles) =>
  Object.fromEntries(
    Object.entries(bundles)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([role, capabilities]) => [role, [...capabilities].sort()]),
  )

if (JSON.stringify(canonical(frontend)) !== JSON.stringify(canonical(backend))) {
  console.error('Dashboard capability bundles do not match internal/core/authorization.go.')
  console.error(`frontend: ${JSON.stringify(canonical(frontend))}`)
  console.error(`backend:  ${JSON.stringify(canonical(backend))}`)
  process.exit(1)
}
