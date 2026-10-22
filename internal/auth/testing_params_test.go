package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetParamsForTestingLowersTheCostAndRestoresIt(t *testing.T) {
	restore := SetParamsForTesting(Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})

	cheap, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.Contains(t, string(cheap), "m=8,t=1,p=1")

	restore()
	assert.Equal(t, DefaultParams, currentParams)
}

func TestParamsCannotBeLoweredOutsideATestBinary(t *testing.T) {
	// A production binary must never be able to weaken password hashing.
	assert.Panics(t, func() { swapParams(false, Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}) })
	assert.Equal(t, DefaultParams, currentParams)
	assert.False(t, strings.Contains(string(mustHash(t)), "m=8,"), "defaults still in effect")
}

func mustHash(t *testing.T) []byte {
	t.Helper()
	h, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	return h
}
