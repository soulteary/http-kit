# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Because Go encodes the major version in the import path, every major release
also changes the module path. The current one is
`github.com/soulteary/http-kit/v2`.

## [Unreleased]

## [2.0.0] — 2026-09-21

### Changed — BREAKING

- **OpenTelemetry moved to the `otelprop` subpackage, behind a `Propagator`
  hook.** The root package no longer imports `go.opentelemetry.io/otel`, so a
  service that makes HTTP calls but emits no spans no longer links it.

  Measured against v1.5.0 for a program importing only the root package
  (`CGO_ENABLED=0 go build -trimpath`, go1.27.0 linux/amd64, both consumers
  resolved through a module proxy and `go mod tidy`-ed):

  |  | v1.5.0 | v2.0.0 |
  |---|---|---|
  | Linked packages | 227 | **191** |
  | Modules in the build | 8 | **1** |
  | `// indirect` lines in the consumer's `go.mod` | 7 | **0** |
  | Modules in the consumer's `go.sum` | 11 | **1** |
  | Binary | 9,487,279 B | **7,918,888 B** (−16.5%) |

  The root package now depends on nothing outside the standard library, which
  is what empties that indirect block: `cespare/xxhash`, `go-logr/logr`,
  `go-logr/stdr`, `auto/sdk`, `otel`, `otel/metric` and `otel/trace` all leave
  the consumer's `go.mod`, and `google/go-cmp`, `stretchr/testify` and
  `go.yaml.in/yaml/v3` leave its `go.sum` along with them.

  A program that *does* trace pays what it paid before: importing the root
  package and `otelprop` gives 228 linked packages and a 9,494,047-byte
  binary, one package and 6,768 bytes more than v1.5.0 — the subpackage
  itself.

  | Removed | Replacement |
  |---|---|
  | `Client.InjectTraceContext(ctx, req)` | `Options.Propagator` + `otelprop.Global()`, or `Client.InjectContext(ctx, req)` |

  Keeping `InjectTraceContext` as a deprecated shim was not an option, and not
  only for the usual reason that a shim has to import the dependency it was
  meant to remove. Keeping the *name* with the new behaviour would have been
  worse: code that compiled unchanged would have silently stopped propagating
  anything until `Options.Propagator` was set. A removed method is a compiler
  error; a silent trace that stops at the network boundary is found weeks
  later, if at all.

  A subpackage is enough; OpenTelemetry does not need its own module. Module
  graph pruning keeps a requirement that no imported package needs out of the
  consumer's `go.mod` and `go.sum` entirely.

  Two things it does **not** do, both verified rather than assumed:

  - Minimal version selection still propagates the floor. A consumer that
    imports only the root package has no otel in its `go.mod`, its `go.sum` or
    its binary, but `go mod graph` and `go list -m all` still show
    `go.opentelemetry.io/otel v1.46.0`, reached through this module's own
    `go.mod`. If that consumer later adds otel for its own reasons, v1.46.0 is
    its lower bound.
  - It does not make the dependency optional at build time in any other sense.
    Importing `otelprop` links all of it.

  **The module path is therefore now `github.com/soulteary/http-kit/v2`**, by
  Go's import compatibility rule. Every user must update the import path,
  including services that never traced anything and are otherwise unaffected.

- **`Options.BaseURL` is no longer required.** `Validate` returned "base URL is
  required" for a client that had one but never used it: nothing in this
  package read `baseURL` except `GetBaseURL`. Callers holding absolute URLs of
  their own passed a placeholder to get past the check — parser-kit's reads
  `BaseURL: "http://localhost", // Placeholder, not used since we use full URLs`.
  A configuration that was previously rejected is now accepted, which is a
  behaviour change for anyone asserting on that error.

### Added

- The `otelprop` subpackage.
  - `otelprop.Global()` resolves `otel.GetTextMapPropagator` at each
    injection, which is what `InjectTraceContext` did and is the part that
    matters: `otel.SetTextMapPropagator` is normally called from `main`, after
    any client a package-level constructor built. A propagator captured at
    construction time would freeze OpenTelemetry's no-op default and inject
    nothing, for the life of the process, with nothing to see anywhere.
  - `otelprop.New(p)` pins a specific `propagation.TextMapPropagator`, for a
    service that configures propagation per client rather than globally. It
    takes OpenTelemetry's own interface, so `TraceContext{}`, `Baggage{}` and
    any composite of them work unchanged. A nil `p` yields a nil
    `httpkit.Propagator` rather than a non-nil interface that panics on the
    first request.
- `httpkit.Propagator`, `httpkit.PropagatorFunc` and `httpkit.MultiPropagator`
  in the root package. `Propagator` takes an `http.Header`, not a
  `*http.Request`: injecting context means setting headers, and an interface
  that asks for no more than that is satisfied by a plain function.
  Propagation that is not OpenTelemetry — a tenant header, a request ID, B3 —
  needs no subpackage at all.
- `Options.Propagator`, applied by `Client.Do` on **every attempt**. A retried
  request is a new request on the wire, so injecting once before the retry
  loop would leave every attempt after the first carrying the first attempt's
  context.
- `Client.InjectContext(ctx, req)`, for a request sent some other way — through
  `GetHTTPClient`, say. It is a no-op when no propagator is configured, so it
  needs no guard at the call site.
