package fsutil

// FreeBytes reports the bytes available to an unprivileged process on the
// filesystem holding path. Where the platform has no probe it returns an
// error wrapping ErrUnsupported, which callers treat as "unknown".
func FreeBytes(path string) (uint64, error) {
	free, _, err := DiskSpace(path)
	return free, err
}
