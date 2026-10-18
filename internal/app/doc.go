// Package app is the application service layer between the transports and
// core. The own API (internal/api) calls it today and the htmx UI will call
// the same services later, so validation, repository calls and event
// publishing live here once. It knows nothing about HTTP.
package app
