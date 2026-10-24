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
not authenticate anyone. The root client and the optional `outbound` policy
client have different destination guarantees, described below.

### Untrusted URLs use the separate `outbound` client

`outbound.NewPolicy` defaults to public HTTPS destinations. Host and CIDR
exceptions may permit private addresses or plaintext HTTP; they do not restrict
other public HTTPS destinations. Treat these exceptions as trusted deployment
configuration, never as values an untrusted caller may choose.

`Policy.Client` validates the complete URL and every DNS answer on each request,
including requests over a reused connection. On new connections it resolves and
checks the answers again, then dials a numeric address without a second resolver
step. Plaintext HTTP must be explicitly trusted in both checks. The client
disables environment proxies and all redirects. Userinfo, fragments, zone IDs,
non-HTTP schemes, noncanonical ports and malformed hosts are rejected.

Exceptions never allow unspecified, multicast or link-local addresses, the
metadata addresses `100.100.100.200`, `168.63.129.16` and `fd00:ec2::254`, or
the always-prohibited ranges listed in `outbound/policy.go` (including selected
IPv4/IPv6 translation and transition ranges). Other special-use ranges are
blocked unless explicitly trusted. This list is an application policy, not a
promise to recognize every present or future cloud-specific service address.

`Validate` on its own cannot close a DNS rebinding window: use the returned
client for the actual request. Replacing its `Transport` or `CheckRedirect`
can bypass protection. It uses the standard library's certificate validation;
the root client's TLS options do not configure this separate client. It does
not authenticate application responses, retry, restrict paths or methods,
limit response bodies, or isolate networks beyond the listed address policy.

Policy/configuration and DNS sentinel errors do not disclose their input, but
`http.Client.Do` wraps failures in a `url.Error` containing the requested URL.
Dial and TLS errors can also include peer details. Redact errors before exposing
or persisting them, especially when URLs carry query credentials.

The root `httpkit.NewClient` is still a general-purpose client: it does not
enforce this destination policy and retains its existing proxy and redirect
behavior. Existing consumers do not acquire SSRF protection merely by updating
the module.

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
