package par2

import (
	"bytes"
	"regexp"
	"strconv"
)

// output is what we read from the English text par2 prints (LC_ALL=C is set
// by the runner). Every pattern matches a whole line and begins with a fixed
// word, never with a file name, so a name in the output (par2 prints them,
// and CheckName refuses names with line breaks) cannot fake one of these
// lines.
type output struct {
	// total is "There are a total of N data blocks." or the Y of "You have X
	// out of Y data blocks available."; haveAvail says the X was seen.
	total      int
	available  int
	haveAvail  bool
	recovery   int // "You have Z recovery blocks available."; absent means loaded
	haveRecov  bool
	loaded     int // sum of "Loaded N new packets including M recovery blocks."
	possible   bool
	impossible bool
	needMore   int  // "You need N more recovery blocks to be able to repair."
	willUse    int  // "N recovery blocks will be used to repair."
	allCorrect bool // "All files are correct, repair is not required."
	required   bool // "Repair is required."
	complete   bool // "Repair complete."
	renames    int  // `File: "x" - is a match for "y".` lines
	noMain     bool // "Main packet not found."
	// partials are the names of extra files par2 found parts of a target in:
	// `File: "x" - found 9 of 10 data blocks from "a.bin".` They are
	// untrusted until checked against the directory listing.
	partials []string
}

var (
	reTotal     = regexp.MustCompile(`^There are a total of (\d+) data blocks\.$`)
	reAvailable = regexp.MustCompile(`^You have (\d+) out of (\d+) data blocks available\.$`)
	reRecovery  = regexp.MustCompile(`^You have (\d+) recovery blocks available\.$`)
	reLoaded    = regexp.MustCompile(`^Loaded \d+ new packets? including (\d+) recovery blocks?$`)
	reNeedMore  = regexp.MustCompile(`^You need (\d+) more recovery blocks to be able to repair\.$`)
	reWillUse   = regexp.MustCompile(`^(\d+) recovery blocks will be used to repair\.$`)
	rePartial   = regexp.MustCompile(`^File: "(.*?)" - found \d+ of \d+ data blocks from ".*"\.$`)
	reMatch     = regexp.MustCompile(`^File: ".*" - is a match for ".*"\.$`)
)

// parseOutput reads par2's stdout. Progress lines end in a carriage return,
// so both \r and \n end a line.
func parseOutput(b []byte) output {
	var o output
	b = bytes.ReplaceAll(b, []byte{'\r'}, []byte{'\n'})
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		s := string(line)
		switch s {
		case "All files are correct, repair is not required.":
			o.allCorrect = true
			continue
		case "Repair is required.":
			o.required = true
			continue
		case "Repair is possible.":
			o.possible = true
			continue
		case "Repair is not possible.":
			o.impossible = true
			continue
		case "Repair complete.":
			o.complete = true
			continue
		case "Main packet not found.":
			o.noMain = true
			continue
		}
		if m := reAvailable.FindStringSubmatch(s); m != nil {
			o.available, o.total, o.haveAvail = atoi(m[1]), atoi(m[2]), true
		} else if m := reTotal.FindStringSubmatch(s); m != nil {
			o.total = atoi(m[1])
		} else if m := reRecovery.FindStringSubmatch(s); m != nil {
			o.recovery, o.haveRecov = atoi(m[1]), true
		} else if m := reLoaded.FindStringSubmatch(s); m != nil {
			o.loaded = satAdd(o.loaded, atoi(m[1]))
		} else if m := reNeedMore.FindStringSubmatch(s); m != nil {
			o.needMore = atoi(m[1])
		} else if m := reWillUse.FindStringSubmatch(s); m != nil {
			o.willUse = atoi(m[1])
		} else if m := rePartial.FindStringSubmatch(s); m != nil {
			o.partials = append(o.partials, m[1])
		} else if reMatch.MatchString(s) {
			o.renames++
		}
	}
	return o
}

// recoveryBlocks is the number of recovery blocks par2 found. A clean verify
// does not print "You have Z recovery blocks available.", so the blocks that
// the loaded files announced are the answer then (par2 loads each packet once,
// so a duplicate volume is not counted twice).
func (o output) recoveryBlocks() int {
	if o.haveRecov {
		return o.recovery
	}
	return o.loaded
}

// damaged is the number of data blocks that are missing or damaged, which is
// the number of recovery blocks a repair consumes.
func (o output) damaged() int {
	if !o.haveAvail {
		return 0
	}
	return max(o.total-o.available, 0)
}

// atoi parses a run of digits, saturating instead of failing.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1<<31 - 1
	}
	return n
}

func satAdd(a, b int) int {
	if a > 1<<31-1-b {
		return 1<<31 - 1
	}
	return a + b
}
