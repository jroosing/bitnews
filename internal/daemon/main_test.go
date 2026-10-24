package daemon_test

import (
	"os"
	"testing"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/postproc/tool/tooltest"
)

// TestMain makes password hashing cheap for the whole test binary. Every test
// that signs in would otherwise run argon2 with its production 64 MiB working
// set, which made this package's tests take minutes under the race detector.
func TestMain(m *testing.M) {
	// The daemon looks up 7zz and par2 on PATH at startup (ADR-0007).
	tooltest.UsePinned()
	restore := auth.SetParamsForTesting(auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	code := m.Run()
	restore()
	os.Exit(code)
}
