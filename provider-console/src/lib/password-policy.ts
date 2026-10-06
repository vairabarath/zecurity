// Client-side hints that mirror ValidateNewPassword
// (controller/internal/provider/password.go). They only save a round trip;
// the server's answer (400 password_policy) is what counts.

export const MIN_PASSWORD_CHARS = 12
export const MAX_PASSWORD_CHARS = 128

export function passwordPolicyProblems(
  newPassword: string,
  { email, currentPassword }: { email?: string; currentPassword?: string } = {},
): string[] {
  const problems: string[] = []
  // Count code points like the server's utf8.RuneCountInString.
  const n = [...newPassword].length
  if (n < MIN_PASSWORD_CHARS || n > MAX_PASSWORD_CHARS) {
    problems.push(`Use ${MIN_PASSWORD_CHARS}–${MAX_PASSWORD_CHARS} characters.`)
  }
  if (email && newPassword.trim().toLowerCase() === email.trim().toLowerCase()) {
    problems.push('Don’t use your email address.')
  }
  if (currentPassword && newPassword === currentPassword) {
    problems.push('Choose a password different from the current one.')
  }
  return problems
}
