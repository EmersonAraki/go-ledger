package reconcile

import (
	"time"

	"github.com/google/uuid"
)

// LedgerTransaction is the ledger's side of the comparison: a transfer reduced
// to the fields a statement can speak about.
type LedgerTransaction struct {
	ID              uuid.UUID
	ExternalRef     *string
	PostedAt        time.Time
	DebitAccountID  uuid.UUID
	CreditAccountID uuid.UUID
	Amount          int64
	Currency        string
}

// BalanceDrift is an account whose cached balance disagrees with the sum of its
// entries.
type BalanceDrift struct {
	AccountID uuid.UUID
	Name      string
	Cached    int64
	Derived   int64
}

// DefaultDateTolerance is how far a statement's posting date may differ from the
// ledger's before it counts as a mismatch. Statements are commonly stamped on
// the settlement date rather than the moment of posting, so a day of slack is
// normal rather than suspicious.
const DefaultDateTolerance = 24 * time.Hour

// Options tunes matching.
type Options struct {
	// DateTolerance overrides DefaultDateTolerance when non-zero.
	DateTolerance time.Duration
	// HeuristicWindow is how far apart a probable match may be. Defaults to
	// DateTolerance when zero.
	HeuristicWindow time.Duration
}

func (o Options) dateTolerance() time.Duration {
	if o.DateTolerance > 0 {
		return o.DateTolerance
	}
	return DefaultDateTolerance
}

func (o Options) heuristicWindow() time.Duration {
	if o.HeuristicWindow > 0 {
		return o.HeuristicWindow
	}
	return o.dateTolerance()
}

// Match compares a statement against the ledger.
//
// Two passes, deliberately in this order:
//
//  1. Exact, by external reference. A shared reference is the counterparty
//     asserting "this is the same movement", so a mismatch here is a real
//     disagreement about a known transfer and is reported field by field.
//  2. Heuristic, by shape -- same accounts, amount and currency within a time
//     window -- for rows with no reference or no reference match. These are
//     reported as probable_match rather than counted as reconciled: two
//     transfers that merely look alike are not necessarily the same one, and
//     deciding otherwise is a human's call.
//
// Anything still unpaired is missing from one side or the other.
func Match(rows []StatementRow, ledger []LedgerTransaction, opts Options) (matched int, discrepancies []Discrepancy) {
	byRef := make(map[string]*LedgerTransaction, len(ledger))
	for i := range ledger {
		if ref := ledger[i].ExternalRef; ref != nil && *ref != "" {
			byRef[*ref] = &ledger[i]
		}
	}

	pairedLedger := make(map[uuid.UUID]bool, len(ledger))
	seenRefs := make(map[string]int, len(rows))
	var unpairedRows []StatementRow

	// Pass 1: exact match on external reference.
	for _, row := range rows {
		if row.ExternalRef == "" {
			unpairedRows = append(unpairedRows, row)
			continue
		}

		// A reference repeated within one statement is the statement
		// contradicting itself; pair only the first and flag the rest.
		if seenRefs[row.ExternalRef] > 0 {
			discrepancies = append(discrepancies, Discrepancy{
				Kind:         KindDuplicateInStatement,
				StatementRef: stringPtr(row.ExternalRef),
				Details: map[string]any{
					"line":            row.Line,
					"first_seen_line": seenRefs[row.ExternalRef],
					"amount":          row.Amount,
				},
			})
			continue
		}
		seenRefs[row.ExternalRef] = row.Line

		tx, ok := byRef[row.ExternalRef]
		if !ok {
			unpairedRows = append(unpairedRows, row)
			continue
		}

		pairedLedger[tx.ID] = true
		if found := compare(row, *tx, opts.dateTolerance()); len(found) > 0 {
			discrepancies = append(discrepancies, found...)
			continue
		}
		matched++
	}

	// Pass 2: heuristic pairing on shape.
	var stillUnpaired []StatementRow
	for _, row := range unpairedRows {
		candidate := findByShape(row, ledger, pairedLedger, opts.heuristicWindow())
		if candidate == nil {
			stillUnpaired = append(stillUnpaired, row)
			continue
		}

		pairedLedger[candidate.ID] = true
		discrepancies = append(discrepancies, Discrepancy{
			Kind:          KindProbableMatch,
			StatementRef:  stringPtr(row.ExternalRef),
			TransactionID: uuidPtr(candidate.ID),
			Details: map[string]any{
				"line":                row.Line,
				"reason":              "matched on accounts, amount and currency without a shared external reference",
				"statement_posted_at": row.PostedAt.UTC(),
				"ledger_posted_at":    candidate.PostedAt.UTC(),
				"amount":              row.Amount,
			},
		})
	}

	// Statement rows the ledger knows nothing about.
	for _, row := range stillUnpaired {
		discrepancies = append(discrepancies, Discrepancy{
			Kind:         KindMissingInLedger,
			StatementRef: stringPtr(row.ExternalRef),
			Details: map[string]any{
				"line":              row.Line,
				"posted_at":         row.PostedAt.UTC(),
				"debit_account_id":  row.DebitAccountID,
				"credit_account_id": row.CreditAccountID,
				"amount":            row.Amount,
				"currency":          row.Currency,
			},
		})
	}

	// Ledger transactions the statement does not mention. The caller has already
	// restricted `ledger` to the statement's window, so everything left here is
	// genuinely unaccounted for rather than merely out of period.
	for i := range ledger {
		tx := ledger[i]
		if pairedLedger[tx.ID] {
			continue
		}
		discrepancies = append(discrepancies, Discrepancy{
			Kind:          KindMissingInStatement,
			StatementRef:  refOrEmpty(tx.ExternalRef),
			TransactionID: uuidPtr(tx.ID),
			Details: map[string]any{
				"posted_at":         tx.PostedAt.UTC(),
				"debit_account_id":  tx.DebitAccountID,
				"credit_account_id": tx.CreditAccountID,
				"amount":            tx.Amount,
				"currency":          tx.Currency,
			},
		})
	}

	return matched, discrepancies
}

