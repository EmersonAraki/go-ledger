package reconcile_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EmersonAraki/go-ledger/internal/reconcile"
)

var (
	debit  = uuid.MustParse(accA)
	credit = uuid.MustParse(accB)
	base   = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
)

func ref(s string) *string { return &s }

func row(r string, amount int64, at time.Time) reconcile.StatementRow {
	return reconcile.StatementRow{
		Line: 2, ExternalRef: r, PostedAt: at,
		DebitAccountID: debit, CreditAccountID: credit,
		Amount: amount, Currency: "BRL",
	}
}

func tx(id uuid.UUID, r *string, amount int64, at time.Time) reconcile.LedgerTransaction {
	return reconcile.LedgerTransaction{
		ID: id, ExternalRef: r, PostedAt: at,
		DebitAccountID: debit, CreditAccountID: credit,
		Amount: amount, Currency: "BRL",
	}
}

// kinds counts discrepancies by kind, which is what most assertions care about.
func kinds(ds []reconcile.Discrepancy) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[d.Kind]++
	}
	return out
}

func TestMatchAgreesWhenEverythingLinesUp(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	matched, found := reconcile.Match(
		[]reconcile.StatementRow{row("TRX-1", 100, base)},
		[]reconcile.LedgerTransaction{tx(id, ref("TRX-1"), 100, base)},
		reconcile.Options{})

	if matched != 1 {
		t.Errorf("matched = %d, want 1", matched)
	}
	if len(found) != 0 {
		t.Errorf("expected no discrepancies, got %+v", found)
	}
}

func TestMatchReportsEveryFieldThatDisagrees(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	statement := row("TRX-1", 100, base)
	statement.Currency = "USD"
	statement.DebitAccountID = uuid.New() // different account
	statement.PostedAt = base.Add(72 * time.Hour)

	ledger := tx(id, ref("TRX-1"), 250, base)

	matched, found := reconcile.Match(
		[]reconcile.StatementRow{statement},
		[]reconcile.LedgerTransaction{ledger},
		reconcile.Options{})

	if matched != 0 {
		t.Errorf("matched = %d, want 0", matched)
	}

	// All four are reported together: an operator fixing an integration wants
	// the whole picture, not one symptom per run.
	got := kinds(found)
	for _, want := range []string{
		reconcile.KindAmountMismatch,
		reconcile.KindCurrencyMismatch,
		reconcile.KindAccountMismatch,
		reconcile.KindDateMismatch,
	} {
		if got[want] != 1 {
			t.Errorf("kind %s appeared %d times, want 1 (all: %v)", want, got[want], got)
		}
	}

	for _, d := range found {
		if d.Kind == reconcile.KindAmountMismatch {
			if d.Details["difference"] != int64(-150) {
				t.Errorf("amount difference = %v, want -150", d.Details["difference"])
			}
		}
	}
}

// A statement stamped with the settlement date rather than the posting moment is
// normal, not a discrepancy.
func TestMatchToleratesSmallDateDifferences(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	_, found := reconcile.Match(
		[]reconcile.StatementRow{row("TRX-1", 100, base.Add(6*time.Hour))},
		[]reconcile.LedgerTransaction{tx(id, ref("TRX-1"), 100, base)},
		reconcile.Options{})

	if len(found) != 0 {
		t.Errorf("a 6-hour difference should be within tolerance, got %+v", found)
	}
}

func TestMatchReportsMissingOnBothSides(t *testing.T) {
	t.Parallel()

	onlyInStatement := row("TRX-ONLY-STATEMENT", 100, base)
	onlyInLedger := tx(uuid.New(), ref("TRX-ONLY-LEDGER"), 200, base)

	matched, found := reconcile.Match(
		[]reconcile.StatementRow{onlyInStatement},
		[]reconcile.LedgerTransaction{onlyInLedger},
		reconcile.Options{})

	if matched != 0 {
		t.Errorf("matched = %d, want 0", matched)
	}
	got := kinds(found)
	if got[reconcile.KindMissingInLedger] != 1 {
		t.Errorf("missing_in_ledger = %d, want 1 (all: %v)", got[reconcile.KindMissingInLedger], got)
	}
	if got[reconcile.KindMissingInStatement] != 1 {
		t.Errorf("missing_in_statement = %d, want 1 (all: %v)", got[reconcile.KindMissingInStatement], got)
	}
}

