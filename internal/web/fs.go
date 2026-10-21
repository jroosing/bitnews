package web

import "io/fs"

// subFS returns the embedded directory dir as its own file system.
func subFS(dir string) (fs.FS, error) { return fs.Sub(assetsFS, dir) }
