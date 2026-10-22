package unpack

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tool"
)

// 7zz exit codes (7-Zip documentation).
const (
	exitOK       = 0
	exitWarning  = 1
	exitFatal    = 2
	exitCommand  = 7
	exitMemory   = 8
	exitUserStop = 255
)

// sevenZip runs 7zz. All the arguments are built here, from fixed words and the
// absolute paths Unpack computed; there is no shell. "--" ends the switches so
// an archive named like a switch ("-so.rar", "@list.rar") is a file name.
type sevenZip struct {
	path string
	cfg  *config
}

// locate finds the 7zz program for a run.
func locate(cfg *config) (*sevenZip, error) {
	p := cfg.sevenZip
	if p == "" {
		var err error
		if p, err = tool.Locate("7zz"); err != nil {
			return nil, err
		}
	}
	return &sevenZip{path: p, cfg: cfg}, nil
}

// stdin feeds the password to the prompt 7zz shows when it meets encrypted
// data. Without a password stdin is empty, so 7zz gives up at the prompt
// ("Break signaled", exit status 255) instead of waiting.
func (z *sevenZip) stdin() (*strings.Reader, error) {
	pw := z.cfg.password
	if pw == "" {
		return nil, nil
	}
	if strings.ContainsAny(pw, "\r\n\x00") || !utf8.ValidString(pw) {
		return nil, fmt.Errorf("%w: the password has a line break, a NUL byte or invalid UTF-8", ErrPassword)
	}
	return strings.NewReader(pw + "\n"), nil
}

func (z *sevenZip) run(ctx context.Context, dir string, maxOutput int, args ...string) (tool.Result, error) {
	in, err := z.stdin()
	if err != nil {
		return tool.Result{}, err
	}
	spec := tool.Spec{Path: z.path, Args: args, Dir: dir, MaxOutput: maxOutput}
	if in != nil {
		spec.Stdin = in
	}
	res, err := tool.Run(ctx, spec)
	if err != nil {
		return res, err
	}
	return res, nil
}

// list returns the entries of the archive whose first volume is first.
func (z *sevenZip) list(ctx context.Context, dir, first string) ([]*entry, error) {
	limit := min(max(z.cfg.maxEntries, 1)*listBytesPerEntry+listBytesFloor, listBytesCeiling)
	res, err := z.run(ctx, dir, limit, "l", "-slt", "-ba", "-sccUTF-8", "--", first)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("%w: the archive listing is longer than %d bytes", ErrTooManyEntries, limit)
	}
	if err := classify(res); err != nil {
		return nil, err
	}
	return parseListing(res.Stdout)
}

// extract unpacks the archive into dir, which is also the working directory.
func (z *sevenZip) extract(ctx context.Context, dir, first string) error {
	res, err := z.run(ctx, dir, 0, "x", "-y", "-bd", "-aos", "-sccUTF-8", "-o"+dir, "--", first)
	if err != nil {
		return err
	}
	return classify(res)
}

// classify turns the outcome of a 7zz run into one of the package's errors. The
// exit status says how bad it was; the text says what, because 7zz uses status
// 2 for everything from a wrong password to a truncated archive.
//
// The text contains entry names, which an attacker chooses, so a name can pick
// between classes (a file called "Wrong password" makes a corrupt archive look
// like a password problem). It cannot do more than that: every class is a
// failure of the Download, and nothing is parsed out of the text but the class.
func classify(res tool.Result) error {
	if res.ExitCode == exitOK {
		return nil
	}
	text := strings.ToLower(string(res.Stdout) + "\n" + string(res.Stderr))
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(text, s) {
				return true
			}
		}
		return false
	}
	detail := excerpt(res)
	switch {
	case has("wrong password", "cannot open encrypted archive"),
		res.ExitCode == exitUserStop && has("enter password"):
		return fmt.Errorf("%w: %s", ErrPassword, detail)
	case has("no space left", "not enough space", "disk full"):
		return fmt.Errorf("%w: %s", ErrDiskFull, detail)
	case res.ExitCode == exitMemory, has("can't allocate required memory", "not enough memory"):
		return fmt.Errorf("%w: 7zz ran out of memory: %s", ErrTooLarge, detail)
	case has("unsupported method", "unsupported feature", "unsupported archive"):
		return fmt.Errorf("%w: %s", ErrUnsupported, detail)
	case res.ExitCode == exitCommand:
		return fmt.Errorf("unpack: 7zz rejected its command line (a bug): %s", detail)
	case res.ExitCode == exitUserStop:
		return fmt.Errorf("unpack: 7zz was stopped: %s", detail)
	case res.ExitCode == exitWarning:
		return fmt.Errorf("%w: 7zz reported a warning (exit status 1): %s", ErrCorrupt, detail)
	case res.ExitCode == exitFatal:
		return fmt.Errorf("%w: %s", ErrCorrupt, detail)
	}
	return fmt.Errorf("%w: 7zz exit status %d: %s", ErrCorrupt, res.ExitCode, detail)
}

// excerpt picks one line out of the output for the error message: the first
// line that starts with ERROR (stderr first), else the first non-empty line of
// stderr. It is bounded and has no control characters.
func excerpt(res tool.Result) string {
	first := ""
	for i, out := range [][]byte{res.Stderr, res.Stdout} {
		for line := range bytes.Lines(out) {
			s := strings.TrimSpace(string(line))
			if s == "" {
				continue
			}
			if strings.HasPrefix(s, "ERROR") {
				return truncateForError(s)
			}
			if i == 0 && first == "" {
				first = s
			}
		}
	}
	if first == "" {
		return "no further detail"
	}
	return truncateForError(first)
}
