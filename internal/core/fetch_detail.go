package core

// FetchDetail is what a finished Usenet fetch knows about the state of a
// Download's Articles: a FetchReport with sizes. The Usenet runner records
// it, the store persists it and the web UI shows it.
type FetchDetail struct {
	// MissingArticles is how many Articles no Server had.
	MissingArticles int
	// MissingBytes is the encoded size, from the NZB, of the Missing
	// Articles. The decoded size is smaller by a few percent.
	MissingBytes int64
	// DamagedArticles is how many Articles were written from a copy that
	// failed its CRC.
	DamagedArticles int
	// DamagedBytes is the decoded size of the damaged ranges.
	DamagedBytes int64
	// MissingFiles counts files none of whose Articles arrived, so the
	// writer never created them.
	MissingFiles int
}

// Report returns the FetchReport form of the detail.
func (d FetchDetail) Report() FetchReport {
	return FetchReport{MissingArticles: d.MissingArticles, DamagedArticles: d.DamagedArticles}
}
