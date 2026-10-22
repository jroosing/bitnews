package tool_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tool"
)

// TestMain lets the test binary act as the program under test: started with
// --helper <mode> it behaves as that helper instead of running the tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--helper" {
		helper(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

func helper(args []string) {
	switch args[0] {
	case "echo-env":
		for _, kv := range os.Environ() {
			os.Stdout.WriteString(kv + "\n")
		}
	case "cat":
		b := make([]byte, 1024)
		n, _ := os.Stdin.Read(b)
		os.Stdout.Write(b[:n])
	case "exit":
		os.Exit(7)
	case "flood":
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range 64 {
			os.Stdout.Write(chunk)
		}
	case "sleep":
		time.Sleep(time.Minute)
	}
}

func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	require.NoError(t, err)
	return p
}

func TestRunScrubsTheEnvironment(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "hunter2")

	res, err := tool.Run(context.Background(), tool.Spec{Path: self(t), Args: []string{"--helper", "echo-env"}, TempDir: t.TempDir()})

	require.NoError(t, err)
	out := string(res.Stdout)
	assert.NotContains(t, out, "SECRET_TOKEN")
	assert.Contains(t, out, "LC_ALL=C")
	assert.Contains(t, out, "TMPDIR=")
}

func TestRunFeedsStdin(t *testing.T) {
	res, err := tool.Run(context.Background(), tool.Spec{Path: self(t), Args: []string{"--helper", "cat"}, Stdin: strings.NewReader("pw\n")})

	require.NoError(t, err)
	assert.Equal(t, "pw\n", string(res.Stdout))
}

func TestRunReportsExitCodeWithoutError(t *testing.T) {
	res, err := tool.Run(context.Background(), tool.Spec{Path: self(t), Args: []string{"--helper", "exit"}})

	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitCode)
}

func TestRunCapsOutputAndKeepsDraining(t *testing.T) {
	res, err := tool.Run(context.Background(), tool.Spec{Path: self(t), Args: []string{"--helper", "flood"}, MaxOutput: 1000})

	require.NoError(t, err)
	assert.Len(t, res.Stdout, 1000)
	assert.True(t, res.Truncated)
	assert.Equal(t, 0, res.ExitCode)
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := tool.Run(ctx, tool.Spec{Path: self(t), Args: []string{"--helper", "sleep"}})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestRunTimesOut(t *testing.T) {
	_, err := tool.Run(context.Background(), tool.Spec{Path: self(t), Args: []string{"--helper", "sleep"}, Timeout: 100 * time.Millisecond})

	assert.ErrorIs(t, err, tool.ErrTimeout)
}

func TestRunReportsAMissingProgram(t *testing.T) {
	_, err := tool.Run(context.Background(), tool.Spec{Path: "/nonexistent/prog"})

	require.Error(t, err)
	assert.False(t, errors.Is(err, tool.ErrTimeout))
}

func TestLocateNamesTheMissingProgram(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := tool.Locate("7zz")

	assert.ErrorIs(t, err, tool.ErrNotFound)
	assert.Contains(t, err.Error(), "7zz")
}
