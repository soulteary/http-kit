// Package otelprop propagates OpenTelemetry context on http-kit requests.
//
// It lives in its own package so that importing the root package does not drag
// OpenTelemetry -- go.opentelemetry.io/otel, its metric and trace modules and
// go.opentelemetry.io/auto/sdk, plus go-logr and cespare/xxhash behind them --
// into binaries that never emit a span. A service that only wants a retrying,
// mTLS-capable HTTP client pays nothing for tracing support existing; only
// importing this package links it in.
//
//	client, err := httpkit.NewClient(&httpkit.Options{
//		BaseURL:    "https://api.example.com",
//		Propagator: otelprop.Global(),
//	})
//
// From there every request the client sends carries the headers the
// configured propagator writes. Nothing else has to be remembered at the call
// site, which is what httpkit.Client.InjectTraceContext required and what made
// a missing traceparent a silent failure.
//
// The package is a translation layer and nothing more: which propagators are
// installed, what a span is and whether anything is sampled all remain
// OpenTelemetry's business.
package otelprop

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	httpkit "github.com/soulteary/http-kit/v2"
)

// Global returns a propagator that resolves otel.GetTextMapPropagator at each
// injection, which is what httpkit.Client.InjectTraceContext did.
//
// Resolving late is the part that matters: otel.SetTextMapPropagator is
// usually called from main, and a client built during package initialisation
// or by a constructor that runs earlier must still pick it up. A propagator
// captured at construction time would freeze OpenTelemetry's no-op default and
// inject nothing, for the whole life of the process, with no error anywhere.
func Global() httpkit.Propagator {
	return httpkit.PropagatorFunc(func(ctx context.Context, h http.Header) {
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
	})
}

// New returns a propagator backed by a specific TextMapPropagator, for a
// service that configures propagation per client rather than globally:
//
//	otelprop.New(propagation.TraceContext{})
//	otelprop.New(propagation.NewCompositeTextMapPropagator(
//		propagation.TraceContext{}, propagation.Baggage{},
//	))
//
// A nil p yields nil, which httpkit.Client treats as "no propagation" rather
// than as an error -- and never as a panic on the first request, which is what
// handing a nil interface straight through would cost.
func New(p propagation.TextMapPropagator) httpkit.Propagator {
	if p == nil {
		return nil
	}
	return httpkit.PropagatorFunc(func(ctx context.Context, h http.Header) {
		p.Inject(ctx, propagation.HeaderCarrier(h))
	})
}
