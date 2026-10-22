package auth

import "testing"

// SetParamsForTesting lowers the argon2 cost HashPassword uses so that other
// packages' tests, which log in many times with a production-cost hash of 64 MiB
// each, stay fast. It returns a function that restores the previous parameters.
//
// It panics unless the running binary is a test binary (testing.Testing), so a
// production binary can never weaken password hashing through it.
func SetParamsForTesting(p Params) (restore func()) { return swapParams(testing.Testing(), p) }

func swapParams(isTestBinary bool, p Params) (restore func()) {
	if !isTestBinary {
		panic("auth: password hashing parameters can only be changed in a test binary")
	}
	old := currentParams
	currentParams = p
	return func() { currentParams = old }
}
