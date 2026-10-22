//go:build !unix

package main

// setUmask does nothing: this platform has no umask.
func setUmask(int) {}
