package core_test

import (
	"go/build"
	"strings"
	"testing"
)

// The dependency rule: core imports nothing else from this project and no
// third-party code, only the standard library. Test files may use test
// libraries, so only the package's non-test files are checked.
func TestCoreImportsOnlyTheStandardLibrary(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("no Go files found")
	}
	for _, path := range pkg.Imports {
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") {
			t.Errorf("core imports %q; only the standard library is allowed", path)
		}
	}
}
