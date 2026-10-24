package scheduler

import (
	"context"
	"errors"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
	"github.com/jroosing/bitnews/internal/usenet/yenc"
)

// outcome classifies one fetch.
type outcome int

const (
	outValid     outcome = iota // Part decoded and CRC matches
	outDamaged                  // Part decoded, CRC mismatch
	outAbsent                   // Server does not have it (or sent garbage)
	outTransient                // worth retrying on this Server
	outDead                     // Server unusable for the whole run
	outCancelled                // ctx ended
)

// fetch asks sv for Article i once, decoding the body as it streams in.
func (s *Scheduler) fetch(ctx context.Context, sv *srv, id string) (yenc.Part, outcome, error) {
	var dec yenc.Decoder
	var decErr error
	var wire int64
	sv.requests.Add(1)
	err := sv.cfg.Pool.Body(ctx, id, func(line []byte) error {
		wire += int64(len(line)) + 2
		if decErr = dec.Write(line); decErr != nil {
			return decErr
		}
		return nil
	})
	sv.bytes.Add(wire)
	if ctx.Err() != nil {
		return yenc.Part{}, outCancelled, ctx.Err()
	}
	switch {
	case decErr != nil:
		return yenc.Part{}, outAbsent, decErr
	case err == nil:
		part, ferr := dec.Finish()
		if ferr != nil {
			return yenc.Part{}, outAbsent, ferr
		}
		// Only a CRC that is announced and wrong makes a part damaged; posts
		// without one are legitimate (same rule as the file writer).
		if !part.HasExpectedCRC || part.CRCMatch() {
			return part, outValid, nil
		}
		return part, outDamaged, errCRC
	case errors.Is(err, nntp.ErrArticleNotFound), errors.Is(err, nntp.ErrInvalidMessageID):
		return yenc.Part{}, outAbsent, err
	case errors.Is(err, nntp.ErrServerDisabled), errors.Is(err, nntp.ErrAuth), errors.Is(err, nntp.ErrPoolClosed):
		return yenc.Part{}, outDead, err
	default:
		return yenc.Part{}, outTransient, err
	}
}

var errCRC = errors.New("scheduler: part failed its CRC check")
