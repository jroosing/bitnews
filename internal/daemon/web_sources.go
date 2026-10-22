package daemon

import "gitlab.com/kyura4k/gitnews/internal/web"

// webRuntime and webTorrents build the live-state sources of the web UI. They
// are stubs until the queue and detail pages define what they need; each page
// replaces its function in its own file (web_runtime.go, web_torrents.go) and
// deletes the stub here.
func webRuntime(Deps) web.RuntimeSource { return nil }

func webTorrents(Deps) web.TorrentSource { return nil }