// compare reports every way a referenced pair disagrees. All fields are checked
// rather than returning on the first difference: an operator fixing a broken
// integration wants the whole picture, not one symptom at a time.
func compare(row StatementRow, tx LedgerTransaction, tolerance time.Duration) []Discrepancy {
	var out []Discrepancy
	ref := stringPtr(row.ExternalRef)

	if row.Amount != tx.Amount {
		out = append(out, Discrepancy{
			Kind: KindAmountMismatch, StatementRef: ref, TransactionID: uuidPtr(tx.ID),
			Details: map[string]any{
				"line": row.Line, "statement_amount": row.Amount, "ledger_amount": tx.Amount,
				"difference": row.Amount - tx.Amount,
			},
		})
	}
	if row.Currency != tx.Currency {
		out = append(out, Discrepancy{
			Kind: KindCurrencyMismatch, StatementRef: ref, TransactionID: uuidPtr(tx.ID),
			Details: map[string]any{
				"line": row.Line, "statement_currency": row.Currency, "ledger_currency": tx.Currency,
			},
		})
	}
	if row.DebitAccountID != tx.DebitAccountID || row.CreditAccountID != tx.CreditAccountID {
		out = append(out, Discrepancy{
			Kind: KindAccountMismatch, StatementRef: ref, TransactionID: uuidPtr(tx.ID),
			Details: map[string]any{
				"line":                     row.Line,
				"statement_debit_account":  row.DebitAccountID,
				"statement_credit_account": row.CreditAccountID,
				"ledger_debit_account":     tx.DebitAccountID,
				"ledger_credit_account":    tx.CreditAccountID,
			},
		})
	}
	if drift := row.PostedAt.Sub(tx.PostedAt); drift > tolerance || drift < -tolerance {
		out = append(out, Discrepancy{
			Kind: KindDateMismatch, StatementRef: ref, TransactionID: uuidPtr(tx.ID),
			Details: map[string]any{
				"line":                row.Line,
				"statement_posted_at": row.PostedAt.UTC(),
				"ledger_posted_at":    tx.PostedAt.UTC(),
				"difference_seconds":  int64(drift.Seconds()),
				"tolerance_seconds":   int64(tolerance.Seconds()),
			},
		})
	}
	return out
}

// findByShape looks for an unpaired ledger transaction that moves the same
// money between the same accounts at about the same time.
func findByShape(row StatementRow, ledger []LedgerTransaction,
	paired map[uuid.UUID]bool, window time.Duration) *LedgerTransaction {
	for i := range ledger {
		tx := &ledger[i]
		if paired[tx.ID] {
			continue
		}
		if tx.Amount != row.Amount || tx.Currency != row.Currency {
			continue
		}
		if tx.DebitAccountID != row.DebitAccountID || tx.CreditAccountID != row.CreditAccountID {
			continue
		}
		if d := row.PostedAt.Sub(tx.PostedAt); d > window || d < -window {
			continue
		}
		return tx
	}
	return nil
}

// DriftDiscrepancies turns balance drift findings into discrepancies.
func DriftDiscrepancies(drifts []BalanceDrift) []Discrepancy {
	out := make([]Discrepancy, 0, len(drifts))
	for _, d := range drifts {
		out = append(out, Discrepancy{
			Kind: KindBalanceDrift,
			Details: map[string]any{
				"account_id":      d.AccountID,
				"account_name":    d.Name,
				"cached_balance":  d.Cached,
				"derived_balance": d.Derived,
				"difference":      d.Cached - d.Derived,
			},
		})
	}
	return out
}

// Window returns the period a statement covers, or nils when it has no rows.
func Window(rows []StatementRow) (start, end *time.Time) {
	if len(rows) == 0 {
		return nil, nil
	}
	lo, hi := rows[0].PostedAt, rows[0].PostedAt
	for _, r := range rows[1:] {
		if r.PostedAt.Before(lo) {
			lo = r.PostedAt
		}
		if r.PostedAt.After(hi) {
			hi = r.PostedAt
		}
	}
	lo, hi = lo.UTC(), hi.UTC()
	return &lo, &hi
}

func refOrEmpty(ref *string) *string {
	if ref == nil || *ref == "" {
		return nil
	}
	return ref
}
