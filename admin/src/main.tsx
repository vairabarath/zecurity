import React from 'react'
import ReactDOM from 'react-dom/client'
import { ApolloProvider } from '@apollo/client/react'
import { BrowserRouter } from 'react-router-dom'
import { Toaster } from 'sonner'
import { apolloClient } from '@/apollo/client'
import App from './App'
import { MfaPrompt } from '@/components/auth/MfaPrompt'
import './index.css'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ApolloProvider client={apolloClient}>
      <BrowserRouter>
        <App />
        {/* PENDING-06: the one MFA prompt, above the routes so it also
            answers MFA_REQUIRED while the route guard probes `me`. */}
        <MfaPrompt />
      </BrowserRouter>
      {/* One toast container for the whole console: every toast() call in
          the app renders here (none did before it was mounted). */}
      <Toaster theme="dark" position="bottom-right" richColors closeButton />
    </ApolloProvider>
  </React.StrictMode>
)
