package blackbox_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jroosing/bitnews/internal/postproc/tool/tooltest"
)

// Stamps given to the binary the suite builds itself, so the version routes
// can be checked exactly. A binary from BITNEWS_E2E_BINARY has its own.
const (
	builtVersion = "blackbox-test"
	builtCommit  = "0123abc"
)

// binaryEnv names the variable that selects the binary under test.
const binaryEnv = "BITNEWS_E2E_BINARY"

// binary is the daemon under test.
var binary struct {
	// path is the absolute path of the executable.
	path string
	// isGo is true when the suite built the Go daemon itself.
	isGo bool
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	// The daemon looks up 7zz and par2 on PATH at startup (ADR-0007).
	tooltest.UsePinned()

	if p := os.Getenv(binaryEnv); p != "" {
		if !filepath.IsAbs(p) {
			fmt.Fprintf(os.Stderr, "blackbox: %s=%q must be an absolute path\n", binaryEnv, p)
			return 2
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			fmt.Fprintf(os.Stderr, "blackbox: %s=%q is not a file\n", binaryEnv, p)
			return 2
		}
		binary.path = p
		return m.Run()
	}

	dir, err := os.MkdirTemp("", "bitnews-blackbox-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "bitnews")
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox: no go command on PATH to build the Go daemon (or set "+binaryEnv+"):", err)
		return 2
	}
	build := exec.Command(goTool, "build", "-o", out,
		"-ldflags", "-X main.version="+builtVersion+" -X main.commit="+builtCommit,
		"github.com/jroosing/bitnews/cmd/bitnews")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "blackbox: build the Go daemon:", err)
		return 2
	}
	binary.path, binary.isGo = out, true
	return m.Run()
}
