import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
    // Node 22+ ships its own (experimental) localStorage global, which hides
    // jsdom's and is undefined without --localstorage-file. Turn it off so the
    // tests see the browser's Web Storage (the no-persistence tests need it).
    execArgv: ['--no-experimental-webstorage'],
  },
})
