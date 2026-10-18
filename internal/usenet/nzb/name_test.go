package nzb_test

import (
	"html"
	"testing"
)

func escape(s string) string { return html.EscapeString(s) }

func nameOf(t *testing.T, subject string) string {
	t.Helper()
	doc := `<nzb><file subject="` + escape(subject) + `"><segments><segment number="1">a@x</segment></segments></file></nzb>`
	return parseString(t, doc).Files[0].Name
}

func TestParse_NameFromFirstQuotedSubstring(t *testing.T) {
	cases := map[string]string{
		`[01/10] - "Some.Show.S01E01.part01.rar" yEnc (1/50)`:        "Some.Show.S01E01.part01.rar",
		`"first.nfo" and "second.rar"`:                               "first.nfo",
		`[3/7] - "a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4" yEnc (1/1)`:      "a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4",
		`Release [1/2] - "../../etc/passwd" yEnc (1/1)`:              "../../etc/passwd", // sanitizing is fsutil's job
		`Some Poster - "with spaces inside.mkv" yEnc (01/99) 1.4 GB`: "with spaces inside.mkv",
	}
	for subject, want := range cases {
		if got := nameOf(t, subject); got != want {
			t.Errorf("subject %q: name = %q, want %q", subject, got, want)
		}
	}
}

func TestParse_NameFallsBackToTrimmedSubject(t *testing.T) {
	cases := map[string]string{
		`  a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4  `: "a8f3e9c2b1d4e5f6a7b8c9d0e1f2a3b4",
		`unterminated "quote`:                  `unterminated "quote`,
		`empty "" quotes`:                      `empty "" quotes`,
	}
	for subject, want := range cases {
		if got := nameOf(t, subject); got != want {
			t.Errorf("subject %q: name = %q, want %q", subject, got, want)
		}
	}
}

func TestParse_NameFromLastFileLikeTokenWhenUnquoted(t *testing.T) {
	cases := map[string]string{
		`Some.Show.S01E01.720p.mkv yEnc (1/50)`:                                  "Some.Show.S01E01.720p.mkv",
		`[1/3] - movie.part01.rar - 1.2 GB yEnc (1/5)`:                           "movie.part01.rar",
		`(movie.nfo) yEnc (1/1)`:                                                 "movie.nfo",
		`[archive.7z.001] yEnc (1/1)`:                                            "archive.7z.001",
		`first.txt then second.r00 yEnc (1/1)`:                                   "second.r00",
		`Some.Release.Name [01/24] - Some.Release.Name.vol00+01.par2 yEnc (1/1)`: "Some.Release.Name.vol00+01.par2",
	}
	for subject, want := range cases {
		if got := nameOf(t, subject); got != want {
			t.Errorf("subject %q: name = %q, want %q", subject, got, want)
		}
	}
}
