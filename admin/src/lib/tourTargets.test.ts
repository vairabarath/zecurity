import { afterEach, describe, expect, it } from 'vitest'
import { findStepTarget } from './tourTargets'

afterEach(() => {
  document.body.innerHTML = ''
})

describe('findStepTarget', () => {
  it('finds a button by its text, preferring an open dialog', () => {
    document.body.innerHTML = `
      <main><button id="page">Add Resource</button></main>
      <div data-tour-dialog><h2>Add Resource</h2><button id="submit">Add Resource</button></div>`
    expect(findStepTarget({ title: '', body: '', button: 'Add Resource' })?.id).toBe('submit')
  })

  it('finds a field by label (htmlFor or sibling), placeholder or aria-label', () => {
    document.body.innerHTML = `
      <label for="n">Policy Name</label><input id="n">
      <div><label>Host <span>*</span></label><input id="host"></div>
      <input id="ph" placeholder="Production VPC">
      <select id="role" aria-label="Role"></select>
      <div><label id="lab">Device Profiles</label><div>cards</div></div>`
    expect(findStepTarget({ title: '', body: '', field: 'Policy Name' })?.id).toBe('n')
    expect(findStepTarget({ title: '', body: '', field: 'Host' })?.id).toBe('host')
    expect(findStepTarget({ title: '', body: '', field: 'Production VPC' })?.id).toBe('ph')
    expect(findStepTarget({ title: '', body: '', field: 'Role' })?.id).toBe('role')
    // A label without a control: the label itself.
    expect(findStepTarget({ title: '', body: '', field: 'Device Profiles' })?.id).toBe('lab')
  })

  it('never matches the tour popover itself', () => {
    document.body.innerHTML = `<div data-guided-tour><button>Next</button></div>`
    expect(findStepTarget({ title: '', body: '', button: 'Next' })).toBeNull()
  })
})
