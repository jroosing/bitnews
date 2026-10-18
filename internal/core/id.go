package core

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// ID identifies a Download, Category, user or API key. It is 128 random
// bits, base32-encoded in lowercase without padding: stable and opaque.
type ID string

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewID returns a fresh random ID.
func NewID() ID {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error.
	return ID(strings.ToLower(idEncoding.EncodeToString(b[:])))
}
