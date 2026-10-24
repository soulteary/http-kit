// Derived from OwlCron. See ../LICENSE-OWLCRON and ../NOTICE.

package outbound

import (
	"context"
	"net/http"
	"time"
)

type requireExplicitTrustKey struct{}

// Client returns a fresh client with its own connection pool. Reuse the returned
// client across requests and call CloseIdleConnections when it is no longer used.
// The timeout is ten seconds. Proxies and redirects are disabled, and TLS uses
// the standard library's certificate verification and protocol defaults.
//
// Every request is checked even when a connection is reused. New connections
// additionally resolve and check the destination immediately before dialing a
// numeric address. Plaintext HTTP requires explicit trust at both stages.
//
// Changing Transport or CheckRedirect can bypass these protections. The client
// does not impose response-size limits; callers must bound the bodies they read.
func (p *Policy) Client() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &policyTransport{policy: p, transport: transport},
	}
}

type policyTransport struct {
	policy    *Policy
	transport *http.Transport
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		closeRequestBody(req)
		return nil, ErrBlocked
	}
	u := req.URL
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		closeRequestBody(req)
		return nil, ErrBlocked
	}
	// URL.String may hide fields (notably User and Host when Opaque is set).
	// Reject those fields directly before validating the serialized URL, including
	// requests assembled as struct literals instead of through http.NewRequest.
	if err := t.policy.Validate(req.Context(), req.URL.String()); err != nil {
		closeRequestBody(req)
		return nil, err
	}
	// A Transport may dial asynchronously, or reuse a connection without dialing.
	// Validate above covers reuse; this private flag makes a subsequent HTTP dial
	// require explicit trust again if the DNS answer changed in between.
	ctx := context.WithValue(req.Context(), requireExplicitTrustKey{}, req.URL.Scheme == "http")
	return t.transport.RoundTrip(req.Clone(ctx))
}

func (t *policyTransport) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
}

func closeRequestBody(req *http.Request) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
}
