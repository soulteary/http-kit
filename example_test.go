package httpkit_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	httpkit "github.com/soulteary/http-kit/v2"
)

// The common case: a client with a base URL, a request built against it, and
// the response.
func Example() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s %s (User-Agent: %s)", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	client, err := httpkit.NewClient(&httpkit.Options{
		BaseURL:   srv.URL,
		Timeout:   5 * time.Second,
		UserAgent: "myservice/1.0",
	})
	if err != nil {
		panic(err)
	}

	req, err := client.NewRequest(context.Background(), http.MethodGet, "v1/users", nil)
	if err != nil {
		panic(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.StatusCode)
	fmt.Println(string(body))
	// Output:
	// 200
	// GET /v1/users (User-Agent: myservice/1.0)
}

// BaseURL is optional. A caller that already holds absolute URLs -- a
// configuration loader fetching from several hosts, say -- passes them
// straight to NewRequest.
func ExampleClient_ResolveURL() {
	client, err := httpkit.NewClient(&httpkit.Options{BaseURL: "https://api.example.com/v1/"})
	if err != nil {
		panic(err)
	}

	for _, ref := range []string{"users", "/users", "users?limit=10", "https://other.example.org/raw"} {
		u, err := client.ResolveURL(ref)
		if err != nil {
			panic(err)
		}
		fmt.Println(u)
	}

	// No BaseURL: absolute references still resolve, relative ones are an error.
	bare, err := httpkit.NewClient(&httpkit.Options{})
	if err != nil {
		panic(err)
	}
	if _, err := bare.ResolveURL("users"); err != nil {
		fmt.Println("error:", err)
	}

	// Output:
	// https://api.example.com/v1/users
	// https://api.example.com/v1/users
	// https://api.example.com/v1/users?limit=10
	// https://other.example.org/raw
	// error: cannot resolve "users": it is not an absolute URL and the client has no BaseURL
}

// A Propagator adds headers to every request the client sends, so no call site
// has to remember to. For OpenTelemetry trace context, use otelprop.Global()
// from the otelprop subpackage; anything else is a function.
func ExamplePropagatorFunc() {
	type tenantKey struct{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("server saw X-Tenant:", r.Header.Get("X-Tenant"))
	}))
	defer srv.Close()

	client, err := httpkit.NewClient(&httpkit.Options{
		BaseURL: srv.URL,
		Propagator: httpkit.PropagatorFunc(func(ctx context.Context, h http.Header) {
			if tenant, ok := ctx.Value(tenantKey{}).(string); ok {
				h.Set("X-Tenant", tenant)
			}
		}),
	})
	if err != nil {
		panic(err)
	}

	ctx := context.WithValue(context.Background(), tenantKey{}, "acme")
	req, err := client.NewRequest(ctx, http.MethodGet, "/data", nil)
	if err != nil {
		panic(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	_ = resp.Body.Close()

	// Output:
	// server saw X-Tenant: acme
}

// Retries apply to idempotent requests with a replayable body. The delay grows
// as RetryDelay * BackoffMultiplier^attempt, capped at MaxRetryDelay, with up
// to 20% jitter subtracted -- so do not assert on exact timings.
func ExampleClient_DoRequestWithRetry() {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client, err := httpkit.NewClient(&httpkit.Options{BaseURL: srv.URL})
	if err != nil {
		panic(err)
	}

	req, err := client.NewRequest(context.Background(), http.MethodGet, "/flaky", nil)
	if err != nil {
		panic(err)
	}
	resp, err := client.DoRequestWithRetry(context.Background(), req, httpkit.DefaultRetryOptions())
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(attempts, resp.StatusCode, string(body))
	// Output:
	// 3 200 ok
}

// The backoff curve, before jitter. Past MaxRetryDelay every attempt waits the
// ceiling -- including when MaxRetryDelay is left at zero, which is a zero
// ceiling and not "no ceiling".
func ExampleRetryOptions_CalculateRetryDelay() {
	opts := httpkit.DefaultRetryOptions()
	for attempt := range 6 {
		fmt.Println(attempt, opts.CalculateRetryDelay(attempt))
	}
	// Output:
	// 0 100ms
	// 1 200ms
	// 2 400ms
	// 3 800ms
	// 4 1.6s
	// 5 2s
}
