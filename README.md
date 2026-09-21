# http-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/http-kit/v2.svg)](https://pkg.go.dev/github.com/soulteary/http-kit/v2)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/http-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/http-kit)

[中文文档](README_CN.md)

A lightweight Go HTTP client library with TLS/mTLS support, automatic retry
with exponential backoff, and a hook for propagating context across services.

## Features

- **TLS/mTLS Support** - Full TLS configuration including CA certificates, client certificates for mutual TLS authentication
- **Automatic Retry** - Exponential backoff with jitter, over a policy that knows which requests are safe to replay
- **Context Propagation** - A `Propagator` hook applied to every request; OpenTelemetry lives in the `otelprop` subpackage
- **Configurable Options** - Flexible client configuration with sensible defaults
- **A Standard-Library-Only Root Package** - importing `httpkit` links nothing else

## Layout

The root package depends on nothing outside the standard library. Anything
needing a third-party module lives in a subpackage, so importing the root
package never links a library your service does not use:

| Package | Brings in |
|---------|-----------|
| `github.com/soulteary/http-kit/v2` | nothing — the standard library only |
| `github.com/soulteary/http-kit/v2/otelprop` | `go.opentelemetry.io/otel` |

Measured for a program importing only the root package, v1.5.0 against v2.0.0
(`CGO_ENABLED=0 go build -trimpath`, go1.27.0 linux/amd64):

| | v1.5.0 | v2.0.0 |
|---|---|---|
| Linked packages | 227 | **191** |
| Modules in the build | 8 | **1** |
| `// indirect` lines in your `go.mod` | 7 | **0** |
| Modules in your `go.sum` | 11 | **1** |
| Binary | 9,487,279 B | **7,918,888 B** (−16.5%) |

A program that *does* trace pays what it paid before: root package plus
`otelprop` is 228 linked packages and a 9,494,047-byte binary — one package
and 6,768 bytes more than v1.5.0.

## Requirements

- **Go 1.27+** (`go.mod` declares `go 1.27.0`)
- `go.opentelemetry.io/otel`, **only** if you import `otelprop`

## Installation

```bash
go get github.com/soulteary/http-kit/v2
```

