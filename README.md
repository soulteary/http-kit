# http-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/http-kit.svg)](https://pkg.go.dev/github.com/soulteary/http-kit)
[![Go Report Card](https://goreportcard.com/badge/github.com/soulteary/http-kit)](https://goreportcard.com/report/github.com/soulteary/http-kit)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/http-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/http-kit)

[中文文档](README_CN.md)

A lightweight Go HTTP client library with TLS/mTLS support, automatic retry with exponential backoff, and OpenTelemetry tracing integration.

## Features

- **TLS/mTLS Support** - Full TLS configuration including CA certificates, client certificates for mutual TLS authentication
- **Automatic Retry** - Configurable retry logic with exponential backoff for transient failures
- **OpenTelemetry Integration** - Built-in trace context propagation for distributed tracing
- **Configurable Options** - Flexible client configuration with sensible defaults
- **Zero External Dependencies** - Only depends on OpenTelemetry for tracing (optional)

## Installation

```bash
go get github.com/soulteary/http-kit
```

## Quick Start

### Basic HTTP Client

```go
import "github.com/soulteary/http-kit"

// Create a simple client
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
    Timeout: 10 * time.Second,
})
if err != nil {
    log.Fatal(err)
}

// Make a request
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/users", nil)
resp, err := client.Do(req)
```

### Client with TLS/mTLS

```go
// Client with server certificate verification
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://secure-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSServerName: "secure-api.example.com",
})

// Client with mutual TLS (mTLS)
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://mtls-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSClientCert: "/path/to/client.crt",
    TLSClientKey:  "/path/to/client.key",
})
```

### Automatic Retry

```go
import "github.com/soulteary/http-kit"

client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// Use default retry options (3 retries, exponential backoff)
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/data", nil)
resp, err := client.DoRequestWithRetry(context.Background(), req, nil)

// Or customize retry behavior
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
resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
```

### OpenTelemetry Tracing

```go
import (
    "github.com/soulteary/http-kit"
    "go.opentelemetry.io/otel"
)

client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// Create a span in your application
tracer := otel.Tracer("my-service")
ctx, span := tracer.Start(context.Background(), "api-call")
defer span.End()

// Inject trace context into request headers
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/data", nil)
client.InjectTraceContext(ctx, req)

resp, err := client.Do(req)
```

## API Reference

### Client Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `BaseURL` | `string` | Required | Base URL for all requests |
| `Timeout` | `time.Duration` | `10s` | Request timeout |
| `UserAgent` | `string` | `""` | User-Agent header value |
| `Transport` | `http.RoundTripper` | `nil` | Custom HTTP transport |
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
| `Do(req)` | Performs an HTTP request |
| `DoRequestWithRetry(ctx, req, retryOpts)` | Performs an HTTP request with automatic retry |
| `InjectTraceContext(ctx, req)` | Injects OpenTelemetry trace context into request headers |
| `GetBaseURL()` | Returns the base URL |
| `GetHTTPClient()` | Returns the underlying `*http.Client` |

## Project Structure

```
http-kit/
├── client.go       # HTTP client with TLS/mTLS support
├── client_test.go  # Client tests
├── retry.go        # Retry logic with exponential backoff
├── retry_test.go   # Retry tests
├── go.mod          # Module definition
└── LICENSE         # Apache 2.0 license
```

## Security Features

| Feature | Description |
|---------|-------------|
| **TLS Verification** | Supports custom CA certificates for server verification |
| **mTLS Authentication** | Client certificate support for mutual TLS |
| **Server Name Verification** | Configurable TLS server name for SNI |
| **Secure Defaults** | TLS verification enabled by default |

## Test Coverage

Run tests with coverage:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## Requirements

- Go 1.21 or later

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
