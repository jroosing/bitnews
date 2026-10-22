//go:build !unix

package tool

import "os/exec"

func setProcessGroup(*exec.Cmd) {}
