import { useState, type FormEvent, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useSignupStore, suggestWorkspaceName } from '@/store/signup'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

// Step 2 — Workspace Name
//
// Route: /signup/workspace
//
// Guard: if email is empty in signup store, redirect to /signup.
// Auto-suggests a workspace name from the email domain on mount.
// Shows a live slug preview that updates on every keystroke.
export default function Step2Workspace() {
  const navigate = useNavigate()
  const { email, workspaceName, slug, setWorkspaceName } = useSignupStore()

  // Guard: if no email, redirect to /signup
  useEffect(() => {
    if (!email || !email.includes('@')) {
      navigate('/signup', { replace: true })
    }
  }, [email, navigate])

  const [localName, setLocalName] = useState(workspaceName)

  // Auto-suggest workspace name from email domain on first mount.
  useEffect(() => {
    if (workspaceName) return // Do not overwrite if already set
    const suggestion = suggestWorkspaceName(email)
    if (suggestion) {
      setWorkspaceName(suggestion)
      setLocalName(suggestion)
    }
  }, [email, workspaceName, setWorkspaceName])

  const canContinue = localName.trim().length > 0

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!canContinue) return
    setWorkspaceName(localName)
    navigate('/signup/auth')
  }

  function handleBack() {
    navigate('/signup')
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Name your network</CardTitle>
          <CardDescription>You can always rename it later.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="flex flex-col gap-6">
            {/* Workspace Name Input */}
            <div className="flex flex-col gap-2">
              <Label htmlFor="workspaceName">Network name</Label>
              <Input
                id="workspaceName"
                type="text"
                placeholder="Acme Corp"
                value={localName}
                onChange={(e) => {
                  setLocalName(e.target.value)
                  setWorkspaceName(e.target.value)
                }}
                autoFocus
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && canContinue) {
                    handleSubmit(e)
                  }
                }}
              />
            </div>

            {/* Slug Preview */}
            <div className="rounded-lg border bg-muted/50 p-4">
              <div className="text-sm text-muted-foreground">
                Your network URL
              </div>
              <div className="mt-1 font-mono text-sm">
                <span className="text-muted-foreground">ztna.yourapp.com/</span>
                <span className="text-foreground">{slug || 'your-network'}</span>
              </div>
            </div>

            {/* Buttons */}
            <div className="flex flex-col gap-3">
              <Button type="submit" disabled={!canContinue}>
                Continue
              </Button>
              <Button type="button" variant="ghost" onClick={handleBack}>
                Back
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
