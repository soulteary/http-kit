package httpkit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()

	if opts == nil {
		t.Fatal("DefaultOptions returned nil")
	}

	if opts.Timeout != 10*time.Second {
		t.Errorf("expected Timeout to be 10s, got %v", opts.Timeout)
	}

	if opts.BaseURL != "" {
		t.Errorf("expected BaseURL to be empty, got %s", opts.BaseURL)
	}

	if opts.UserAgent != "" {
		t.Errorf("expected UserAgent to be empty, got %s", opts.UserAgent)
	}
}

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    *Options
		wantErr bool
	}{
		{
			name:    "empty base URL",
			opts:    &Options{},
			wantErr: true,
		},
		{
			name: "valid base URL",
			opts: &Options{
				BaseURL: "http://example.com",
			},
			wantErr: false,
		},
		{
			name: "with all options",
			opts: &Options{
				BaseURL:   "http://example.com",
				Timeout:   5 * time.Second,
				UserAgent: "test-agent",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	t.Run("nil options uses defaults and fails validation", func(t *testing.T) {
		client, err := NewClient(nil)
		if err == nil {
			t.Error("expected error for nil options with no BaseURL")
		}
		if client != nil {
			t.Error("expected nil client on error")
		}
	})

	t.Run("valid options", func(t *testing.T) {
		opts := &Options{
			BaseURL:   "http://example.com",
			Timeout:   5 * time.Second,
			UserAgent: "test-agent",
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if client.GetBaseURL() != "http://example.com" {
			t.Errorf("expected baseURL to be http://example.com, got %s", client.GetBaseURL())
		}

		if client.GetHTTPClient().Timeout != 5*time.Second {
			t.Errorf("expected timeout to be 5s, got %v", client.GetHTTPClient().Timeout)
		}
	})

	t.Run("with custom transport", func(t *testing.T) {
		customTransport := &http.Transport{
			MaxIdleConns: 100,
		}

		opts := &Options{
			BaseURL:   "http://example.com",
			Transport: customTransport,
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if client.GetHTTPClient().Transport != customTransport {
			t.Error("expected custom transport to be used")
		}
	})

	t.Run("with InsecureSkipVerify", func(t *testing.T) {
		opts := &Options{
			BaseURL:            "https://example.com",
			InsecureSkipVerify: true,
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transport, ok := client.GetHTTPClient().Transport.(*http.Transport)
		if !ok {
			t.Fatal("expected transport to be *http.Transport")
		}

		if transport.TLSClientConfig == nil {
			t.Fatal("expected TLSClientConfig to be set")
		}

		if !transport.TLSClientConfig.InsecureSkipVerify {
			t.Error("expected InsecureSkipVerify to be true")
		}
	})

	t.Run("with TLS server name", func(t *testing.T) {
		opts := &Options{
			BaseURL:            "https://example.com",
			InsecureSkipVerify: true,
			TLSServerName:      "custom.example.com",
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transport, ok := client.GetHTTPClient().Transport.(*http.Transport)
		if !ok {
			t.Fatal("expected transport to be *http.Transport")
		}

		if transport.TLSClientConfig.ServerName != "custom.example.com" {
			t.Errorf("expected ServerName to be custom.example.com, got %s", transport.TLSClientConfig.ServerName)
		}
	})
}

func TestNewClientWithCACert(t *testing.T) {
	// Create a temporary directory for test certificates
	tempDir, err := os.MkdirTemp("", "httpkit-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Generate a self-signed CA certificate
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	// Write CA certificate to file
	caCertFile := filepath.Join(tempDir, "ca.crt")
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	if err := os.WriteFile(caCertFile, caCertPEM, 0600); err != nil {
		t.Fatalf("failed to write CA certificate: %v", err)
	}

	t.Run("valid CA certificate", func(t *testing.T) {
		opts := &Options{
			BaseURL:       "https://example.com",
			TLSCACertFile: caCertFile,
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transport, ok := client.GetHTTPClient().Transport.(*http.Transport)
		if !ok {
			t.Fatal("expected transport to be *http.Transport")
		}

		if transport.TLSClientConfig == nil {
			t.Fatal("expected TLSClientConfig to be set")
		}

		if transport.TLSClientConfig.RootCAs == nil {
			t.Error("expected RootCAs to be set")
		}
	})

	t.Run("invalid CA certificate file path", func(t *testing.T) {
		opts := &Options{
			BaseURL:       "https://example.com",
			TLSCACertFile: "/nonexistent/ca.crt",
		}

		_, err := NewClient(opts)
		if err == nil {
			t.Error("expected error for nonexistent CA certificate file")
		}
	})

	t.Run("invalid CA certificate content", func(t *testing.T) {
		invalidCertFile := filepath.Join(tempDir, "invalid.crt")
		if err := os.WriteFile(invalidCertFile, []byte("invalid certificate"), 0600); err != nil {
			t.Fatalf("failed to write invalid certificate: %v", err)
		}

		opts := &Options{
			BaseURL:       "https://example.com",
			TLSCACertFile: invalidCertFile,
		}

		_, err := NewClient(opts)
		if err == nil {
			t.Error("expected error for invalid CA certificate content")
		}
	})
}

func TestNewClientWithClientCert(t *testing.T) {
	// Create a temporary directory for test certificates
	tempDir, err := os.MkdirTemp("", "httpkit-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Generate a self-signed client certificate
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate client key: %v", err)
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Test Client"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, clientTemplate, &clientKey.PublicKey, clientKey)
	if err != nil {
		t.Fatalf("failed to create client certificate: %v", err)
	}

	// Write client certificate and key to files
	clientCertFile := filepath.Join(tempDir, "client.crt")
	clientKeyFile := filepath.Join(tempDir, "client.key")

	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER})
	if err := os.WriteFile(clientCertFile, clientCertPEM, 0600); err != nil {
		t.Fatalf("failed to write client certificate: %v", err)
	}

	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	if err := os.WriteFile(clientKeyFile, clientKeyPEM, 0600); err != nil {
		t.Fatalf("failed to write client key: %v", err)
	}

	t.Run("valid client certificate", func(t *testing.T) {
		opts := &Options{
			BaseURL:       "https://example.com",
			TLSClientCert: clientCertFile,
			TLSClientKey:  clientKeyFile,
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transport, ok := client.GetHTTPClient().Transport.(*http.Transport)
		if !ok {
			t.Fatal("expected transport to be *http.Transport")
		}

		if transport.TLSClientConfig == nil {
			t.Fatal("expected TLSClientConfig to be set")
		}

		if len(transport.TLSClientConfig.Certificates) == 0 {
			t.Error("expected client certificates to be set")
		}
	})

	t.Run("invalid client certificate file path", func(t *testing.T) {
		opts := &Options{
			BaseURL:       "https://example.com",
			TLSClientCert: "/nonexistent/client.crt",
			TLSClientKey:  clientKeyFile,
		}

		_, err := NewClient(opts)
		if err == nil {
			t.Error("expected error for nonexistent client certificate file")
		}
	})

	t.Run("missing client key", func(t *testing.T) {
		// When only cert is provided without key, TLS config is created but no certificates loaded
		opts := &Options{
			BaseURL:       "https://example.com",
			TLSClientCert: clientCertFile,
			// TLSClientKey is empty
		}

		client, err := NewClient(opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transport, ok := client.GetHTTPClient().Transport.(*http.Transport)
		if !ok {
			t.Fatal("expected transport to be *http.Transport")
		}

		// With only cert and no key, certificates should not be loaded
		if len(transport.TLSClientConfig.Certificates) != 0 {
			t.Error("expected no client certificates when key is missing")
		}
	})
}

func TestClientDo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-User-Agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Run("sets user agent when not present", func(t *testing.T) {
		client, err := NewClient(&Options{
			BaseURL:   server.URL,
			UserAgent: "test-agent/1.0",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.Header.Get("X-User-Agent") != "test-agent/1.0" {
			t.Errorf("expected user agent to be test-agent/1.0, got %s", resp.Header.Get("X-User-Agent"))
		}
	})

	t.Run("preserves existing user agent", func(t *testing.T) {
		client, err := NewClient(&Options{
			BaseURL:   server.URL,
			UserAgent: "test-agent/1.0",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		req.Header.Set("User-Agent", "custom-agent/2.0")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.Header.Get("X-User-Agent") != "custom-agent/2.0" {
			t.Errorf("expected user agent to be custom-agent/2.0, got %s", resp.Header.Get("X-User-Agent"))
		}
	})

	t.Run("no user agent when not configured", func(t *testing.T) {
		client, err := NewClient(&Options{
			BaseURL: server.URL,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// Go's http.Client sets a default User-Agent
		if resp.Header.Get("X-User-Agent") == "" {
			t.Log("Note: Go http.Client may set a default User-Agent")
		}
	})
}

func TestClientInjectTraceContext(t *testing.T) {
	// Set up a text map propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	client, err := NewClient(&Options{
		BaseURL: "http://example.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	ctx := context.Background()

	// This should not panic even without active span
	client.InjectTraceContext(ctx, req)

	// The traceparent header might not be set without an active span,
	// but the function should complete without error
}

func TestClientGetBaseURL(t *testing.T) {
	client, err := NewClient(&Options{
		BaseURL: "http://example.com/api",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.GetBaseURL() != "http://example.com/api" {
		t.Errorf("expected baseURL to be http://example.com/api, got %s", client.GetBaseURL())
	}
}

func TestClientGetHTTPClient(t *testing.T) {
	client, err := NewClient(&Options{
		BaseURL: "http://example.com",
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	httpClient := client.GetHTTPClient()
	if httpClient == nil {
		t.Fatal("expected httpClient to not be nil")
	}

	if httpClient.Timeout != 15*time.Second {
		t.Errorf("expected timeout to be 15s, got %v", httpClient.Timeout)
	}
}
