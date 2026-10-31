// The ELK worker starts when the Board asks for the map's chunk, so the
// worker compiles its script while the chunk and the map's read load. This
// module holds only the worker script's address: no ELK code enters the
// Board's chunk (DEC-76; component-web-task-surfaces, Board map; req-board-map
// AC-3.2).
import workerUrl from 'elkjs/lib/elk-worker.min.js?url'

let started: Worker | undefined

export function startLayoutWorker() {
  if (started || typeof Worker === 'undefined') return
  const worker = new Worker(workerUrl)
  // A worker that fails before layout.ts adopts it is discarded, and
  // layout.ts starts a fresh one; once adopted, layout.ts handles failures.
  worker.addEventListener('error', () => {
    if (started !== worker) return
    started = undefined
    worker.terminate()
  })
  started = worker
}

// The started worker, at most once; otherwise a fresh worker for the URL.
export function adoptLayoutWorker(url: string) {
  const worker = started ?? new Worker(url)
  started = undefined
  return worker
}
