package handler

import "net/http"

// Middleware wraps an http.Handler to add cross-cutting behavior (auth,
// logging, metrics, tracing) without RegisterRoutes needing to know about
// it. This is the extension point a closed-source layer built on xpro-oss
// uses to add auth or observability on top — decorate, don't fork.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware in the order given: Chain(a, b, c)(h) runs as
// a(b(c(h))) — a is outermost, so a request hits a first. Chain() with no
// arguments is the identity: it returns h unchanged.
func Chain(mws ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}
