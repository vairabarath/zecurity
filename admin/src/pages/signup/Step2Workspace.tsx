import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  AuthCard,
  AuthInput,
  AuthShell,
  ContinueLabel,
  Field,
  FooterLink,
  PreviewPanel,
  PrimaryButton,
  WizardHeader,
} from '@/components/auth/AuthLayout'
import { suggestWorkspaceName, useSignupStore } from '@/store/signup'

export default function Step2Workspace() {
  const navigate = useNavigate()
  const { email, workspaceName, slug, setWorkspaceName } = useSignupStore()
  const [localName, setLocalName] = useState(workspaceName)

  useEffect(() => {
    if (!email || !email.includes('@')) {
      navigate('/signup', { replace: true })
    }
  }, [email, navigate])

  useEffect(() => {
    if (workspaceName) return
    const suggestion = suggestWorkspaceName(email)
    if (!suggestion) return
    setWorkspaceName(suggestion)
    setLocalName(suggestion)
  }, [email, setWorkspaceName, workspaceName])

  const canContinue = localName.trim().length > 0

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (!canContinue) return
    setWorkspaceName(localName)
    navigate('/signup/auth')
  }

  return (
    <AuthShell>
      <AuthCard>
        <WizardHeader
          step="Step 2 of 3"
          title="Name your network"
          description="Pick the subdomain your team will use to reach it."
        />

        <form onSubmit={handleSubmit}>
          <Field label="Network name" hint="You can rename the workspace later without repeating signup.">
            <AuthInput
              value={localName}
              onChange={(event) => {
                setLocalName(event.target.value)
                setWorkspaceName(event.target.value)
              }}
              placeholder="zero"
              autoFocus
            />
          </Field>

          {slug ? <PreviewPanel label="Your network URL" value={slug} suffix=".zecurity.in" /> : null}

          <PrimaryButton type="submit" disabled={!canContinue}>
            Continue
            <ContinueLabel />
          </PrimaryButton>
        </form>

        <FooterLink text="Need to change your account details?" cta="Go back" onClick={() => navigate('/signup')} />
      </AuthCard>
    </AuthShell>
  )
}
