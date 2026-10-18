package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChain_Order(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := Chain(mw("a"), mw("b"), mw("c"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := []string{"a", "b", "c", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestChain_EmptyIsIdentity(t *testing.T) {
	called := false
	h := Chain()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("handler was not called")
	}
}

// TestRegisterRoutes_HealthzBypassesMiddleware confirms /healthz stays a
// bare liveness probe while /process and /status go through the supplied
// chain — a middleware that rejects everything must not affect /healthz.
func TestRegisterRoutes_HealthzBypassesMiddleware(t *testing.T) {
	var wrapped bool
	blockAll := Middleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped = true
			w.WriteHeader(http.StatusForbidden)
		})
	})

	h := &Handlers{} // zero-value is enough: healthz/status don't touch cfg/target
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, blockAll)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 (should bypass middleware)", rec.Code)
	}
	if wrapped {
		t.Fatal("middleware ran on /healthz, want it to bypass entirely")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/status status = %d, want 403 (middleware should have run)", rec.Code)
	}
}
