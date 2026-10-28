package daemon

import (
	"errors"
	"fmt"

	"github.com/jroosing/bitnews/internal/postproc/par2"
	"github.com/jroosing/bitnews/internal/postproc/tool"
)

// postprocTools are the programs Post-processing runs (ADR-0007), found once
// at startup so a missing one stops the daemon with a clear message instead of
// failing the first Download.
type postprocTools struct {
	sevenZip string
	par2     par2.Repairer
}

// locateTools finds 7zz and par2 on PATH. The error names every missing
// program.
func locateTools() (postprocTools, error) {
	var t postprocTools
	var errs []error
	if p, err := tool.Locate("7zz"); err != nil {
		errs = append(errs, err)
	} else {
		t.sevenZip = p
	}
	if p, err := tool.Locate("par2"); err != nil {
		errs = append(errs, err)
	} else {
		t.par2 = par2.Repairer{Path: p}
	}
	if len(errs) > 0 {
		return postprocTools{}, fmt.Errorf("daemon: post-processing tools are missing: %w", errors.Join(errs...))
	}
	return t, nil
}
