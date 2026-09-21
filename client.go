package httpkit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is a generic HTTP client with common functionality
type Client struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
	propagator Propagator
}

// Options for creating a new Client
type Options struct {
	// BaseURL is the prefix Client.NewRequest resolves a relative path
	// against, and what Client.GetBaseURL returns. It is optional: a client
	// built without one still serves Do and DoRequestWithRetry for requests
	// carrying absolute URLs, which is how callers that hold full URLs of
	// their own already used this package. Requiring it only made them pass a
	// placeholder that nothing read.
	BaseURL string

	Timeout   time.Duration
	UserAgent string
	Transport http.RoundTripper

	// Propagator injects cross-process context -- trace headers, baggage, a
	// request ID -- into every request Do sends. Leave it nil to send none.
	//
	// For OpenTelemetry, use the otelprop subpackage:
	// Propagator: otelprop.Global(). Nothing in this package imports
	// OpenTelemetry, so a client that does not trace does not link it.
	Propagator Propagator

	TLSCACertFile      string // For verifying server certificate
	TLSClientCert      string // Client certificate file for mTLS
	TLSClientKey       string // Client private key file for mTLS
	TLSServerName      string // Server name for TLS verification
	InsecureSkipVerify bool   // Skip TLS certificate verification (not recommended)
}

// DefaultOptions returns default options
func DefaultOptions() *Options {
	return &Options{
		Timeout: 10 * time.Second,
	}
}

// hasTLSSettings reports whether any TLS-related option was set.
func (o *Options) hasTLSSettings() bool {
	return o.TLSCACertFile != "" || o.TLSClientCert != "" || o.TLSClientKey != "" ||
		o.TLSServerName != "" || o.InsecureSkipVerify
}

// Validate validates the options
func (o *Options) Validate() error {
	// BaseURL is deliberately not checked; see its field documentation.

	// A caller-supplied Transport carries its own TLS configuration. Silently
	// dropping the TLS options here is how an mTLS client certificate ends up
	// never being presented, with no error anywhere, so the conflict is
	// reported instead. Configure TLS on the Transport itself.
	if o.Transport != nil && o.hasTLSSettings() {
		return fmt.Errorf("transport and TLS options are mutually exclusive: configure TLS on the Transport itself")
	}
	if (o.TLSClientCert == "") != (o.TLSClientKey == "") {
		return fmt.Errorf("TLSClientCert and TLSClientKey must be set together")
	}
	return nil
}

// NewClient creates a new generic HTTP client
func NewClient(opts *Options) (*Client, error) {
	if opts == nil {
		opts = DefaultOptions()
	}

	if err := opts.Validate(); err != nil {
		return nil, err
	}

	// Configure TLS
	var tlsConfig *tls.Config
	if opts.hasTLSSettings() {
		tlsConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // opt-in, documented as not recommended
			ServerName:         opts.TLSServerName,
		}

		// Load CA certificate for server verification
		if opts.TLSCACertFile != "" {
			caCert, err := os.ReadFile(opts.TLSCACertFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read CA certificate: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to parse CA certificate")
			}
			tlsConfig.RootCAs = caCertPool
		}

		// Load client certificate for mTLS
		if opts.TLSClientCert != "" && opts.TLSClientKey != "" {
			cert, err := tls.LoadX509KeyPair(opts.TLSClientCert, opts.TLSClientKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}

	// Create HTTP client
	httpClient := &http.Client{
		Timeout: opts.Timeout,
	}

	if opts.Transport != nil {
		httpClient.Transport = opts.Transport
	} else if tlsConfig != nil {
		transport := standardTransport()
		transport.TLSClientConfig = tlsConfig
		httpClient.Transport = transport
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    opts.BaseURL,
		userAgent:  opts.UserAgent,
		propagator: opts.Propagator,
	}, nil
}

// standardTransport returns an *http.Transport carrying the standard library's
// defaults, ready to have TLSClientConfig set on it.
//
// A zero-value http.Transport has no Proxy (so HTTPS_PROXY stops working), no
// ForceAttemptHTTP2, and a pool limit of 2 idle connections per host --
// configuring TLS should not silently cost all of that, so the defaults are
// taken from http.DefaultTransport when it still is one. That is a mutable
// package-level variable, though: an application that replaces it with a
// tracing or metrics wrapper must not thereby lose the ability to build a TLS
// client, so the same defaults are reconstructed when the assertion fails.
func standardTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewRequest builds a request against the client's base URL, with the
// client's User-Agent already applied.
//
// ref is either an absolute URL, used as it stands, or a path that is appended
// to BaseURL with exactly one slash between the two; its query string and
// fragment are kept. Building the URL by hand -- which is what this package
// left callers to do, `client.GetBaseURL()+"/users"` -- produces
// "https://api.example.com/v1//users" the moment either side changes its mind
// about the slash.
//
// It is a convenience, not a requirement: a request built any other way still
// works with Do and DoRequestWithRetry.
func (c *Client) NewRequest(ctx context.Context, method, ref string, body io.Reader) (*http.Request, error) {
	target, err := c.ResolveURL(ref)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return req, nil
}

// ResolveURL returns the URL NewRequest would request for ref. See NewRequest
// for the rule.
//
// An absolute ref is returned unchanged, so a caller holding full URLs of its
// own can route them through the same path as relative ones. A relative ref
// with no BaseURL to resolve against is an error rather than a request to
// nowhere.
func (c *Client) ResolveURL(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", ref, err)
	}
	if u.IsAbs() {
		return ref, nil
	}
	if c.baseURL == "" {
		return "", fmt.Errorf("cannot resolve %q: it is not an absolute URL and the client has no BaseURL", ref)
	}
	if ref == "" {
		return c.baseURL, nil
	}
	return strings.TrimSuffix(c.baseURL, "/") + "/" + strings.TrimPrefix(ref, "/"), nil
}

// Do performs an HTTP request, applying the client's User-Agent and
// Propagator first.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// A *http.Request built as a struct literal rather than by
	// http.NewRequest has a nil Header, and Header.Set on a nil map panics --
	// on the User-Agent line below, before anything was sent. Reading one is
	// fine, which is why this went unnoticed.
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	// The request's own context, which DoRequestWithRetry has already set to
	// the caller's. Injecting here rather than leaving it to the caller is the
	// point of the hook: the old InjectTraceContext had to be remembered at
	// every call site, and a forgotten one broke a trace silently.
	c.InjectContext(req.Context(), req)
	return c.httpClient.Do(req)
}

// InjectContext applies the configured Propagator to req's headers, and is a
// no-op when there is none.
//
// Do calls this on every attempt, so a request sent through Do or
// DoRequestWithRetry already carries the headers. Call it directly only for a
// request sent some other way -- through GetHTTPClient, say.
//
// It replaces InjectTraceContext, which called otel.GetTextMapPropagator
// directly. Keeping that name for this behaviour would have been worse than
// removing it: code that compiled unchanged would have silently stopped
// propagating anything until Options.Propagator was set. See
// github.com/soulteary/http-kit/v2/otelprop.
func (c *Client) InjectContext(ctx context.Context, req *http.Request) {
	if c.propagator == nil || req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	c.propagator.Inject(ctx, req.Header)
}

// GetBaseURL returns the base URL
func (c *Client) GetBaseURL() string {
	return c.baseURL
}

// GetHTTPClient returns the underlying http.Client
func (c *Client) GetHTTPClient() *http.Client {
	return c.httpClient
}
