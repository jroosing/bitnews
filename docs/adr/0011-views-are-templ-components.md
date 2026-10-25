# Views are templ components, not html/template

The web UI's HTML is written as [templ](https://templ.guide) components in `internal/web` instead of `html/template` files under `assets/templates`. This supersedes the `html/template` part of [ADR-0005](0005-web-ui-stack.md); the rest of that ADR (htmx, Alpine, Tailwind, CSP, vendored assets) stands.

The reason is type safety and maintainability, not speed. Components were called with `{{template "x" dict "Key" "value"}}`, so a misspelled key, a wrong enum value or an unknown icon name only showed up at render time, as a blank spot or a 500. templ compiles components to Go functions, so those become compile errors. Any render-time performance or memory gain is a bonus and is not a goal.

## Decisions

- **Same package, no new seam.** `.templ` files sit next to the handlers in package `web` (`layout*.templ`, `comp_*.templ`, `page_*.templ`). Page data stays the existing unexported typed structs; `View.Page any` becomes typed component arguments. A separate `ui` package is a possible later move, not part of this change.
- **Generated code is committed**, like `app.css` and `icons_gen.go`: `go build` and CI need no generate step. The generator is pinned through a `tool` directive in `go.mod` (`go tool templ generate`), so no binary is downloaded. `make generate` regenerates, `make generate-check` diffs a fresh run against the committed files and is part of `make check` but not CI. The `a-h/templ` runtime is the one new production dependency.
- **Typed parameters.** Small components take positional arguments. A component with more than about three parameters or any optional ones takes a props struct. Enum-like values (variant, size, badge state) are named string types with constants, never bare strings.
- **Allowlisted htmx attributes.** The runtime `attrs` and `hxVals` helpers (a denylist checked at render time) are replaced by a typed `Hx` struct (`Get`, `Post`, `Target`, `Swap`, `Trigger`, ...). Event handlers, `style` and `srcdoc` cannot be expressed. URLs must be relative and `hx-vals` values ID-shaped, checked when the struct is built. Static attributes are written inline in the `.templ` file.
- **Typed icons.** `scripts/gen-icons.sh` also emits `type IconName string` and one constant per icon. An unknown icon is a compile error.
- **Rendering stays buffered.** `render` takes a `templ.Component`, renders into a buffer, and only then writes, so a render error is still a clean 500. `renderFragment`, the per-page template clones and `dict` go away.
- **Big-bang migration.** Everything moves in one stretch (components and layouts, then pages, then `html/template` is deleted). Mixing the two engines needs a bridge and duplicate layouts that would be thrown away.
- **Safety net is the existing tests**: the CSP hygiene tests (no inline script or style, event handlers, `hx-on` or `javascript:` URL in any rendered page) and the handler tests. No golden HTML snapshots. The accessibility test is removed; existing `aria-*`, `role` and label markup is carried over unchanged but no longer enforced.

## Considered options

- **Stay on `html/template`.** No new dependency or generator. Rejected because the stringly-typed component calls are the thing being fixed.
- **Keep both engines behind a bridge** (templ calling `html/template` or the reverse) and migrate page by page. Rejected: double buffering and duplicated layouts and components, all deleted at the end.
- **Port `attrs` and `hxVals` as they are** (a `templ.Attributes` map checked at render time). Smallest diff, but a denylist of bad names and the stringly-typed map stay.
- **Generate at build time instead of committing.** Cleaner diffs, but `go build` and CI would need the generator.

## Consequences

- Changing a `.templ` file needs `make generate` (and `make css` if class names changed) before committing. Tailwind scans the `.templ` sources.
- Generated `*_templ.go` files make diffs noisy; a forgotten `make generate` fails `make check`, not CI.
- Templates can only do what Go code in the component can. The CSP still decides what markup is allowed; templ does not change it.
- The `/ui/styleguide` page stays the visual test page and is rewritten as a templ page.