// Without a shared reference, a same-shape pairing is a guess. It must be
// surfaced for a human rather than counted as reconciled.
func TestMatchFlagsProbableMatchesInsteadOfReconcilingThem(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	matched, found := reconcile.Match(
		[]reconcile.StatementRow{row("", 100, base)}, // no external reference
		[]reconcile.LedgerTransaction{tx(id, nil, 100, base)},
		reconcile.Options{})

	if matched != 0 {
		t.Errorf("matched = %d; a probable match must not count as reconciled", matched)
	}
	got := kinds(found)
	if got[reconcile.KindProbableMatch] != 1 {
		t.Fatalf("probable_match = %d, want 1 (all: %v)", got[reconcile.KindProbableMatch], got)
	}
	// And it must not also be reported as missing on either side.
	if got[reconcile.KindMissingInLedger] != 0 || got[reconcile.KindMissingInStatement] != 0 {
		t.Errorf("a probable match was double-reported as missing: %v", got)
	}

	for _, d := range found {
		if d.Kind == reconcile.KindProbableMatch && (d.TransactionID == nil || *d.TransactionID != id) {
			t.Errorf("probable match does not name the transaction it paired with: %+v", d)
		}
	}
}

func TestMatchFlagsDuplicateReferencesInOneStatement(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	first := row("TRX-1", 100, base)
	second := row("TRX-1", 100, base)
	second.Line = 3

	matched, found := reconcile.Match(
		[]reconcile.StatementRow{first, second},
		[]reconcile.LedgerTransaction{tx(id, ref("TRX-1"), 100, base)},
		reconcile.Options{})

	if matched != 1 {
		t.Errorf("matched = %d, want 1 (the first occurrence)", matched)
	}
	got := kinds(found)
	if got[reconcile.KindDuplicateInStatement] != 1 {
		t.Errorf("duplicate_in_statement = %d, want 1 (all: %v)", got[reconcile.KindDuplicateInStatement], got)
	}
}

// One ledger transaction must not satisfy two statement rows.
func TestMatchDoesNotReuseALedgerTransaction(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	a := row("", 100, base)
	b := row("", 100, base)
	b.Line = 3

	_, found := reconcile.Match(
		[]reconcile.StatementRow{a, b},
		[]reconcile.LedgerTransaction{tx(id, nil, 100, base)},
		reconcile.Options{})

	got := kinds(found)
	if got[reconcile.KindProbableMatch] != 1 {
		t.Errorf("probable_match = %d, want 1 (all: %v)", got[reconcile.KindProbableMatch], got)
	}
	if got[reconcile.KindMissingInLedger] != 1 {
		t.Errorf("missing_in_ledger = %d, want 1 -- the second row has nothing left to pair with (all: %v)",
			got[reconcile.KindMissingInLedger], got)
	}
}

func TestWindowSpansTheStatement(t *testing.T) {
	t.Parallel()

	if start, end := reconcile.Window(nil); start != nil || end != nil {
		t.Error("an empty statement should have no window")
	}

	rows := []reconcile.StatementRow{
		row("a", 1, base.Add(48*time.Hour)),
		row("b", 1, base),
		row("c", 1, base.Add(24*time.Hour)),
	}
	start, end := reconcile.Window(rows)
	if start == nil || end == nil {
		t.Fatal("expected a window")
	}

	// Rounded outward to whole days: statement timestamps are coarse, and a
	// window narrower than the period the document describes would under-report.
	wantStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 3, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v (start of the first day)", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v (end of the last day)", end, wantEnd)
	}

	// Every row must fall inside the window it produced.
	for _, r := range rows {
		if r.PostedAt.Before(*start) || r.PostedAt.After(*end) {
			t.Errorf("row at %v falls outside its own statement window [%v, %v]",
				r.PostedAt, start, end)
		}
	}
}
