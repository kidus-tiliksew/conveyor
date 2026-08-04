import Markdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'

// Shared safe GFM presentation for human-authored prose. React Markdown keeps
// raw HTML disabled unless rehype-raw is added; do not add it here.
export function MarkdownProse({
  children,
  className = '',
  components,
}: {
  children: string
  className?: string
  components?: Components
}) {
  return (
    <div className={`markdown ${className}`.trim()}>
      <Markdown remarkPlugins={[remarkGfm]} components={components}>
        {children}
      </Markdown>
    </div>
  )
}
