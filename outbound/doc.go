// Package outbound provides a standard-library-only HTTP client for destinations
// supplied by users, such as webhook URLs. It validates every request and the
// addresses actually dialed, including requests that reuse an existing connection.
//
// The default policy allows public HTTPS destinations. Exact host and CIDR
// allowlists may additionally trust private or plaintext HTTP destinations, but
// never enable metadata endpoints, link-local, multicast, unspecified, or selected
// address-translation ranges. An allowlist is an exception to the public-only
// default, not a restriction that disables other public HTTPS destinations.
//
// Clients returned by Policy.Client disable environment proxies and all redirects.
// Use that client for the actual request: Validate alone is a check-then-use and
// cannot prevent DNS rebinding. Keep the client's transport and redirect policy
// intact. This package does not retry requests, authenticate destinations, limit
// response bodies, or redact net/http's URL-bearing errors.
//
// The policy originated in OwlCron. Its MIT license and attribution are retained
// in LICENSE-OWLCRON and NOTICE at the module root.
package outbound
