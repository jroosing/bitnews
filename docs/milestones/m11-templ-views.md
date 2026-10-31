# M11 — templ views

## Goal

Replace `html/template` with templ components so a wrong component parameter, enum value or icon name is a compile error instead of a blank spot or a 500 at render time. Decision and trade-offs: [ADR-0011](../adr/0011-views-are-templ-components.md).

## Scope

**In:** every template under `internal/web/assets/templates` (layouts, head, nav, 22 components, 14 pages including the style guide) as templ components in package `web`; the `Hx` attribute struct; typed `IconName` constants; `make generate` and `make generate-check`; removal of `html/template`, `dict`, `attrs`, `hxVals`, per-page template clones and the accessibility test.

**Out:** a separate `ui` package, any visual or markup change, new UI features, golden-HTML snapshots, enforcing accessibility (existing `aria-*`, `role` and label markup is carried over unchanged).

## Design

- Big-bang: no bridge between the two engines. Order is wiring, layouts and leaf components, composite components, pages, cleanup.
- Generated `*_templ.go` are committed; the generator is pinned with a `tool` directive in `go.mod`.
- Small components take positional arguments, larger ones a props struct; enum-like values are named string types with constants.
- `render` takes a `templ.Component` and still renders into a buffer before writing.
- Safety net: the existing CSP hygiene tests and handler tests. `contrast_test.go` stays (it checks CSS tokens).

## Issues

### M11-01: Views as templ components
- **Labels:** area:ui, ready-for-agent

**Goal:** The web UI renders from templ components with identical visible output, and `html/template` is gone from `internal/web`.

**Acceptance criteria**
- [x] `go.mod` has the `a-h/templ` runtime and a `tool` directive for the generator; `make generate` and `make generate-check` exist and `generate-check` is part of `make check` (not CI).
- [x] `scripts/gen-icons.sh` also emits `type IconName string` and one constant per icon; an unknown icon does not compile.
- [x] A typed `Hx` struct replaces `attrs` and `hxVals`: no event handler, `style` or `srcdoc` can be expressed, URLs must be relative, `hx-vals` values must be ID-shaped; unit tests cover the validation.
- [x] Layouts, head, nav, all components and all pages (including `/ui/styleguide`) are `.templ` files in package `web`; Tailwind scans the `.templ` sources and `app.css` is rebuilt.
- [x] `render` takes a `templ.Component` and renders into a buffer first; `renderFragment`, `loadTemplates`, `templateFuncs`, `dict`, `attrs`, `hxVals` and `assets/templates` are removed.
- [x] `a11y_test.go` is removed; the CSP hygiene tests and all handler tests pass unchanged in intent.
- [x] ADR-0005 points to ADR-0011 where it names `html/template`; the web README and AGENTS.md mention `make generate`.
- [x] `make fmt fix` and `make check` are green.
