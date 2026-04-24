import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation } from '@apollo/client/react'
import {
  InitiateAuthDocument,
  type InitiateAuthMutation,
  type InitiateAuthMutationVariables,
} from '@/generated/graphql'
import {
  AuthCard,
  AuthShell,
  FooterLink,
  LockLabel,
  PreviewPanel,
  PrimaryButton,
  WizardHeader,
} from '@/components/auth/AuthLayout'
import { useSignupStore } from '@/store/signup'

export default function Step3Auth() {
  const navigate = useNavigate()
  const { email, workspaceName, slug, reset } = useSignupStore()

  const [initiateAuth, { loading, error }] = useMutation<
    InitiateAuthMutation,
    InitiateAuthMutationVariables
  >(InitiateAuthDocument)

  useEffect(() => {
    if (!email || !email.includes('@') || !workspaceName) {
      navigate('/signup', { replace: true })
    }
  }, [email, navigate, workspaceName])

  async function handleSignIn() {
    try {
      const result = await initiateAuth({
        variables: { provider: 'google', workspaceName },
      })
      const { redirectUrl, state } = result.data!.initiateAuth
      sessionStorage.setItem('oauth_state', state)
      reset()
      window.location.href = redirectUrl
    } catch (mutationError) {
      console.error('initiateAuth failed:', mutationError)
    }
  }

  return (
    <AuthShell>
      <AuthCard>
        <WizardHeader
          step="Step 3 of 3"
          title="Verify with Google"
          description="Confirm your identity to finish provisioning the workspace."
        />

        <div className="space-y-3">
          <PreviewPanel label="Email" value={email} />
          <PreviewPanel label="Network" value={workspaceName} />
          {slug ? <PreviewPanel label="Endpoint" value={slug} suffix=".zecurity.in" /> : null}
        </div>

        {error ? (
          <p className="mt-4 text-sm font-medium text-destructive">
            Something went wrong starting OAuth. Please retry.
          </p>
        ) : null}

        <PrimaryButton onClick={handleSignIn} disabled={loading}>
          <LockLabel />
          {loading ? 'Redirecting...' : 'Sign in with Google'}
        </PrimaryButton>

        <FooterLink text="Need to rename the network first?" cta="Back to previous step" onClick={() => navigate('/signup/workspace')} />
      </AuthCard>
    </AuthShell>
  )
}
