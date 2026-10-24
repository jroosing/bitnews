# bitnews

## Communication

**Caveman ultra mode, always, every response.** Rules: `.claude/caveman-ultra.md`. Enforced by hooks in `.claude/settings.json` (SessionStart injects rules, UserPromptSubmit re-injects reminder each turn).

## Development workflow

- **Go:** 1.27.3. The Go module is `github.com/jroosing/bitnews`, the same as the GitHub repository `jroosing/bitnews` (public).
- **Specs:** `docs/milestones/` holds the design doc and issue specs per milestone, and is the source of truth. Open work also has a GitHub issue that mirrors its spec; there is no wiki. When a spec changes, update the repo file and its GitHub issue. Issue IDs (`M1-01`, …) never change.
- **Commits:** commit directly on `main` and push it. No branches and no merge requests. Close finished issues yourself (`gh issue close <n> --comment "..."`), or put `Closes #n` (one per line) in the commit message.
- **Before pushing:** run `make fmt fix`, then `make check` (`fmt-check`, `fix-check`, `lint`, `test`, `e2e`, `vuln`). Run `make fuzz-smoke` when touching a parser. Fuzzing and the end-to-end tests (`make e2e`, build tag `e2e`) run locally only, never in CI. CI is GitHub Actions (`.github/workflows/ci.yml`): `lint` (vet, staticcheck, govulncheck), `test` (`-race` tests) and `build`, plus `docker-image` (ghcr.io, on `main` and tags). Deliberately no format, fuzz or e2e job; tag pushes skip `lint` and `test`.
- **Tests:** test first, in vertical slices (one test, then the code for it), through the public API. `github.com/stretchr/testify` is allowed in tests. Production code uses the standard library plus as few dependencies as possible. Every parser of hostile input (NZB, NNTP, yEnc, PAR2, archives, names) gets a fuzz target.
- **Security rules:** subprocesses only for the pinned post-processing tools `7zz` and `par2`, started through the one runner package with an argument list, no shell and a scrubbed environment (ADR-0007), strict TLS with no skip-verify, every hostile name goes through `internal/fsutil`, and secrets use `core.Secret` so they never reach logs or JSON.
- **Parallel agents:** when several agents work in one tree, give each its own files or package, tell them not to edit `go.mod`/`go.sum` (add dependencies yourself first), and run the full gate after they finish.

## Agent skills

### Issue tracker

Issues live in GitHub Issues (jroosing/bitnews), managed via the `gh` CLI. External PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
