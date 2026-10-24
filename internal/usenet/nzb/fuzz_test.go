package nzb_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

func FuzzParse(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.nzb"))
	if err != nil {
		f.Fatal(err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(nFiles(3)))
	f.Add([]byte(billionLaughsDTD + `<nzb>` + validBody + `</nzb>`))
	f.Add([]byte(oneFile(`<segment number="2">&lt;b@x&gt;</segment><segment number="2">c@x</segment><segment number="1">a b</segment>`)))

	// Small limits so the fuzzer can reach every cap.
	lim := nzb.Limits{MaxBytes: 1 << 16, MaxFiles: 20, MaxSegments: 100}

	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := nzb.Parse(bytes.NewReader(data), lim)
		if err != nil {
			if n != nil {
				t.Fatalf("non-nil NZB with error %v", err)
			}
			if !isKnownError(err) {
				t.Fatalf("error of unknown kind: %v", err)
			}
			return
		}
		if len(n.Files) == 0 {
			t.Fatal("success without files")
		}
		for _, f := range n.Files {
			if len(f.Segments) == 0 {
				t.Fatalf("file %q has no segments", f.Subject)
			}
			prev := 0
			for _, s := range f.Segments {
				if s.Number <= prev {
					t.Fatalf("file %q: segment numbers not strictly increasing: %d after %d", f.Subject, s.Number, prev)
				}
				prev = s.Number
				if !wellFormedMessageID(s.MessageID) {
					t.Fatalf("invalid message id %q", s.MessageID)
				}
				if s.Bytes < 0 {
					t.Fatalf("negative bytes %d", s.Bytes)
				}
			}
		}
	})
}

func isKnownError(err error) bool {
	for _, e := range []error{nzb.ErrMalformed, nzb.ErrTooLarge, nzb.ErrTooManyFiles, nzb.ErrTooManySegments, nzb.ErrNoFiles} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// wellFormedMessageID is the test's own statement of a valid message ID:
// 1..250 bytes of printable ASCII with an '@', angle brackets removed.
func wellFormedMessageID(id string) bool {
	if len(id) == 0 || len(id) > 250 || !strings.Contains(id, "@") {
		return false
	}
	if strings.HasPrefix(id, "<") || strings.HasSuffix(id, ">") {
		return false
	}
	for _, c := range []byte(id) {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}
