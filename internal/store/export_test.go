package store

// SetLinkFile replaces the hard-link step of key file creation and returns a
// function that restores it. It lets tests model filesystems without hard links.
func SetLinkFile(f func(oldname, newname string) error) (restore func()) {
	prev := linkFile
	linkFile = f
	return func() { linkFile = prev }
}
