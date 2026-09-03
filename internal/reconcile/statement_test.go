package reconcile_test

import (
	"strings"
	"testing"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

const header = "external_ref,posted_at,debit_account_id,credit_account_id,amount,currency\n"

const (
	accA = "11111111-1111-4111-8111-111111111111"
	accB = "22222222-2222-4222-8222-222222222222"
)

func TestParseStatementReadsRows(t *testing.T) {
	t.Parallel()

	in := header +
		"TRX-1,2026-09-01T10:00:00Z," + accA + "," + accB + ",12345,BRL\n" +
		"TRX-2,2026-09-02," + accB + "," + accA + ",500,BRL\n"

	rows, bad, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("got %d unparseable rows, want 0: %+v", len(bad), bad)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	if rows[0].ExternalRef != "TRX-1" || rows[0].Amount != 12345 || rows[0].Currency != "BRL" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[0].Line != 2 {
		t.Errorf("row 0 line = %d, want 2 (1-based, header is line 1)", rows[0].Line)
	}
	// A bare date is accepted; producers commonly send one.
	if rows[1].PostedAt.Format("2006-01-02") != "2026-09-02" {
		t.Errorf("row 1 posted_at = %v", rows[1].PostedAt)
	}
}

// Column order is the producer's choice; only the names matter.
func TestParseStatementAcceptsAnyColumnOrder(t *testing.T) {
	t.Parallel()

	in := "amount,currency,external_ref,credit_account_id,debit_account_id,posted_at\n" +
		"999,BRL,TRX-9," + accB + "," + accA + ",2026-09-01\n"

	rows, _, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 1 || rows[0].Amount != 999 || rows[0].ExternalRef != "TRX-9" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].DebitAccountID.String() != accA {
		t.Errorf("debit account = %s, want %s", rows[0].DebitAccountID, accA)
	}
}

// One bad line must not hide the rest of the file.
func TestParseStatementReportsBadRowsAndKeepsGoing(t *testing.T) {
	t.Parallel()

	in := header +
		"TRX-1,2026-09-01," + accA + "," + accB + ",100,BRL\n" +
		"TRX-2,not-a-date," + accA + "," + accB + ",100,BRL\n" +
		"TRX-3,2026-09-01,not-a-uuid," + accB + ",100,BRL\n" +
		"TRX-4,2026-09-01," + accA + "," + accB + ",abc,BRL\n" +
		"TRX-5,2026-09-01," + accA + "," + accB + ",100,BRL\n"

	rows, bad, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d good rows, want 2 (the first and the last)", len(rows))
	}
	if len(bad) != 3 {
		t.Fatalf("got %d bad rows, want 3", len(bad))
	}
	for _, d := range bad {
		if d.Kind != reconcile.KindUnparseableRow {
			t.Errorf("kind = %q, want %q", d.Kind, reconcile.KindUnparseableRow)
		}
		if d.Details["line"] == nil || d.Details["reason"] == nil {
			t.Errorf("bad row is missing line or reason: %+v", d.Details)
		}
	}
}

// A decimal amount means the producer is using major units. Accepting it would
// misreconcile by a factor of 100, silently.
func TestParseStatementRejectsDecimalAmounts(t *testing.T) {
	t.Parallel()

	in := header + "TRX-1,2026-09-01," + accA + "," + accB + ",123.45,BRL\n"

	rows, bad, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a decimal amount was accepted: %+v", rows)
	}
	if len(bad) != 1 {
		t.Fatalf("got %d findings, want 1", len(bad))
	}
	if reason, _ := bad[0].Details["reason"].(string); !strings.Contains(reason, "minor units") {
		t.Errorf("reason = %q, want it to explain minor units", reason)
	}
}

