# bitnews

## Communication

**Caveman ultra mode, always, every response.** Rules: `.claude/caveman-ultra.md`. Enforced by hooks in `.claude/settings.json` (SessionStart injects rules, UserPromptSubmit re-injects reminder each turn).

## Development workflow

- **Go:** 1.27.3. The Go module is `gitlab.com/kyura4k/gitnews`; the GitLab project is `kyura4k/bitnews`.
- **Specs:** `docs/milestones/` holds the design doc and issue specs per milestone, and is the source of truth. GitLab issues and wiki mirror it. When a spec changes, update the repo file, the GitLab issue and the wiki page. Issue IDs (`M1-01`, …) never change.
- **Branches:** never commit to `main`. Work on a branch, push it, and open a merge request. List each issue on its own line in the MR description (`Closes #8`, `Closes #9`), because GitLab does not reliably auto-close a comma-separated list.
- **Before pushing:** run `make fmt fix`, then `make check` (`fmt-check`, `fix-check`, `lint`, `test`, `vuln`). Run `make fuzz-smoke` when touching a parser. CI has three jobs (`lint` with vet, staticcheck and govulncheck; `test` with `-race` tests and fuzz smoke; `build`) and deliberately no format job.
- **Tests:** test first, in vertical slices (one test, then the code for it), through the public API. `github.com/stretchr/testify` is allowed in tests. Production code uses the standard library plus as few dependencies as possible. Every parser of hostile input (NZB, NNTP, yEnc, PAR2, archives, names) gets a fuzz target.
- **Security rules:** subprocesses only for the pinned post-processing tools `7zz` and `par2`, started through the one runner package with an argument list, no shell and a scrubbed environment (ADR-0007), strict TLS with no skip-verify, every hostile name goes through `internal/fsutil`, and secrets use `core.Secret` so they never reach logs or JSON.
- **Parallel agents:** when several agents work in one tree, give each its own files or package, tell them not to edit `go.mod`/`go.sum` (add dependencies yourself first), and run the full gate after they finish.

## Agent skills

### Issue tracker

Issues live in GitLab Issues (kyura4k/bitnews), managed via the `glab` CLI. External MRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
