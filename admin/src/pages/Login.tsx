import { useMutation } from '@apollo/client/react'
import {
  InitiateAuthDocument,
  type InitiateAuthMutation,
  type InitiateAuthMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'

// Login page.
// Single action: "Sign in with Google".
// Calls initiateAuth mutation → gets redirectUrl → redirects browser there.
//
// No form, no password, no email input.
// Google handles all identity verification.
export default function Login() {
  const [initiateAuth, { loading, error }] = useMutation<
    InitiateAuthMutation,
    InitiateAuthMutationVariables
  >(InitiateAuthDocument)

  async function handleSignIn() {
    try {
      const result = await initiateAuth({
        variables: { provider: 'google' },
        // X-Public-Operation header is set by authLink automatically
        // because operation name is "InitiateAuth"
      })

      const { redirectUrl, state } = result.data!.initiateAuth

      // Store state for CSRF verification.
      // We store in sessionStorage (not localStorage) because:
      //   - sessionStorage is cleared when the tab closes
      //   - It only needs to survive the OAuth redirect round-trip
      //   - localStorage would persist state across sessions (unnecessary)
      // Note: Member 2's callback handler verifies state via HMAC,
      // but we still store it here in case we need to compare it.
      sessionStorage.setItem('oauth_state', state)

      // Redirect the browser to Google's auth page.
      // This is a full browser redirect — not fetch, not axios.
      // The current page unloads. Google handles auth.
      // Google will redirect back to /auth/callback on our server.
      window.location.href = redirectUrl

    } catch (e) {
      // Error is shown via Apollo's error state below
      console.error('initiateAuth failed:', e)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle className="text-center">ZTNA Admin Console</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {error && (
            <p className="text-destructive text-sm text-center">
              Sign in failed. Please try again.
            </p>
          )}
          <Button
            onClick={handleSignIn}
            disabled={loading}
            className="w-full"
          >
            {loading ? 'Redirecting...' : 'Sign in with Google'}
          </Button>
        </CardContent>
      </Card>
    </div>
  )
}
