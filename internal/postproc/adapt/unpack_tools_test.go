package adapt

import (
	"os"
	"os/exec"
	"path/filepath"
)

// The unpack tests start the real 7zz. `make test` puts bin/tools on PATH; for
// a plain `go test` this finds the directory that `make tools` fills.
func init() {
	if _, err := exec.LookPath("7zz"); err == nil {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			tools := filepath.Join(dir, "bin", "tools")
			if _, err := os.Stat(filepath.Join(tools, "7zz")); err == nil {
				_ = os.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}
