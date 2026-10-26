import { Link } from 'react-router-dom'
import { ProviderBrand } from '@/components/ProviderBrand'

export function NotFound() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-4 p-6 text-center">
      <ProviderBrand size="lg" className="mb-4" />
      <h1 className="font-display text-xl font-semibold">Page not found</h1>
      <p className="text-sm text-subtle-foreground">The link may be out of date.</p>
      <Link to="/" className="text-sm text-primary hover:underline">
        Back to the Provider Console
      </Link>
    </main>
  )
}
