package web

// RuntimeSource is what the UI asks the running daemon about live state that
// does not live in a Download row: transfer speeds, whether a Queue is
// paused, and the post-processing stage. The queue page owns this interface
// and extends it; internal/daemon implements it (web_runtime.go).
type RuntimeSource interface{}

// TorrentSource is what the UI asks about torrents beyond their Download row:
// info hash, ratio, peers, seeding time and Share Limit. The detail page owns
// this interface and extends it; internal/daemon implements it
// (web_torrents.go).
type TorrentSource interface{}
