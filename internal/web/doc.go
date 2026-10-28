// Package web is the browser UI: server-rendered HTML (templ components) with
// htmx, Alpine.js (CSP build) and Tailwind CSS, all embedded in the binary
// and served by the daemon itself, with a strict CSP.
//
// The handlers call the same application services as the JSON API (they do
// not go through it) and render pages and htmx fragments. New returns one
// http.Handler for everything outside /api/ and /sabnzbd/; internal/api
// mounts it. See README.md for how to add a page, a fragment route and a
// component, and for the CSP rules every view must follow.
package web
