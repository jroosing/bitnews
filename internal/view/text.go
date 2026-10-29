package view

import (
	"strings"
	"unicode"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/redact"
)

// maxDetailBytes caps CleanDetail.
const maxDetailBytes = 300

// CleanDetail makes a failure detail safe to show: URLs are masked, control
// characters become spaces, invisible format characters (bidi overrides) go, white space is collapsed and the length is
// capped at maxDetailBytes on a character boundary (then "..." is added).
func CleanDetail(s string) string {
	s = redact.URLsIn(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1 // invisible format characters such as bidi overrides
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes]
		for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
			s = s[:len(s)-1]
		}
		s = strings.ToValidUTF8(s, "") + "..."
	}
	return s
}

// Label is the word the UI shows for a phase.
func (p Phase) Label() string {
	switch p {
	case PhaseWaiting:
		return "Queued"
	case PhaseMetadata:
		return "Fetching metadata"
	case PhasePaused:
		return "Paused"
	case PhaseDownloading:
		return "Downloading"
	case PhaseVerifying:
		return "Verifying"
	case PhaseRepairing:
		return "Repairing"
	case PhaseUnpacking:
		return "Unpacking"
	case PhaseFinishing:
		return "Finishing"
	case PhaseReady:
		return "Ready"
	case PhaseFailed:
		return "Failed"
	}
	return "Queued"
}

// PriorityLabel is the word for a Download Priority; an unknown or empty one
// is Normal.
func PriorityLabel(p core.DownloadPriority) string {
	switch p {
	case core.DownloadPriorityForce:
		return "Force"
	case core.DownloadPriorityHigh:
		return "High"
	case core.DownloadPriorityLow:
		return "Low"
	}
	return "Normal"
}
