package httpkit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// retryAfterServer answers the first request with the given Retry-After and a
// 429, then succeeds.
func retryAfterServer(t *testing.T, retryAfter string) *httptest.Server {
	t.Helper()
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRetryHonoursRetryAfter checks the server's own backoff request wins over
// the computed backoff, up to the configured ceiling.
func TestRetryHonoursRetryAfter(t *testing.T) {
	srv := retryAfterServer(t, "1")

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	opts := fastRetryOpts(2)
	opts.MaxRetryDelay = 2 * time.Second // room for the server's 1s

	start := time.Now()
	resp, err := client.DoRequestWithRetry(context.Background(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("waited %s, want at least the 1s the server asked for", elapsed)
	}
}

// TestRetryAfterIsCappedByMaxRetryDelay is the regression test for a
// server-supplied Retry-After bypassing MaxRetryDelay entirely. With a
// background context, a server answering "Retry-After: 86400" suspended the
// call for a day -- and http.Client.Timeout does not cover that sleep.
func TestRetryAfterIsCappedByMaxRetryDelay(t *testing.T) {
	srv := retryAfterServer(t, "86400")

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	opts := fastRetryOpts(2)
	opts.MaxRetryDelay = 50 * time.Millisecond

	start := time.Now()
	resp, err := client.DoRequestWithRetry(context.Background(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("waited %s for a Retry-After of 86400s; MaxRetryDelay (%s) must bound it", elapsed, opts.MaxRetryDelay)
	}
}

// TestPerAttemptTimeoutIsRetried is the regression test for classifying
// http.Client.Timeout as permanent. It surfaces as a *url.Error wrapping
// context.DeadlineExceeded, exactly like an expired request context, so a
// transiently slow first attempt failed the whole call even though a second
// attempt would have succeeded.
func TestPerAttemptTimeoutIsRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			time.Sleep(400 * time.Millisecond) // outlives the client timeout
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Client timeout well under the server's first-attempt delay.
	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(3))
	if err != nil {
		t.Fatalf("DoRequestWithRetry error = %v; a per-attempt client timeout must be retried", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Errorf("attempts = %d, want the slow first attempt to be retried", got)
	}
}

// TestCallerContextExpiryIsNotRetried: the caller's own deadline still ends the
// call -- that is the one timeout retries must not paper over.
func TestCallerContextExpiryIsNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := client.DoRequestWithRetry(ctx, req, fastRetryOpts(3)); err == nil {
		t.Fatal("DoRequestWithRetry returned nil error after the caller's context expired")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: an expired caller context must not be retried", got)
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

// --- Codex review round 2 (PR #2) ---

// TestRetryAfterRespectsAZeroCeiling is the regression test for the
// `MaxRetryDelay > 0` guard added in the last round. A zero MaxRetryDelay is a
// zero ceiling -- a configuration CalculateRetryDelay already honours -- and
// the guard let every positive Retry-After bypass it, so "Retry-After: 86400"
// could still suspend a background-context call for a day.
func TestRetryAfterRespectsAZeroCeiling(t *testing.T) {
	srv := retryAfterServer(t, "86400")

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	opts := fastRetryOpts(2)
	opts.MaxRetryDelay = 0 // an explicit zero ceiling

	start := time.Now()
	resp, err := client.DoRequestWithRetry(context.Background(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %s with MaxRetryDelay=0; a zero ceiling must bound Retry-After too", elapsed)
	}
}

// TestCancelledRetryDoesNotLeakARewoundBody is the regression test for
// rewinding before the backoff. rewindBody calls GetBody, which opens a new
// reader for a file-backed body; creating it and then returning on a cancelled
// context dropped it unclosed, leaking a descriptor per cancelled retry.
func TestCancelledRetryDoesNotLeakARewoundBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var opened, closed int32
	req, _ := http.NewRequest(http.MethodPut, srv.URL, strings.NewReader("payload"))
	req.GetBody = func() (io.ReadCloser, error) {
		atomic.AddInt32(&opened, 1)
		return &countingBody{Reader: strings.NewReader("payload"), closed: &closed}, nil
	}

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})

	opts := fastRetryOpts(3)
	opts.RetryDelay = 200 * time.Millisecond
	opts.MaxRetryDelay = 200 * time.Millisecond

	// Cancel while the first backoff is in flight.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := client.DoRequestWithRetry(ctx, req, opts); err == nil {
		t.Fatal("DoRequestWithRetry returned nil error after cancellation")
	}

	if o, c := atomic.LoadInt32(&opened), atomic.LoadInt32(&closed); o != c {
		t.Errorf("GetBody opened %d bodies but %d were closed; a cancelled retry leaked one", o, c)
	}
}

// countingBody counts Close calls so a leaked body is visible.
type countingBody struct {
	*strings.Reader
	closed *int32
}

func (b *countingBody) Close() error {
	atomic.AddInt32(b.closed, 1)
	return nil
}

