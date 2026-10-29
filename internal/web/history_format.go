package web

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// whenView is a point in time as the templates show it: a relative text, the
// absolute time for the title attribute and the machine-readable form.
type whenView struct {
	Valid bool
	Rel   string
	Abs   string
	ISO   string
}

// histWhen describes t relative to now. A zero t is not valid.
func histWhen(now, t time.Time) whenView {
	if t.IsZero() {
		return whenView{}
	}
	return whenView{
		Valid: true,
		Rel:   histRelative(now, t),
		Abs:   t.UTC().Format("2006-01-02 15:04:05 UTC"),
		ISO:   t.UTC().Format(time.RFC3339),
	}
}

func histRelative(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = -d
		return "in " + histSpan(d)
	}
	if d < 45*time.Second {
		return "just now"
	}
	return histSpan(d) + " ago"
}

// histSpan is a coarse duration: "5 minutes", "3 hours", "2 days".
func histSpan(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Minute:
		return plural(max(int(d/time.Second), 1), "second")
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 60*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d < 730*24*time.Hour:
		return plural(int(d/(30*24*time.Hour)), "month")
	}
	return plural(int(d/(365*24*time.Hour)), "year")
}

// failureInfo is how a FailureReason reads to a person.
type failureInfo struct {
	// Title is the short headline ("Not enough repair data").
	Title string
	// Text explains what happened.
	Text string
	// Hint says what to do about it.
	Hint string
}

// failureInfos holds the wording for every FailureReason. The headlines
// follow the SABnzbd adapter's fail messages, which media managers show; the
// hints are plain language for people.
var failureInfos = map[core.FailureReason]failureInfo{
	core.FailureMissingArticles: {
		Title: "Articles are missing",
		Text:  "Parts of this release are no longer on your Usenet servers, and there is no repair data to recover them.",
		Hint:  "The release was probably taken down or is older than your provider keeps. Sonarr and Radarr will usually grab another release.",
	},
	core.FailureRepairInsufficient: {
		Title: "Not enough repair data",
		Text:  "The download is damaged and the PAR2 recovery data does not cover all of the damage.",
		Hint:  "Not enough recovery data to repair it. Sonarr and Radarr will usually grab another release.",
	},
	core.FailureArchiveUnsupported: {
		Title: "Unsupported archive format",
		Text:  "bitnews could not unpack this archive because it uses a format it does not support.",
		Hint:  "Try another release of the same title.",
	},
	core.FailureArchivePassword: {
		Title: "Archive needs a password",
		Text:  "The archive is password protected and the password was missing or wrong.",
		Hint:  "Add the download again with the right archive password, or try another release.",
	},
	core.FailureArchiveCorrupt: {
		Title: "Archive is damaged",
		Text:  "The archive is damaged in a way unpacking cannot get past.",
		Hint:  "Try another release of the same title.",
	},
	core.FailureDiskFull: {
		Title: "Not enough disk space",
		Text:  "There was not enough free space to continue.",
		Hint:  "Free up space on the download drive, then add the download again.",
	},
	core.FailurePathViolation: {
		Title: "Unsafe file path",
		Text:  "The download contains a file name that would write outside its own folder, so bitnews refused it.",
		Hint:  "Do not use this release. Try another one.",
	},
	core.FailureSourceInvalid: {
		Title: "The source cannot be used",
		Text:  "The NZB or torrent is damaged or does not describe a usable download.",
		Hint:  "Get the NZB or torrent again, or try another release.",
	},
	core.FailureInternal: {
		Title: "Internal error",
		Text:  "bitnews ran into an unexpected problem.",
		Hint:  "Details are in the server log. Please report it if it keeps happening.",
	},
}

// failureOf returns the wording for reason; an unknown or empty reason gets a
// generic one, so a new reason never renders an empty card.
func failureOf(reason core.FailureReason) failureInfo {
	if fi, ok := failureInfos[reason]; ok {
		return fi
	}
	return failureInfo{
		Title: "Download failed",
		Text:  "The download failed.",
		Hint:  "Details may be in the server log.",
	}
}

// failureDetail is the failure detail of a View as the UI shows it. The
// view module masks URLs, control characters and the length
// (view.CleanDetail); histClean also drops invisible format characters such
// as bidi overrides, which would reorder the text around it.
func failureDetail(v view.View) string {
	if v.Failure == nil {
		return ""
	}
	return histClean(v.Failure.Detail, 1024)
}

// histTruncate cuts s to at most max bytes on a rune boundary and appends an
// ellipsis when it cut.
func histTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !validRuneStart(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return strings.ToValidUTF8(s, "") + "..."
}

func validRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// badgeOf is the badge of a Download as the queue, the history and the
// detail page show it: the badge class and its word. The word is the
// Phase's, except for a Fetched Download, which says so (a torrent is
// Seeding then) rather than naming the first Post-processing step.
func badgeOf(v view.View) (class, label string) {
	d := v.Download
	if d.State == core.StateFetched {
		switch {
		case d.Paused:
			return "paused", view.PhasePaused.Label()
		case d.SourceKind == core.SourceTorrent:
			return "ready", "Seeding"
		}
		return "processing", "Fetched"
	}
	label = v.Phase.Label()
	switch v.Phase {
	case view.PhasePaused:
		return "paused", label
	case view.PhaseMetadata, view.PhaseDownloading:
		return "downloading", label
	case view.PhaseVerifying, view.PhaseRepairing, view.PhaseUnpacking, view.PhaseFinishing:
		return "processing", label
	case view.PhaseReady:
		return "ready", label
	case view.PhaseFailed:
		return "failed", label
	}
	return "queued", label
}

// kindLabel is the text and icon of a Source kind.
// sourceLabel names the Source of a Download the way the glossary does, with
// the icon that goes with it.
func sourceLabel(k core.SourceKind) (label, icon string) {
	if k == core.SourceTorrent {
		return "Torrent", "bolt"
	}
	return "NZB", "server"
}

func terminal(st core.State) bool { return st == core.StateReady || st == core.StateFailed }

// histClean makes untrusted text (names, paths) safe to show: invalid UTF-8
// is dropped, control characters and invisible format characters (such as
// bidi overrides) are removed, white space is collapsed and the length is
// capped. Templates still escape the result.
func histClean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return histTruncate(strings.Join(strings.Fields(s), " "), max)
}