- `Client.NewRequest(ctx, method, ref, body)` and `Client.ResolveURL(ref)`.
  `ref` is either an absolute URL, used as it stands, or a path joined to
  `BaseURL` with exactly one slash; its query and fragment survive. The
  documented way to build a request was `client.GetBaseURL()+"/users"`, which
  produces `https://api.example.com/v1//users` the moment either side changes
  its mind about the slash. `NewRequest` also applies the client's User-Agent
  and keeps `GetBody` populated, so a body built through it stays replayable
  by `DoRequestWithRetry`.
- Runnable examples that `go test` verifies, in both packages, so they cannot
  drift from the API.
- A package doc in `doc.go` describing the layout, what is retried and why,
  and the propagation hook.
- `CHANGELOG.md` and `SECURITY.md`.
- `.github/workflows/release.yml`. Seven tags exist with nothing having checked
  any of them, and the mistake a `/vN` module path invites — a `vN` tag on a
  commit whose `go.mod` says something else, leaving the tag unfetchable — is
  caught at tag time or not at all. It runs on a `v*` tag (and on demand): the
  module path must carry the tag's major version, with v0 and v1 taking no
  suffix, and both READMEs' `go get` line must name that same path. Then the
  CI gate against the tagged commit — gofmt, `go mod tidy` cleanliness, vet,
  golangci-lint, `go test -race` with coverage, and govulncheck. Verification
  only: it publishes nothing and takes no write permissions.
- `.github/dependabot.yml`. Weekly gomod and github-actions updates, minor and
  patch grouped into one PR, majors left separate — for this module a
  dependency major is a judgement call. The release gate is an action too, so
  a silently stale action would be a stale release check.

### Fixed

- **`Client.Do` no longer panics on a request with no header map.** A
  `*http.Request` built as a struct literal rather than by `http.NewRequest`
  has a nil `Header`, and `Header.Set` on a nil map panics — on the User-Agent
  line, before anything was sent. Reading a nil map is fine, which is why the
  `Get` on the line above never showed it.
- The OpenTelemetry tests asserted almost nothing. They never established a
  span context, so no propagator could have written a `traceparent` even if
  one had been installed; two of the three subtests ended in a comment saying
  headers "may or may not be set". Their replacements in `otelprop` pin the
  exact `traceparent` value, the composite trace-context-plus-baggage case,
  and the late resolution of the global propagator.

### Added — tests

- `TestRootPackageImportsOnlyTheStandardLibrary` shells out to `go list -deps`
  and fails on any third-party import reaching the root package. This release
  is worth nothing the moment one comes back, and a dependency that arrives
  three levels down is not something a reading of the source will catch.
  Reverse-verified: adding `_ "go.opentelemetry.io/otel"` to `client.go` makes
  it name eleven packages.
- `TestOtelpropStillCarriesOpenTelemetry`, the other half — a split is only
  worth anything if the functionality moved rather than disappeared.

### Dependencies

- No upgrades were available. `go.opentelemetry.io/otel` v1.46.0 and the
  go-logr, `auto/sdk` and `cespare/xxhash` modules behind it are already the
  newest releases; the only newer otel tag is `v1.47.0-rc.1`, a release
  candidate. The footprint above comes entirely from where the dependency is
  imported from, not from which version of it.

## [1.5.0] — 2026-09-12

### Fixed

- Retries are replayable. A retried request reused the same `*http.Request`,
  whose body the first attempt had already consumed and closed, so a retried
  POST or PUT sent an empty body — or failed outright with
  `ContentLength=N with Body length 0`. Bodies are rewound through `GetBody`,
  and a request that cannot be rewound is attempted once.
- Only idempotent requests are retried. POST and PATCH are attempted once
  unless they carry an `Idempotency-Key` header.
- A caller-supplied `Transport` no longer silently discards the TLS options —
  an mTLS client certificate that was never presented, with no error anywhere.
  The combination is a `Validate` error, as is a half-configured cert/key pair.
- Backoff is exponential rather than linear, with up to 20% jitter.
- Permanent failures — certificate verification, an unsupported scheme, a
  cancelled context — are no longer retried the full `MaxRetries` times.
- `Retry-After` is honoured, capped unconditionally at `MaxRetryDelay`, and
  saturated before conversion so a large value cannot wrap to a negative
  duration and fire immediately.
- Discarded response bodies are drained so their connection returns to the
  pool, bounded by bytes, by a timeout and by the context — the drain never
  holds up a retry.
- The context passed to `DoRequestWithRetry` is applied to the request itself,
  so cancelling it aborts an in-flight attempt rather than only the sleep.

## [1.4.0] — 2026-08-27

### Changed

- `go.mod` declares `go 1.27.0`.

## [1.3.0] — 2026-08-26

### Changed

- Dependency refresh.

## [1.2.0] — 2026-08-12

### Changed

- Dependency refresh.

## [1.1.0] — 2026-03-06

### Changed

- Dependency refresh.

## [1.0.0] — 2026-01-25

Initial release: the client with TLS and mTLS options, retry with backoff, and
OpenTelemetry trace-context injection.

[Unreleased]: https://github.com/soulteary/http-kit/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/soulteary/http-kit/compare/v1.5.0...v2.0.0
[1.5.0]: https://github.com/soulteary/http-kit/compare/v1.4.0...v1.5.0
[1.4.0]: https://github.com/soulteary/http-kit/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/soulteary/http-kit/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/soulteary/http-kit/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/soulteary/http-kit/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/soulteary/http-kit/releases/tag/v1.0.0
