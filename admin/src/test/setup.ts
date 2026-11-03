import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach } from 'vitest'
import { cleanup } from '@testing-library/react'
import { Role } from '@/generated/graphql'
import { setTestRole } from '@/test/roles'

// Pages render write controls per capability (PENDING-19). Existing tests
// exercise the full console, so each test starts as an Admin; role tests call
// setTestRole themselves.
beforeEach(() => {
  setTestRole(Role.Admin)
})

afterEach(() => {
  cleanup()
})
