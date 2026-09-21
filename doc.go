// Package httpkit provides an HTTP client for talking to other services:
// TLS and mTLS configuration, retries with exponential backoff and jitter
// over a policy that knows which requests are safe to replay, and a hook for
// propagating cross-process context.
//
// # Layout
//
// The root package depends on nothing outside the standard library. It
// provides [Client], the retry policy in [RetryOptions], and the [Propagator]
// seam described below.
//
// Anything that needs a third-party module lives in a subpackage instead, so
// importing the root package never links a library the service does not use:
//
//   - github.com/soulteary/http-kit/v2/otelprop -- OpenTelemetry trace
//     context, and with it go.opentelemetry.io/otel.
//
// A service that makes HTTP calls but emits no spans pays nothing for
// tracing support existing; only importing the subpackage links it in.
// Measured for a program importing only the root package, v1.5.0 against
// v2.0.0: 36 fewer linked packages, 7 fewer modules, a 16.5% smaller binary,
// and an empty indirect requirement block in its own go.mod.
//
// # Getting started
//
//	client, err := httpkit.NewClient(&httpkit.Options{
//		BaseURL:   "https://api.example.com",
//		Timeout:   5 * time.Second,
//		UserAgent: "myservice/1.0",
//	})
//
//	req, err := client.NewRequest(ctx, http.MethodGet, "v1/users", nil)
//	resp, err := client.DoRequestWithRetry(ctx, req, httpkit.DefaultRetryOptions())
//
// [Options.BaseURL] is optional. A caller that already holds absolute URLs
// passes them to [Client.NewRequest] unchanged, or skips it and builds
// requests with net/http.
//
// # What gets retried
//
// Retrying is not free of consequences, so [Client.DoRequestWithRetry]
// replays a request only when replaying it is safe: the method must be
// idempotent by RFC 9110, or carry an Idempotency-Key header, and the body
// must be replayable (nil, http.NoBody, or a GetBody the request carries --
// http.NewRequest and [Client.NewRequest] populate it for the common
// in-memory body types). A streaming body is sent once.
//
// Permanent failures -- a certificate that will not verify, an unsupported
// scheme, a cancelled context -- are not retried, because the only thing a
// second attempt adds is delay. Retry-After is honoured, and capped at
// [RetryOptions.MaxRetryDelay] like any other delay. A zero MaxRetryDelay is a
// zero ceiling, not the absence of one.
//
// # Propagating context across services
//
// [Propagator] is how trace headers, baggage or a request ID reach the wire.
// [Client.Do] applies the configured one to every attempt, so no call site has
// to remember to:
//
//	httpkit.NewClient(&httpkit.Options{
//		BaseURL:    "https://api.example.com",
//		Propagator: otelprop.Global(),
//	})
//
// This replaces v1's Client.InjectTraceContext, which called
// otel.GetTextMapPropagator directly. That coupled every user of this package
// to OpenTelemetry, and left a forgotten call site as a trace that silently
// stopped at the network boundary. Anything that is not OpenTelemetry is a
// [PropagatorFunc]; [MultiPropagator] composes several.
package httpkit
