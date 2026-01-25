package httpkit

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

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

// IsRetryableError checks if an error should trigger a retry
func (r *RetryOptions) IsRetryableError(err error, statusCode int) bool {
	if r.MaxRetries == 0 {
		return false
	}

	// Network errors are always retryable
	if err != nil {
		return true
	}

	// Check if status code is in retryable list
	for _, code := range r.RetryableStatusCodes {
		if statusCode == code {
			return true
		}
	}

	return false
}

// CalculateRetryDelay calculates the delay for the next retry attempt using exponential backoff
func (r *RetryOptions) CalculateRetryDelay(attempt int) time.Duration {
	delay := time.Duration(float64(r.RetryDelay) * float64(attempt+1) * r.BackoffMultiplier)
	if delay > r.MaxRetryDelay {
		delay = r.MaxRetryDelay
	}
	return delay
}

// DoRequestWithRetry performs an HTTP request with retry logic
func (c *Client) DoRequestWithRetry(ctx context.Context, req *http.Request, retryOpts *RetryOptions) (*http.Response, error) {
	if retryOpts == nil {
		retryOpts = DefaultRetryOptions()
	}

	var lastErr error

	// Initial attempt + retries
	maxAttempts := retryOpts.MaxRetries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Calculate delay before retry
			delay := retryOpts.CalculateRetryDelay(attempt - 1)

			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Make the request
		resp, err := c.Do(req)
		if err != nil {
			lastErr = err
			if !retryOpts.IsRetryableError(err, 0) {
				return nil, fmt.Errorf("failed to execute request: %w", err)
			}
			if attempt >= retryOpts.MaxRetries {
				return nil, fmt.Errorf("failed to execute request after retries: %w", lastErr)
			}
			continue
		}

		// Check if status code is retryable and we have retries left
		if retryOpts.IsRetryableError(nil, resp.StatusCode) && attempt < retryOpts.MaxRetries {
			// Close response body before retry
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("server error: status %d", resp.StatusCode)
			continue
		}

		// Success or non-retryable error or last attempt - return response
		return resp, nil
	}

	// This is only reached if maxAttempts is 0 (MaxRetries = -1)
	if lastErr != nil {
		return nil, fmt.Errorf("failed after retries: %w", lastErr)
	}
	return nil, fmt.Errorf("no attempts made")
}
