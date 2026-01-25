package httpkit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Client is a generic HTTP client with common functionality
type Client struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
}

// Options for creating a new Client
type Options struct {
	BaseURL            string
	Timeout            time.Duration
	UserAgent          string
	Transport          http.RoundTripper
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

// Validate validates the options
func (o *Options) Validate() error {
	if o.BaseURL == "" {
		return fmt.Errorf("base URL is required")
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
	if opts.TLSCACertFile != "" || opts.TLSClientCert != "" || opts.InsecureSkipVerify {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: opts.InsecureSkipVerify,
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
		httpClient.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    opts.BaseURL,
		userAgent:  opts.UserAgent,
	}, nil
}

// Do performs an HTTP request
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return c.httpClient.Do(req)
}

// InjectTraceContext injects OpenTelemetry trace context into request headers
func (c *Client) InjectTraceContext(ctx context.Context, req *http.Request) {
	propagator := otel.GetTextMapPropagator()
	propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// GetBaseURL returns the base URL
func (c *Client) GetBaseURL() string {
	return c.baseURL
}

// GetHTTPClient returns the underlying http.Client
func (c *Client) GetHTTPClient() *http.Client {
	return c.httpClient
}
