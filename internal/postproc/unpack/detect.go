package unpack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Format is the archive format of a set.
type Format int

// The supported archive formats.
const (
	FormatRAR Format = iota + 1
	Format7z
)

// String returns the format name.
func (f Format) String() string {
	switch f {
	case FormatRAR:
		return "rar"
	case Format7z:
		return "7z"
	}
	return "unknown"
}

// ArchiveSet is one archive: a single file or an ordered list of volumes.
type ArchiveSet struct {
	Format Format
	// Name is the archive name without volume suffixes, for example "movie"
	// for movie.part01.rar.
	Name string
	// Volumes are the full paths of the volumes, first volume first.
	Volumes []string
	// Err is set when the set cannot be unpacked as found, for example when a
	// middle volume is missing. Unpack refuses such a set.
	Err error
}

var (
	magicRAR4 = []byte("Rar!\x1a\x07\x00")
	magicRAR5 = []byte("Rar!\x1a\x07\x01\x00")
	magic7z   = []byte("7z\xbc\xaf\x27\x1c")
)

var (
	reNewStyle = regexp.MustCompile(`(?i)^(.+)\.part(\d+)\.rar$`)
	reOldFirst = regexp.MustCompile(`(?i)^(.+)\.rar$`)
	reOldCont  = regexp.MustCompile(`(?i)^(.+)\.([r-z])(\d\d)$`)
	re7zVolume = regexp.MustCompile(`(?i)^(.+\.7z)\.(\d{3,9})$`)
)

// family says how the volumes of a group are named.
type family int

const (
	famRARNew family = iota + 1 // name.partNN.rar
	famRAROld                   // name.rar, name.r00, name.r01, ...
	fam7z                       // name.7z.001, name.7z.002, ...
)

// group collects the volumes found for one archive name and family.
type group struct {
	fam    family
	name   string
	vols   map[int][]string // volume number -> files, normally one
	hasMag map[int]bool     // volume number -> file starts with archive magic
}

func (g *group) add(num int, path string, magic bool) {
	g.vols[num] = append(g.vols[num], path)
	if magic {
		g.hasMag[num] = true
	}
}

// Detect finds the archive sets among the regular files of dir. Files are
// classified by their magic bytes, never by extension alone; the extension
// only decides which volumes belong together. Symbolic links, directories and
// special files are ignored. The result is ordered by format and name.
func Detect(dir string) ([]ArchiveSet, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var (
		sets   []ArchiveSet
		groups = map[string]*group{}
	)
	groupFor := func(fam family, name string) *group {
		key := fmt.Sprintf("%d/%s", fam, strings.ToLower(name))
		g := groups[key]
		if g == nil {
			g = &group{fam: fam, name: name, vols: map[int][]string{}, hasMag: map[int]bool{}}
			groups[key] = g
		}
		return g
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		p := filepath.Join(dir, name)
		if m := re7zVolume.FindStringSubmatch(name); m != nil {
			// Volumes after the first have no magic of their own.
			num, _ := strconv.Atoi(m[2])
			if num == 1 && sniff(p) != Format7z {
				continue
			}
			groupFor(fam7z, m[1]).add(num, p, num == 1)
			continue
		}
		format := sniff(p)
		switch {
		case format == 0:
		case format == Format7z:
			sets = append(sets, ArchiveSet{Format: Format7z, Name: strings.TrimSuffix(name, filepath.Ext(name)), Volumes: []string{p}})
		default:
			if m := reNewStyle.FindStringSubmatch(name); m != nil {
				num, err := strconv.Atoi(m[2])
				if err != nil {
					num = -1
				}
				groupFor(famRARNew, m[1]).add(num, p, true)
			} else if m := reOldFirst.FindStringSubmatch(name); m != nil {
				groupFor(famRAROld, m[1]).add(0, p, true)
			} else if m := reOldCont.FindStringSubmatch(name); m != nil {
				nn, _ := strconv.Atoi(m[3])
				groupFor(famRAROld, m[1]).add(int(unicode.ToLower(rune(m[2][0]))-'r')*100+nn+1, p, true)
			} else {
				// Obfuscated name: a single-volume set of its own.
				sets = append(sets, ArchiveSet{Format: FormatRAR, Name: name, Volumes: []string{p}})
			}
		}
	}
	for _, g := range groups {
		if s, ok := g.set(); ok {
			sets = append(sets, s)
		}
	}
	sort.Slice(sets, func(i, j int) bool {
		a, b := sets[i], sets[j]
		if a.Format != b.Format {
			return a.Format < b.Format
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		return a.Volumes[0] < b.Volumes[0]
	})
	return sets, nil
}

// set turns a group into an ArchiveSet, recording gaps as the set's Err.
func (g *group) set() (ArchiveSet, bool) {
	first := 1
	format := FormatRAR
	switch g.fam {
	case famRAROld:
		first = 0
	case fam7z:
		format = Format7z
	}
	set := ArchiveSet{Format: format, Name: g.name}
	if g.fam == fam7z {
		set.Name = strings.TrimSuffix(g.name, filepath.Ext(g.name))
		if !g.hasMag[1] {
			return set, false // a lone continuation volume is not an archive
		}
	}
	nums := make([]int, 0, len(g.vols))
	for n := range g.vols {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for i, n := range nums {
		if len(g.vols[n]) > 1 {
			set.Err = errors.Join(set.Err, fmt.Errorf("%w: %s has several files for volume %d", ErrMissingVolume, g.name, n))
		}
		set.Volumes = append(set.Volumes, g.vols[n]...)
		want := first + i
		if n != want && set.Err == nil {
			set.Err = fmt.Errorf("%w: %s is missing volume %d", ErrMissingVolume, g.name, want)
		}
	}
	if len(nums) > 0 && nums[0] != first && set.Err == nil {
		set.Err = fmt.Errorf("%w: %s has no first volume", ErrMissingVolume, g.name)
	}
	return set, true
}

// sniff reads the first bytes of the file and classifies them by magic.
func sniff(path string) Format {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 8)
	n, err := io.ReadFull(f, buf)
	if err != nil && n == 0 {
		return 0
	}
	buf = buf[:n]
	switch {
	case bytes.HasPrefix(buf, magicRAR5), bytes.HasPrefix(buf, magicRAR4):
		return FormatRAR
	case bytes.HasPrefix(buf, magic7z):
		return Format7z
	}
	return 0
}
