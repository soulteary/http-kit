package otelprop_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	httpkit "github.com/soulteary/http-kit/v2"
	"github.com/soulteary/http-kit/v2/otelprop"
)

// ctxWithSpan returns a context carrying a valid, sampled span context, which
// is what makes a propagator write a traceparent at all. Injecting without one
// writes nothing -- the old test in the root package never established one, so
// it asserted only that the call did not panic.
func ctxWithSpan(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("trace ID: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("span ID: %v", err)
	}
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
}

const wantTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestNewInjectsTraceContext(t *testing.T) {
	h := http.Header{}
	otelprop.New(propagation.TraceContext{}).Inject(ctxWithSpan(t), h)

	if got := h.Get("Traceparent"); got != wantTraceparent {
		t.Errorf("Traceparent = %q, want %q", got, wantTraceparent)
	}
}

func TestNewInjectsBaggageFromAComposite(t *testing.T) {
	member, err := baggage.NewMember("tenant", "acme")
	if err != nil {
		t.Fatalf("baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage: %v", err)
	}
	ctx := baggage.ContextWithBaggage(ctxWithSpan(t), bag)

	h := http.Header{}
	otelprop.New(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)).Inject(ctx, h)

	if got := h.Get("Traceparent"); got != wantTraceparent {
		t.Errorf("Traceparent = %q, want %q", got, wantTraceparent)
	}
	if got := h.Get("Baggage"); !strings.Contains(got, "tenant=acme") {
		t.Errorf("Baggage = %q, want it to carry tenant=acme", got)
	}
}

// TestNewWithNilYieldsNil: httpkit.Client compares Options.Propagator against
// nil, so a non-nil interface wrapping a nil propagator would look configured
// and then panic on the first request.
func TestNewWithNilYieldsNil(t *testing.T) {
	if p := otelprop.New(nil); p != nil {
		t.Fatalf("New(nil) = %v, want nil", p)
	}

	client, err := httpkit.NewClient(&httpkit.Options{
		BaseURL:    "http://example.com",
		Propagator: otelprop.New(nil),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	client.InjectContext(context.Background(), req) // must not panic
	if len(req.Header) != 0 {
		t.Errorf("headers = %v, want none", req.Header)
	}
}

// TestGlobalResolvesTheGlobalPropagatorLate is the behaviour
// httpkit.Client.InjectTraceContext had and that a captured propagator would
// lose: otel.SetTextMapPropagator is normally called from main, after the
// clients a package-level constructor built. Freezing OpenTelemetry's no-op
// default at construction time injects nothing, for the life of the process,
// with no error anywhere.
func TestGlobalResolvesTheGlobalPropagatorLate(t *testing.T) {
	restore := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(restore) })

	// Built while the global is still OpenTelemetry's no-op default.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	p := otelprop.Global()

	ctx := ctxWithSpan(t)
	h := http.Header{}
	p.Inject(ctx, h)
	if got := h.Get("Traceparent"); got != "" {
		t.Fatalf("Traceparent = %q before a propagator was installed, want none", got)
	}

	otel.SetTextMapPropagator(propagation.TraceContext{})

	h = http.Header{}
	p.Inject(ctx, h)
	if got := h.Get("Traceparent"); got != wantTraceparent {
		t.Errorf("Traceparent = %q, want %q -- the global was resolved at construction time", got, wantTraceparent)
	}
}

// TestGlobalOnAClientReachesTheWire is the end-to-end shape from the README:
// set the propagator on the client and every request carries the headers,
// with nothing to remember at the call site.
func TestGlobalOnAClientReachesTheWire(t *testing.T) {
	restore := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(restore) })
	otel.SetTextMapPropagator(propagation.TraceContext{})

	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := httpkit.NewClient(&httpkit.Options{
		BaseURL:    srv.URL,
		Propagator: otelprop.Global(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, err := client.NewRequest(ctxWithSpan(t), http.MethodGet, "/data", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()

	if got := seen.Get("Traceparent"); got != wantTraceparent {
		t.Errorf("server saw Traceparent %q, want %q", got, wantTraceparent)
	}
}

// TestGlobalComposesWithAnApplicationHeader: trace context and a service's own
// convention come from different places, and composing them should not need an
// adapter type.
func TestGlobalComposesWithAnApplicationHeader(t *testing.T) {
	restore := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(restore) })
	otel.SetTextMapPropagator(propagation.TraceContext{})

	p := httpkit.MultiPropagator(
		otelprop.Global(),
		httpkit.PropagatorFunc(func(_ context.Context, h http.Header) {
			h.Set("X-Request-ID", "req-1")
		}),
	)

	h := http.Header{}
	p.Inject(ctxWithSpan(t), h)

	if got := h.Get("Traceparent"); got != wantTraceparent {
		t.Errorf("Traceparent = %q, want %q", got, wantTraceparent)
	}
	if got := h.Get("X-Request-ID"); got != "req-1" {
		t.Errorf("X-Request-ID = %q, want %q", got, "req-1")
	}
}
