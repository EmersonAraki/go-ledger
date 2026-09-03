// Package reconcile compares an external statement against the ledger and
// reports where they disagree.
//
// Reconciliation is strictly read-only against the ledger. It never corrects
// anything: a real correction is a reversing transaction posted through the
// normal API, with its own entries and its own audit trail. A reconciliation job
// that silently adjusted balances would destroy the very property the ledger
// exists to provide.
package reconcile

import (
	"time"

	"github.com/google/uuid"
)

// Discrepancy kinds. These are reported to operators and stored, so they are
// contract: renaming one breaks saved reports and any dashboard reading them.
const (
	// KindMissingInLedger is a statement row with no corresponding transaction.
	// Money the counterparty says moved that this ledger has no record of.
	KindMissingInLedger = "missing_in_ledger"

	// KindMissingInStatement is a ledger transaction inside the statement's
	// window that the statement does not mention.
	KindMissingInStatement = "missing_in_statement"

	// KindAmountMismatch means the reference matched but the amounts differ.
	KindAmountMismatch = "amount_mismatch"

	// KindCurrencyMismatch means the reference matched but the currencies differ.
	KindCurrencyMismatch = "currency_mismatch"

	// KindAccountMismatch means the reference matched but the money moved
	// between different accounts.
	KindAccountMismatch = "account_mismatch"

	// KindDateMismatch means the reference matched but the posting dates differ
	// by more than the allowed tolerance.
	KindDateMismatch = "date_mismatch"

	// KindDuplicateInStatement means one external reference appears on more than
	// one statement row.
	KindDuplicateInStatement = "duplicate_in_statement"

	// KindUnparseableRow means a row could not be read. Reported rather than
	// aborting the run, so one bad line does not hide every other finding.
	KindUnparseableRow = "unparseable_row"

	// KindProbableMatch is a heuristic pairing made without a shared reference.
	// Reported rather than silently reconciled: a human decides whether two
	// movements that merely look alike are the same one.
	KindProbableMatch = "probable_match"

	// KindStatementTruncated means the statement exceeded a processing limit and
	// was only partly read. Reported as a finding rather than an error so the
	// partial result is still usable, and so the report can never silently claim
	// to have compared more than it did.
	KindStatementTruncated = "statement_truncated"

	// KindUnreconcilableTransaction is a ledger transaction inside the statement's
	// window with a shape the statement format cannot express -- more than two
	// legs. It is reported rather than skipped: silently excluding it would let
	// the job announce a clean period while money it never examined had moved,
	// which is precisely the failure this job exists to catch.
	KindUnreconcilableTransaction = "unreconcilable_transaction"

	// KindBalanceDrift means an account's cached balance disagrees with the sum
	// of its entries. Not a statement finding at all -- an internal integrity
	// check that rides along, because this job already holds a consistent
	// snapshot of the whole ledger.
	KindBalanceDrift = "balance_drift"
)

// Processing limits.
//
// The parser reads rows one at a time, but its OUTPUT is proportional to the
// input: an unbounded file yields unbounded slices of rows and findings, each
// far larger than the bytes that produced it. A 32 MiB body of empty comma-only
// lines expands into hundreds of megabytes of retained heap, and there is no
// per-client limit on this endpoint. These caps bound the work a single upload
// can cause.
const (
	// MaxStatementRows is the most data rows read from one statement.
	MaxStatementRows = 100_000

	// MaxFindings is the most discrepancies stored and returned for one run.
	// Beyond it the report is truncated: a run with more findings than this is
	// telling the operator something systemic, and listing every instance helps
	// nobody while making the response and the insert unbounded.
	MaxFindings = 10_000

	// MaxReportedParseErrors is the most individual unparseable-row findings
	// collected. Beyond it the remainder are counted, not listed: a file that is
	// wrong on every line needs one clear answer, not a hundred thousand copies
	// of it.
	MaxReportedParseErrors = 1_000
)

// Discrepancy is one disagreement worth an operator's attention.
type Discrepancy struct {
	Kind string
	// StatementRef is the external reference involved, when there is one.
	StatementRef *string
	// TransactionID is the ledger transaction involved, when there is one.
	TransactionID *uuid.UUID
	// Details carries the specifics -- expected versus actual -- as free-form
	// JSON, because what matters differs per kind.
	Details map[string]any
}

// Run is the result of one reconciliation.
type Run struct {
	ID               uuid.UUID
	SourceName       string
	StatementRows    int
	MatchedCount     int
	DiscrepancyCount int
	// WindowStart and WindowEnd bound the period the statement covers, derived
	// from its rows. Ledger transactions outside it are not this statement's
	// business and are not reported as missing.
	WindowStart *time.Time
	WindowEnd   *time.Time
	CreatedAt   time.Time

	Discrepancies []Discrepancy
}

// Clean reports whether the statement and the ledger agreed completely.
func (r Run) Clean() bool { return r.DiscrepancyCount == 0 }

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }
