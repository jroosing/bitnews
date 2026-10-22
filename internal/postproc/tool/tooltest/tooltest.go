// Package tooltest finds the pinned post-processing tools for tests.
package tooltest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Path returns the path of the program (7zz or par2) for a test: from PATH,
// else from bin/tools at the module root (what `make tools` fills). A missing
// program fails the test instead of skipping it, so CI cannot pass without the
// tools being exercised.
func Path(t testing.TB, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if root := moduleRoot(); root != "" {
		p := filepath.Join(root, "bin", "tools", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	t.Fatalf("%s not found: run `make tools` (or put it on PATH)", name)
	return ""
}

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// UsePinned puts bin/tools at the module root first on PATH, when it exists,
// so a test that starts the daemon (which looks the tools up on PATH) finds
// the pinned versions. Call it from TestMain.
func UsePinned() {
	root := moduleRoot()
	if root == "" {
		return
	}
	dir := filepath.Join(root, "bin", "tools")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
}
