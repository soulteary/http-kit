package httpkit

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// drainLimit bounds how much of a discarded response body is read before the
// connection is returned to the pool.
const drainLimit = 64 << 10

// drainTimeout bounds how long that read may take.
//
// A byte ceiling alone does not bound time: a server can answer with a
// retryable status and then trickle far fewer than drainLimit bytes, keeping
// the read alive indefinitely. Nothing else stops it in that case -- a Client
// with Timeout == 0 sets no read deadline, and a caller passing a non-expiring
// context never cancels the transport -- so the retry loop would block forever
// on a courtesy read.
const drainTimeout = 2 * time.Second

// RetryOptions configuration for retry logic
type RetryOptions struct {
	MaxRetries           int
	RetryDelay           time.Duration
	MaxRetryDelay        time.Duration
	BackoffMultiplier    float64
	RetryableStatusCodes []int
}

// DefaultRetryOptions returns default retry options
func DefaultRetryOptions() *RetryOptions {
	return &RetryOptions{
		MaxRetries:        3,
		RetryDelay:        100 * time.Millisecond,
		MaxRetryDelay:     2 * time.Second,
		BackoffMultiplier: 2.0,
		RetryableStatusCodes: []int{
			http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		},
	}
}

// IsRetryableError checks if an error should trigger a retry.
//
// A transport error is retryable only when it is transient. Certificate
// verification failures, an unsupported URL scheme, a cancelled context and
// similar permanent errors used to be retried the full MaxRetries times: the
// request could never succeed, so the only effect was to delay the failure.
func (r *RetryOptions) IsRetryableError(err error, statusCode int) bool {
	return r.IsRetryableErrorCtx(context.Background(), err, statusCode)
}

// IsRetryableErrorCtx is IsRetryableError with the caller's context, so a
// per-attempt http.Client.Timeout can be told apart from the caller's own
// deadline. Only the latter ends the call.
func (r *RetryOptions) IsRetryableErrorCtx(ctx context.Context, err error, statusCode int) bool {
	if r.MaxRetries == 0 {
		return false
	}

	if err != nil {
		return isTransientErrorCtx(ctx, err)
	}

	// Check if status code is in retryable list
	for _, code := range r.RetryableStatusCodes {
		if statusCode == code {
			return true
		}
	}

	return false
}

// isTransientErrorCtx reports whether err is worth another attempt.
//
// Only the CALLER giving up suppresses retries, which is why the caller's
// context is a parameter: http.Client.Timeout is a PER-ATTEMPT limit and
// surfaces as a *url.Error wrapping context.DeadlineExceeded exactly like an
// expired request context. Classifying every DeadlineExceeded as permanent
// therefore made one slow first attempt fail the whole call, even where a
// second attempt would have succeeded.
func isTransientErrorCtx(ctx context.Context, err error) bool {
	// The caller gave up, or the caller's own deadline passed.
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Reaching here means the caller's own context is still live, so this
		// deadline belongs to a single attempt. A *url.Error says the HTTP
		// client reported it -- http.Client.Timeout -- and a transiently slow
		// attempt is worth retrying. A bare sentinel says nothing about
		// whether another attempt would differ, so it stays permanent.
		var urlErr *url.Error
		if !errors.As(err, &urlErr) {
			return false
		}
	}

	// TLS verification failed: the same certificate will fail again.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return false
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return false
	}

	// A malformed request or unsupported scheme is a programming error.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if strings.Contains(urlErr.Err.Error(), "unsupported protocol scheme") {
			return false
		}
	}

	// Anything the net package classifies as temporary or a timeout, plus
	// connection-level failures, are worth another attempt.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	return true
}

// CalculateRetryDelay returns the delay before the given retry attempt
// (0-based) using exponential backoff: RetryDelay * BackoffMultiplier^attempt,
// capped at MaxRetryDelay.
//
// The previous formula was RetryDelay * (attempt+1) * BackoffMultiplier, which
// grows linearly no matter what the multiplier is -- 200ms, 400ms, 600ms for a
// multiplier of 2 -- despite the field name and the documented "exponential
// backoff".
func (r *RetryOptions) CalculateRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	// A multiplier below 1 would shrink the delay towards zero and hammer the
	// server; treat it as "no growth".
	multiplier := r.BackoffMultiplier
	if multiplier < 1 {
		multiplier = 1
	}

	growth := math.Pow(multiplier, float64(attempt))
	delay := time.Duration(float64(r.RetryDelay) * growth)
	if delay > r.MaxRetryDelay || delay < 0 {
		delay = r.MaxRetryDelay
	}
	return delay
}

// jitter returns d reduced by a random amount of up to 20%, so a fleet of
// clients retrying after the same upstream failure does not do so in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	max := big.NewInt(int64(d) / 5)
	if max.Sign() <= 0 {
		return d
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return d
	}
	return d - time.Duration(n.Int64())
}

// isIdempotent reports whether replaying the request is safe.
//
// Retrying a POST or PATCH that already reached the server duplicates its
// side effects. RFC 9110 defines GET, HEAD, PUT, DELETE, OPTIONS and TRACE as
// idempotent; anything else must opt in with the Idempotency-Key header, which
// is the convention for making a POST safe to replay.
func isIdempotent(req *http.Request) bool {
	// An empty Method means GET for a client request (net/http documents
	// this), so classifying "" as non-idempotent refused to retry a 408, 429
	// or 5xx on what is in fact a GET.
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodPut,
		http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	}
	return req.Header.Get("Idempotency-Key") != ""
}

