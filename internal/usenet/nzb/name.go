package nzb

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// guessName derives a provisional file name from a subject line: the first
// non-empty double-quoted substring, else the last whitespace-separated
// token that looks like a file name, else the trimmed subject.
func guessName(subject string) string {
	rest := subject
	for {
		open := strings.IndexByte(rest, '"')
		if open < 0 {
			break
		}
		closing := strings.IndexByte(rest[open+1:], '"')
		if closing < 0 {
			break
		}
		if quoted := rest[open+1 : open+1+closing]; quoted != "" {
			return quoted
		}
		rest = rest[open+1+closing+1:]
	}
	fields := strings.Fields(subject)
	for _, field := range slices.Backward(fields) {
		if tok := strings.Trim(field, "[](){}<>'"); looksLikeFileName(tok) {
			return tok
		}
	}
	return strings.TrimSpace(subject)
}

// maxExtLen bounds what counts as a file extension in looksLikeFileName.
const maxExtLen = 10

// looksLikeFileName reports whether tok has the shape base.ext, where ext is
// 1..10 ASCII letters or digits and base contains a letter. The letter rule
// keeps sizes such as "1.2" from being taken for names.
func looksLikeFileName(tok string) bool {
	dot := strings.LastIndexByte(tok, '.')
	if dot <= 0 {
		return false
	}
	base, ext := tok[:dot], tok[dot+1:]
	if ext == "" || len(ext) > maxExtLen || !strings.ContainsFunc(base, isASCIILetter) {
		return false
	}
	for _, r := range ext {
		if !isASCIILetter(r) && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func isASCIILetter(r rune) bool { return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') }

// par2VolumeRE matches a PAR2 recovery volume name and captures MM, the
// number of recovery blocks.
var par2VolumeRE = regexp.MustCompile(`(?i)\.vol\d+\+(\d+)\.par2$`)

// classifyName returns the Kind of a file name and, for PAR2 volumes, the
// recovery block count from its name.
func classifyName(name string) (Kind, int) {
	if m := par2VolumeRE.FindStringSubmatch(name); m != nil {
		blocks, err := strconv.Atoi(m[1])
		if err != nil {
			blocks = 0
		}
		return KindPar2Volume, blocks
	}
	if strings.HasSuffix(strings.ToLower(name), ".par2") {
		return KindPar2Index, 0
	}
	return KindData, 0
}
