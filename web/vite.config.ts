import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'normalize-generated-whitespace',
      renderChunk(code) {
        return code.replace(/[\t ]+$/gm, '')
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