// TestEmptyMethodIsTreatedAsGET: net/http documents an empty Method on a
// client request as GET, so a 408, 429 or configured 5xx on it must be
// retried rather than classified non-idempotent.
func TestEmptyMethodIsTreatedAsGET(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.org/", nil)
	req.Method = ""
	if !isIdempotent(req) {
		t.Error("an empty method was classified non-idempotent; net/http sends it as GET")
	}

	post, _ := http.NewRequest(http.MethodPost, "http://example.org/", nil)
	if isIdempotent(post) {
		t.Error("POST without an Idempotency-Key was classified idempotent")
	}
}

// TestRetryAfterZeroRetriesImmediately is the regression test for guarding the
// Retry-After override on "lastRetryAfter > 0". "Retry-After: 0" parses to a
// valid zero duration, but a zero-valued guard read that as "no header" and
// fell back to the computed backoff -- delaying a retry the server had
// explicitly waived, by RetryDelay rather than by nothing.
func TestRetryAfterZeroRetriesImmediately(t *testing.T) {
	srv := retryAfterServer(t, "0")

	client, _ := NewClient(&Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	opts := DefaultRetryOptions()
	opts.MaxRetries = 2
	opts.RetryDelay = 3 * time.Second
	opts.MaxRetryDelay = 10 * time.Second

	start := time.Now()
	resp, err := client.DoRequestWithRetry(context.Background(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s after \"Retry-After: 0\"; the server asked for an immediate retry", elapsed)
	}
}

// TestDrainIsTimeBounded is the regression test for draining a discarded body
// under a byte ceiling but no time ceiling. A server that answers with a
// retryable status and then trickles fewer than drainLimit bytes kept the
// synchronous io.Copy alive forever: a Client with Timeout == 0 sets no read
// deadline and a background context never cancels the transport, so the body
// was never closed and the retry never happened.
func TestDrainIsTimeBounded(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("trickle"))
			w.(http.Flusher).Flush()
			<-stall // far short of drainLimit, and never another byte
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	// Not srv.Close: it blocks on the handler still parked in <-stall.
	t.Cleanup(srv.CloseClientConnections)

	// Timeout 0 and a background context below: nothing but drainTimeout
	// bounds the drain.
	client, _ := NewClient(&Options{BaseURL: srv.URL})
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(1))
		done <- result{resp, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("retry failed: %v", got.err)
		}
		_ = got.resp.Body.Close()
		if got.resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, want 200 from the second attempt", got.resp.StatusCode)
		}
	case <-time.After(drainTimeout + 10*time.Second):
		t.Fatal("draining a stalled body blocked the retry; drainTimeout must bound it")
	}
}

// closeWaitsForReadBody is a response body whose Close waits for the active
// Read to finish -- permitted for http.Response.Body, which carries no
// concurrent Read/Close requirement, and reachable through Options.Transport.
type closeWaitsForReadBody struct {
	readDone chan struct{} // closed when Read returns
	release  chan struct{} // closed to let Read return
}

func (b *closeWaitsForReadBody) Read(p []byte) (int, error) {
	defer close(b.readDone)
	<-b.release
	return 0, io.EOF
}

func (b *closeWaitsForReadBody) Close() error {
	<-b.readDone // exactly what net/http bodies do NOT do
	return nil
}

// stallingTransport answers the first request with a retryable status and a
// body that stalls, then succeeds.
type stallingTransport struct {
	attempts int32
	body     *closeWaitsForReadBody
}

func (t *stallingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.attempts, 1) == 1 {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{},
			Body:       t.body,
			Request:    r,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    r,
	}, nil
}

// TestDrainCloseCannotBlockTheRetry is the regression test for closing the
// body inline once the drain timer has fired.
//
// Closing a net/http body mid-read unblocks the reader, but
// http.Response.Body carries no such requirement, and Options.Transport lets a
// caller supply a RoundTripper whose Close waits for the active Read. The
// synchronous Close then blocked behind the very drain the timeout exists to
// abandon, and the retry still never happened -- the byte and time bounds
// bought nothing.
func TestDrainCloseCannotBlockTheRetry(t *testing.T) {
	body := &closeWaitsForReadBody{
		readDone: make(chan struct{}),
		release:  make(chan struct{}),
	}
	t.Cleanup(func() { close(body.release) })

	tr := &stallingTransport{body: body}
	client, err := NewClient(&Options{BaseURL: "http://example.org", Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.org/x", nil)

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(1))
		done <- result{resp, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("retry failed: %v", got.err)
		}
		_ = got.resp.Body.Close()
		if got.resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, want 200 from the second attempt", got.resp.StatusCode)
		}
	case <-time.After(drainTimeout + 10*time.Second):
		t.Fatal("a body whose Close waits for its Read blocked the retry")
	}
}

// --- Codex review round 6 (PR #2) ---

