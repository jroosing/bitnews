package core

// PostProcessOptions are the two optional steps ported from the TRaSH SABnzbd
// scripts (ADR-0009). Both are off by default.
type PostProcessOptions struct {
	// CleanNames strips the suffixes and tags release bots add to an NZB's
	// name when the NZB is added (Clean.py, a pre-queue script). It never
	// applies to torrents.
	CleanNames bool `json:"clean_names"`
	// UnderscoresToDots replaces underscores with dots in the names at the
	// top level of an NZB Download's result, after Unpack and before the
	// move to the complete folder (replace_for.py). It never renames a
	// torrent's files, which are being seeded.
	UnderscoresToDots bool `json:"underscores_to_dots"`
}
