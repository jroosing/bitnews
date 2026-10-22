//go:build unix

package unpack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A stand-in for 7zz: a shell script that prints a canned listing for "l" and
// runs a snippet for "x". It lets the tests make "7zz" do what a buggy or
// compromised one could (plant a symlink, write too much, hang, fail with a
// given status and message) without needing an archive that provokes it. The
// snippet sees $out, the directory after -o.

// listingFor renders entries as 7zz would list them.
func listingFor(ents ...[3]string) string {
	var b strings.Builder
	for _, e := range ents { // name, size, attributes
		fmt.Fprintf(&b, "Path = %s\nSize = %s\nAttributes = %s\nEncrypted = -\n\n", e[0], e[1], e[2])
	}
	return b.String()
}

func fakeSevenZip(t testing.TB, listOutput, extractSnippet string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
cmd=$1
out=
for a in "$@"; do
  case "$a" in -o*) out=${a#-o};; esac
done
case "$cmd" in
l)
cat <<'LISTING_EOF'
%s
LISTING_EOF
exit 0 ;;
x)
%s
exit 0 ;;
esac
exit 7
`, listOutput, extractSnippet)
	p := filepath.Join(t.TempDir(), "fake7zz")
	require.NoError(t, os.WriteFile(p, []byte(script), 0o700))
	return p
}

// fakeSet is a set whose single volume exists (the fake never reads it).
func fakeSet(t testing.TB) ArchiveSet {
	t.Helper()
	return oneSet(writeFile(t, t.TempDir(), "x.7z", []byte("not read")))
}

func unpackWithFake(t testing.TB, fake string, j jail, opts ...Option) (Result, error) {
	t.Helper()
	return Unpack(t.Context(), fakeSet(t), j.dst, append([]Option{WithSevenZip(fake), WithLogger(nil)}, opts...)...)
}
