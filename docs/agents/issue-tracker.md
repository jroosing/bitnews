# Issue tracker: GitHub

Issues and PRDs for this repo live as GitHub issues in [jroosing/bitnews](https://github.com/jroosing/bitnews/issues). Use the [`gh`](https://cli.github.com/) CLI for all operations.

## Conventions

- **Create an issue**: `gh issue create --title "..." --body "..."`. Use a heredoc or `--body-file` for multi-line bodies. Add `--label` and `--milestone` as needed.
- **Read an issue**: `gh issue view <number> --comments`. Use `--json title,body,labels,comments` for machine-readable output.
- **List issues**: `gh issue list --json number,title,labels,state` with appropriate `--label` filters.
- **Comment on an issue**: `gh issue comment <number> --body "..."`.
- **Apply / remove labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`. Multiple labels can be comma-separated.
- **Close**: `gh issue close <number> --comment "..."` closes with an explanation in one step.

Infer the repo from `git remote -v` — `gh` does this automatically when run inside a clone.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external pull requests as feature requests; `/triage` reads this flag.)_

When set to `yes`, PRs run through the same labels and states as issues:

- **Read a PR**: `gh pr view <number> --comments` and `gh pr diff <number>` for the diff.
- **List external PRs for triage**: `gh pr list --json number,title,author`, then keep only PRs whose author is not a collaborator (a contributor's PR, not a maintainer's in-flight work).
- **Comment / label / close**: `gh pr comment`, `gh pr edit --add-label`/`--remove-label`, `gh pr close`.

GitHub numbers issues and PRs in one sequence, so `#42` is unambiguous.

## When a skill says "publish to the issue tracker"

Create a GitHub issue.

## When a skill says "fetch the relevant ticket"

Run `gh issue view <number> --comments`.
