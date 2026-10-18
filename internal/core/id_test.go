package core_test

import (
	"encoding/base32"
	"strings"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestNewIDIsLowercaseUnpaddedBase32Of128Bits(t *testing.T) {
	id := core.NewID()

	s := string(id)
	if s != strings.ToLower(s) {
		t.Fatalf("ID %q is not lowercase", s)
	}
	if strings.Contains(s, "=") {
		t.Fatalf("ID %q is padded", s)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
	if err != nil {
		t.Fatalf("ID %q is not base32: %v", s, err)
	}
	if len(raw) != 16 {
		t.Fatalf("ID %q decodes to %d bytes, want 16", s, len(raw))
	}
}

func TestNewIDsAreUnique(t *testing.T) {
	seen := make(map[core.ID]bool)
	for range 1000 {
		id := core.NewID()
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}
