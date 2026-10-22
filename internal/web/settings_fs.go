package web

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// pathChecker is what the paths settings ask of the file system. The real
// one is osPathChecker; tests replace it to simulate two filesystems.
type pathChecker interface {
	// Stat is os.Stat.
	Stat(path string) (fs.FileInfo, error)
	// Writable reports whether files can be created in the existing
	// directory dir. It leaves nothing behind.
	Writable(dir string) error
	// SameFilesystem reports whether a and b are on one filesystem; a path
	// that does not exist counts as its nearest existing ancestor.
	SameFilesystem(a, b string) (bool, error)
}

type osPathChecker struct{}

func (osPathChecker) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (osPathChecker) SameFilesystem(a, b string) (bool, error) { return fsutil.SameFilesystem(a, b) }

// Writable creates and removes an empty file in dir. The name is random and
// the file is created exclusively, so an existing file is never touched, and
// it is removed even when closing fails.
func (osPathChecker) Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".bitnews-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	rmErr := os.Remove(name)
	return errors.Join(closeErr, rmErr)
}

// pathWarning is one non-blocking remark after saving the folders.
type pathWarning struct {
	Title string
	Body  string
}

// pathRoot is one folder to check, with the words used to name it.
type pathRoot struct {
	Label string // "Usenet incomplete folder"
	Path  string
}

// checkRoot reports problems with one folder: missing, not a directory, not
// writable. It writes only inside directories that exist.
func checkRoot(c pathChecker, r pathRoot) []pathWarning {
	fi, err := c.Stat(r.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return []pathWarning{{
			Title: r.Label + " does not exist yet",
			Body:  fmt.Sprintf("%s was not found. bitnews tries to create it when a download needs it, so check the path for typos and that the parent folder is writable.", r.Path),
		}}
	case err != nil:
		return []pathWarning{{
			Title: r.Label + " cannot be read",
			Body:  fmt.Sprintf("%s could not be inspected: %s.", r.Path, pathReason(err)),
		}}
	case !fi.IsDir():
		return []pathWarning{{
			Title: r.Label + " is a file",
			Body:  r.Path + " exists but is not a folder. Downloads cannot be stored there.",
		}}
	}
	if err := c.Writable(r.Path); err != nil {
		return []pathWarning{{
			Title: r.Label + " is not writable",
			Body:  r.Path + " exists, but bitnews cannot create files in it. Downloads will fail until you fix the folder permissions or the user bitnews runs as.",
		}}
	}
	return nil
}

// checkProtocolRoots checks both folders of one protocol, and warns when
// they live on different filesystems.
func checkProtocolRoots(c pathChecker, name, incomplete, complete string) []pathWarning {
	out := checkRoot(c, pathRoot{name + " incomplete folder", incomplete})
	out = append(out, checkRoot(c, pathRoot{name + " complete folder", complete})...)
	same, err := c.SameFilesystem(incomplete, complete)
	if err == nil && !same {
		out = append(out, pathWarning{
			Title: name + " folders are on different filesystems",
			Body: "The incomplete and complete folders (" + filepath.Clean(incomplete) + " and " + filepath.Clean(complete) + ") are not on the same disk or mount. " +
				"When a download finishes, bitnews would have to copy it instead of just moving it: that is slower, needs temporary extra space, and it breaks hardlinks, " +
				"which media managers use to import a file without keeping two copies. Keep both folders under one mount, such as /data.",
		})
	}
	return out
}

// pathReason is the short cause of a file system error, without the path.
func pathReason(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
