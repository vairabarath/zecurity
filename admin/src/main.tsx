import React from 'react'
import ReactDOM from 'react-dom/client'
import { ApolloProvider } from '@apollo/client/react'
import { BrowserRouter } from 'react-router-dom'
import { Toaster } from 'sonner'
import { apolloClient } from '@/apollo/client'
import App from './App'
import './index.css'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ApolloProvider client={apolloClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
      {/* One toast container for the whole console: every toast() call in
          the app renders here (none did before it was mounted). */}
      <Toaster theme="dark" position="bottom-right" richColors closeButton />
    </ApolloProvider>
  </React.StrictMode>
)
