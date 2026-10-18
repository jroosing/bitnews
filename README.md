# bitnews

bitnews is a self-hosted download daemon for homelabs. It handles Usenet (like NZBGet/SABnzbd) and BitTorrent (like qBittorrent), and media managers such as Sonarr and Radarr can use it unchanged.

- [Architecture](docs/architecture.md)
- [Glossary](CONTEXT.md)
- [Decisions (ADRs)](docs/adr/)
- [Milestones and issue specs](docs/milestones/README.md)

## Development

You need Go 1.27.2 or later. Lint and vulnerability tools are pinned in `go.mod` and run through `go tool`, so there is nothing else to install.

```sh
make fmt         # gofmt -s -w (fmt-check only reports)
make fix         # go fix ./... (fix-check only reports)
make test        # go test -race ./...
make lint        # go vet + staticcheck
make vuln        # govulncheck
make fuzz-smoke  # every Fuzz* target for 10s
make build       # static binary in bin/
```
