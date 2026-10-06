import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

// Zecurity Provider Console (Sprint 21, D-18): a separate app on its own origin.
// In development the dev server proxies /provider/* to the controller, so the
// console is same-origin and needs no CORS. In production it is served from its
// own domain and the controller allows it via PROVIDER_CONSOLE_ORIGIN.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5174,
    strictPort: true,
    proxy: {
      '/provider': 'http://localhost:8080',
    },
  },
})
