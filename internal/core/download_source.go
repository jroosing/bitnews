package core

import "context"

// DownloadSourceRepo persists what the Usenet runner needs to fetch an NZB
// Download again after a restart: where the stored .nzb file is and the
// password of the release's archives. The NZB itself stays on disk; the
// runner parses it again.
type DownloadSourceRepo interface {
	// Create stores the source of the Download, which must exist. The
	// password is encrypted; an empty one is stored as none.
	Create(ctx context.Context, downloadID ID, nzbPath string, password Secret) error
	// Get returns the NZB path and the password. A Download without a
	// source is ErrNotFound; a password that cannot be decrypted is an
	// error matching ErrSecretUnavailable.
	Get(ctx context.Context, downloadID ID) (nzbPath string, password Secret, err error)
}