func TestParseStatementRejectsUnusableFiles(t *testing.T) {
	t.Parallel()

	if _, _, err := reconcile.ParseStatement(strings.NewReader("")); err == nil {
		t.Error("expected an empty file to be rejected")
	}

	// Missing a required column: every row would be meaningless, so this is a
	// file-level failure rather than a per-row finding.
	in := "external_ref,posted_at,amount\nTRX-1,2026-09-01,100\n"
	_, _, err := reconcile.ParseStatement(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected a missing-column file to be rejected")
	}
	for _, want := range []string{"debit_account_id", "credit_account_id", "currency"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the missing column %q: %v", want, err)
		}
	}
}

// Rejection branches that had no coverage. The negative-amount case matters
// most: a refund line is a plausible real statement row, and bucketing it as
// unparseable rather than reconciling it is a decision worth pinning down.
func TestParseStatementRejectsInvalidFieldValues(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"negative amount":     "TRX-1,2026-09-01," + accA + "," + accB + ",-500,BRL",
		"zero amount":         "TRX-1,2026-09-01," + accA + "," + accB + ",0,BRL",
		"same account":        "TRX-1,2026-09-01," + accA + "," + accA + ",100,BRL",
		"two-letter currency": "TRX-1,2026-09-01," + accA + "," + accB + ",100,BR",
		"empty currency":      "TRX-1,2026-09-01," + accA + "," + accB + ",100,",
		"empty date":          "TRX-1,," + accA + "," + accB + ",100,BRL",
	}

	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			rows, bad, err := reconcile.ParseStatement(strings.NewReader(header + line + "\n"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("row was accepted: %+v", rows)
			}
			if len(bad) != 1 || bad[0].Kind != reconcile.KindUnparseableRow {
				t.Errorf("findings = %+v, want one unparseable_row", bad)
			}
		})
	}
}

// A structurally broken line (bad quoting) must be reported, not abort the file.
func TestParseStatementSurvivesStructuralDamage(t *testing.T) {
	t.Parallel()

	in := header +
		"TRX-1,2026-09-01," + accA + "," + accB + ",100,BRL\n" +
		"\"unterminated,2026-09-01," + accA + "," + accB + ",100,BRL\n" +
		"TRX-3,2026-09-01," + accA + "," + accB + ",300,BRL\n"

	rows, bad, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("a structurally broken line must not fail the whole file: %v", err)
	}
	if len(bad) == 0 {
		t.Error("the broken line was not reported")
	}
	if len(rows) == 0 {
		t.Error("no good rows survived a single broken line")
	}
}

// Excel writes a UTF-8 BOM. Without stripping it the first column looks absent.
func TestParseStatementStripsByteOrderMark(t *testing.T) {
	t.Parallel()

	in := "\ufeff" + header + "TRX-1,2026-09-01," + accA + "," + accB + ",100,BRL\n"

	rows, _, err := reconcile.ParseStatement(strings.NewReader(in))
	if err != nil {
		t.Fatalf("a BOM-prefixed statement was rejected: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalRef != "TRX-1" {
		t.Errorf("rows = %+v", rows)
	}
}

// An unbounded statement must not produce unbounded work. Without a cap, a body
// of comma-only lines expands into hundreds of megabytes of findings.
func TestParseStatementCapsRunawayInput(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(header)
	for range reconcile.MaxStatementRows + 5_000 {
		b.WriteString(",,,,,\n")
	}

	rows, bad, err := reconcile.ParseStatement(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows from garbage input, want 0", len(rows))
	}

	// Individual findings are capped, plus one truncation finding.
	if len(bad) > reconcile.MaxReportedParseErrors+1 {
		t.Errorf("collected %d findings, want at most %d",
			len(bad), reconcile.MaxReportedParseErrors+1)
	}

	var truncation *reconcile.Discrepancy
	for i := range bad {
		if bad[i].Kind == reconcile.KindStatementTruncated {
			truncation = &bad[i]
		}
	}
	if truncation == nil {
		t.Fatal("truncation was not reported; a partial read must never look complete")
	}
	if truncation.Details["unreported_parse_errors"] == nil {
		t.Errorf("truncation finding does not say how much was suppressed: %+v", truncation.Details)
	}
}