// retryAfter returns the delay requested by a Retry-After header, if any.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// rewindBody prepares req for another attempt.
//
// The first Do consumes and closes req.Body, so replaying the same *Request
// sends an empty body -- or fails outright with "ContentLength=N with Body
// length 0". http.NewRequest populates GetBody for the common in-memory body
// types; when it is absent there is no way to replay the body and the request
// must not be retried.
func rewindBody(req *http.Request) error {
	if req.Body == nil || req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("cannot rewind request body for retry: %w", err)
	}
	req.Body = body
	return nil
}

// canRetryBody reports whether the request body can be replayed.
func canRetryBody(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// drainAndClose empties resp.Body so its connection can go back to the pool,
// then closes it. The read is bounded by drainLimit bytes, by drainTimeout,
// and by ctx, whichever comes first.
//
// Draining is only an optimisation, so it must never outrank making progress:
// neither the copy NOR the close is allowed to hold up the retry. Giving up on
// the drain costs at most one pooled connection; waiting for it costs the
// whole retry.
func drainAndClose(ctx context.Context, resp *http.Response) {
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	}()

	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()

	select {
	case <-copied:
		// The read is over, so Close cannot be waiting on one. Safe for any
		// body implementation.
		_ = resp.Body.Close()

	case <-timer.C:
		closeAsync(resp)

	case <-ctx.Done():
		closeAsync(resp)
	}
}

// closeAsync closes a body this call has stopped waiting for.
//
// From a goroutine, because Close is not guaranteed to return either. A
// net/http body takes Close on a separate path from Read, so closing one
// mid-read unblocks the reader and both goroutines finish -- but
// http.Response.Body carries no such requirement in general, and
// Options.Transport lets a caller supply a RoundTripper whose Close waits for
// the active Read to end. Calling it inline would then block the retry behind
// the very drain the timeout exists to abandon.
//
// The cost when that happens is one parked goroutine per such response, which
// is what the alternative already was, minus the stalled request.
func closeAsync(resp *http.Response) {
	go func() { _ = resp.Body.Close() }()
}

// DoRequestWithRetry performs an HTTP request with retry logic.
//
// ctx is applied to the request itself, not only to the waits between
// attempts, so cancelling it aborts an in-flight attempt.
//
// A request is only retried when replaying it is safe: the method must be
// idempotent (or carry an Idempotency-Key), and the body must be replayable.
// Streaming bodies without GetBody are sent once.
func (c *Client) DoRequestWithRetry(ctx context.Context, req *http.Request, retryOpts *RetryOptions) (*http.Response, error) {
	if retryOpts == nil {
		retryOpts = DefaultRetryOptions()
	}

	req = req.WithContext(ctx)

	// Replaying a request needs a replayable body and an idempotent method.
	replayable := canRetryBody(req) && isIdempotent(req)

	var lastErr error
	var lastRetryAfter time.Duration
	var haveRetryAfter bool
	maxAttempts := retryOpts.MaxRetries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := jitter(retryOpts.CalculateRetryDelay(attempt - 1))
			if haveRetryAfter {
				// Honour Retry-After, but never past the configured ceiling.
				// http.Client.Timeout does not cover this sleep, so an
				// unbounded value let a server returning "Retry-After: 86400"
				// suspend the call for a day.
				//
				// The cap is applied UNCONDITIONALLY, exactly as
				// CalculateRetryDelay applies it: a MaxRetryDelay of zero is a
				// zero ceiling, a configuration the package already supports,
				// and guarding on "> 0" let every positive Retry-After sail
				// straight past it.
				//
				// The guard is the parser's boolean, not "lastRetryAfter > 0".
				// "Retry-After: 0" -- and an HTTP-date that has already passed
				// -- are valid headers meaning "retry now", and testing the
				// duration threw them away, falling back to a backoff the
				// server had explicitly waived.
				delay = min(lastRetryAfter, retryOpts.MaxRetryDelay)
			}

			// Wait BEFORE rewinding. rewindBody calls GetBody, which for a
			// file-backed body opens a new reader; creating it first and then
			// returning on a cancelled context dropped it unclosed, leaking a
			// descriptor on every cancelled retry.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}

			if err := rewindBody(req); err != nil {
				return nil, err
			}

			lastRetryAfter, haveRetryAfter = 0, false
		}

		resp, err := c.Do(req)
		if err != nil {
			lastErr = err
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("failed to execute request: %w", err)
			}
			if !replayable || !retryOpts.IsRetryableErrorCtx(ctx, err, 0) {
				return nil, fmt.Errorf("failed to execute request: %w", err)
			}
			if attempt >= retryOpts.MaxRetries {
				return nil, fmt.Errorf("failed to execute request after retries: %w", lastErr)
			}
			continue
		}

		if replayable && retryOpts.IsRetryableError(nil, resp.StatusCode) && attempt < retryOpts.MaxRetries {
			// Honour an explicit Retry-After over our own backoff.
			if d, ok := retryAfter(resp); ok {
				lastRetryAfter, haveRetryAfter = d, true
			}
			// Drain before closing so the connection can be reused.
			drainAndClose(ctx, resp)
			lastErr = fmt.Errorf("server error: status %d", resp.StatusCode)
			continue
		}

		// Success, non-retryable status, non-replayable request, or last attempt.
		return resp, nil
	}

	// Only reached when maxAttempts is 0 (MaxRetries = -1).
	if lastErr != nil {
		return nil, fmt.Errorf("failed after retries: %w", lastErr)
	}
	return nil, fmt.Errorf("no attempts made")
}
