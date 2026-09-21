# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Report it through GitHub's private vulnerability reporting: go to the
[Security tab](https://github.com/soulteary/http-kit/security) and choose
**Report a vulnerability**. That opens a private advisory visible only to the
maintainers.

If you do not see that option, open a normal issue saying only that you have a
security report and need a private channel — **no details, no reproducer** —
and a maintainer will arrange one.

Please include, once you have a private channel:

- the affected version or commit,
- what an attacker can do, and what they need in order to do it,
- a reproducer, if you have one.

Expect an acknowledgement within a few days. This is a small
volunteer-maintained project, so please allow reasonable time for a fix before
disclosing publicly.

## Supported versions

| Version | Supported |
| ------- | --------- |
| 2.x     | ✅ |
| 1.x     | ❌ superseded by 2.x — see the [migration notes](CHANGELOG.md#200--2026-09-21) |
| 0.x     | ❌ |

Fixes land on the latest minor of the current major. There are no long-term
support branches.

## What this library does, and what it does not

http-kit is an outbound HTTP client. It does not serve requests, and it does
not authenticate anyone. Its security-relevant behaviour is concentrated in
three places, and all three depend on how *you* configure it.

### TLS verification is on unless you turn it off

`Options.InsecureSkipVerify` disables certificate verification for every
request the client makes, which makes the connection trivially
machine-in-the-middle-able. It exists for talking to a development server with
a self-signed certificate. If you need to trust a private CA in production,
use `TLSCACertFile` instead — it adds a root, it does not remove the check.

A client built with any TLS option sets `MinVersion: tls.VersionTLS12`.

### A custom Transport takes over TLS entirely

`Options.Transport` and the `TLS*` options are mutually exclusive, and
`Validate` rejects the combination. This is deliberate: the TLS options used
to be dropped silently when a `Transport` was supplied, so an mTLS client
certificate was configured, never presented, and nothing said so. If you
supply a `Transport`, configure its `TLSClientConfig` yourself.

`TLSClientCert` and `TLSClientKey` must be set together, for the same reason —
half a configuration is not a working one.

### Retries repeat a request, so they are restricted

`DoRequestWithRetry` replays a request only when the method is idempotent by
RFC 9110, or carries an `Idempotency-Key` header. A POST or PATCH without one
is sent exactly once. Adding that header to a request whose handler is not
actually idempotent is how a retry becomes a duplicate charge.

`MaxRetryDelay` bounds every wait, including one a server asks for with
`Retry-After` — an unbounded `Retry-After` is a remote party suspending your
call for as long as it likes. Note that a zero `MaxRetryDelay` is a zero
ceiling, not the absence of one, and retries then fire as fast as the network
allows.

### Propagation sends your context to whoever you call

`Options.Propagator` writes headers on every outbound request, including
requests to third parties. Trace IDs and baggage are metadata about your
system; `propagation.Baggage` in particular carries whatever key/value pairs
your services put in it. Do not attach a propagator that carries internal
identifiers to a client you point at someone else's API.

The client sends whatever the propagator writes and inspects none of it.
