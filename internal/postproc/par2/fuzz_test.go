package par2_test

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func fuzzSeeds(f *testing.F) {
	set := newSet(16, testFiles(40, 5, 100)...)
	index := set.Index()
	vol := set.Volume(0, 1, 2)
	f.Add([]byte(nil))
	f.Add([]byte("PAR2\x00PKT"))
	f.Add(index)
	f.Add(vol)
	f.Add(concat(index, vol))
	f.Add(index[:len(index)-7])
	f.Add(concat([]byte("garbage"), index, []byte("PAR2\x00PKT\xff\xff\xff\xff\xff\xff\xff\xff"), vol))
	flipped := bytes.Clone(index)
	flipped[100] ^= 0x10
	f.Add(flipped)
	huge := rawPacket(set.SetID, typeMain, binary.LittleEndian.AppendUint32(make([]byte, 8), 0xFFFFFFFF))
	f.Add(huge)
	big := rawPacket(set.SetID, typeFileDesc, nil)
	binary.LittleEndian.PutUint64(big[8:16], 1<<30)
	f.Add(big)
}

// FuzzScan checks that arbitrary bytes never panic or hang the scanner, that
// memory stays bounded, and that every packet it yields really verifies.
func FuzzScan(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		last := int64(0)
		for p, err := range par2.Scan(bytes.NewReader(data), int64(len(data))) {
			require.NoError(t, err)
			require.GreaterOrEqual(t, p.Offset, last, "packets must not overlap")
			require.GreaterOrEqual(t, p.Length, int64(64))
			require.Zero(t, p.Length%4)
			require.LessOrEqual(t, p.Offset+p.Length, int64(len(data)))
			last = p.Offset + p.Length

			raw := data[p.Offset : p.Offset+p.Length]
			require.Equal(t, "PAR2\x00PKT", string(raw[:8]))
			require.Equal(t, uint64(p.Length), binary.LittleEndian.Uint64(raw[8:16]))
			sum := md5.Sum(raw[32:])
			require.Equal(t, sum[:], raw[16:32], "yielded packet must have a valid MD5")
			require.Equal(t, p.SetID[:], raw[32:48])

			switch b := p.Body.(type) {
			case *par2.Main:
				require.LessOrEqual(t, len(b.RecoveryIDs)+len(b.NonRecoveryIDs), par2.MaxFiles)
			case *par2.FileDesc:
				require.LessOrEqual(t, len(b.Name), par2.MaxNameLen)
			case *par2.IFSC:
				require.LessOrEqual(t, len(b.Slices), par2.MaxSlicesPerFile)
			case *par2.RecvSlic:
				require.GreaterOrEqual(t, b.DataOffset, p.Offset+64)
				require.LessOrEqual(t, b.DataOffset+b.DataLength, p.Offset+p.Length)
			case *par2.Creator:
				require.LessOrEqual(t, len(b.Text), par2.MaxNameLen)
			default:
				t.Fatalf("unexpected body %T", p.Body)
			}
		}

		runtime.ReadMemStats(&after)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(48<<20)+16*uint64(len(data)))
	})
}

// FuzzLoadFiles feeds arbitrary bytes through the whole Set pipeline.
func FuzzLoadFiles(f *testing.F) {
	fuzzSeeds(f)
	dir := f.TempDir()
	path := filepath.Join(dir, "fuzz.par2")
	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(path, data, 0o600))

		s, err := par2.LoadFiles([]string{path})

		if err != nil {
			require.Nil(t, s)
			return
		}
		require.NotZero(t, s.SliceSize)
		require.Zero(t, s.SliceSize%4)
		require.LessOrEqual(t, len(s.Warnings), 101)
		for id, ifsc := range s.IFSCs {
			require.Contains(t, s.FileDescs, id)
			require.Equal(t, s.SliceCount(s.FileDescs[id].Length), uint64(len(ifsc.Slices)))
		}
		for _, rs := range s.Recovery {
			require.Equal(t, int64(s.SliceSize), rs.Length)
		}
	})
}
