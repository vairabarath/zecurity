import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation } from '@apollo/client/react'
import {
  InitiateAuthDocument,
  type InitiateAuthMutation,
  type InitiateAuthMutationVariables,
} from '@/generated/graphql'
import { useSignupStore } from '@/store/signup'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

// Step 3 — Sign In With Google
//
// Route: /signup/auth
//
// Guard: if email or workspaceName is empty, redirect to /signup.
// Shows a summary card with the chosen email and workspace name.
// Calls initiateAuth with workspaceName, stores state, resets signup store,
// and redirects to Google OAuth.
export default function Step3Auth() {
  const navigate = useNavigate()
  const { email, workspaceName, reset } = useSignupStore()

  const [initiateAuth, { loading, error }] = useMutation<
    InitiateAuthMutation,
    InitiateAuthMutationVariables
  >(InitiateAuthDocument)

  // Guard: if email or workspaceName is empty, redirect to /signup
  useEffect(() => {
    if (!email || !email.includes('@') || !workspaceName) {
      navigate('/signup', { replace: true })
    }
  }, [email, workspaceName, navigate])

  async function handleSignIn() {
    try {
      const result = await initiateAuth({
        variables: { provider: 'google', workspaceName },
      })

      const { redirectUrl, state } = result.data!.initiateAuth

      // Store state for CSRF verification (same as Login.tsx)
      sessionStorage.setItem('oauth_state', state)

      // Reset signup store — workspace name is now in Redis on the backend,
      // it does not need to live in memory anymore.
      reset()

      // Full browser redirect to Google OAuth
      window.location.href = redirectUrl
    } catch (e) {
      console.error('initiateAuth failed:', e)
    }
  }

  function handleBack() {
    navigate('/signup/workspace')
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>One last step</CardTitle>
          <CardDescription>
            Sign in with Google to verify your identity and create your network.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-6">
          {/* Summary Card */}
          <div className="rounded-lg border bg-muted/50 p-4">
            <dl className="space-y-2 text-sm">
              <div className="flex justify-between">
                <dt className="text-muted-foreground">Email</dt>
                <dd className="font-medium">{email}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-muted-foreground">Network name</dt>
                <dd className="font-medium">{workspaceName}</dd>
              </div>
            </dl>
          </div>

          {/* Error Message */}
          {error && (
            <p className="text-destructive text-sm text-center">
              Something went wrong. Please try again.
            </p>
          )}

          {/* Sign In Button */}
          <Button
            onClick={handleSignIn}
            disabled={loading}
            className="w-full"
          >
            {loading ? 'Redirecting...' : 'Sign in with Google'}
          </Button>

          {/* Back button */}
          <Button type="button" variant="ghost" onClick={handleBack}>
            Back
          </Button>
        </CardContent>
      </Card>
    </div>
  )
}
