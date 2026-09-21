package httpkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// headerRecorder captures the headers of every request it serves.
type headerRecorder struct {
	*httptest.Server
	seen []http.Header
}

func newHeaderRecorder(t *testing.T, handle func(w http.ResponseWriter, attempt int)) *headerRecorder {
	t.Helper()
	rec := &headerRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.seen = append(rec.seen, r.Header.Clone())
		if handle != nil {
			handle(w, len(rec.seen))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func mustClient(t *testing.T, opts *Options) *Client {
	t.Helper()
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestDoInjectsConfiguredPropagator is the core of the hook: the propagator is
// applied by Do, so no call site has to remember to apply it. Forgetting the
// old InjectTraceContext broke a trace with no error anywhere.
func TestDoInjectsConfiguredPropagator(t *testing.T) {
	srv := newHeaderRecorder(t, nil)

	client := mustClient(t, &Options{
		BaseURL: srv.URL,
		Propagator: PropagatorFunc(func(ctx context.Context, h http.Header) {
			h.Set("X-Trace", ctx.Value(traceKey{}).(string))
		}),
	})

	req, err := client.NewRequest(context.WithValue(context.Background(), traceKey{}, "abc123"), http.MethodGet, "/data", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if len(srv.seen) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(srv.seen))
	}
	if got := srv.seen[0].Get("X-Trace"); got != "abc123" {
		t.Errorf("X-Trace = %q, want %q", got, "abc123")
	}
}

type traceKey struct{}

// TestDoInjectsOnEveryRetryAttempt: a retried request is a new request on the
// wire, so it needs its own headers. Injecting once before the loop would
// leave every attempt after the first carrying the first attempt's context.
func TestDoInjectsOnEveryRetryAttempt(t *testing.T) {
	srv := newHeaderRecorder(t, func(w http.ResponseWriter, attempt int) {
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	var n atomic.Int64
	client := mustClient(t, &Options{
		BaseURL: srv.URL,
		Propagator: PropagatorFunc(func(_ context.Context, h http.Header) {
			h.Set("X-Attempt", strconv.FormatInt(n.Add(1), 10))
		}),
	})

	req, err := client.NewRequest(context.Background(), http.MethodGet, "/flaky", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.DoRequestWithRetry(context.Background(), req, &RetryOptions{
		MaxRetries:           3,
		RetryDelay:           time.Millisecond,
		MaxRetryDelay:        5 * time.Millisecond,
		BackoffMultiplier:    2,
		RetryableStatusCodes: []int{http.StatusServiceUnavailable},
	})
	if err != nil {
		t.Fatalf("DoRequestWithRetry: %v", err)
	}
	_ = resp.Body.Close()

	if len(srv.seen) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(srv.seen))
	}
	for i, h := range srv.seen {
		want := strconv.Itoa(i + 1)
		if got := h.Get("X-Attempt"); got != want {
			t.Errorf("attempt %d carried X-Attempt=%q, want %q -- headers were not refreshed", i+1, got, want)
		}
	}
}

// TestDoWithoutPropagatorSendsNothingExtra: the hook is opt-in, and a client
// that sets none must behave exactly as before.
func TestDoWithoutPropagatorSendsNothingExtra(t *testing.T) {
	srv := newHeaderRecorder(t, nil)
	client := mustClient(t, &Options{BaseURL: srv.URL})

	req, err := client.NewRequest(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if got := srv.seen[0].Get("Traceparent"); got != "" {
		t.Errorf("unexpected Traceparent %q on a client with no propagator", got)
	}
}

// TestInjectContextIsANoOpWithoutAPropagator pins that the exported method is
// safe to call unconditionally -- a caller sending a request through
// GetHTTPClient should not have to check first.
func TestInjectContextIsANoOpWithoutAPropagator(t *testing.T) {
	client := mustClient(t, &Options{BaseURL: "http://example.com"})
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	req.Header.Set("X-Custom", "kept")

	client.InjectContext(context.Background(), req)

	if len(req.Header) != 1 || req.Header.Get("X-Custom") != "kept" {
		t.Errorf("headers = %v, want the caller's single header untouched", req.Header)
	}
	client.InjectContext(context.Background(), nil) // must not panic
}

// TestInjectContextCreatesAMissingHeaderMap: a *http.Request built as a struct
// literal has a nil Header, and Header.Set on a nil map panics.
func TestInjectContextCreatesAMissingHeaderMap(t *testing.T) {
	client := mustClient(t, &Options{
		BaseURL: "http://example.com",
		Propagator: PropagatorFunc(func(_ context.Context, h http.Header) {
			h.Set("X-Trace", "v")
		}),
	})

	req := &http.Request{Method: http.MethodGet}
	client.InjectContext(context.Background(), req)

	if got := req.Header.Get("X-Trace"); got != "v" {
		t.Errorf("X-Trace = %q, want %q", got, "v")
	}
}

// TestDoAcceptsARequestWithNoHeaderMap is the regression test for the same nil
// map reaching Do: setting the User-Agent panicked before anything was sent.
// Reading a nil map is fine, which is why it went unnoticed.
func TestDoAcceptsARequestWithNoHeaderMap(t *testing.T) {
	srv := newHeaderRecorder(t, nil)
	client := mustClient(t, &Options{BaseURL: srv.URL, UserAgent: "kit/1"})

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp, err := client.Do(&http.Request{Method: http.MethodGet, URL: u})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if got := srv.seen[0].Get("User-Agent"); got != "kit/1" {
		t.Errorf("User-Agent = %q, want %q", got, "kit/1")
	}
}

// TestDoInjectsTheRequestsOwnContext: Do has no ctx parameter, so it uses
// req.Context() -- which DoRequestWithRetry has already set to the caller's.
func TestDoInjectsTheRequestsOwnContext(t *testing.T) {
	srv := newHeaderRecorder(t, nil)
	client := mustClient(t, &Options{
		BaseURL: srv.URL,
		Propagator: PropagatorFunc(func(ctx context.Context, h http.Header) {
			v, _ := ctx.Value(traceKey{}).(string)
			h.Set("X-Trace", v)
		}),
	})

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	ctx := context.WithValue(context.Background(), traceKey{}, "from-retry-caller")

	resp, err := client.DoRequestWithRetry(ctx, req, &RetryOptions{MaxRetries: 0})
	if err != nil {
		t.Fatalf("DoRequestWithRetry: %v", err)
	}
	_ = resp.Body.Close()

	if got := srv.seen[0].Get("X-Trace"); got != "from-retry-caller" {
		t.Errorf("X-Trace = %q, want the context DoRequestWithRetry was given", got)
	}
}

// TestNilPropagatorFuncIsANoOp: PropagatorFunc(nil) is a non-nil Propagator
// that Client cannot tell apart from a real one, so the guard belongs here
// rather than at every call site.
func TestNilPropagatorFuncIsANoOp(t *testing.T) {
	var f PropagatorFunc
	h := http.Header{}
	f.Inject(context.Background(), h) // must not panic
	if len(h) != 0 {
		t.Errorf("headers = %v, want none", h)
	}

	srv := newHeaderRecorder(t, nil)
	client := mustClient(t, &Options{BaseURL: srv.URL, Propagator: PropagatorFunc(nil)})
	req, err := client.NewRequest(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	// And through MultiPropagator, which keeps it as a live entry.
	h = http.Header{}
	MultiPropagator(PropagatorFunc(nil), PropagatorFunc(func(_ context.Context, h http.Header) {
		h.Set("X-A", "1")
	})).Inject(context.Background(), h)
	if h.Get("X-A") != "1" {
		t.Errorf("headers = %v, want the live entry to have run", h)
	}
}

func TestMultiPropagator(t *testing.T) {
	set := func(k, v string) Propagator {
		return PropagatorFunc(func(_ context.Context, h http.Header) { h.Set(k, v) })
	}

	t.Run("applies each in order", func(t *testing.T) {
		var order []string
		record := func(name string) Propagator {
			return PropagatorFunc(func(_ context.Context, _ http.Header) { order = append(order, name) })
		}
		h := http.Header{}
		MultiPropagator(record("a"), record("b"), record("c")).Inject(context.Background(), h)
		if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
			t.Errorf("order = %v, want [a b c]", order)
		}
	})

	t.Run("skips nil entries", func(t *testing.T) {
		h := http.Header{}
		MultiPropagator(nil, set("X-A", "1"), nil, set("X-B", "2")).Inject(context.Background(), h)
		if h.Get("X-A") != "1" || h.Get("X-B") != "2" {
			t.Errorf("headers = %v, want both set", h)
		}
	})

	t.Run("no live entry yields nil", func(t *testing.T) {
		// Not a non-nil interface holding an empty slice: Options.Propagator is
		// compared against nil, so that would make "no propagators" look
		// configured.
		if p := MultiPropagator(); p != nil {
			t.Errorf("MultiPropagator() = %v, want nil", p)
		}
		if p := MultiPropagator(nil, nil); p != nil {
			t.Errorf("MultiPropagator(nil, nil) = %v, want nil", p)
		}
	})

	t.Run("single entry is returned unwrapped", func(t *testing.T) {
		only := set("X-A", "1")
		if got := MultiPropagator(nil, only); got == nil {
			t.Fatal("MultiPropagator dropped its only live entry")
		}
		h := http.Header{}
		MultiPropagator(nil, only).Inject(context.Background(), h)
		if h.Get("X-A") != "1" {
			t.Errorf("headers = %v, want X-A set", h)
		}
	})
}
