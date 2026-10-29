// Package sabnzbd emulates the subset of the SABnzbd HTTP API that media
// managers (Sonarr, Radarr, Prowlarr) use, on top of the app services. It is
// mounted at /sabnzbd/api by the daemon; see docs/milestones/m4-sabnzbd-adapter.md
// for the modes, the status mapping and the response shapes.
//
// The adapter does its own authentication: the apikey parameter must be the app
// key (auth.AppKeyService, ADR-0006). It is never behind
// the UI authentication, so disable-auth and LAN bypass cannot reach it.
// Errors use SAB's own shape, {"status": false, "error": "..."}, with HTTP
// 200, and every response is JSON whatever the output parameter says.
//
// The response shapes were written from the SABnzbd API documentation and
// from what Sonarr and Radarr are known to parse. They have NOT been checked
// against recorded traffic (M4-01 was dropped).
package sabnzbd