Upgrading from v1? The import path changed and `Client.InjectTraceContext` is
gone — see [Upgrade Notes (v2.0.0)](#upgrade-notes-v200).

## Quick Start

### Basic HTTP Client

```go
import httpkit "github.com/soulteary/http-kit/v2"

// Create a simple client
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:   "https://api.example.com",
    Timeout:   10 * time.Second,
    UserAgent: "myservice/1.0",
})
if err != nil {
    log.Fatal(err)
}

// Build a request against the base URL and send it
req, err := client.NewRequest(ctx, http.MethodGet, "v1/users", nil)
if err != nil {
    log.Fatal(err)
}
resp, err := client.Do(req)
```

`BaseURL` is optional. If you already hold absolute URLs, pass one to
`NewRequest` — or skip the helper and build requests with `net/http`; `Do` and
`DoRequestWithRetry` take any `*http.Request`.

```go
client, _ := httpkit.NewClient(&httpkit.Options{Timeout: 10 * time.Second})
req, _ := client.NewRequest(ctx, http.MethodGet, "https://other.example.org/raw", nil)
```

### Client with TLS/mTLS

```go
// Server certificate verification against a custom CA
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://secure-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSServerName: "secure-api.example.com",
})

// Mutual TLS — both cert and key are required
client, err = httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://mtls-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSClientCert: "/path/to/client.crt",
    TLSClientKey:  "/path/to/client.key",
})
```

`NewClient` validates the combination and returns an error rather than building
a client that quietly does less than you asked:

- **`Transport` and the TLS options are mutually exclusive.** A caller-supplied
  `RoundTripper` carries its own TLS configuration; configure TLS on the
  transport itself.
- **`TLSClientCert` and `TLSClientKey` must be set together.** One without the
  other would produce a TLS config with no certificate in it.

When the TLS options are used, the transport is cloned from
`http.DefaultTransport`, so proxy support (`HTTPS_PROXY`), HTTP/2 and the
standard connection-pool limits are kept. TLS 1.2 is the floor.

Call `opts.Validate()` yourself if you want to check a configuration before
constructing a client.

### Automatic Retry

```go
client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// Default retry options: 3 retries, exponential backoff with jitter
req, _ := client.NewRequest(ctx, http.MethodGet, "/data", nil)
resp, err := client.DoRequestWithRetry(ctx, req, nil)

// Or customize
retryOpts := &httpkit.RetryOptions{
    MaxRetries:        5,
    RetryDelay:        200 * time.Millisecond,
    MaxRetryDelay:     5 * time.Second,
    BackoffMultiplier: 2.0,
    RetryableStatusCodes: []int{
        http.StatusTooManyRequests,
        http.StatusServiceUnavailable,
        http.StatusGatewayTimeout,
    },
}
resp, err = client.DoRequestWithRetry(context.Background(), req, retryOpts)
```

#### What gets retried

A request is retried only when all three hold:

1. **The method is idempotent, or opted in.** RFC 9110 idempotent methods —
   `GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`, `TRACE`, and an empty method
   (net/http treats that as `GET`) — are retried. `POST` and `PATCH` are
   attempted **once** unless the request carries an `Idempotency-Key` header:

   ```go
   req.Header.Set("Idempotency-Key", uuid.NewString())
   ```

2. **The body can be replayed.** The first attempt consumes and closes
   `req.Body`, so a retry needs `req.GetBody`. `http.NewRequest` populates it
   for the common in-memory body types (`*bytes.Buffer`, `*bytes.Reader`,
   `*strings.Reader`). A body built from an arbitrary `io.Reader` has no
   `GetBody`, and such a request is attempted once rather than replayed empty.

3. **The failure is transient.** A retryable status code, or a transport error
   that another attempt could plausibly fix. Permanent failures are not
   retried: certificate verification failures, an unsupported URL scheme, a
   cancelled caller context.

#### Backoff

The delay is `RetryDelay × BackoffMultiplier^attempt`, capped at
`MaxRetryDelay`, with up to 20% jitter so a fleet retrying after the same
upstream failure does not do so in lockstep. A multiplier below 1 means no
growth.

A `Retry-After` response header wins over the computed delay — including
`Retry-After: 0` and an HTTP-date already in the past, both of which mean
"retry now". It is still capped by `MaxRetryDelay` (unconditionally, a zero
ceiling included), and an out-of-range value saturates rather than wrapping, so
a malformed or adversarial upstream cannot step past your ceiling in either
direction.

The caller's context is attached to the request, so cancelling it aborts an
in-flight attempt, not just the sleep between attempts.

Discarded response bodies are drained so the connection returns to the pool,
bounded by bytes, by a timeout, and by the context — draining never holds up a
retry.

```go
// Inspect the policy directly if you need to
delay := retryOpts.CalculateRetryDelay(2)
retryable := retryOpts.IsRetryableError(err, resp.StatusCode)
retryable = retryOpts.IsRetryableErrorCtx(ctx, err, resp.StatusCode) // tells a
// per-attempt http.Client.Timeout apart from the caller's own deadline
```

### Context Propagation

A `Propagator` writes headers on every request the client sends. Configure it
once, on the client — there is nothing to remember at the call site, which is
what the old `InjectTraceContext` required and what made a forgotten call a
trace that silently stopped at the network boundary.

```go
type Propagator interface {
    Inject(ctx context.Context, h http.Header)
}
```

#### OpenTelemetry

```go
import (
    httpkit "github.com/soulteary/http-kit/v2"
    "github.com/soulteary/http-kit/v2/otelprop"
    "go.opentelemetry.io/otel"
)

client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL:    "https://api.example.com",
    Propagator: otelprop.Global(), // uses otel.GetTextMapPropagator()
})

// Create a span in your application
tracer := otel.Tracer("my-service")
ctx, span := tracer.Start(context.Background(), "api-call")
defer span.End()

// traceparent is injected automatically
req, _ := client.NewRequest(ctx, http.MethodGet, "/data", nil)
resp, err := client.Do(req)
```

`otelprop.Global()` resolves the global propagator **at each injection**, not
at construction time. That matters because `otel.SetTextMapPropagator` is
normally called from `main`, after any client a package-level constructor
built — capturing it early would freeze OpenTelemetry's no-op default and
inject nothing for the life of the process.

To pin one per client instead:

```go
Propagator: otelprop.New(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{}, propagation.Baggage{},
))
```

#### Anything else

Propagation that is not OpenTelemetry needs no subpackage — it is a function.
`MultiPropagator` composes several, in order.

```go
Propagator: httpkit.MultiPropagator(
    otelprop.Global(),
    httpkit.PropagatorFunc(func(ctx context.Context, h http.Header) {
        if id, ok := ctx.Value(requestIDKey).(string); ok {
            h.Set("X-Request-ID", id)
        }
    }),
),
```

The propagator runs on **every retry attempt**, because a retried request is a
new request on the wire. For a request you send some other way — through
`GetHTTPClient`, say — call `client.InjectContext(ctx, req)` yourself; it is a
no-op when no propagator is configured.

## API Reference

### Client Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `BaseURL` | `string` | `""` | Prefix `NewRequest` resolves a relative path against. Optional |
| `Timeout` | `time.Duration` | `10s` | Request timeout |
| `UserAgent` | `string` | `""` | User-Agent header value |
| `Transport` | `http.RoundTripper` | `nil` | Custom HTTP transport |
| `Propagator` | `Propagator` | `nil` | Injects headers into every request. See [Context Propagation](#context-propagation) |
| `TLSCACertFile` | `string` | `""` | Path to CA certificate file |
| `TLSClientCert` | `string` | `""` | Path to client certificate file (for mTLS) |
| `TLSClientKey` | `string` | `""` | Path to client private key file (for mTLS) |
| `TLSServerName` | `string` | `""` | Server name for TLS verification |
| `InsecureSkipVerify` | `bool` | `false` | Skip TLS certificate verification (not recommended) |

### Retry Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `MaxRetries` | `int` | `3` | Maximum number of retry attempts |
| `RetryDelay` | `time.Duration` | `100ms` | Initial delay between retries |
| `MaxRetryDelay` | `time.Duration` | `2s` | Maximum delay between retries |
| `BackoffMultiplier` | `float64` | `2.0` | Multiplier for exponential backoff |
| `RetryableStatusCodes` | `[]int` | `[408, 429, 500, 502, 503, 504]` | HTTP status codes that trigger retry |

### Client Methods

| Method | Description |
|--------|-------------|
| `NewClient(opts)` | Creates a new HTTP client with the given options |
| `NewRequest(ctx, method, ref, body)` | Builds a request against `BaseURL`, with the User-Agent applied |
| `ResolveURL(ref)` | The URL `NewRequest` would request for `ref` |
| `Do(req)` | Performs an HTTP request, applying the User-Agent and `Propagator` |
| `DoRequestWithRetry(ctx, req, retryOpts)` | Performs an HTTP request with automatic retry |
| `InjectContext(ctx, req)` | Applies the configured `Propagator` to a request sent some other way |
| `GetBaseURL()` | Returns the base URL |
| `GetHTTPClient()` | Returns the underlying `*http.Client` |

`ref` is either an absolute URL, used as it stands, or a path joined to
`BaseURL` with exactly one slash between the two; its query and fragment
survive. A relative `ref` with no `BaseURL` is an error rather than a request
to nowhere.

### Propagation

| Symbol | Description |
|--------|-------------|
| `Propagator` | `Inject(ctx, http.Header)` — the hook `Do` applies to every attempt |
| `PropagatorFunc` | Adapts a plain function to `Propagator` |
| `MultiPropagator(ps...)` | Applies each in order; nil entries are skipped, and no live entry yields `nil` |
| `otelprop.Global()` | OpenTelemetry's global propagator, resolved at each injection |
| `otelprop.New(p)` | A specific `propagation.TextMapPropagator` |

### Helpers

| Function | Description |
|----------|-------------|
| `DefaultOptions()` | `Options` with the defaults in the table above |
| `DefaultRetryOptions()` | `RetryOptions` with the defaults in the table above |
| `(*Options).Validate()` | Check a configuration without building a client |
| `(*RetryOptions).CalculateRetryDelay(attempt)` | The backoff delay for an attempt, before jitter |
| `(*RetryOptions).IsRetryableError(err, statusCode)` | Whether a failure is worth retrying |
| `(*RetryOptions).IsRetryableErrorCtx(ctx, err, statusCode)` | The same, distinguishing a per-attempt timeout from the caller's deadline |

## Project Structure

```
http-kit/
├── doc.go             # Package documentation
├── client.go          # Client, Options, TLS/mTLS, request building
├── propagator.go      # The Propagator hook
├── retry.go           # Retry policy, backoff, body replay, Retry-After
├── otelprop/          # OpenTelemetry propagation — the only package that links otel
│   └── otelprop.go
├── example_test.go    # Runnable examples, verified by go test
├── regression_test.go # Gate: the root package must stay standard-library-only
├── CHANGELOG.md
├── SECURITY.md
├── go.mod             # Module definition
└── LICENSE            # Apache 2.0 license
```

## Security Features

| Feature | Description |
|---------|-------------|
| **TLS Verification** | Supports custom CA certificates for server verification |
| **mTLS Authentication** | Client certificate support for mutual TLS |
| **Server Name Verification** | Configurable TLS server name for SNI |
| **Secure Defaults** | TLS verification enabled by default; TLS 1.2 is the minimum version |
| **No Silent Downgrade** | `Transport` together with TLS options is rejected, rather than dropping the TLS config |
| **Replay Safety** | `POST`/`PATCH` are attempted once unless an `Idempotency-Key` is present |
| **Bounded Backoff** | A `Retry-After` header cannot exceed `MaxRetryDelay`, and an out-of-range value saturates instead of wrapping negative |
| **Explicit Propagation** | Headers are sent only when you configure a `Propagator`; see [SECURITY.md](SECURITY.md) on pointing one at a third party |

## Upgrade Notes (v2.0.0)

The import path changed, and one method was removed. Everything else is
additive.

- **The module path is `github.com/soulteary/http-kit/v2`.** Go encodes the
  major version in the import path, so every user must update it — including
  services that never traced anything.

  ```diff
  -import "github.com/soulteary/http-kit"
  +import httpkit "github.com/soulteary/http-kit/v2"
  ```

  ```bash
  go get github.com/soulteary/http-kit/v2
  ```

- **`Client.InjectTraceContext(ctx, req)` is gone.** It called
  `otel.GetTextMapPropagator` directly, which is why every user of this
  package linked OpenTelemetry. Set a propagator on the client instead, and
  delete the per-request call:

  ```diff
   client, _ := httpkit.NewClient(&httpkit.Options{
       BaseURL: "https://api.example.com",
  +    Propagator: otelprop.Global(),
   })

   req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
  -client.InjectTraceContext(ctx, req)
   resp, err := client.DoRequestWithRetry(ctx, req, retryOpts)
  ```

  It was not kept as a deprecated shim, and not only because a shim would have
  to import the dependency it was meant to remove. Keeping the *name* would
  have been worse: your code would still compile and would silently stop
  propagating anything until `Options.Propagator` was set. A compiler error is
  the point.

  If you are a **library** calling http-kit, consider accepting a
  `httpkit.Propagator` from your own caller and passing it through, rather
  than importing `otelprop` yourself — then the application decides, and your
  users who do not trace do not link OpenTelemetry either.

- **`Client.InjectContext(ctx, req)`** is the direct replacement for a request
  you send some other way. It applies whatever `Options.Propagator` holds, and
  is a no-op when that is nil.

- **`BaseURL` is no longer required.** `NewClient` used to reject a client
  without one, even though nothing in the package read it except
  `GetBaseURL()`. If you passed a placeholder to get past that check, delete
  it. If you asserted on the `"base URL is required"` error, that assertion
  now fails.

- **`Client.NewRequest(ctx, method, ref, body)`** replaces
  `http.NewRequest(method, client.GetBaseURL()+"/path", body)`. It joins with
  exactly one slash, keeps the query string, applies the User-Agent and leaves
  `GetBody` populated so the body stays replayable by `DoRequestWithRetry`.

- **`Do` no longer panics on a request with a nil `Header`.** A
  `*http.Request` built as a struct literal has one, and setting the
  User-Agent on it panicked before anything was sent.

## Upgrade Notes (v1.5.0)

Two of these change whether a request is sent at all, and one can turn a
working configuration into a startup error. No API was removed; one method was
added.

- **`Transport` plus any TLS option is now an error.** Setting `Transport`
  silently discarded every TLS option — the branch building the TLS config was
  never reached, so an mTLS client certificate was **never presented and nothing
  reported it**. `NewClient` now returns an error. If you hit this, move the TLS
  configuration onto your transport.
- **`TLSClientCert` without `TLSClientKey` is now an error.** It used to build a
  TLS config with no certificate in it.
- **Retried requests replay their body.** The same `*http.Request` was reused
  across attempts, and the first `Do` consumes and closes `req.Body` — so a
  retried `POST` or `PUT` sent an **empty body**, or failed with
  `ContentLength=N with Body length 0`. Retries now rewind through
  `req.GetBody`; a request whose body cannot be replayed is attempted once.
- **`POST` and `PATCH` are no longer retried by default.** They were, so a
  request that had already reached the server was duplicated. Add an
  `Idempotency-Key` header to opt a non-idempotent request back in. **If you
  relied on automatic `POST` retries, set that header.**
- **Permanent failures are no longer retried.** Certificate verification
  failures, an unsupported URL scheme and a cancelled context were all retried,
  which only delayed the failure.
- **The backoff is actually exponential.** It computed
  `RetryDelay × (attempt+1) × BackoffMultiplier`, which grows linearly whatever
  the multiplier — 200ms, 400ms, 600ms for a multiplier of 2 — despite the field
  name. It is now `RetryDelay × BackoffMultiplier^attempt`, **with up to 20%
  jitter**. Expect different (and larger) delays at higher attempt numbers, and
  don't assert on exact values.
- **`Retry-After` is honoured**, capped by `MaxRetryDelay`. An explicit
  `Retry-After: 0` and an elapsed HTTP-date both mean "retry now". An
  out-of-range value saturates instead of wrapping to a negative duration that
  would bypass the ceiling entirely.
- **The context aborts an in-flight attempt.** The `ctx` argument was only used
  for the sleeps between attempts and never attached to the request.
- **An empty `Method` is treated as `GET`.** net/http documents that for client
  requests; it was classified non-idempotent, so a 408, 429 or 5xx on what is in
  fact a `GET` was not retried.
- **The TLS transport keeps the standard defaults.** It was a bare
  `&http.Transport{}`: no `Proxy` (so `HTTPS_PROXY` stopped working), no
  `ForceAttemptHTTP2`, and 2 idle connections per host. It is cloned from
  `http.DefaultTransport` now, with a TLS 1.2 floor.
- **Requirements said Go 1.26**; `go.mod` requires `1.27.0`.

## Test Coverage

Run tests with coverage:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
