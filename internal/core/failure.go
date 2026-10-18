package core

// FailureReason says why a Download is Failed. The enum is shared across
// protocols and is extended in later milestones.
type FailureReason string

// The reasons a Download can be Failed.
const (
	// FailureMissingArticles: Missing Articles that Repair cannot recover
	// (or no PAR2 set to recover them with).
	FailureMissingArticles FailureReason = "missing_articles"
	// FailureRepairInsufficient: more damage than all recovery data covers.
	FailureRepairInsufficient FailureReason = "repair_insufficient"
	// FailureArchiveUnsupported: Unpack cannot handle the archive format.
	FailureArchiveUnsupported FailureReason = "archive_unsupported"
	// FailureArchivePassword: the archive password is missing or wrong.
	FailureArchivePassword FailureReason = "archive_password"
	// FailureArchiveCorrupt: the archive is damaged beyond Unpack.
	FailureArchiveCorrupt FailureReason = "archive_corrupt"
	// FailureDiskFull: there is not enough space to continue.
	FailureDiskFull FailureReason = "disk_full"
	// FailurePathViolation: a path would escape its root.
	FailurePathViolation FailureReason = "path_violation"
	// FailureSourceInvalid: the NZB or Torrent cannot be used.
	FailureSourceInvalid FailureReason = "source_invalid"
	// FailureInternal: a bug or unexpected error in bitnews itself.
	FailureInternal FailureReason = "internal"
)

func (r FailureReason) known() bool {
	switch r {
	case FailureMissingArticles, FailureRepairInsufficient, FailureArchiveUnsupported,
		FailureArchivePassword, FailureArchiveCorrupt, FailureDiskFull,
		FailurePathViolation, FailureSourceInvalid, FailureInternal:
		return true
	}
	return false
}
