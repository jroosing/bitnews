package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleBlob() resumeBlob {
	return resumeBlob{
		Writer: []byte("writer-snapshot"),
		Segments: []segRecord{
			{Key: segKey{File: 0, Seg: 1}, Status: segDone},
			{Key: segKey{File: 0, Seg: 3}, Status: segMissing},
			{Key: segKey{File: 12, Seg: 99}, Status: segDamaged},
		},
	}
}

func TestResumeBlobRoundTrips(t *testing.T) {
	want := sampleBlob()

	got, err := decodeBlob(encodeBlob(want))

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestResumeBlobRejectsCorruption(t *testing.T) {
	good := encodeBlob(sampleBlob())
	flipped := append([]byte(nil), good...)
	flipped[len(flipped)/2] ^= 0x40
	wrongVersion := append([]byte(nil), good...)
	wrongVersion[len(blobMagic)] = 9

	for name, data := range map[string][]byte{
		"empty":         nil,
		"truncated":     good[:len(good)-3],
		"bit flip":      flipped,
		"wrong version": wrongVersion,
		"trailing byte": append(append([]byte(nil), good...), 0),
	} {
		_, err := decodeBlob(data)
		assert.ErrorIs(t, err, errBadBlob, name)
	}
}

func FuzzDecodeBlob(f *testing.F) {
	f.Add(encodeBlob(sampleBlob()))
	f.Add([]byte("GNRP\x01"))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := decodeBlob(data)
		if err != nil {
			return
		}
		// Whatever decodes must re-encode to something that decodes equal.
		again, err := decodeBlob(encodeBlob(b))
		require.NoError(t, err)
		assert.Equal(t, b, again)
	})
}
