# Go instead of Zig

Zig was the preferred language, but security without concessions is a hard requirement. Go was chosen for that reason.

The daemon parses hostile input all the time: bencode, peer wire messages and DHT traffic from arbitrary internet hosts, plus NZB XML and NNTP/yEnc. Go is memory-safe, so a parser bug panics instead of becoming an exploit. Zig manages memory manually, which leaves use-after-free and double-free possible. Go's `crypto/tls` is mature, has been independently audited, and includes a TLS server; Zig 0.17's TLS is client-only and has not been audited. Go also has a stability promise and vulnerability tooling (`govulncheck`, the race detector, built-in fuzzing), while Zig's pre-1.0 std breaks each release.

Zig's std did cover nearly all the crypto primitives needed. Primitives were not the deciding factor; memory safety and maturity were.

## Consequences

- Run tests with `-race` in CI, because data races can break Go's memory safety.
- Pin dependencies and run `govulncheck` to manage supply-chain risk.
- Mature libraries such as `anacrolix/torrent` become available to reuse.
