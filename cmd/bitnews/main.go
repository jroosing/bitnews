// Command bitnews is a self-hosted Usenet and BitTorrent download daemon.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// run is main without the process globals, so it can be tested. It returns
// the exit code: 0 on success, 2 for invalid flags or environment.
func run(args []string, getenv func(string) (string, bool), stdout, stderr io.Writer) int {
	cfg, err := bootstrap.Load(args, getenv)
	switch {
	case errors.Is(err, bootstrap.ErrVersion):
		fmt.Fprintf(stdout, "bitnews %s (%s)\n", version, commit)
		return 0
	case errors.Is(err, bootstrap.ErrHelp):
		bootstrap.Usage(stdout)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "bitnews: %v\nRun 'bitnews --help' for usage.\n", err)
		return 2
	}

	log := bootstrap.NewLogger(stderr, cfg)
	log.Info("starting bitnews",
		"version", version,
		"commit", commit,
		"listen", cfg.Listen,
		"config_dir", cfg.ConfigDir,
		"key_file", cfg.KeyFile,
	)
	return 0
}
