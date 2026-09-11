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

// isTransientError reports whether err is worth another attempt.
//
// Only the CALLER giving up suppresses retries. http.Client.Timeout is a
// per-attempt limit, and it surfaces as a *url.Error wrapping
// context.DeadlineExceeded exactly like an expired request context -- so
// classifying every DeadlineExceeded as permanent made one slow first attempt
// fail the whole call, even where a second attempt would have succeeded. Use
// isTransientErrorCtx where the caller's context is available; this form
// assumes it is still live.
func isTransientError(err error) bool {
	return isTransientErrorCtx(context.Background(), err)
}

// isTransientErrorCtx is isTransientError with the caller's context, whose
// cancellation or expiry is the one deadline that ends the call.
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
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPut,
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
	maxAttempts := retryOpts.MaxRetries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := rewindBody(req); err != nil {
				return nil, err
			}

			delay := jitter(retryOpts.CalculateRetryDelay(attempt - 1))
			if lastRetryAfter > 0 {
				// Honour Retry-After, but never past the configured ceiling.
				// http.Client.Timeout does not cover this sleep, so an
				// unbounded value let a server returning "Retry-After: 86400"
				// suspend the call for a day.
				delay = lastRetryAfter
				if retryOpts.MaxRetryDelay > 0 && delay > retryOpts.MaxRetryDelay {
					delay = retryOpts.MaxRetryDelay
				}
			}

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			lastRetryAfter = 0
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
				lastRetryAfter = d
			}
			// Drain before closing so the connection can be reused.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
			_ = resp.Body.Close()
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
