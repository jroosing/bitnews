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

// ADR-0007: the only subprocesses are the post-processing tools, and the tool
// runner (internal/postproc/tool) is the only place that starts them. This
// scans every non-test Go file under internal/ for an import of os/exec and
// allows exactly that package; a second place that imports it is a failure.
// Test files are exempt: they may start tools to build or compare archives,
// and they do not ship in the product.
func TestOnlyTheToolRunnerImportsOsExec(t *testing.T) {
	root := filepath.Join("..", "..") // internal/
	allowed := filepath.Join(root, "postproc", "tool")
	fset := token.NewFileSet()
	scanned := 0
	importers := 0
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
			if path != "os/exec" && !strings.HasSuffix(path, "/os/exec") {
				continue
			}
			importers++
			// The runner's own files, but not other packages that merely sit
			// below it: tooltest is test support and is allowed too.
			if filepath.Dir(p) != allowed && filepath.Dir(p) != filepath.Join(allowed, "tooltest") {
				t.Errorf("%s imports %s; only internal/postproc/tool may start subprocesses (ADR-0007)", p, path)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, scanned, 10, "the scan must actually find the Go files under internal/")
	require.Positive(t, importers, "the tool runner must be found, or the scan is looking in the wrong place")
}
