import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  AccountTypeCard,
  AuthCard,
  AuthInput,
  AuthShell,
  ContinueLabel,
  Field,
  FooterLink,
  PrimaryButton,
  WizardHeader,
} from '@/components/auth/AuthLayout'
import { useSignupStore } from '@/store/signup'

export default function Step1Email() {
  const navigate = useNavigate()
  const { email, accountType, setEmail, setAccountType } = useSignupStore()

  const [localEmail, setLocalEmail] = useState(email)
  const [localAccountType, setLocalAccountType] = useState<'home' | 'office' | ''>(accountType || '')

  const isValidEmail = localEmail.includes('@')
  const canContinue = localAccountType !== '' && isValidEmail

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (!canContinue) return
    setEmail(localEmail)
    setAccountType(localAccountType)
    navigate('/signup/workspace')
  }

  return (
    <AuthShell>
      <AuthCard>
        <WizardHeader
          step="Step 1 of 3"
          title="Deploy your network"
          description="Start with an email and pick what you’re protecting."
        />

        <form onSubmit={handleSubmit}>
          <Field label="Email address" hint="Used to anchor your workspace and Google sign-in.">
            <AuthInput
              type="email"
              value={localEmail}
              onChange={(event) => setLocalEmail(event.target.value)}
              placeholder="you@example.com"
              autoFocus
            />
          </Field>

          <div className="mb-5">
            <label className="mb-2 block text-[12px] font-semibold text-foreground">Account type</label>
            <div className="grid grid-cols-2 gap-3">
              <AccountTypeCard
                type="home"
                active={localAccountType === 'home'}
                onClick={() => setLocalAccountType('home')}
              />
              <AccountTypeCard
                type="office"
                active={localAccountType === 'office'}
                onClick={() => setLocalAccountType('office')}
              />
            </div>
          </div>

          <PrimaryButton type="submit" disabled={!canContinue}>
            Continue
            <ContinueLabel />
          </PrimaryButton>
        </form>

        <FooterLink text="Already have a network?" cta="Sign in" onClick={() => navigate('/login')} />
      </AuthCard>
    </AuthShell>
  )
}
