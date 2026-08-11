import { defineConfig, devices } from '@playwright/test'
import { createServer } from 'node:net'

async function selectAvailablePort(preferred: number): Promise<number> {
  const listen = (port: number) =>
    new Promise<number>((resolve, reject) => {
      const server = createServer()
      server.once('error', reject)
      server.listen(port, '127.0.0.1', () => {
        const address = server.address()
        if (address === null || typeof address === 'string') {
          server.close()
          reject(new Error('could not resolve the selected Playwright port'))
          return
        }
        server.close((error) => (error === undefined ? resolve(address.port) : reject(error)))
      })
    })

  try {
    return await listen(preferred)
  } catch {
    return listen(0)
  }
}

const configuredPort = process.env.PLAYWRIGHT_PORT
const parsedPort = configuredPort === undefined ? undefined : Number(configuredPort)
if (parsedPort !== undefined && (!Number.isInteger(parsedPort) || parsedPort < 1 || parsedPort > 65535)) {
  throw new Error(`PLAYWRIGHT_PORT must be an integer from 1 to 65535, got ${configuredPort}`)
}

const configuredWorkers = process.env.PLAYWRIGHT_WORKERS
const workers = configuredWorkers === undefined ? 2 : Number(configuredWorkers)
if (!Number.isInteger(workers) || workers < 1) {
  throw new Error(`PLAYWRIGHT_WORKERS must be a positive integer, got ${configuredWorkers}`)
}

const port = parsedPort ?? (await selectAvailablePort(49152 + (process.pid % 16384)))
if (configuredPort === undefined) {
  // Playwright evaluates this config again in each test worker. Export the
  // invocation's selected port so the workers inherit the web server's URL
  // instead of choosing their own ports.
  process.env.PLAYWRIGHT_PORT = String(port)
}

export default defineConfig({
  testDir: './tests',
  workers,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    ...devices['Desktop Chrome'],
  },
  webServer: {
    command: `npm run dev -- --host 127.0.0.1 --port ${port} --strictPort`,
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: false,
  },
})
