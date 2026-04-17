import { defineConfig } from 'vite'
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
  server: {
    port: 5173,
    // Proxy API calls to Go controller in development
    // so CORS is not needed during local dev
    //
    // Only /graphql and /auth/refresh are proxied.
    // /auth/callback is NOT proxied — Google's OAuth redirect goes directly
    // to the backend at localhost:8080. The React client-side route handles
    // reading the #token= hash after the backend redirects back.
    proxy: {
      '/graphql':      'http://localhost:8080',
      '/auth/refresh': 'http://localhost:8080',
      '/api':          'http://localhost:8080',
      '/ca.crt':       'http://localhost:8080',
    },
  },
})
