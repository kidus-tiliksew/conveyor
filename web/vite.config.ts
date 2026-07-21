import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'normalize-generated-whitespace',
      enforce: 'post',
      renderChunk(code) {
        return code.replace(/[\t ]+$/gm, '')
      },
      generateBundle(_options, bundle) {
        for (const output of Object.values(bundle)) {
          if (output.type === 'chunk') {
            output.code = output.code.replace(/[\t ]+$/gm, '')
          }
        }
      },
    },
  ],
  build: {
    outDir: '../internal/httpapi/dashboard',
    emptyOutDir: true,
  },
  server: {
    proxy: { '/v1': 'http://127.0.0.1:8080' },
  },
})
