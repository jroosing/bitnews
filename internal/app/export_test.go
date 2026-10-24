package app

import (
	"context"
	"io"
	"time"

	"github.com/jroosing/bitnews/internal/core"
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

// The staged steps behind AddService.Receive and Add, for the tests that
// exercise one step at a time.

func (s *AddService) Stage(ctx context.Context, r io.Reader) (*StagedNZB, error) {
	return s.stage(ctx, r)
}
func (s *AddService) StageTorrent(ctx context.Context, r io.Reader) (*StagedTorrent, error) {
	return s.stageTorrent(ctx, r)
}
func (s *AddService) StageMagnet(ctx context.Context, uri string) (*StagedTorrent, error) {
	return s.stageMagnet(ctx, uri)
}
func (s *AddService) Fetch(ctx context.Context, u string) (FetchedSource, error) {
	return s.fetchAny(ctx, u)
}
func (s *AddService) FetchNZB(ctx context.Context, u string) (*StagedNZB, error) {
	return s.fetchNZB(ctx, u)
}
func (s *AddService) Check(ctx context.Context, req AddRequest) error { return s.check(ctx, req) }
func (s *AddService) AddStaged(ctx context.Context, st *StagedNZB, req AddRequest) (core.Download, error) {
	return s.addNZB(ctx, st, req)
}
func (s *AddService) AddTorrent(ctx context.Context, st *StagedTorrent, req AddRequest) (core.Download, error) {
	return s.addTorrent(ctx, st, req)
}
