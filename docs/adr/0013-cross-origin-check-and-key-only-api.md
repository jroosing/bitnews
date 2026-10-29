# The own API takes the app key only; CSRF is the standard library's cross-origin check

Two changes to how browsers are kept from abusing bitnews.

**The own API (`/api/v1/`) authenticates with the app key and nothing else.** The browser UI never called it: the htmx UI has its own routes and its own event stream (internal/web). So the JSON sign-in, sign-out, session info and setup routes (`/api/v1/auth/*`, `/api/v1/setup`) are gone, and a UI session cookie, LAN bypass or disable auth no longer open any `/api/v1/` route. The auth section of the Settings cannot be changed through the API at all; it is changed in the UI.

**CSRF protection is `http.CrossOriginProtection` (Go 1.25+), not a synchronizer token.** A state-changing request whose browser says it comes from another site (`Sec-Fetch-Site`, or else an `Origin` that is not the request's `Host`) is refused with 403, unless its `Origin` is a page under one of the allowed hosts (any port, http or https). A request without these headers passes: it comes from a script, which holds no ambient credential a hostile page could make the browser send. The rule is in one place (`auth.CheckOrigin`), used by the UI and the qBittorrent adapter. The Host allowlist that answers 421 while disable auth or LAN bypass is on stays: it is the defence against DNS rebinding, which the cross-origin check does not cover.

## Why

The token was threaded through the whole UI: a `CSRF` field in 18 view structs, a hidden `_csrf` field in every form, a meta tag read by app.js, a middleware that copied the form field into a header, and a per-process token for LAN-bypass requests. The API's session fallback and the JSON sign-in existed for a UI that was never built on them. Every modern browser sends `Sec-Fetch-Site` and `Origin` on the requests that matter, which is what the standard library checks.

## Consequences

- The token is gone as a second layer: a browser that sends neither `Sec-Fetch-Site` nor `Origin` on a cross-site POST would not be stopped. Browsers since 2020 send both; `SameSite=Strict` on the session cookie also keeps it off cross-site requests.
- The qBittorrent adapter no longer reads `Referer`; `Origin` and `Sec-Fetch-Site` decide, as for the UI.
- auth no longer writes the UI's error pages through a JSON detour: `UIAuth.Middleware` and `CSRF` take a `Failure` callback, and the web UI passes its own renderer.

## Considered options

- **Keep the token, read it from the request context in templ.** Removes the struct fields but keeps the token machinery and its per-process LAN-bypass token. Rejected for the same protection at more code.
- **Keep the API's session fallback.** Nothing uses it, and it is the only reason disable auth and LAN bypass were ever near `/api/v1/`. Rejected.
