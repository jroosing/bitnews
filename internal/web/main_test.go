package web_test

import (
	"os"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/auth"
)

// TestMain makes password hashing cheap for the whole test binary. Every test
// that signs in would otherwise run argon2 with its production 64 MiB working
// set, which made this package's tests take minutes under the race detector.
func TestMain(m *testing.M) {
	restore := auth.SetParamsForTesting(auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	code := m.Run()
	restore()
	os.Exit(code)
}
