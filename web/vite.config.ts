import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build lands where the Go binary embeds it (internal/webui/dist).
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../internal/webui/dist', emptyOutDir: true },
  server: { proxy: { '/api': 'http://127.0.0.1:8088' } },
})
