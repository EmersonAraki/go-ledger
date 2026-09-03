package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/httpapi/problem"
	"github.com/EmersonAraki/go-ledger/internal/reconcile"
	"github.com/EmersonAraki/go-ledger/internal/storage/postgres"
)

// maxStatementSize caps an uploaded statement. Reconciliation reads the file a
// row at a time, but the request body still has to be bounded or a single
// upload can exhaust the process.
const maxStatementSize = 32 << 20 // 32 MiB

// statementReadTimeout is how long the upload alone may take to arrive. Sized so
// the size cap above is actually reachable over a slow connection.
const statementReadTimeout = 2 * time.Minute

// statementField is the multipart form field carrying the CSV.
const statementField = "file"

type discrepancyResponse struct {
	Kind          string         `json:"kind"`
	StatementRef  *string        `json:"statement_ref"`
	TransactionID *uuid.UUID     `json:"transaction_id"`
	Details       map[string]any `json:"details"`
}

func newDiscrepancyResponse(d reconcile.Discrepancy) discrepancyResponse {
	return discrepancyResponse{
		Kind:          d.Kind,
		StatementRef:  d.StatementRef,
		TransactionID: d.TransactionID,
		Details:       d.Details,
	}
}

type runResponse struct {
	ID               uuid.UUID             `json:"id"`
	SourceName       string                `json:"source_name"`
	StatementRows    int                   `json:"statement_rows"`
	MatchedCount     int                   `json:"matched_count"`
	DiscrepancyCount int                   `json:"discrepancy_count"`
	Clean            bool                  `json:"clean"`
	WindowStart      *string               `json:"window_start"`
	WindowEnd        *string               `json:"window_end"`
	CreatedAt        string                `json:"created_at"`
	Discrepancies    []discrepancyResponse `json:"discrepancies"`
	// NextCursor is the value to pass as ?after= for the next page, or null when
	// this is the last one.
	NextCursor *int64 `json:"next_cursor,omitempty"`
}

func newRunResponse(run *reconcile.Run, discrepancies []reconcile.Discrepancy, next *int64) runResponse {
	out := make([]discrepancyResponse, 0, len(discrepancies))
	for _, d := range discrepancies {
		out = append(out, newDiscrepancyResponse(d))
	}

	return runResponse{
		ID:               run.ID,
		SourceName:       run.SourceName,
		StatementRows:    run.StatementRows,
		MatchedCount:     run.MatchedCount,
		DiscrepancyCount: run.DiscrepancyCount,
		Clean:            run.Clean(),
		WindowStart:      formatTimePtr(run.WindowStart),
		WindowEnd:        formatTimePtr(run.WindowEnd),
		CreatedAt:        run.CreatedAt.UTC().Format(timeFormat),
		Discrepancies:    out,
		NextCursor:       next,
	}
}

// handleReconcile accepts a CSV statement and compares it against the ledger.
//
// The ledger is never modified: a real correction is a reversing transaction
// posted through the normal API. This endpoint only reports.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	// The server's ReadTimeout is sized for small JSON bodies. A statement is
	// orders of magnitude larger, and at the default a slow upload would be cut
	// off mid-body -- the client seeing a dropped connection rather than a
	// problem+json. Extend the deadline for this handler alone rather than
	// raising it for every endpoint.
	if err := http.NewResponseController(w).SetReadDeadline(
		time.Now().Add(statementReadTimeout)); err != nil {
		slog.WarnContext(r.Context(), "could not extend read deadline for statement upload",
			"error", err)
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxStatementSize)

	file, header, err := r.FormFile(statementField)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			problem.Write(w, http.StatusRequestEntityTooLarge, "statement_too_large",
				"Statement Too Large", "the uploaded statement exceeds 32 MiB")
			return
		}
		problem.Write(w, http.StatusBadRequest, "statement_required", "Statement Required",
			`a multipart form with a CSV file in the "`+statementField+`" field is required`)
		return
	}
	defer func() { _ = file.Close() }()

	rows, parseErrors, err := reconcile.ParseStatement(file)
	if err != nil {
		// The file itself is unusable -- no header, or missing columns. That is a
		// bad request, distinct from individual rows failing to parse, which are
		// reported as findings.
		problem.Write(w, http.StatusBadRequest, "statement_unreadable",
			"Statement Unreadable", err.Error())
		return
	}

	run, err := s.reconciler.Reconcile(r.Context(), header.Filename, rows, parseErrors, reconcile.Options{})
	if err != nil {
		problem.Internal(w, err)
		return
	}

	// Return at most a page of findings, matching what GET would serve, so a run
	// with thousands of them does not produce one enormous response. The full set
	// is stored and paginated through GET /reconciliation/{id}.
	shown := run.Discrepancies
	var next *int64
	if len(shown) > defaultDiscrepancyPage {
		shown = shown[:defaultDiscrepancyPage]
		// Findings are stored in order, so the first page ends at the Nth row.
		cursor := int64(defaultDiscrepancyPage)
		next = &cursor
	}
	writeJSON(w, http.StatusCreated, newRunResponse(run, shown, next))
}

// handleGetReconciliation returns a stored run and a page of its findings.
func (s *Server) handleGetReconciliation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}

	after, ok := parseCursor(w, r.URL.Query().Get("after"))
	if !ok {
		return
	}
	limit, ok := parseLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}

	run, err := s.reconciler.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, postgres.ErrRunNotFound) {
			problem.Write(w, http.StatusNotFound, "reconciliation_run_not_found",
				"Reconciliation Run Not Found", err.Error())
			return
		}
		problem.Internal(w, err)
		return
	}

	discrepancies, last, hasMore, err := s.reconciler.ListDiscrepancies(r.Context(), id, after, limit)
	if err != nil {
		problem.Internal(w, err)
		return
	}

	var next *int64
	if hasMore {
		next = &last
	}

	writeJSON(w, http.StatusOK, newRunResponse(run, discrepancies, next))
}

func parseCursor(w http.ResponseWriter, raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		problem.Write(w, http.StatusBadRequest, "invalid_cursor", "Invalid Cursor",
			"after must be a non-negative integer")
		return 0, false
	}
	return v, true
}

// defaultDiscrepancyPage is the page size when the client does not choose one.
const defaultDiscrepancyPage = 100

func parseLimit(w http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return defaultDiscrepancyPage, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > 500 {
		problem.Write(w, http.StatusBadRequest, "invalid_limit", "Invalid Limit",
			"limit must be between 1 and 500")
		return 0, false
	}
	return v, true
}