// blockingCloseBody reads to EOF at once but blocks in Close, independently of
// any read. Nothing in http.Response.Body forbids it, and Options.Transport
// lets a caller supply the RoundTripper that returns one.
type blockingCloseBody struct {
	release   chan struct{}
	closeCall chan struct{} // closed on the first Close
	once      sync.Once
}

func (b *blockingCloseBody) Read(p []byte) (int, error) { return 0, io.EOF }

func (b *blockingCloseBody) Close() error {
	b.once.Do(func() { close(b.closeCall) })
	<-b.release
	return nil
}

// respondingTransport answers the first request with a retryable status and
// the given body, then succeeds.
type respondingTransport struct {
	attempts int32
	body     io.ReadCloser
	header   http.Header
}

func (t *respondingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&t.attempts, 1) == 1 {
		header := t.header
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     header,
			Body:       t.body,
			Request:    r,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("ok")),
		Request:    r,
	}, nil
}

// TestCompletedDrainCloseCannotBlockTheRetry is the regression test for
// closing the body inline once the COPY finished.
//
// Round 5 moved the timeout and context closes off the calling goroutine, but
// left the completed-copy branch synchronous on the reasoning that a finished
// read leaves Close nothing to wait on. That only holds for bodies whose Close
// blocks BECAUSE of the read. A Close that blocks on its own stalled the retry
// forever, because by then the timer was no longer participating -- the same
// unbounded wait the drain bounds exist to prevent, reached one branch over.
func TestCompletedDrainCloseCannotBlockTheRetry(t *testing.T) {
	body := &blockingCloseBody{
		release:   make(chan struct{}),
		closeCall: make(chan struct{}),
	}
	t.Cleanup(func() { close(body.release) })

	tr := &respondingTransport{body: body}
	client, err := NewClient(&Options{BaseURL: "http://example.org", Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.org/x", nil)

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.DoRequestWithRetry(context.Background(), req, fastRetryOpts(1))
		done <- result{resp, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("retry failed: %v", got.err)
		}
		_ = got.resp.Body.Close()
		if got.resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, want 200 from the second attempt", got.resp.StatusCode)
		}
	case <-time.After(drainTimeout + 10*time.Second):
		t.Fatal("a body whose Close blocks on its own blocked the retry")
	}

	// The body must still have been closed, just not on the calling path.
	select {
	case <-body.closeCall:
	case <-time.After(5 * time.Second):
		t.Error("the body was never closed")
	}
}

// TestRetryAfterSecondsSaturate is the regression test for a Retry-After value
// that overflows time.Duration.
//
// Duration counts nanoseconds, so "Retry-After: 9223372037" parses fine and
// multiplies to a NEGATIVE duration. That negative then won
// min(lastRetryAfter, MaxRetryDelay) and time.After fired immediately, so a
// malformed or adversarial upstream stepped straight past the configured
// ceiling -- the opposite of what bounding Retry-After is for.
func TestRetryAfterSecondsSaturate(t *testing.T) {
	const ceiling = 300 * time.Millisecond

	for _, value := range []string{
		"9223372037",           // one second past what Duration holds
		"99999999999999999999", // beyond int64 entirely
	} {
		t.Run(value, func(t *testing.T) {
			tr := &respondingTransport{
				body:   io.NopCloser(strings.NewReader("")),
				header: http.Header{"Retry-After": []string{value}},
			}
			client, err := NewClient(&Options{BaseURL: "http://example.org", Transport: tr})
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, "http://example.org/x", nil)

			opts := DefaultRetryOptions()
			opts.MaxRetries = 1
			opts.RetryDelay = time.Millisecond
			opts.MaxRetryDelay = ceiling

			start := time.Now()
			resp, err := client.DoRequestWithRetry(context.Background(), req, opts)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			_ = resp.Body.Close()

			if elapsed < ceiling {
				t.Errorf("retried after %v, want at least the %v ceiling: the overflowed value bypassed it", elapsed, ceiling)
			}
			if elapsed > ceiling+10*time.Second {
				t.Errorf("retried after %v, want it capped at %v", elapsed, ceiling)
			}
		})
	}
}

// TestRetryAfterAtTheDurationLimit: the largest representable value must not
// be saturated away, and is still capped by MaxRetryDelay.
func TestRetryAfterAtTheDurationLimit(t *testing.T) {
	got, ok := retryAfter(&http.Response{
		Header: http.Header{"Retry-After": []string{strconv.FormatInt(maxRetryAfterSeconds, 10)}},
	})
	if !ok || got <= 0 {
		t.Fatalf("retryAfter = (%v, %v), want the maximum representable delay", got, ok)
	}

	over, ok := retryAfter(&http.Response{
		Header: http.Header{"Retry-After": []string{strconv.FormatInt(maxRetryAfterSeconds+1, 10)}},
	})
	if !ok || over != got {
		t.Errorf("retryAfter(max+1) = (%v, %v), want it saturated to %v", over, ok, got)
	}
}
