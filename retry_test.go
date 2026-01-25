package httpkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultRetryOptions(t *testing.T) {
	opts := DefaultRetryOptions()

	if opts == nil {
		t.Fatal("DefaultRetryOptions returned nil")
	}

	if opts.MaxRetries != 3 {
		t.Errorf("expected MaxRetries to be 3, got %d", opts.MaxRetries)
	}

	if opts.RetryDelay != 100*time.Millisecond {
		t.Errorf("expected RetryDelay to be 100ms, got %v", opts.RetryDelay)
	}

	if opts.MaxRetryDelay != 2*time.Second {
		t.Errorf("expected MaxRetryDelay to be 2s, got %v", opts.MaxRetryDelay)
	}

	if opts.BackoffMultiplier != 2.0 {
		t.Errorf("expected BackoffMultiplier to be 2.0, got %f", opts.BackoffMultiplier)
	}

	expectedCodes := []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}

	if len(opts.RetryableStatusCodes) != len(expectedCodes) {
		t.Errorf("expected %d retryable status codes, got %d", len(expectedCodes), len(opts.RetryableStatusCodes))
	}

	for i, code := range expectedCodes {
		if opts.RetryableStatusCodes[i] != code {
			t.Errorf("expected status code %d at index %d, got %d", code, i, opts.RetryableStatusCodes[i])
		}
	}
}

