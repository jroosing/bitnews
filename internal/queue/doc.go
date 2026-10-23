// Package queue is the Queue of one Source kind (ADR-0008): which of its
// Downloads should be Fetching, and why it is not starting any.
//
// A Queue owns the policy both protocols share: admission through
// core.Select with the Queue's concurrency limit, Pause reasons, the disk
// guard, holds while a Download's files are removed, picking up Downloads
// left Fetching without a running fetch, recovery after a restart, and the
// "change the state, then publish it" step. A protocol plugs in through the
// Protocol interface and keeps only what is its own: Articles and Servers
// for Usenet, the engine, metadata and Seeding for torrents.
//
// The runner owns the control loop and calls Reconcile on each pass; the
// Queue calls Config.Wake when something changed that needs another pass.
package queue
