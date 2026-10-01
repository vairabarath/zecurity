import { Link, Route, Routes } from 'react-router-dom'
import { ProviderBrand } from '@/components/ProviderBrand'
import { Toaster } from '@/components/ui/toaster'

// Router skeleton (Sprint 21 C-a, commit 2). Authentication, session handling,
// guards and the real pages arrive in commits 3–4; these routes are
// placeholders so the app builds, renders and can be tested end to end.

function ScaffoldHome() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-4 p-6">
      <ProviderBrand />
      <p className="text-sm text-muted-foreground">Provider console scaffold.</p>
    </main>
  )
}

function NotFound() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-3 p-6">
      <h1 className="font-display text-xl font-semibold">Page not found</h1>
      <Link to="/" className="text-sm text-primary underline-offset-4 hover:underline">
        Back to the Provider Console
      </Link>
    </main>
  )
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<ScaffoldHome />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}

export default function App() {
  return (
    <>
      <AppRoutes />
      <Toaster />
    </>
  )
}
