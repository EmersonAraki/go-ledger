package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EmersonAraki/go-ledger/internal/httpapi"
	"github.com/EmersonAraki/go-ledger/internal/platform/pgtest"
)

func TestHealthzIsAlwaysOK(t *testing.T) {
	t.Parallel()

	// Liveness must not depend on the database, so a nil pool is fine here:
	// if this handler ever touches the pool, this test panics and says so.
	srv := httpapi.NewServer(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want \"ok\"", body["status"])
	}
}

func TestReadyzReportsReadyWhenDatabaseIsUp(t *testing.T) {
	t.Parallel()

	pool := pgtest.Pool(t)
	srv := httpapi.NewServer(pool)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// The point of a readiness probe is to fail when the dependency is gone.
// A probe that only ever returns 200 is worse than no probe at all.
func TestReadyzReportsUnavailableWhenDatabaseIsDown(t *testing.T) {
	t.Parallel()

	pool := pgtest.Pool(t)
	srv := httpapi.NewServer(pool)

	// Close the pool out from under the handler to simulate a database outage.
	pool.Close()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the database is unreachable", rec.Code)
	}
}

func TestUnknownRouteReturnsProblemJSON(t *testing.T) {
	t.Parallel()

	srv := httpapi.NewServer(nil)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var p struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if p.Type != "not_found" || p.Status != http.StatusNotFound {
		t.Errorf("problem = %+v, want type=not_found status=404", p)
	}
}

func TestWrongMethodReturnsProblemJSON(t *testing.T) {
	t.Parallel()

	srv := httpapi.NewServer(nil)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}
