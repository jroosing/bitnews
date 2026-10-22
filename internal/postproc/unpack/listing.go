package unpack

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// parseListing parses the output of `7zz l -slt -ba`: one block of
// "Key = Value" lines per entry, blocks separated by an empty line, every block
// starting with its "Path" line.
//
// The output is attacker-influenced, because entry names and comments are
// printed into it. The parser is therefore strict about the shape: after the
// first block starts, a line that is not "Key = Value" or "Path = ...", and a
// key that appears twice in one block, fail the listing with ErrCorrupt. A
// name or comment that contains a line break, which could forge fields of the
// entry (a smaller "Size", a missing "Encrypted"), is caught by that rule.
// Lines before the first block (the "Enter password:" prompt, which goes to
// stdout) are ignored.
func parseListing(out []byte) ([]*entry, error) {
	var (
		entries []*entry
		cur     map[string]string
	)
	finish := func() error {
		if cur == nil {
			return nil
		}
		e, err := entryFromFields(cur)
		cur = nil
		if err != nil {
			return err
		}
		entries = append(entries, e)
		return nil
	}
	for line := range bytes.Lines(out) {
		s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if s == "" {
			if err := finish(); err != nil {
				return nil, err
			}
			continue
		}
		if name, ok := strings.CutPrefix(s, "Path = "); ok {
			if cur != nil {
				return nil, fmt.Errorf("%w: listing has a second Path line inside one entry", ErrCorrupt)
			}
			cur = map[string]string{"Path": name}
			continue
		}
		if cur == nil {
			if len(entries) == 0 {
				continue // prompt or banner before the first entry
			}
			return nil, fmt.Errorf("%w: unexpected output after the entries: %q", ErrCorrupt, truncateForError(s))
		}
		key, val, ok := strings.Cut(s, " = ")
		if !ok {
			key, ok = strings.CutSuffix(s, " =")
			val = ""
		}
		if !ok || key == "" || len(key) > 64 {
			return nil, fmt.Errorf("%w: malformed listing line %q", ErrCorrupt, truncateForError(s))
		}
		if _, dup := cur[key]; dup {
			return nil, fmt.Errorf("%w: listing repeats the field %q within one entry", ErrCorrupt, truncateForError(key))
		}
		cur[key] = val
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return entries, nil
}

// entryFromFields builds an entry from the fields of one block.
func entryFromFields(f map[string]string) (*entry, error) {
	e := &entry{name: f["Path"], encrypted: f["Encrypted"] == "+"}
	if v := f["Size"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 1<<62 {
			return nil, fmt.Errorf("%w: bad size %q for %q", ErrCorrupt, truncateForError(v), truncateForError(e.name))
		}
		e.size = n
	}
	e.kind = kindFromAttributes(f["Attributes"])
	if f["Folder"] == "+" && e.kind == kindFile {
		e.kind = kindDir
	}
	if f["Symbolic Link"] != "" {
		e.kind = kindSymlink
	}
	if f["Hard Link"] != "" {
		e.kind = kindHardlink
	}
	if f["Link"] != "" && e.kind != kindHardlink {
		e.kind = kindSymlink
	}
	if e.kind == kindDir {
		e.size = 0
	}
	return e, nil
}

// kindFromAttributes reads the Attributes field of the listing: optional
// Windows attribute letters, then optionally the Unix mode string.
//
//	"A -rw-r--r--"   file          "D drwxr-xr-x"  directory
//	" lrwxrwxrwx"    symlink       "A"             Windows file
//	"RAL"            reparse point (a Windows symlink or junction)
func kindFromAttributes(attr string) entryKind {
	var dos, unix string
	for f := range strings.FieldsSeq(attr) {
		if isUnixMode(f) {
			unix = f
		} else {
			dos += f
		}
	}
	if unix != "" {
		switch unix[0] {
		case '-':
			return kindFile
		case 'd':
			return kindDir
		case 'l':
			return kindSymlink
		case 'c', 'b':
			return kindDevice
		case 'p':
			return kindFIFO
		}
		return kindOther
	}
	switch {
	case strings.Contains(dos, "L"): // reparse point
		return kindSymlink
	case strings.Contains(dos, "d"): // device
		return kindDevice
	case strings.Contains(dos, "D"):
		return kindDir
	}
	return kindFile
}

// isUnixMode matches an "ls -l" style mode such as "-rwxr-xr-x".
func isUnixMode(s string) bool {
	if len(s) != 10 || !strings.ContainsRune("-dlcbpsh?", rune(s[0])) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !strings.ContainsRune("rwxsStT-", rune(s[i])) {
			return false
		}
	}
	return true
}
