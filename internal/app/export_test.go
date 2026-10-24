package app

import (
	"context"
	"time"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
)

// ProbeConn is the part of an NNTP connection a probe uses.
type ProbeConn = probeConn

// NewTestProber returns an NNTPProber whose dial and clock are replaced.
func NewTestProber(dial func(context.Context, nntp.Config) (ProbeConn, error), now func() time.Time) *NNTPProber {
	p := NewNNTPProber()
	p.dial, p.now = dial, now
	return p
}
