// Package view turns a Download row and what is live about it into one
// protocol-neutral View (ADR-0008).
//
// The SABnzbd and qBittorrent adapters and the web UI each used to work out
// the same facts for themselves: which list a Download is in, its progress and
// ETA, its Post-processing stage, a torrent's ratio and Share Limit, a
// readable failure. This module works them out once; a transport only
// translates the View to its wire format.
//
// Live facts (stage, transfer rates, torrent counters) come from one Live
// source, which the daemon implements on the event tracker and the torrent
// runner.
package view
