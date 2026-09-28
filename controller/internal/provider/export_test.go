package provider

// Test-only exports for the external provider_test package (end-to-end tests
// that wire the real middleware, which imports this package).
var NewTestStore = newTestStore
