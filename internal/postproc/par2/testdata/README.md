# Test data

`golden/explicit_uniform` is one PAR2 set written by par2cmdline 1.4.0 (`-s4096 -c20 -u -n4`): payload files in
`data/`, the `.par2` files in `par2/` and the expected values in `manifest.json`. The package tests of
`internal/postproc/par2` no longer use it (they build their sets with `par2 c`, ADR-0007). It stays because the
daemon end-to-end tests (`internal/daemon`, `loadGolden`) serve it from the fake NNTP server. The other golden
cases, their generator and the conformance notes were deleted with the pure-Go PAR2 code.
