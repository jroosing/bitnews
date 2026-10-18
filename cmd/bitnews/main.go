// Command bitnews is a self-hosted Usenet and BitTorrent download daemon.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("bitnews", flag.ContinueOnError)
	fs.SetOutput(stdout)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "bitnews %s (%s)\n", version, commit)
		return 0
	}
	return 0
}
