package nzb_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

func TestOnlyRecoveryVolumesAreDeferred(t *testing.T) {
	assert.False(t, nzb.File{Kind: nzb.KindData}.Deferred())
	assert.False(t, nzb.File{Kind: nzb.KindPar2Index}.Deferred(), "the index is needed to verify")
	assert.True(t, nzb.File{Kind: nzb.KindPar2Volume}.Deferred())
}

func TestUpFrontBytesLeavesOutDeferredFilesAndSaturates(t *testing.T) {
	seg := func(b int64) []nzb.Segment { return []nzb.Segment{{Number: 1, Bytes: b}} }
	n := &nzb.NZB{Files: []nzb.File{
		{Kind: nzb.KindData, Segments: seg(1000)},
		{Kind: nzb.KindPar2Index, Segments: seg(10)},
		{Kind: nzb.KindPar2Volume, Segments: seg(500)},
	}}
	assert.Equal(t, int64(1010), nzb.UpFrontBytes(n))

	n.Files = append(n.Files, nzb.File{Kind: nzb.KindData, Segments: seg(math.MaxInt64)})
	assert.Equal(t, int64(math.MaxInt64), nzb.UpFrontBytes(n))
}
