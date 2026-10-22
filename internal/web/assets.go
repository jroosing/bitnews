package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// assetsFS holds the templates and the static files. The generated
// stylesheet (assets/static/app.css) is committed, so a plain `go build`
// needs no Tailwind step.
//
//go:embed all:assets
var assetsFS embed.FS

const (
	staticPrefix = "/static/"
	// hashLen is how many hex digits of the content hash go into a name.
	hashLen = 8

	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"
)

// assetFile is one embedded static file, held in memory.
type assetFile struct {
	name   string // path below static/, e.g. "js/app.js"
	hashed string // name with the content hash, e.g. "js/app.1a2b3c4d.js"
	data   []byte
	etag   string
	ctype  string
}

// assets serves the embedded static files under /static/. Every file is
// readable by its plain name (revalidated on each use) and by its content
// hash name (cached for a year). Templates link the hashed name through
// the asset template function, so a new build never serves stale files.
//
// All files are loaded into maps at startup and requests are answered by
// exact map lookup: there is no file system access at request time, so path
// traversal and directory listings are impossible by construction.
type assets struct {
	byName   map[string]*assetFile
	byHashed map[string]*assetFile
}

// cssURL matches url(...) references in a stylesheet.
var cssURL = regexp.MustCompile(`url\(\s*(['"]?)([^'")]+)['"]?\s*\)`)

// loadAssets reads every file below dir of fsys and computes the hash names.
// Stylesheets are rewritten first so that their url(...) references (fonts)
// point at hashed names too; the stylesheet's own hash covers the rewrite.
func loadAssets(fsys fs.FS, dir string) (*assets, error) {
	a := &assets{byName: map[string]*assetFile{}, byHashed: map[string]*assetFile{}}
	var css []*assetFile
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, dir+"/")
		f := &assetFile{name: name, data: data, ctype: contentType(name)}
		a.byName[name] = f
		if path.Ext(name) == ".css" {
			css = append(css, f)
		} else {
			a.seal(f)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: load assets: %w", err)
	}
	for _, f := range css {
		f.data = a.rewriteCSS(f)
		a.seal(f)
	}
	return a, nil
}

// seal fixes a file's hash name, ETag and index entry.
func (a *assets) seal(f *assetFile) {
	sum := sha256.Sum256(f.data)
	h := hex.EncodeToString(sum[:])[:hashLen]
	ext := path.Ext(f.name)
	f.hashed = strings.TrimSuffix(f.name, ext) + "." + h + ext
	f.etag = `"` + h + `"`
	a.byHashed[f.hashed] = f
}

// rewriteCSS points the url(...) references of a stylesheet at hashed names.
// References it cannot resolve (data: URIs, absolute URLs) stay as they are.
func (a *assets) rewriteCSS(f *assetFile) []byte {
	dir := path.Dir(f.name)
	return cssURL.ReplaceAllFunc(f.data, func(m []byte) []byte {
		ref := string(cssURL.FindSubmatch(m)[2])
		if strings.HasPrefix(ref, "data:") || strings.Contains(ref, "://") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "#") {
			return m
		}
		target, ok := a.byName[path.Join(dir, ref)]
		if !ok || target.hashed == "" {
			return m
		}
		return []byte(`url("` + staticPrefix + target.hashed + `")`)
	})
}

// Path returns the URL of the hashed file for a plain name, or an error for
// an unknown name, so a typo in a template fails its render.
func (a *assets) Path(name string) (string, error) {
	f, ok := a.byName[strings.TrimPrefix(name, "/")]
	if !ok {
		return "", fmt.Errorf("web: unknown asset %q", name)
	}
	return staticPrefix + f.hashed, nil
}

// ServeHTTP answers GET and HEAD for /static/<name>. Unknown names are 404.
func (a *assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, staticPrefix)
	f, immutable := a.byHashed[name]
	if !immutable {
		f = a.byName[name]
	}
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	if immutable {
		h.Set("Cache-Control", cacheImmutable)
	} else {
		h.Set("Cache-Control", cacheRevalidate)
	}
	http.ServeContent(w, r, f.name, time.Time{}, bytes.NewReader(f.data))
}

// contentType picks the media type by extension. The table is explicit so the
// result does not depend on the host's mime database.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".svg":
		return "image/svg+xml"
	case ".txt", ".md":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
