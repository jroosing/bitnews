// Command bitnews is a self-hosted Usenet and BitTorrent download daemon.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, privdrop.OS(), os.Stdout, os.Stderr))
}

// run is main without the process globals, so it can be tested. It returns
// the exit code: 0 on success, 1 for a startup or runtime failure (such as a
// failed privilege drop), 2 for invalid flags or environment. sys is the
// process-wide state the privilege drop changes; tests pass a fake.
func run(args []string, getenv func(string) (string, bool), sys privdrop.Syscalls, stdout, stderr io.Writer) int {
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

	// Startup order step 2: nothing may be opened or parsed before the
	// process has dropped root.
	log := bootstrap.NewLogger(stderr, cfg)
	if err := privdrop.Apply(privdrop.Options{RunAs: cfg.RunAs, Umask: cfg.Umask}, sys, log); err != nil {
		fmt.Fprintf(stderr, "bitnews: %v\n", err)
		return 1
	}

	log.Info("starting bitnews",
		"version", version,
		"commit", commit,
		"listen", cfg.Listen,
		"config_dir", cfg.ConfigDir,
		"key_file", cfg.KeyFile,
	)
	return 0
}
