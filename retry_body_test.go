package httpkit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastRetryOpts(maxRetries int) *RetryOptions {
	o := DefaultRetryOptions()
	o.MaxRetries = maxRetries
	o.RetryDelay = time.Millisecond
	o.MaxRetryDelay = 5 * time.Millisecond
	return o
}

// TestRetryResendsBody is the regression test for replaying a consumed request:
// the first Do reads and closes req.Body, so a naive retry sent an empty body.
func TestRetryResendsBody(t *testing.T) {
	var attempts int32
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewClient(&Options{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// PUT is idempotent, and NewRequest sets GetBody for a strings.Reader.
	req, err := http.NewRequest(http.MethodPut, srv.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(3))
	if err != nil {
		t.Fatalf("DoRequestWithRetry() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(bodies) != 3 {
		t.Fatalf("upstream saw %d attempts, want 3", len(bodies))
	}
	for i, b := range bodies {
		if b != "payload" {
			t.Errorf("attempt %d received body %q, want %q", i+1, b, "payload")
		}
	}
}

// TestRetrySkipsNonIdempotentMethods: replaying a POST that already reached the
// server duplicates its side effects.
func TestRetrySkipsNonIdempotentMethods(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 5 * time.Second})

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("charge"))
	resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(3))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("POST was attempted %d times, want 1 (retrying duplicates side effects)", got)
	}

	// An explicit Idempotency-Key opts the POST back in.
	atomic.StoreInt32(&attempts, 0)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("charge"))
	req2.Header.Set("Idempotency-Key", "abc-123")
	resp2, err := client.DoRequestWithRetry(context.Background(), req2, fastRetryOpts(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp2.Body.Close()

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("POST with Idempotency-Key attempted %d times, want 3", got)
	}
}

// TestRetryCancelsWithContext: ctx must apply to the request, not only to the
// waits between attempts.
func TestRetryCancelsWithContext(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 30 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	done := make(chan error, 1)
	go func() {
		_, err := client.DoRequestWithRetry(ctx, req, fastRetryOpts(0))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected the cancelled context to abort the in-flight request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not abort the request")
	}
}

// TestRetryHonoursRetryAfter checks the server's own backoff request wins.
func TestRetryHonoursRetryAfter(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	start := time.Now()
	resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(2))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("waited %s, want at least the 1s the server asked for", elapsed)
	}
}

// TestPermanentErrorsAreNotRetried: retrying an error that can never succeed
// only delays the failure.
func TestPermanentErrorsAreNotRetried(t *testing.T) {
	opts := DefaultRetryOptions()

	if opts.IsRetryableError(context.Canceled, 0) {
		t.Error("context.Canceled should not be retried")
	}
	if opts.IsRetryableError(context.DeadlineExceeded, 0) {
		t.Error("context.DeadlineExceeded should not be retried")
	}
	if !opts.IsRetryableError(io.ErrUnexpectedEOF, 0) {
		t.Error("a transient transport error should be retried")
	}
}

// TestBackoffIsExponential guards the documented growth curve.
func TestBackoffIsExponential(t *testing.T) {
	o := &RetryOptions{
		RetryDelay:        100 * time.Millisecond,
		MaxRetryDelay:     10 * time.Second,
		BackoffMultiplier: 2,
	}
	want := []time.Duration{100, 200, 400, 800}
	for i, w := range want {
		if got := o.CalculateRetryDelay(i); got != w*time.Millisecond {
			t.Errorf("CalculateRetryDelay(%d) = %v, want %v", i, got, w*time.Millisecond)
		}
	}
}
