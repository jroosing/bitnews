# bitnews

## Communication

**Caveman ultra mode, always, every response.** Rules: `.claude/caveman-ultra.md`. Enforced by hooks in `.claude/settings.json` (SessionStart injects rules, UserPromptSubmit re-injects reminder each turn).

## Agent skills

### Issue tracker

Issues live in GitLab Issues (kyura4k/bitnews), managed via the `glab` CLI. External MRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
