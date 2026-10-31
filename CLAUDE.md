# bitnews

## Communication

**Caveman ultra mode, always, every response.** Rules: `.claude/caveman-ultra.md`. Enforced by hooks in `.claude/settings.json` (SessionStart injects rules, UserPromptSubmit re-injects reminder each turn).

## Development workflow

- **Go:** 1.27.3. The Go module is `github.com/jroosing/bitnews`, the same as the GitHub repository `jroosing/bitnews` (public).
- **Specs:** `docs/milestones/` holds the design doc and issue specs per milestone, and is the source of truth. Open work also has a GitHub issue that mirrors its spec; there is no wiki. When a spec changes, update the repo file and its GitHub issue. Issue IDs (`M1-01`, …) never change.
- **Commits:** commit directly on `main` and push it. No branches and no pull requests. Close finished issues yourself (`gh issue close <n> --comment "..."`), or put `Closes #n` (one per line) in the commit message.
- **Before pushing:** run `make fmt fix`, then `make generate` after changing a `.templ` file, then `make css` after changing a `.templ` file or the stylesheet sources, then `make check` (`fmt-check`, `fix-check`, `generate-check`, `css-check`, `lint`, `test`, `e2e`, `vuln`). Run `make fuzz-smoke` when touching a parser. Fuzzing and the end-to-end tests (`make e2e`, build tag `e2e`) run locally only, never in CI. CI is GitHub Actions (`.github/workflows/ci.yml`) and runs only on a release (tag push), to save Actions minutes; pushes to `main` run nothing, so `make check` locally is the gate. It runs `lint` (vet, staticcheck, govulncheck), `test` (`-race` tests) and `build`, then `docker-image` (ghcr.io, `latest` plus the tag). Deliberately no format, fuzz or e2e job.
- **Tests:** test first, in vertical slices (one test, then the code for it), through the public API. `github.com/stretchr/testify` is allowed in tests. External libraries are allowed in production code when they simplify the code base or perform better than our own code; pin them and keep `govulncheck` green. Every parser of hostile input (NZB, NNTP, yEnc, PAR2, archives, names) gets a fuzz target.
- **Security rules:** subprocesses only for the pinned post-processing tools `7zz` and `par2`, started through the one runner package with an argument list, no shell and a scrubbed environment (ADR-0007), strict TLS with no skip-verify, every hostile name goes through `internal/fsutil`, and secrets use `core.Secret` so they never reach logs or JSON.
- **Parallel agents:** when several agents work in one tree, give each its own files or package, tell them not to edit `go.mod`/`go.sum` (add dependencies yourself first), and run the full gate after they finish. Subagents are expensive: use the project `worker` agent (`.claude/agents/worker.md`, Haiku) instead of `general-purpose`, pass `model: "sonnet"` only for security-sensitive or design-heavy slices, keep a slice small enough to finish in one go, and never leave a `--fuzz` run going.

## Zig branch

The `zig` branch rewrites bitnews in Zig ([ADR-0014](docs/adr/0014-zig-memory-safe-where-practical.md), spec [M15](docs/milestones/m15-zig.md)). It lives next to `main`, commits go straight onto it, and it is merged into `main` only once the parity gate in M15 is met. Nothing from it is published: Docker images are built locally only. On this branch:

- **Zig:** 0.17.0 (`scripts/get-zig.sh`), code in `zig/` with its own `build.zig`. The Go code stays as the reference and the parity harness; files both need are copied into `zig/` by the sync step and checked for drift.
- **Gate:** `make zig-check` (`zig build check`: format, tests on the debug allocator, sync check; then the Go black-box suite against the Zig binary, `make blackbox-zig`) and `make check` for the Go side; run the Zig tests in Debian too (`zig/Dockerfile.dev`) when touching sockets, files or the OS; fuzz a parser you touch (`zig build test-<group> --fuzz`, one run at a time).
- **Memory rules:** an arena per request, Article and post-processing step; one documented owner for long-lived state; hard caps on hostile input; no pointers into buffers that can grow.

## Agent skills

### Issue tracker

Issues live in GitHub Issues (jroosing/bitnews), managed via the `gh` CLI. External PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
