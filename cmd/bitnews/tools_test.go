package main

import "github.com/jroosing/bitnews/internal/postproc/tool/tooltest"

// The daemon looks up 7zz and par2 on PATH at startup (ADR-0007); the tests
// that start it use the pinned ones from bin/tools.
func init() { tooltest.UsePinned() }
