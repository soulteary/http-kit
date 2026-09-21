package otelprop_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	httpkit "github.com/soulteary/http-kit/v2"
	"github.com/soulteary/http-kit/v2/otelprop"
)

// Set the propagator once, on the client, and every request it sends carries
// the trace headers. The former Client.InjectTraceContext had to be called at
// each call site, and a forgotten one broke the trace with no error anywhere.
func Example() {
	// Normally done once in main.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("traceparent:", r.Header.Get("Traceparent"))
	}))
	defer srv.Close()

	client, err := httpkit.NewClient(&httpkit.Options{
		BaseURL:    srv.URL,
		Propagator: otelprop.Global(),
	})
	if err != nil {
		panic(err)
	}

	req, err := client.NewRequest(exampleSpanContext(), http.MethodGet, "/data", nil)
	if err != nil {
		panic(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	_ = resp.Body.Close()

	// Output:
	// traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
}

// New pins a propagator per client, for a service that does not configure one
// globally. MultiPropagator composes it with the service's own headers.
func ExampleNew() {
	p := httpkit.MultiPropagator(
		otelprop.New(propagation.TraceContext{}),
		httpkit.PropagatorFunc(func(_ context.Context, h http.Header) {
			h.Set("X-Request-ID", "req-1")
		}),
	)

	h := http.Header{}
	p.Inject(exampleSpanContext(), h)

	fmt.Println(h.Get("Traceparent"))
	fmt.Println(h.Get("X-Request-ID"))
	// Output:
	// 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
	// req-1
}

// exampleSpanContext stands in for a context carrying a live span. A
// propagator writes nothing without one.
func exampleSpanContext() context.Context {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
}
