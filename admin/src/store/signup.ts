import { create } from 'zustand'

// SignupState holds the transient state for the 3-step signup wizard.
// Completely separate from the auth store.
// Reset after OAuth redirect — the backend takes over from there.
interface SignupState {
  email: string
  accountType: 'home' | 'office' | ''
  workspaceName: string
  slug: string

  setEmail: (email: string) => void
  setAccountType: (type: 'home' | 'office') => void
  setWorkspaceName: (name: string) => void
  reset: () => void
}

// slugify mirrors Member 3's slugify() exactly.
// "Acme Corp" → "acme-corp"
// "My Company!" → "my-company"
// "My Network" → "my-network"
function slugify(name: string): string {
  let slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  if (slug === '') {
    slug = 'workspace'
  }
  return slug
}

// Suggests a workspace name from the email domain.
// alice@acme.com → "Acme"
// bob@my-corp.io → "My Corp"
// carol@gmail.com → "" (generic provider, skip)
function suggestWorkspaceName(email: string): string {
  const genericProviders = [
    'gmail.com',
    'yahoo.com',
    'hotmail.com',
    'outlook.com',
    'icloud.com',
    'proton.me',
  ]

  const atIdx = email.lastIndexOf('@')
  if (atIdx === -1 || atIdx === email.length - 1) return ''

  const domain = email.slice(atIdx + 1).toLowerCase()
  if (genericProviders.includes(domain)) return ''

  const firstSegment = domain.split('.')[0]
  if (!firstSegment) return ''

  // Replace hyphens/underscores with spaces, title-case
  return firstSegment
    .replace(/[-_]/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase())
}

export const useSignupStore = create<SignupState>((set) => ({
  email: '',
  accountType: '',
  workspaceName: '',
  slug: '',

  setEmail: (email) => set({ email }),

  setAccountType: (accountType) => set({ accountType }),

  setWorkspaceName: (workspaceName) =>
    set({ workspaceName, slug: slugify(workspaceName) }),

  reset: () =>
    set({
      email: '',
      accountType: '',
      workspaceName: '',
      slug: '',
    }),
}))

// Export slugify and suggestWorkspaceName for use in Step2Workspace
export { slugify, suggestWorkspaceName }
