// Package blackbox is the black-box parity suite of the daemon. Its tests
// start the bitnews binary as a separate process with a temporary config dir
// and talk to it only over HTTP and through the files it leaves on disk, so
// the same tests judge the Go daemon and the Zig rewrite (M15, ADR-0014).
//
// The binary under test is chosen with the environment variable
// BITNEWS_E2E_BINARY (an absolute path). When it is unset, TestMain builds
// ./cmd/bitnews into a temporary directory and runs that, so the suite is
// always validated against the Go version first. Run it against another
// binary with
//
//	BITNEWS_E2E_BINARY=/abs/path/to/bitnews go test -count=1 ./internal/blackbox/
//
// The suite writes config dirs the way a previous Go run would have left
// them, with the Go packages directly (the master key, a migrated database,
// the app key, an admin, a Server with a sealed password and a Category), and
// reads the config dir a fresh run of the binary leaves behind with the same
// packages. That covers data compatibility in both directions.
//
// The package has no production code: everything is in _test files.
package blackbox
