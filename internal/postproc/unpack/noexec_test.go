package unpack

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ADR-0002: post-processing never starts a subprocess. This scans every
// non-test Go file under internal/ for an import of os/exec. Test files are
// exempt: the PAR2 golden test compares against par2cmdline,
// and it does not ship in the product.
func TestNothingUnderInternalImportsOsExec(t *testing.T) {
	root := filepath.Join("..", "..") // internal/
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "os/exec" || strings.HasSuffix(path, "/os/exec") {
				t.Errorf("%s imports %s; subprocesses are forbidden (ADR-0002)", p, path)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, scanned, 10, "the scan must actually find the Go files under internal/")
}
