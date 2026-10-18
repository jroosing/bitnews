package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlagPrintsVersionAndCommit(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	var out bytes.Buffer

	code := run([]string{"--version"}, &out)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := out.String()
	for _, want := range []string{"bitnews", "1.2.3", "abc1234"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}
