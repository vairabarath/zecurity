import { useState, type FormEvent } from 'react'
import { useNavigate, Link } from 'react-router-dom'
import { useSignupStore } from '@/store/signup'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

// Step 1 — Email + Account Type
//
// Route: /signup
//
// Purely local. No backend call.
// Collects email and account type (Home / Office).
// Stores in signup store, navigates to /signup/workspace.
export default function Step1Email() {
  const navigate = useNavigate()
  const { email, accountType, setEmail, setAccountType } = useSignupStore()

  const [localEmail, setLocalEmail] = useState(email)
  const [localAccountType, setLocalAccountType] = useState<'home' | 'office' | ''>(
    accountType || ''
  )

  const isValidEmail = localEmail.includes('@')
  const canContinue = localAccountType !== '' && isValidEmail

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!canContinue) return

    setEmail(localEmail)
    setAccountType(localAccountType)
    navigate('/signup/workspace')
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Create your network</CardTitle>
          <CardDescription>
            Tell us about yourself so we can set things up for you.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="flex flex-col gap-6">
            {/* Email */}
            <div className="flex flex-col gap-2">
              <Label htmlFor="email">Email address</Label>
              <Input
                id="email"
                type="email"
                placeholder="you@example.com"
                value={localEmail}
                onChange={(e) => setLocalEmail(e.target.value)}
                autoFocus
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && canContinue) {
                    handleSubmit(e)
                  }
                }}
              />
            </div>

            {/* Account Type */}
            <div className="flex flex-col gap-3">
              <Label>Account type</Label>
              <div className="grid grid-cols-2 gap-3">
                <button
                  type="button"
                  onClick={() => setLocalAccountType('home')}
                  className={`rounded-lg border-2 p-4 text-left transition-colors ${
                    localAccountType === 'home'
                      ? 'border-primary bg-primary/5'
                      : 'border-border hover:border-primary/50'
                  }`}
                >
                  <div className="font-medium">Home</div>
                  <div className="text-sm text-muted-foreground">
                    Personal devices and home lab
                  </div>
                </button>
                <button
                  type="button"
                  onClick={() => setLocalAccountType('office')}
                  className={`rounded-lg border-2 p-4 text-left transition-colors ${
                    localAccountType === 'office'
                      ? 'border-primary bg-primary/5'
                      : 'border-border hover:border-primary/50'
                  }`}
                >
                  <div className="font-medium">Office</div>
                  <div className="text-sm text-muted-foreground">
                    Team and company resources
                  </div>
                </button>
              </div>
            </div>

            {/* Continue */}
            <Button type="submit" disabled={!canContinue}>
              Continue
            </Button>

            {/* Sign in link */}
            <p className="text-center text-sm text-muted-foreground">
              Already have a network?{' '}
              <Link to="/login" className="text-primary hover:underline">
                Sign in
              </Link>
            </p>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
