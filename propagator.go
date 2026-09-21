package httpkit

import (
	"context"
	"net/http"
)

// Propagator injects cross-process context into an outgoing request's headers:
// W3C trace context, B3, baggage, a tenant or request ID -- whatever the
// surrounding system carries between services.
//
// It is the seam that keeps this package free of any particular tracing
// library. Client.InjectTraceContext used to call otel.GetTextMapPropagator
// directly, which meant every user of this package linked OpenTelemetry --
// its four modules and 130-odd packages -- whether or not they traced
// anything. The OpenTelemetry implementation now lives in the otelprop
// subpackage and is reached through this interface:
//
//	client, err := httpkit.NewClient(&httpkit.Options{
//		BaseURL:    "https://api.example.com",
//		Propagator: otelprop.Global(),
//	})
//
// Header rather than *http.Request on purpose: injecting context means setting
// headers, and an interface that asks for no more than that can be satisfied
// by anything -- including a plain function, via PropagatorFunc.
//
// Inject must be safe for concurrent use: one Client serves many goroutines,
// and Client.Do calls it once per attempt.
type Propagator interface {
	Inject(ctx context.Context, h http.Header)
}

// PropagatorFunc adapts a plain function to Propagator, so a one-line
// convention needs no type of its own:
//
//	Propagator: httpkit.PropagatorFunc(func(ctx context.Context, h http.Header) {
//		if id, ok := ctx.Value(requestIDKey).(string); ok {
//			h.Set("X-Request-ID", id)
//		}
//	}),
type PropagatorFunc func(ctx context.Context, h http.Header)

// Inject calls f, and does nothing when f is nil.
//
// A nil func is reachable by accident -- PropagatorFunc(cfg.Inject) with an
// unset field is a non-nil Propagator wrapping nothing, which Client cannot
// tell apart from a real one. Calling it would panic on the first request
// rather than at configuration time, which is the worst place to find out.
func (f PropagatorFunc) Inject(ctx context.Context, h http.Header) {
	if f == nil {
		return
	}
	f(ctx, h)
}

// MultiPropagator returns a Propagator that applies each of ps in order.
//
// Trace context and an application's own headers usually come from different
// places, and composing them should not require writing an adapter type. Nil
// entries are skipped; with no non-nil entry the result is nil, which Client
// treats as "no propagation" rather than as an error.
func MultiPropagator(ps ...Propagator) Propagator {
	live := make([]Propagator, 0, len(ps))
	for _, p := range ps {
		if p != nil {
			live = append(live, p)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	return multiPropagator(live)
}

type multiPropagator []Propagator

func (m multiPropagator) Inject(ctx context.Context, h http.Header) {
	for _, p := range m {
		p.Inject(ctx, h)
	}
}