func TestRetryOptionsIsRetryableError(t *testing.T) {
	tests := []struct {
		name       string
		opts       *RetryOptions
		err        error
		statusCode int
		want       bool
	}{
		{
			name: "MaxRetries is 0",
			opts: &RetryOptions{
				MaxRetries:           0,
				RetryableStatusCodes: []int{500},
			},
			err:        nil,
			statusCode: 500,
			want:       false,
		},
		{
			name: "network error is retryable",
			opts: &RetryOptions{
				MaxRetries:           3,
				RetryableStatusCodes: []int{500},
			},
			err:        errors.New("connection refused"),
			statusCode: 0,
			want:       true,
		},
		{
			name: "retryable status code",
			opts: &RetryOptions{
				MaxRetries:           3,
				RetryableStatusCodes: []int{500, 502, 503},
			},
			err:        nil,
			statusCode: 502,
			want:       true,
		},
		{
			name: "non-retryable status code",
			opts: &RetryOptions{
				MaxRetries:           3,
				RetryableStatusCodes: []int{500, 502, 503},
			},
			err:        nil,
			statusCode: 400,
			want:       false,
		},
		{
			name: "success status code",
			opts: &RetryOptions{
				MaxRetries:           3,
				RetryableStatusCodes: []int{500, 502, 503},
			},
			err:        nil,
			statusCode: 200,
			want:       false,
		},
		{
			name: "empty retryable status codes",
			opts: &RetryOptions{
				MaxRetries:           3,
				RetryableStatusCodes: []int{},
			},
			err:        nil,
			statusCode: 500,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.opts.IsRetryableError(tt.err, tt.statusCode)
			if got != tt.want {
				t.Errorf("IsRetryableError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryOptionsCalculateRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		opts    *RetryOptions
		attempt int
		want    time.Duration
	}{
		{
			name: "first attempt",
			opts: &RetryOptions{
				RetryDelay:        100 * time.Millisecond,
				MaxRetryDelay:     2 * time.Second,
				BackoffMultiplier: 2.0,
			},
			attempt: 0,
			want:    200 * time.Millisecond, // 100ms * 1 * 2.0
		},
		{
			name: "second attempt",
			opts: &RetryOptions{
				RetryDelay:        100 * time.Millisecond,
				MaxRetryDelay:     2 * time.Second,
				BackoffMultiplier: 2.0,
			},
			attempt: 1,
			want:    400 * time.Millisecond, // 100ms * 2 * 2.0
		},
		{
			name: "third attempt",
			opts: &RetryOptions{
				RetryDelay:        100 * time.Millisecond,
				MaxRetryDelay:     2 * time.Second,
				BackoffMultiplier: 2.0,
			},
			attempt: 2,
			want:    600 * time.Millisecond, // 100ms * 3 * 2.0
		},
		{
			name: "exceeds max delay",
			opts: &RetryOptions{
				RetryDelay:        500 * time.Millisecond,
				MaxRetryDelay:     1 * time.Second,
				BackoffMultiplier: 2.0,
			},
			attempt: 5,
			want:    1 * time.Second, // capped at MaxRetryDelay
		},
		{
			name: "no backoff multiplier",
			opts: &RetryOptions{
				RetryDelay:        100 * time.Millisecond,
				MaxRetryDelay:     2 * time.Second,
				BackoffMultiplier: 1.0,
			},
			attempt: 2,
			want:    300 * time.Millisecond, // 100ms * 3 * 1.0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.opts.CalculateRetryDelay(tt.attempt)
			if got != tt.want {
				t.Errorf("CalculateRetryDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestDoRequestWithRetry(t *testing.T) {
	t.Run("successful request on first attempt", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.DoRequestWithRetry(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 1 {
			t.Errorf("expected 1 request, got %d", requestCount)
		}
	})

	t.Run("retry on server error then success", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := atomic.AddInt32(&requestCount, 1)
			if count < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           3,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 3 {
			t.Errorf("expected 3 requests, got %d", requestCount)
		}
	})

	t.Run("all retries exhausted with server error", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           2,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// After all retries, return the last response
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", resp.StatusCode)
		}

		// 1 initial + 2 retries = 3 total
		if atomic.LoadInt32(&requestCount) != 3 {
			t.Errorf("expected 3 requests, got %d", requestCount)
		}
	})

	t.Run("non-retryable status code", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           3,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", resp.StatusCode)
		}

		// Should not retry for non-retryable status
		if atomic.LoadInt32(&requestCount) != 1 {
			t.Errorf("expected 1 request, got %d", requestCount)
		}
	})

	t.Run("context cancelled during retry", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           5,
			RetryDelay:           100 * time.Millisecond,
			MaxRetryDelay:        1 * time.Second,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		// Cancel context after first request
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		_, err = client.DoRequestWithRetry(ctx, req, retryOpts)
		if err == nil {
			t.Error("expected error due to context cancellation")
		}

		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled error, got %v", err)
		}
	})

	t.Run("nil retry options uses defaults", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.DoRequestWithRetry(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("network error with retries exhausted", func(t *testing.T) {
		// Use a server that immediately closes
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		serverURL := server.URL
		server.Close() // Close immediately to simulate network error

		client, err := NewClient(&Options{BaseURL: serverURL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, serverURL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           2,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		_, err = client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err == nil {
			t.Error("expected error due to network failure")
		}
	})

	t.Run("network error not retryable when MaxRetries is 0", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		serverURL := server.URL
		server.Close()

		client, err := NewClient(&Options{BaseURL: serverURL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, serverURL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           0,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		_, err = client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err == nil {
			t.Error("expected error due to network failure")
		}
	})
}

func TestDoRequestWithRetryEdgeCases(t *testing.T) {
	t.Run("retry with successful response after retryable errors", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := atomic.AddInt32(&requestCount, 1)
			if count == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success"))
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           3,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusInternalServerError},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 2 {
			t.Errorf("expected 2 requests, got %d", requestCount)
		}
	})

	t.Run("last retry returns response", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := atomic.AddInt32(&requestCount, 1)
			if count <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// On last attempt (3rd), return success
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           2, // 1 initial + 2 retries = 3 total
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("retryable error on last attempt returns error", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			// Always return retryable error
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           1, // 1 initial + 1 retry = 2 total
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusBadGateway},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// On last attempt, returns the response even if retryable
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("expected status 502, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 2 {
			t.Errorf("expected 2 requests, got %d", requestCount)
		}
	})

	t.Run("zero max retries executes once", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           0,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 1 {
			t.Errorf("expected 1 request, got %d", requestCount)
		}
	})

	t.Run("server error on zero max retries returns response", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           0,
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// With 0 retries, returns response immediately
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", resp.StatusCode)
		}

		if atomic.LoadInt32(&requestCount) != 1 {
			t.Errorf("expected 1 request, got %d", requestCount)
		}
	})

	t.Run("negative max retries makes no attempts", func(t *testing.T) {
		var requestCount int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client, err := NewClient(&Options{BaseURL: server.URL})
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		retryOpts := &RetryOptions{
			MaxRetries:           -1, // maxAttempts = 0
			RetryDelay:           1 * time.Millisecond,
			MaxRetryDelay:        10 * time.Millisecond,
			BackoffMultiplier:    1.0,
			RetryableStatusCodes: []int{http.StatusServiceUnavailable},
		}

		_, err = client.DoRequestWithRetry(context.Background(), req, retryOpts)
		if err == nil {
			t.Error("expected error when no attempts are made")
		}

		if atomic.LoadInt32(&requestCount) != 0 {
			t.Errorf("expected 0 requests, got %d", requestCount)
		}
	})
}
