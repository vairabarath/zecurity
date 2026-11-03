package providerquery

// Test helpers exported to the external providerquery_test package, which
// exists because a test importing internal/relay (relay → provider →
// providerquery) can't live in the internal test package without an import
// cycle. Compiled only into tests.
var (
	NewTestDB = newTestDB
	MustExec  = mustExec
	SeedRelay = seedRelay
	TestT0    = t0
)
