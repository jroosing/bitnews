package auth

import "testing"

// UseFastParams swaps the argon2 cost for the cheapest valid one so tests
// stay quick. It restores the production parameters when t ends.
func UseFastParams(t *testing.T) {
	t.Helper()
	old := currentParams
	currentParams = Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	t.Cleanup(func() { currentParams = old })
}

// SetParamsForTest sets the parameters HashPassword uses until t ends.
func SetParamsForTest(t *testing.T, p Params) {
	t.Helper()
	old := currentParams
	currentParams = p
	t.Cleanup(func() { currentParams = old })
}
