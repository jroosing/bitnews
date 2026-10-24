package nzb

import (
	"math"
	"strconv"
	"time"
)

// NZB is a parsed, normalized NZB document.
type NZB struct {
	Meta  Meta
	Files []File

	// SkippedSegments counts segments dropped because their number or
	// message ID was invalid. Duplicates are not counted.
	SkippedSegments int
}

// Meta holds the optional values from the NZB <head>. Empty means absent.
type Meta struct {
	Name     string
	Password string
	Category string
}

// File is one file posted to Usenet, split into Segments.
type File struct {
	Subject  string
	Poster   string
	Date     time.Time // posting time from the unix "date" attribute; zero when absent
	Groups   []string
	Segments []Segment

	// Name is a provisional file name guessed from Subject. It is not
	// sanitized; the yEnc name= header is authoritative once fetched.
	Name string

	// Kind classifies the file by Name, so PAR2 recovery volumes can be
	// deferred until Repair needs them.
	Kind Kind

	// RecoveryBlocks is the MM of a "name.volNN+MM.par2" volume: the number
	// of recovery blocks it claims to hold. It is 0 for other kinds, and also
	// for a volume whose MM does not fit in an int.
	RecoveryBlocks int
}

// Deferred reports whether f is left out of a Download's first fetch: PAR2
// recovery volumes are fetched only when Repair needs their blocks (M3-07).
// This is the one place that decides it.
func (f File) Deferred() bool { return f.Kind == KindPar2Volume }

// UpFrontBytes is the size of the Articles a Download fetches before any
// Repair: every file that is not Deferred. It saturates instead of
// overflowing.
func UpFrontBytes(n *NZB) int64 {
	var total int64
	for _, f := range n.Files {
		if f.Deferred() {
			continue
		}
		for _, seg := range f.Segments {
			if seg.Bytes > 0 && total > math.MaxInt64-seg.Bytes {
				return math.MaxInt64
			}
			total += max(seg.Bytes, 0)
		}
	}
	return total
}

// Kind is the role of a File, guessed from its name.
type Kind int

const (
	// KindData is any file that is not PAR2, including obfuscated names.
	KindData Kind = iota
	// KindPar2Index is a PAR2 file without recovery blocks ("name.par2").
	KindPar2Index
	// KindPar2Volume is a PAR2 recovery volume ("name.volNN+MM.par2").
	KindPar2Volume
)

func (k Kind) String() string {
	switch k {
	case KindData:
		return "data"
	case KindPar2Index:
		return "par2-index"
	case KindPar2Volume:
		return "par2-volume"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Segment points at one Article carrying part of a File.
type Segment struct {
	Number    int
	Bytes     int64
	MessageID string
}
