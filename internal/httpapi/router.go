// Package httpapi translates HTTP requests into domain calls and domain errors
// into problem+json responses. It holds no business logic of its own.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
)

// RequestTimeout bounds how long a single handler may run. It is exported
// because http.Server's WriteTimeout must be strictly greater: if the two are
// equal, the connection's write deadline expires at the same instant the
// timeout response is written, and the client sees a dropped connection
// instead of the error.
const RequestTimeout = 30 * time.Second

// Server wires dependencies into HTTP handlers.
type Server struct {
	pool *pgxpool.Pool
}

// NewServer builds the API handler set.
func NewServer(pool *pgxpool.Pool) *Server {
	return &Server{pool: pool}
}

// Routes returns the fully-configured HTTP handler.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// Deliberately no middleware.RealIP: it rewrites RemoteAddr from
	// X-Forwarded-For / X-Real-IP without verifying the hop is a trusted proxy,
	// so any client can forge it. Add it back only behind a trusted-proxy
	// allowlist, when something actually needs the client IP.
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	// A request that outlives this deadline is a bug; fail it rather than let
	// it pin a database connection indefinitely. This deadline is also what
	// bounds a pgxpool Acquire on the request path.
	r.Use(middleware.Timeout(RequestTimeout))

	// Unmatched routes must speak problem+json too, so clients only ever have
	// one error shape to parse.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, http.StatusNotFound, "not_found", "Not Found",
			"No route matches "+r.Method+" "+r.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method Not Allowed",
			r.Method+" is not supported on "+r.URL.Path)
	})

	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	return r
}

// handleHealth reports process liveness. It deliberately touches nothing
// external, so a database outage does not get the process killed.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports whether the process can actually serve traffic, which
// means the database must be reachable.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		slog.WarnContext(ctx, "readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write json response", "error", err)
	}
}

// requestLogger emits one structured line per request, carrying the request ID
// so a client-reported failure can be traced to its log entry.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		slog.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
