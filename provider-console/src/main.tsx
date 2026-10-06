import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import { rehydrate } from './auth/flows'
import './index.css'

// Restore a session from this tab's sessionStorage before the first render.
// rehydrate() moves the store to "loading" synchronously, so the guards show a
// loading state (not Login) until GET /provider/me has confirmed the token.
void rehydrate()

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
)
